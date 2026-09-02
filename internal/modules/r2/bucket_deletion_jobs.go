package r2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/audit"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
)

const (
	bucketDeletionStageFenced   = "fenced"
	bucketDeletionStageSettled  = "settled"
	bucketDeletionStageClearing = "clearing"
	bucketDeletionStageDeleting = "deleting_bucket"
	bucketDeletionMaxRounds     = 10000
)

type BucketDeletionJobs struct {
	Service Service
	Jobs    *jobs.Store
	Remote  BucketDeletionRemote
	Clear   BucketClearBackend
	Audit   *audit.Store
}

func (h BucketDeletionJobs) Handle(ctx context.Context, job jobs.Job) error {
	payload, err := decodeBucketDeletionJob(job)
	if err != nil {
		return jobs.NewFailure("bucket_identity_unverifiable", err, true)
	}
	fenced := h.ownsFence(ctx, job, payload)
	if err := h.validateDependencies(payload); err != nil {
		return h.fail(ctx, job, payload, fenced, jobs.NewFailure("bucket_identity_unverifiable", err, true))
	}
	if err := validateBucketDeletionJobMetadata(job, payload); err != nil {
		return h.fail(ctx, job, payload, fenced, jobs.NewFailure("bucket_identity_unverifiable", err, true))
	}
	if payload.Jurisdiction != "default" {
		return h.fail(ctx, job, payload, fenced, permanentFailure("unsupported_jurisdiction"))
	}
	account, err := h.Service.Accounts.Get(ctx, payload.AccountID, true)
	if err != nil {
		return h.fail(ctx, job, payload, fenced, jobs.NewFailure("permission_denied", errors.New("无法读取账号凭据，任务已停止"), true))
	}
	if account.CloudflareAccountID != payload.CloudflareAccountID {
		return h.fail(ctx, job, payload, fenced, permanentFailure("bucket_identity_changed"))
	}
	if payload.LocalBucketID == "" {
		if err := h.discoverLocalBucket(ctx, job.ID, &payload); err != nil {
			return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(err, "local_finalize_failed"))
		}
		fenced = h.ownsFence(ctx, job, payload)
	}

	missing, err := h.verifyIdentity(ctx, account, payload)
	if err != nil {
		return h.fail(ctx, job, payload, fenced, err)
	}
	if missing && h.localFinalizationAlreadyComplete(ctx, payload) {
		h.recordAudit(ctx, job, payload, "success", "")
		return nil
	}
	fenced, err = h.fence(ctx, job, &payload)
	if err != nil {
		return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(err, "bucket_busy"))
	}
	if missing {
		if err := h.markRemoteMutated(ctx, job.ID, &payload); err != nil {
			return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(err, "local_finalize_failed"))
		}
		if err := h.settleLocalAfterRemoteMissing(ctx, job, &payload); err != nil {
			return h.fail(ctx, job, payload, fenced, err)
		}
		stillMissing, verifyErr := h.verifyIdentity(ctx, account, payload)
		if verifyErr != nil {
			return h.fail(ctx, job, payload, fenced, verifyErr)
		}
		if !stillMissing {
			return h.fail(ctx, job, payload, fenced, retryableFailure("cloudflare_unavailable"))
		}
		return h.finalize(ctx, job, &payload, fenced)
	}

	if payload.Mode == BucketDeletionEmptyOnly {
		if err := h.settleLocalWithoutMutation(ctx, job, &payload); err != nil {
			return h.fail(ctx, job, payload, fenced, err)
		}
		page, listErr := h.Remote.ListR2Objects(ctx, account.CloudflareAccountID, account.APIToken,
			payload.Jurisdiction, payload.BucketName, "", 1)
		if listErr != nil {
			if isRemoteBucketNotFound(listErr) {
				stillMissing, verifyErr := h.verifyIdentity(ctx, account, payload)
				if verifyErr != nil {
					return h.fail(ctx, job, payload, fenced, verifyErr)
				}
				if stillMissing {
					return h.finalize(ctx, job, &payload, fenced)
				}
				return h.fail(ctx, job, payload, fenced, retryableFailure("cloudflare_unavailable"))
			}
			return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(listErr, "cloudflare_unavailable"))
		}
		if len(page.Objects) != 0 {
			return h.fail(ctx, job, payload, fenced, permanentFailure("bucket_not_empty"))
		}
	} else {
		// Persist a write-ahead marker before settlement can abort multipart
		// uploads or recover an interrupted remote write.
		if err := h.markRemoteMutated(ctx, job.ID, &payload); err != nil {
			return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(err, "local_finalize_failed"))
		}
		if err := h.settleLocal(ctx, job, &payload); err != nil {
			return h.fail(ctx, job, payload, fenced, err)
		}
		if err := h.clearRemote(ctx, job, account, &payload); err != nil {
			return h.fail(ctx, job, payload, fenced, err)
		}
	}

	payload.Stage = bucketDeletionStageDeleting
	if err := h.persist(ctx, job.ID, payload, .92); err != nil {
		return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(err, "local_finalize_failed"))
	}
	missing, err = h.verifyIdentity(ctx, account, payload)
	if err != nil {
		return h.fail(ctx, job, payload, fenced, err)
	}
	if missing {
		return h.finalize(ctx, job, &payload, fenced)
	}
	// The DELETE result can be ambiguous on timeout or a 5xx response. Record
	// the possible remote mutation before sending it so retries never reopen a
	// managed bucket whose remote bucket may already be gone.
	remoteMutatedBeforeDelete := payload.RemoteMutated
	if err := h.markRemoteMutated(ctx, job.ID, &payload); err != nil {
		return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(err, "local_finalize_failed"))
	}
	err = h.Remote.DeleteR2Bucket(ctx, account.CloudflareAccountID, account.APIToken,
		payload.Jurisdiction, payload.BucketName)
	if err != nil {
		if isRemoteBucketNotFound(err) {
			missing, verifyErr := h.verifyIdentity(ctx, account, payload)
			if verifyErr != nil {
				return h.fail(ctx, job, payload, fenced, verifyErr)
			}
			if missing {
				return h.finalize(ctx, job, &payload, fenced)
			}
			return h.fail(ctx, job, payload, fenced, retryableFailure("cloudflare_unavailable"))
		}
		if isAmbiguousBucketDelete(err) {
			missing, verifyErr := h.verifyIdentity(ctx, account, payload)
			if verifyErr != nil {
				return h.fail(ctx, job, payload, fenced, verifyErr)
			}
			if missing {
				return h.finalize(ctx, job, &payload, fenced)
			}
		}
		if isBucketNotEmpty(err) {
			if payload.Mode == BucketDeletionEmptyOnly && !remoteMutatedBeforeDelete {
				payload.RemoteMutated = false
				if persistErr := h.Jobs.SetPayload(ctx, job.ID, payload); persistErr != nil {
					payload.RemoteMutated = true
					return h.fail(ctx, job, payload, fenced, jobs.NewFailure("local_finalize_failed",
						errors.New("Cloudflare 已明确拒绝删除，但本地状态保存失败，请重试任务"), true))
				}
			}
			code := "bucket_not_empty"
			if payload.Mode == BucketDeletionEmptyAndDelete {
				page, listErr := h.Remote.ListR2Objects(ctx, account.CloudflareAccountID, account.APIToken,
					payload.Jurisdiction, payload.BucketName, "", 1)
				if listErr != nil {
					return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(listErr, "cloudflare_unavailable"))
				}
				if len(page.Objects) != 0 || (account.R2AccessKeyID != "" && account.R2SecretAccessKey != "" && h.Clear != nil) {
					code = "external_writes_detected"
				} else {
					code = "s3_credentials_required"
				}
			}
			return h.fail(ctx, job, payload, fenced, permanentFailure(code))
		}
		return h.fail(ctx, job, payload, fenced, classifyBucketDeletionError(err, "cloudflare_unavailable"))
	}
	return h.finalize(ctx, job, &payload, fenced)
}

