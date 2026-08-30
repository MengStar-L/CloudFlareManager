package r2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const OrphanFinding = "orphan"

var errRebalanceObjectChanged = errors.New("rebalance source object changed")

type RemoteObject struct {
	Key          string
	Size         int64
	ETag         string
	ContentType  string
	Metadata     map[string]string
	LastModified time.Time
}

type RemoteObjectList struct {
	Objects           []RemoteObject
	ContinuationToken string
}

type MaintenanceBackend interface {
	Head(context.Context, Target, string) (RemoteObject, error)
	ListRemote(context.Context, Target, string, string, int32) (RemoteObjectList, error)
}

type ScanReport struct {
	Scanned   int   `json:"scanned"`
	Imported  int   `json:"imported"`
	Conflicts int   `json:"conflicts"`
	Orphans   int   `json:"orphans"`
	Bytes     int64 `json:"bytes"`
}

func (s Service) AdoptBucket(ctx context.Context, bucketID string) (ScanReport, error) {
	if err := s.Index.AcquireBucketMaintenance(ctx, bucketID, "adopt"); err != nil {
		return ScanReport{}, err
	}
	defer s.Index.ReleaseBucketMaintenance(context.Background(), bucketID)
	backend, err := s.maintenanceBackend()
	if err != nil {
		return ScanReport{}, err
	}
	target, err := s.target(ctx, bucketID)
	if err != nil {
		return ScanReport{}, err
	}
	var report ScanReport
	var continuation string
	for {
		page, err := backend.ListRemote(ctx, target, "", continuation, 1000)
		if err != nil {
			return report, err
		}
		for _, listed := range page.Objects {
			report.Scanned++
			report.Bytes += listed.Size
			remote, err := backend.Head(ctx, target, listed.Key)
			if err != nil {
				return report, fmt.Errorf("read metadata for %q: %w", listed.Key, err)
			}
			mutationCtx, guard, err := s.BeginWebDAVTreeMutation(ctx, remote.Key)
			if err != nil {
				return report, err
			}
			_, adoptErr := s.Index.AdoptObject(mutationCtx, bucketID, remote)
			if guard != nil {
				guard.Release()
			}
			if adoptErr != nil {
				if errors.Is(adoptErr, ErrObjectConflict) {
					report.Conflicts++
					continue
				}
				return report, adoptErr
			}
			report.Imported++
		}
		if page.ContinuationToken == "" {
			break
		}
		continuation = page.ContinuationToken
	}
	return report, s.Index.FinishBucketScan(ctx, bucketID, report.Bytes, true)
}

func (s Service) ScanOrphans(ctx context.Context, bucketID string) (ScanReport, error) {
	if err := s.Index.AcquireBucketMaintenance(ctx, bucketID, "orphan-scan"); err != nil {
		return ScanReport{}, err
	}
	defer s.Index.ReleaseBucketMaintenance(context.Background(), bucketID)
	backend, err := s.maintenanceBackend()
	if err != nil {
		return ScanReport{}, err
	}
	bucket, err := s.Index.GetBucket(ctx, bucketID)
	if err != nil {
		return ScanReport{}, err
	}
	target, err := s.target(ctx, bucketID)
	if err != nil {
		return ScanReport{}, err
	}
	if err := s.Index.ClearScanFindings(ctx, bucketID, OrphanFinding); err != nil {
		return ScanReport{}, err
	}
	var report ScanReport
	var continuation string
	for {
		page, err := backend.ListRemote(ctx, target, "", continuation, 1000)
		if err != nil {
			return report, err
		}
		for _, remote := range page.Objects {
			report.Scanned++
			report.Bytes += remote.Size
			mapped, err := s.Index.HasPhysicalMapping(ctx, bucketID, remote.Key)
			if err != nil {
				return report, err
			}
			if mapped {
				continue
			}
			report.Orphans++
			if err := s.Index.RecordScanFinding(ctx, ScanFinding{
				BucketID: bucketID, Key: remote.Key, Kind: OrphanFinding,
				Detail: fmt.Sprintf("unindexed remote object (%d bytes)", remote.Size),
			}); err != nil {
				return report, err
			}
		}
		if page.ContinuationToken == "" {
			break
		}
		continuation = page.ContinuationToken
	}
	return report, s.Index.FinishBucketScan(ctx, bucketID, report.Bytes, bucket.Adopted)
}

func (s Service) RebuildIndex(ctx context.Context) (ScanReport, error) {
	buckets, err := s.Index.ListBuckets(ctx)
	if err != nil {
		return ScanReport{}, err
	}
	var total ScanReport
	for _, bucket := range buckets {
		report, err := s.AdoptBucket(ctx, bucket.ID)
		total.Scanned += report.Scanned
		total.Imported += report.Imported
		total.Conflicts += report.Conflicts
		total.Bytes += report.Bytes
		if err != nil {
			return total, fmt.Errorf("rebuild bucket %s: %w", bucket.Name, err)
		}
	}
	return total, nil
}