func validateBucketDeletionJobMetadata(job jobs.Job, payload BucketDeletionPayload) error {
	resourceKey := fmt.Sprintf("%s/%s/%s", payload.AccountID, payload.Jurisdiction, payload.BucketName)
	if job.ResourceKey != "" && job.ResourceKey != resourceKey {
		return errors.New("bucket deletion resource key does not match its payload")
	}
	if job.ParentJobID != payload.ParentJobID {
		return errors.New("bucket deletion parent job does not match its payload")
	}
	return nil
}

func (h BucketDeletionJobs) ownsFence(ctx context.Context, job jobs.Job, payload BucketDeletionPayload) bool {
	if payload.LocalBucketID == "" || h.Service.Index == nil {
		return false
	}
	bucket, err := h.Service.Index.GetBucket(ctx, payload.LocalBucketID)
	return err == nil && bucket.AccountID == payload.AccountID && bucket.Name == payload.BucketName &&
		bucket.LifecycleState == BucketDeleting && bucket.DeletionJobID == job.ID
}

func (h BucketDeletionJobs) localFinalizationAlreadyComplete(ctx context.Context, payload BucketDeletionPayload) bool {
	if payload.LocalBucketID == "" || h.Service.Index == nil {
		return false
	}
	_, err := h.Service.Index.GetBucket(ctx, payload.LocalBucketID)
	return errors.Is(err, ErrBucketNotFound)
}