func (s Service) RecoverInterrupted(ctx context.Context) error {
	if err := s.recoverInterruptedState(ctx); err != nil {
		return err
	}
	_, err := s.ProcessPhysicalCleanups(ctx, 500)
	return err
}

// RecoverInterruptedBeforeServing resolves transactional state that could
// conflict with requests. Deferred physical deletion remains a background job
// and must not make the HTTP listeners unavailable.
func (s Service) RecoverInterruptedBeforeServing(ctx context.Context) error {
	return s.recoverInterruptedState(ctx)
}

func (s Service) recoverInterruptedState(ctx context.Context) error {
	backend, err := s.maintenanceBackend()
	if err != nil {
		return err
	}
	intents, err := s.Index.ListWriteIntents(ctx, 10000)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if err := s.recoverWriteIntent(ctx, backend, intent); err != nil {
			return err
		}
	}
	if err := s.recoverUnboundLegacyMultipart(ctx); err != nil {
		return err
	}

	var after string
	for {
		page, err := s.Index.ListObjectsByStates(ctx, []ObjectState{StatePending, StateDeleting, StateError}, after, 500)
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			if err := s.recoverLegacyObject(ctx, backend, object); err != nil {
				return err
			}
		}
		if page.NextMarker == "" {
			break
		}
		after = page.NextMarker
	}
	return nil
}

func (s Service) recoverWriteIntent(ctx context.Context, backend MaintenanceBackend, intent WriteIntent) error {
	ctx, guard, err := s.BeginWebDAVTreeMutation(ctx, intent.Key)
	if err != nil {
		return err
	}
	if guard != nil {
		defer guard.Release()
	}
	finishActivity, acquired := s.Index.tryBeginWriteActivity(intent.Key)
	if !acquired {
		return nil
	}
	defer finishActivity()
	intent, err = s.Index.GetWriteIntent(ctx, intent.ID)
	if errors.Is(err, ErrWriteIntentNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	target, err := s.target(ctx, intent.BucketID)
	if err != nil {
		return err
	}
	if intent.Operation == WriteOperationDelete {
		object, err := s.Index.GetObjectByID(ctx, intent.PreviousObjectID)
		if errors.Is(err, ErrObjectNotFound) {
			if err := s.Index.AbortWrite(ctx, intent.ID); err != nil {
				return err
			}
			return s.cleanupDeletedWebDAVLocks(context.WithoutCancel(ctx), intent.Key, guard)
		}
		if err != nil {
			return err
		}
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
		remote, headErr := backend.Head(ctx, target, object.PhysicalKey)
		if isRemoteNotFound(headErr) {
			if err := s.Index.CommitDeleteWrite(ctx, intent.ID); err != nil {
				return err
			}
			return s.cleanupDeletedWebDAVLocks(context.WithoutCancel(ctx), intent.Key, guard)
		}
		if headErr != nil {
			_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
			return headErr
		}
		if !remoteMatchesObjectVersion(object, remote) {
			_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
			return ErrWriteRecoveryAmbiguous
		}
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
		if err := s.Backend.Delete(ctx, target, object.PhysicalKey); err != nil {
			_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
			return err
		}
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
		if _, err := backend.Head(ctx, target, object.PhysicalKey); !isRemoteNotFound(err) {
			if err == nil {
				err = errors.New("delete recovery could not confirm removal")
			}
			_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
			return err
		}
		if err := s.Index.CommitDeleteWrite(ctx, intent.ID); err != nil {
			return err
		}
		return s.cleanupDeletedWebDAVLocks(context.WithoutCancel(ctx), intent.Key, guard)
	}

	upload, multipartErr := s.Index.GetMultipartByWriteIntent(ctx, intent.ID)
	clientMultipart := multipartErr == nil
	if multipartErr != nil && !errors.Is(multipartErr, ErrMultipartNotFound) {
		return multipartErr
	}
	if clientMultipart && upload.Status == MultipartActive && intent.State == WriteUploading {
		return nil
	}
	if clientMultipart && upload.Status == MultipartCompleting && intent.State == WriteConsumed {
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
		remote, headErr := backend.Head(ctx, target, intent.Key)
		state, classifyErr := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
		if classifyErr != nil {
			return classifyErr
		}
		if state == remoteWriteAmbiguous {
			return ErrWriteRecoveryAmbiguous
		}
		if state == remoteWritePublished {
			_, err := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
			return err
		}
		if err := s.Index.MarkWriteAborting(ctx, intent.ID, upload.UpstreamID); err != nil {
			return err
		}
		intent.State = WriteAborting
	}
	if clientMultipart && upload.Status == MultipartCompleting && intent.State == WriteAborting {
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
		remote, headErr := backend.Head(ctx, target, intent.Key)
		state, classifyErr := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
		if classifyErr != nil {
			return classifyErr
		}
		if state == remoteWriteAmbiguous {
			return ErrWriteRecoveryAmbiguous
		}
		if state == remoteWritePublished {
			_, err := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
			return err
		}
	}
	if intent.UpstreamUploadID != "" && (intent.State == WriteAborting ||
		(intent.InternalMultipart && intent.State == WriteUploading)) {
		return s.abortRecoveringMultipart(ctx, backend, target, intent, upload, clientMultipart)
	}

	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, headErr := backend.Head(ctx, target, intent.Key)
	state, classifyErr := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
	if classifyErr != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, intent.UpstreamUploadID)
		return classifyErr
	}
	if state == remoteWriteAmbiguous {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, intent.UpstreamUploadID)
		return ErrWriteRecoveryAmbiguous
	}
	if state == remoteWritePublished {
		_, err := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
		return err
	}
	if clientMultipart && upload.Status == MultipartCompleting {
		if err := s.Index.ResetMultipart(ctx, upload.ID); err != nil {
			return err
		}
		return s.Index.MarkWriteUploading(ctx, intent.ID, upload.UpstreamID)
	}
	if state == remoteWritePrevious || state == remoteWriteAbsent {
		if clientMultipart {
			if upload.UpstreamID == "" {
				return nil
			}
			return s.abortRecoveringMultipart(ctx, backend, target, intent, upload, true)
		}
		if intent.InternalMultipart && intent.UpstreamUploadID != "" {
			return s.abortRecoveringMultipart(ctx, backend, target, intent, MultipartUpload{}, false)
		}
		return s.Index.AbortWrite(ctx, intent.ID)
	}
	return ErrWriteRecoveryAmbiguous
}

func (s Service) abortRecoveringMultipart(
	ctx context.Context,
	maintenance MaintenanceBackend,
	target Target,
	intent WriteIntent,
	upload MultipartUpload,
	clientMultipart bool,
) error {
	multipartBackend, err := s.multipartBackend()
	if err != nil {
		return err
	}
	key, upstreamID := intent.Key, intent.UpstreamUploadID
	if clientMultipart {
		key, upstreamID = upload.Key, upload.UpstreamID
	}
	if upstreamID == "" {
		if clientMultipart {
			return s.Index.AbortClientMultipart(ctx, upload.ID)
		}
		return s.Index.AbortWrite(ctx, intent.ID)
	}
	if err := s.Index.MarkWriteAborting(ctx, intent.ID, upstreamID); err != nil {
		return err
	}
	remote, state, err := s.abortMultipartThenClassify(ctx, multipartBackend, maintenance, target, intent, key, upstreamID)
	if err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, upstreamID)
		return err
	}
	if state == remoteWritePublished {
		_, err := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
		return err
	}
	if state == remoteWriteAmbiguous {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, upstreamID)
		return ErrWriteRecoveryAmbiguous
	}
	if clientMultipart {
		return s.Index.AbortClientMultipart(ctx, upload.ID)
	}
	return s.Index.AbortWrite(ctx, intent.ID)
}