func (h BucketDeletionJobs) discoverLocalBucket(ctx context.Context, jobID string, payload *BucketDeletionPayload) error {
	bucket, err := h.Service.Index.GetBucketByAccountAndName(ctx, payload.AccountID, payload.BucketName)
	if errors.Is(err, ErrBucketNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	payload.LocalBucketID = bucket.ID
	return h.Jobs.SetPayload(ctx, jobID, *payload)
}

func decodeBucketDeletionJob(job jobs.Job) (BucketDeletionPayload, error) {
	var payload BucketDeletionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return payload, fmt.Errorf("decode bucket deletion job: %w", err)
	}
	if payload.Jurisdiction == "" {
		payload.Jurisdiction = "default"
	}
	if payload.AccountID == "" || payload.CloudflareAccountID == "" || payload.BucketName == "" ||
		(payload.ExpectedCreationDate == "" && !payload.RemoteMissingAtEnqueue) {
		return payload, errors.New("bucket deletion identity is incomplete")
	}
	if payload.Mode != BucketDeletionEmptyOnly && payload.Mode != BucketDeletionEmptyAndDelete {
		return payload, errors.New("bucket deletion mode is invalid")
	}
	return payload, nil
}

func (h BucketDeletionJobs) validateDependencies(payload BucketDeletionPayload) error {
	if h.Service.Accounts == nil || h.Service.Index == nil || h.Remote == nil || h.Jobs == nil {
		return errors.New("bucket deletion worker is not configured")
	}
	return nil
}

func (h BucketDeletionJobs) verifyIdentity(
	ctx context.Context,
	account accounts.Account,
	payload BucketDeletionPayload,
) (bool, error) {
	current, err := h.Remote.GetR2Bucket(ctx, account.CloudflareAccountID, account.APIToken,
		payload.Jurisdiction, payload.BucketName)
	if err != nil {
		if isRemoteBucketNotFound(err) {
			return true, nil
		}
		return false, classifyBucketDeletionError(err, "cloudflare_unavailable")
	}
	if current.Name != payload.BucketName || normalizedJurisdiction(current.Jurisdiction) != payload.Jurisdiction {
		return false, permanentFailure("bucket_identity_changed")
	}
	if payload.RemoteMissingAtEnqueue {
		return false, permanentFailure("bucket_identity_changed")
	}
	expected, expectedErr := time.Parse(time.RFC3339Nano, payload.ExpectedCreationDate)
	actual, actualErr := time.Parse(time.RFC3339Nano, current.CreationDate)
	if expectedErr != nil || actualErr != nil || current.CreationDate == "" {
		return false, permanentFailure("bucket_identity_unverifiable")
	}
	if !expected.UTC().Equal(actual.UTC()) {
		return false, permanentFailure("bucket_identity_changed")
	}
	return false, nil
}

func normalizedJurisdiction(value string) string {
	if value == "" {
		return "default"
	}
	return value
}

func (h BucketDeletionJobs) fence(ctx context.Context, job jobs.Job, payload *BucketDeletionPayload) (bool, error) {
	if payload.LocalBucketID == "" {
		return false, nil
	}
	bucket, err := h.Service.Index.GetBucket(ctx, payload.LocalBucketID)
	if err != nil {
		return false, err
	}
	if bucket.AccountID != payload.AccountID || bucket.Name != payload.BucketName {
		return false, permanentFailure("bucket_identity_changed")
	}
	if bucket.LifecycleState == BucketDeleting && bucket.DeletionJobID == job.ID {
		if payload.Stage == "" {
			payload.Stage = bucketDeletionStageFenced
			return true, h.persist(ctx, job.ID, *payload, .08)
		}
		return true, nil
	}
	if _, err := h.Service.Index.BeginBucketDeletion(ctx, bucket.ID, job.ID, payload.ParentJobID); err != nil {
		return false, err
	}
	payload.Stage = bucketDeletionStageFenced
	return true, h.persist(ctx, job.ID, *payload, .08)
}

func (h BucketDeletionJobs) settleLocal(ctx context.Context, job jobs.Job, payload *BucketDeletionPayload) error {
	if payload.LocalBucketID == "" {
		return nil
	}
	uploads, err := h.Service.Index.listBucketDeletionMultipart(ctx, payload.LocalBucketID)
	if err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	for _, upload := range uploads {
		if err := h.Service.AbortMultipart(ctx, upload.Key, upload.ID); err != nil && !errors.Is(err, ErrMultipartNotFound) {
			if strings.Contains(err.Error(), "does not have R2 S3 credentials") {
				return permanentFailure("s3_credentials_required")
			}
			return classifyBucketDeletionError(err, "bucket_busy")
		}
	}
	active, err := h.Service.Index.HasDeletionBlockingActivity(ctx, payload.LocalBucketID)
	if err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	if active {
		intents, listErr := h.Service.Index.listBucketDeletionWriteIntents(ctx, payload.LocalBucketID)
		if listErr != nil {
			return classifyBucketDeletionError(listErr, "bucket_busy")
		}
		maintenance, backendErr := h.Service.maintenanceBackend()
		if backendErr != nil {
			return classifyBucketDeletionError(backendErr, "bucket_busy")
		}
		for _, intent := range intents {
			if recoverErr := h.Service.recoverWriteIntent(ctx, maintenance, intent); recoverErr != nil {
				return classifyBucketDeletionError(recoverErr, "bucket_busy")
			}
		}
		active, err = h.Service.Index.HasDeletionBlockingActivity(ctx, payload.LocalBucketID)
		if err != nil {
			return classifyBucketDeletionError(err, "bucket_busy")
		}
	}
	if active {
		return retryableFailure("bucket_busy")
	}
	if err := h.Service.Index.AcquireBucketDeletionMaintenance(ctx, payload.LocalBucketID, job.ID); err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	payload.Stage = bucketDeletionStageSettled
	return h.persist(ctx, job.ID, *payload, .12)
}

func (h BucketDeletionJobs) settleLocalWithoutMutation(
	ctx context.Context,
	job jobs.Job,
	payload *BucketDeletionPayload,
) error {
	if payload.LocalBucketID == "" {
		return nil
	}
	active, err := h.Service.Index.HasDeletionBlockingActivity(ctx, payload.LocalBucketID)
	if err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	if active {
		return permanentFailure("bucket_not_empty")
	}
	if err := h.Service.Index.AcquireBucketDeletionMaintenance(ctx, payload.LocalBucketID, job.ID); err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	payload.Stage = bucketDeletionStageSettled
	return h.persist(ctx, job.ID, *payload, .12)
}