func (s Service) recoverLegacyObject(ctx context.Context, backend MaintenanceBackend, object Object) error {
	ctx, guard, err := s.BeginWebDAVTreeMutation(ctx, object.Key)
	if err != nil {
		return err
	}
	if guard != nil {
		defer guard.Release()
	}
	finishActivity, acquired := s.Index.tryBeginWriteActivity(object.Key)
	if !acquired {
		return nil
	}
	defer finishActivity()
	current, err := s.Index.GetObjectByID(ctx, object.ObjectID)
	if errors.Is(err, ErrObjectNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Key != object.Key || current.State != object.State {
		return nil
	}
	object = current
	target, err := s.target(ctx, object.BucketID)
	if err != nil {
		return err
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, headErr := backend.Head(ctx, target, object.PhysicalKey)
	switch {
	case headErr == nil && object.State != StateDeleting:
		return s.Index.RecoverLegacyObject(ctx, object.ObjectID, remote.ETag, remote.Size)
	case isRemoteNotFound(headErr):
		if err := s.Index.RemoveLegacyObject(ctx, object, false, ""); err != nil {
			return err
		}
		return s.cleanupDeletedWebDAVLocks(context.WithoutCancel(ctx), object.Key, guard)
	default:
		detail := "legacy state could not be confirmed; remote object was left untouched"
		if headErr != nil {
			detail += ": " + headErr.Error()
		}
		return s.Index.RemoveLegacyObject(ctx, object, true, detail)
	}
}

func (s Service) recoverUnboundLegacyMultipart(ctx context.Context) error {
	for _, status := range []MultipartStatus{MultipartInitiating, MultipartActive, MultipartCompleting, MultipartError} {
		uploads, err := s.Index.ListMultipartByStatus(ctx, status, 1000)
		if err != nil {
			return err
		}
		for _, upload := range uploads {
			if upload.WriteIntentID != "" {
				continue
			}
			if upload.UpstreamID == "" {
				if err := s.Index.DeleteMultipart(ctx, upload.ID); err != nil {
					return err
				}
				continue
			}
			backend, err := s.multipartBackend()
			if err != nil {
				return err
			}
			target, err := s.target(ctx, upload.BucketID)
			if err != nil {
				return err
			}
			if err := s.settleUnboundMultipart(ctx, backend, target, upload, upload.Key, ErrWriteRecoveryAmbiguous); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s Service) Rebalance(ctx context.Context, sourceBucketID, targetBucketID, prefix string) (int, error) {
	if sourceBucketID == "" || targetBucketID == "" || sourceBucketID == targetBucketID {
		return 0, errors.New("distinct source and target bucket ids are required")
	}
	targetBucket, err := s.Index.GetBucket(ctx, targetBucketID)
	if err != nil {
		return 0, err
	}
	if !targetBucket.Writable || targetBucket.HealthStatus != "healthy" {
		return 0, errors.New("target bucket is not healthy and writable")
	}
	var moved int
	var after string
	for {
		page, err := s.Index.ListObjectsByBucket(ctx, sourceBucketID, after, 500)
		if err != nil {
			return moved, err
		}
		for _, object := range page.Objects {
			if prefix != "" && !strings.HasPrefix(object.Key, prefix) {
				continue
			}
			if err := s.moveObject(ctx, object, targetBucketID); errors.Is(err, errRebalanceObjectChanged) {
				continue
			} else if err != nil {
				return moved, fmt.Errorf("move %q: %w", object.Key, err)
			}
			moved++
		}
		if page.NextMarker == "" {
			return moved, nil
		}
		after = page.NextMarker
	}
}

func (s Service) moveObject(ctx context.Context, object Object, targetBucketID string) error {
	ctx, guard, err := s.BeginWebDAVTreeMutation(ctx, object.Key)
	if err != nil {
		return err
	}
	if guard != nil {
		defer guard.Release()
	}
	finishActivity := s.Index.beginWriteActivity(object.Key)
	defer finishActivity()
	current, err := s.Index.GetObject(ctx, object.Key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			return errRebalanceObjectChanged
		}
		return err
	}
	if current.ObjectID != object.ObjectID || current.BucketID != object.BucketID {
		return errRebalanceObjectChanged
	}
	object, err = s.objectWithETag(ctx, current)
	if err != nil {
		return err
	}
	intent, err := s.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{
		Key: object.Key, Size: object.Size, ContentType: object.ContentType, Metadata: object.Metadata,
	}, ExpectedClassA: 1, TargetBucketID: targetBucketID})
	if err != nil {
		return err
	}
	sourceTarget, err := s.target(ctx, object.BucketID)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	target, err := s.target(ctx, targetBucketID)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	putOptions, err := s.physicalPutOptions(ctx, intent)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	if err := s.Index.ConsumeOperation(ctx, sourceTarget.AccountID, OperationClassB); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	result, err := s.Backend.Get(ctx, sourceTarget, object.PhysicalKey, GetOptions{IfMatch: quoteETag(object.ETag)})
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	defer result.Body.Close()
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	if err := s.Index.MarkWriteUploading(ctx, intent.ID, ""); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	etag, err := s.Backend.Put(ctx, target, object.Key, result.Body, object.Size, object.ContentType,
		upstreamWriteMetadata(object.Metadata, intent.ID), putOptions)
	if err != nil {
		if isDefinitiveWriteRejection(err) {
			_ = s.Index.AbortWrite(ctx, intent.ID)
			return err
		}
		_, resolveErr := s.resolveAmbiguousWrite(ctx, intent, target, err)
		return resolveErr
	}
	if err := s.Index.MarkWriteCompleting(ctx, intent.ID, etag, object.Size); err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
		return err
	}
	_, err = s.Index.CommitWrite(ctx, intent.ID, etag, object.Size)
	return err
}

func (s Service) maintenanceBackend() (MaintenanceBackend, error) {
	if s.Index == nil || s.Accounts == nil || s.Backend == nil {
		return nil, errors.New("R2 service is not configured")
	}
	backend, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		return nil, errors.New("R2 backend does not support maintenance operations")
	}
	return backend, nil
}