func (h BucketDeletionJobs) settleLocalAfterRemoteMissing(
	ctx context.Context,
	job jobs.Job,
	payload *BucketDeletionPayload,
) error {
	if payload.LocalBucketID == "" {
		return nil
	}
	uploads, err := h.Service.Index.listBucketDeletionMultipart(ctx, payload.LocalBucketID)
	if err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	for _, upload := range uploads {
		if err := h.Service.Index.AbortClientMultipart(ctx, upload.ID); err != nil && !errors.Is(err, ErrMultipartNotFound) {
			return classifyBucketDeletionError(err, "bucket_busy")
		}
	}
	intents, err := h.Service.Index.listBucketDeletionWriteIntents(ctx, payload.LocalBucketID)
	if err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	for _, intent := range intents {
		if err := h.Service.Index.AbortWrite(ctx, intent.ID); err != nil && !errors.Is(err, ErrWriteIntentNotFound) {
			return classifyBucketDeletionError(err, "bucket_busy")
		}
	}
	active, err := h.Service.Index.HasDeletionBlockingActivity(ctx, payload.LocalBucketID)
	if err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	if active {
		return retryableFailure("bucket_busy")
	}
	if err := h.Service.Index.AcquireBucketDeletionMaintenance(ctx, payload.LocalBucketID, job.ID); err != nil {
		return classifyBucketDeletionError(err, "bucket_busy")
	}
	payload.Stage = bucketDeletionStageSettled
	return h.persist(ctx, job.ID, *payload, .12)
}

func (h BucketDeletionJobs) clearRemote(
	ctx context.Context,
	job jobs.Job,
	account accounts.Account,
	payload *BucketDeletionPayload,
) error {
	payload.Stage = bucketDeletionStageClearing
	if err := h.persist(ctx, job.ID, *payload, .15); err != nil {
		return classifyBucketDeletionError(err, "local_finalize_failed")
	}
	if account.R2AccessKeyID != "" && account.R2SecretAccessKey != "" && h.Clear != nil {
		target := Target{AccountID: account.ID, CloudflareAccountID: account.CloudflareAccountID,
			AccessKeyID: account.R2AccessKeyID, SecretAccessKey: account.R2SecretAccessKey, Bucket: payload.BucketName}
		if err := h.abortRemoteMultipart(ctx, job.ID, account, target, payload); err != nil {
			return err
		}
		return h.clearObjectsS3(ctx, job.ID, account, target, payload)
	}
	return h.clearObjectsREST(ctx, job.ID, account, payload)
}

func (h BucketDeletionJobs) abortRemoteMultipart(
	ctx context.Context,
	jobID string,
	account accounts.Account,
	target Target,
	payload *BucketDeletionPayload,
) error {
	for {
		missing, err := h.verifyIdentity(ctx, account, *payload)
		if err != nil {
			return err
		}
		if missing {
			return nil
		}
		page, err := h.Clear.ListRemoteMultipart(ctx, target, "", "", 1000)
		if err != nil {
			return classifyBucketDeletionError(err, "cloudflare_unavailable")
		}
		if len(page.Uploads) == 0 {
			return nil
		}
		if payload.DeleteRounds >= bucketDeletionMaxRounds {
			return permanentFailure("external_writes_detected")
		}
		if err := h.markRemoteMutated(ctx, jobID, payload); err != nil {
			return classifyBucketDeletionError(err, "local_finalize_failed")
		}
		for _, upload := range page.Uploads {
			if err := h.Clear.AbortMultipart(ctx, target, upload.Key, upload.UploadID); err != nil && !isMultipartNotFound(err) {
				return classifyBucketDeletionError(err, "partial_delete_failed")
			}
			payload.AbortedMultipart++
		}
		payload.DeleteRounds++
		if err := h.persistBatch(ctx, jobID, *payload); err != nil {
			return classifyBucketDeletionError(err, "local_finalize_failed")
		}
	}
}

func (h BucketDeletionJobs) clearObjectsS3(
	ctx context.Context,
	jobID string,
	account accounts.Account,
	target Target,
	payload *BucketDeletionPayload,
) error {
	deletedAny := false
	for {
		missing, err := h.verifyIdentity(ctx, account, *payload)
		if err != nil {
			return err
		}
		if missing {
			return nil
		}
		page, err := h.Clear.ListRemote(ctx, target, "", "", 1000)
		if err != nil {
			return classifyBucketDeletionError(err, "cloudflare_unavailable")
		}
		if len(page.Objects) == 0 {
			if !deletedAny {
				return nil
			}
			return h.confirmNoExternalWritesS3(ctx, account, target, *payload)
		}
		if payload.DeleteRounds >= bucketDeletionMaxRounds {
			return permanentFailure("external_writes_detected")
		}
		keys := make([]string, 0, len(page.Objects))
		for _, object := range page.Objects {
			keys = append(keys, object.Key)
		}
		if err := h.markRemoteMutated(ctx, jobID, payload); err != nil {
			return classifyBucketDeletionError(err, "local_finalize_failed")
		}
		deleted, err := h.Clear.DeleteRemoteBatch(ctx, target, keys)
		payload.DeletedObjects += int64(deleted)
		payload.DeleteRounds++
		if persistErr := h.persistBatch(ctx, jobID, *payload); persistErr != nil {
			return classifyBucketDeletionError(persistErr, "local_finalize_failed")
		}
		if err != nil || deleted != len(keys) {
			return permanentFailure("partial_delete_failed")
		}
		deletedAny = true
	}
}

func (h BucketDeletionJobs) confirmNoExternalWritesS3(
	ctx context.Context,
	account accounts.Account,
	target Target,
	payload BucketDeletionPayload,
) error {
	missing, err := h.verifyIdentity(ctx, account, payload)
	if err != nil || missing {
		return err
	}
	page, err := h.Clear.ListRemote(ctx, target, "", "", 1)
	if err != nil {
		return classifyBucketDeletionError(err, "cloudflare_unavailable")
	}
	if len(page.Objects) != 0 {
		return permanentFailure("external_writes_detected")
	}
	return nil
}

func (h BucketDeletionJobs) clearObjectsREST(
	ctx context.Context,
	jobID string,
	account accounts.Account,
	payload *BucketDeletionPayload,
) error {
	deletedAny := false
	for {
		missing, err := h.verifyIdentity(ctx, account, *payload)
		if err != nil {
			return err
		}
		if missing {
			return nil
		}
		page, err := h.Remote.ListR2Objects(ctx, account.CloudflareAccountID, account.APIToken,
			payload.Jurisdiction, payload.BucketName, "", 1000)
		if err != nil {
			return classifyBucketDeletionError(err, "cloudflare_unavailable")
		}
		if len(page.Objects) == 0 {
			if !deletedAny {
				return nil
			}
			return h.confirmNoExternalWritesREST(ctx, account, *payload)
		}
		if payload.DeleteRounds >= bucketDeletionMaxRounds {
			return permanentFailure("external_writes_detected")
		}
		if err := h.markRemoteMutated(ctx, jobID, payload); err != nil {
			return classifyBucketDeletionError(err, "local_finalize_failed")
		}
		for _, object := range page.Objects {
			if err := h.Remote.DeleteR2Object(ctx, account.CloudflareAccountID, account.APIToken,
				payload.Jurisdiction, payload.BucketName, object.Key); err != nil {
				switch {
				case accounts.IsR2ObjectNotFound(err):
					// Another writer already removed this object.
				case accounts.IsR2BucketNotFound(err):
					missing, verifyErr := h.verifyIdentity(ctx, account, *payload)
					if verifyErr != nil {
						return verifyErr
					}
					if missing {
						return nil
					}
					return retryableFailure("cloudflare_unavailable")
				default:
					return classifyBucketDeletionError(err, "partial_delete_failed")
				}
			}
			payload.DeletedObjects++
		}
		payload.DeleteRounds++
		if err := h.persistBatch(ctx, jobID, *payload); err != nil {
			return classifyBucketDeletionError(err, "local_finalize_failed")
		}
		deletedAny = true
	}
}

func (h BucketDeletionJobs) confirmNoExternalWritesREST(
	ctx context.Context,
	account accounts.Account,
	payload BucketDeletionPayload,
) error {
	missing, err := h.verifyIdentity(ctx, account, payload)
	if err != nil || missing {
		return err
	}
	page, err := h.Remote.ListR2Objects(ctx, account.CloudflareAccountID, account.APIToken,
		payload.Jurisdiction, payload.BucketName, "", 1)
	if err != nil {
		return classifyBucketDeletionError(err, "cloudflare_unavailable")
	}
	if len(page.Objects) != 0 {
		return permanentFailure("external_writes_detected")
	}
	return nil
}

func (h BucketDeletionJobs) markRemoteMutated(ctx context.Context, jobID string, payload *BucketDeletionPayload) error {
	if payload.RemoteMutated {
		return nil
	}
	payload.RemoteMutated = true
	return h.Jobs.SetPayload(ctx, jobID, *payload)
}

func (h BucketDeletionJobs) persistBatch(ctx context.Context, jobID string, payload BucketDeletionPayload) error {
	progress := math.Min(.90, .18+float64(payload.DeleteRounds)*.03)
	return h.persist(ctx, jobID, payload, progress)
}

func (h BucketDeletionJobs) persist(ctx context.Context, jobID string, payload BucketDeletionPayload, progress float64) error {
	if err := h.Jobs.SetPayload(ctx, jobID, payload); err != nil {
		return err
	}
	return h.Jobs.SetProgress(ctx, jobID, progress)
}

func (h BucketDeletionJobs) finalize(ctx context.Context, job jobs.Job, payload *BucketDeletionPayload, fenced bool) error {
	if !fenced || payload.LocalBucketID == "" {
		h.recordAudit(ctx, job, *payload, "success", "")
		return nil
	}
	if err := h.markRemoteMutated(ctx, job.ID, payload); err != nil {
		return h.fail(ctx, job, *payload, true, jobs.NewFailure("local_finalize_failed", errors.New("远端存储桶已删除，但本地状态保存失败"), true))
	}
	if err := h.Jobs.SetProgress(ctx, job.ID, .94); err != nil {
		return h.fail(ctx, job, *payload, true, jobs.NewFailure("local_finalize_failed", errors.New("远端存储桶已删除，但本地进度保存失败"), true))
	}
	if err := h.Service.Index.FinalizeDeletedBucket(ctx, payload.LocalBucketID, job.ID); err != nil {
		return h.fail(ctx, job, *payload, true, jobs.NewFailure("local_finalize_failed", errors.New("远端存储桶已删除，但本地记录清理失败，请重试任务"), true))
	}
	h.recordAudit(ctx, job, *payload, "success", "")
	return nil
}

func (h BucketDeletionJobs) fail(
	ctx context.Context,
	job jobs.Job,
	payload BucketDeletionPayload,
	fenced bool,
	err error,
) error {
	var failure *jobs.HandlerError
	if !errors.As(err, &failure) {
		err = classifyBucketDeletionError(err, "cloudflare_unavailable")
		_ = errors.As(err, &failure)
	}
	terminal := failure != nil && (failure.Permanent || job.Attempts >= job.MaxAttempts)
	if !terminal {
		return err
	}
	if fenced && payload.LocalBucketID != "" {
		var transitionErr error
		if payload.RemoteMutated {
			transitionErr = h.Service.Index.MarkBucketDeletionFailed(ctx, payload.LocalBucketID, job.ID)
		} else {
			transitionErr = h.Service.Index.RestoreBucketActive(ctx, payload.LocalBucketID, job.ID)
		}
		if transitionErr != nil && !errors.Is(transitionErr, ErrBucketNotFound) {
			h.recordAudit(ctx, job, payload, "failure", "local_finalize_failed")
			return jobs.NewFailure("local_finalize_failed", fmt.Errorf("%v；本地删除状态更新失败", err), true)
		}
	}
	code := ""
	if failure != nil {
		code = failure.Code
	}
	h.recordAudit(ctx, job, payload, "failure", code)
	return err
}

func (h BucketDeletionJobs) recordAudit(
	ctx context.Context,
	job jobs.Job,
	payload BucketDeletionPayload,
	result, errorCode string,
) {
	if h.Audit == nil {
		return
	}
	detail := map[string]any{
		"job_id": job.ID, "account_id": payload.AccountID, "jurisdiction": payload.Jurisdiction,
		"mode": string(payload.Mode), "deleted_objects": payload.DeletedObjects,
		"aborted_multipart": payload.AbortedMultipart,
	}
	if errorCode != "" {
		detail["error_code"] = errorCode
	}
	_, _ = h.Audit.Record(ctx, audit.Event{
		ID: "r2-bucket-delete-" + job.ID + "-" + result, Actor: "system", Protocol: "job",
		Action: BucketDeletionJobType, Resource: "r2/remote-buckets/" + payload.BucketName,
		Result: result, RequestID: job.ID, Detail: detail,
	})
}

func classifyBucketDeletionError(err error, fallback string) error {
	if err == nil {
		return nil
	}
	var failure *jobs.HandlerError
	if errors.As(err, &failure) {
		return err
	}
	if errors.Is(err, ErrRateLimited) {
		return retryableFailure("rate_limited")
	}
	var statusErr interface{ HTTPStatusCode() int }
	var codeErr interface{ ErrorCode() string }
	status, code := 0, ""
	if errors.As(err, &statusErr) {
		status = statusErr.HTTPStatusCode()
	}
	if errors.As(err, &codeErr) {
		code = codeErr.ErrorCode()
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || code == "AccessDenied" ||
		code == "InvalidAccessKeyId" || code == "SignatureDoesNotMatch" {
		return permanentFailure("permission_denied")
	}
	if errors.Is(err, ErrBucketDeleting) {
		return permanentFailure("bucket_deleting")
	}
	if errors.Is(err, ErrBucketBusy) {
		return retryableFailure("bucket_busy")
	}
	var apiErr *accounts.CloudflareAPIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden ||
			apiErr.Code == 9109 || apiErr.Code == 10000:
			return permanentFailure("permission_denied")
		case apiErr.StatusCode == http.StatusLocked:
			return permanentFailure("bucket_locked")
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return retryableFailure("rate_limited")
		case apiErr.StatusCode >= 500:
			return retryableFailure("cloudflare_unavailable")
		}
	}
	var batchErr *RemoteBatchDeleteError
	if errors.As(err, &batchErr) {
		return permanentFailure("partial_delete_failed")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return retryableFailure("cloudflare_unavailable")
	}
	return jobs.NewFailure(fallback, errors.New(bucketDeletionMessage(fallback)), fallback == "partial_delete_failed")
}

func permanentFailure(code string) error {
	return jobs.NewFailure(code, errors.New(bucketDeletionMessage(code)), true)
}

func retryableFailure(code string) error {
	return jobs.NewFailure(code, errors.New(bucketDeletionMessage(code)), false)
}

func bucketDeletionMessage(code string) string {
	switch code {
	case "bucket_not_empty":
		return "存储桶不是空桶，请先删除桶内所有文件和未完成的分片上传后再删除"
	case "bucket_busy":
		return "存储桶仍有写入或分片任务正在收尾，系统稍后会自动重试"
	case "bucket_deleting":
		return "该存储桶已有删除任务正在运行"
	case "bucket_locked":
		return "存储桶当前被锁定，无法删除"
	case "permission_denied":
		return "Cloudflare API Token 没有删除此存储桶所需的权限"
	case "s3_credentials_required":
		return "存储桶内仍有未完成的分片上传，请先配置 R2 S3 访问密钥后再执行一键清空并删除"
	case "external_writes_detected":
		return "清空期间检测到外部写入，任务已停止；请停止其他客户端写入后重试"
	case "bucket_identity_changed":
		return "检测到同名存储桶已被重新创建，为避免误删，任务已停止"
	case "bucket_identity_unverifiable":
		return "Cloudflare 未返回可验证的存储桶创建时间，为避免误删，任务已停止"
	case "unsupported_jurisdiction":
		return "当前版本只支持删除默认管辖区的 R2 存储桶"
	case "partial_delete_failed":
		return "部分文件删除失败；未成功删除的文件仍保留在桶内，请检查权限后重试"
	case "rate_limited":
		return "Cloudflare 请求过于频繁，系统稍后会自动重试"
	case "local_finalize_failed":
		return "远端存储桶已处理，但本地记录清理失败，请重试任务"
	default:
		return "Cloudflare 暂时不可用，系统稍后会自动重试"
	}
}

func isRemoteBucketNotFound(err error) bool {
	return accounts.IsR2BucketNotFound(err)
}

func isBucketNotEmpty(err error) bool {
	var apiErr *accounts.CloudflareAPIError
	return errors.As(err, &apiErr) && (apiErr.Code == 10008 || apiErr.StatusCode == http.StatusConflict)
}

func isAmbiguousBucketDelete(err error) bool {
	var apiErr *accounts.CloudflareAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	return true
}
