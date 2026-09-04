package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
)

type createR2BucketDeletionInput struct {
	AccountID        string                `json:"account_id"`
	Jurisdiction     string                `json:"jurisdiction"`
	Mode             r2.BucketDeletionMode `json:"mode"`
	ConfirmationName string                `json:"confirmation_name"`
	AdminPassword    string                `json:"admin_password"`
}

func (a *API) createR2BucketDeletion(w http.ResponseWriter, request *http.Request) {
	if a.deps.Auth == nil || a.deps.Accounts == nil || a.deps.Jobs == nil || a.deps.R2 == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "存储桶删除服务尚未配置。")
		return
	}

	bucketName := strings.TrimSpace(request.PathValue("bucket_name"))
	if bucketName == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "存储桶名称不能为空。")
		return
	}
	var input createR2BucketDeletionInput
	if err := decodeJSON(w, request, &input); err != nil {
		return
	}
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.Jurisdiction = strings.TrimSpace(input.Jurisdiction)
	input.Mode = r2.BucketDeletionMode(strings.TrimSpace(string(input.Mode)))
	if input.AccountID == "" || input.Mode == "" || input.AdminPassword == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "必须提供账号、删除方式和管理员密码。")
		return
	}
	if input.Jurisdiction == "" {
		input.Jurisdiction = "default"
	}
	if input.Jurisdiction != "default" {
		writeError(w, http.StatusBadRequest, "unsupported_jurisdiction", "当前版本仅支持删除默认管辖区（default）的 R2 存储桶。")
		return
	}
	if input.Mode != r2.BucketDeletionEmptyOnly && input.Mode != r2.BucketDeletionEmptyAndDelete {
		writeError(w, http.StatusBadRequest, "invalid_mode", "删除方式必须是删除空桶或一键清空并删除桶。")
		return
	}
	if input.Mode == r2.BucketDeletionEmptyAndDelete && input.ConfirmationName != bucketName {
		writeError(w, http.StatusBadRequest, "confirmation_name_mismatch", "请输入完整且完全一致的存储桶名称后再删除。")
		return
	}
	if err := a.deps.Auth.Authenticate(request.Context(), input.AdminPassword); err != nil {
		writeError(w, http.StatusForbidden, "confirmation_failed", "管理员密码验证失败。")
		return
	}

	account, err := a.deps.Accounts.Get(request.Context(), input.AccountID, true)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "未找到指定账号。")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取账号信息。")
		return
	}

	localBucket, localErr := a.deps.R2.GetBucketByAccountAndName(request.Context(), account.ID, bucketName)
	if localErr != nil && !errors.Is(localErr, r2.ErrBucketNotFound) {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取本地存储桶状态。")
		return
	}
	managed := localErr == nil

	remoteBucket, remoteErr := a.deps.Remote.GetR2Bucket(
		request.Context(), account.CloudflareAccountID, account.APIToken, input.Jurisdiction, bucketName,
	)
	remoteMissing := isCloudflareNotFound(remoteErr)
	if remoteErr != nil && !remoteMissing {
		writeR2BucketDeletionRemoteError(w, remoteErr)
		return
	}
	if remoteMissing && !managed {
		writeError(w, http.StatusNotFound, "not_found", "Cloudflare 上不存在该存储桶，本地也没有可收尾的登记记录。")
		return
	}

	parentJobID := ""
	localBucketID := ""
	var parentPayload r2.BucketDeletionPayload
	if managed {
		localBucketID = localBucket.ID
		if localBucket.LifecycleState == r2.BucketDeleteFailed || localBucket.LifecycleState == r2.BucketDeleting {
			candidateParentID := localBucket.DeletionJobID
			parentJob, getErr := a.deps.Jobs.Get(request.Context(), candidateParentID)
			if getErr != nil || json.Unmarshal(parentJob.Payload, &parentPayload) != nil ||
				parentJob.Type != r2.BucketDeletionJobType ||
				parentJob.ResourceKey != fmt.Sprintf("%s/%s/%s", input.AccountID, input.Jurisdiction, bucketName) ||
				parentPayload.ParentJobID != parentJob.ParentJobID ||
				parentPayload.AccountID != input.AccountID || parentPayload.CloudflareAccountID != account.CloudflareAccountID ||
				parentPayload.BucketName != bucketName || parentPayload.Jurisdiction != input.Jurisdiction ||
				parentPayload.LocalBucketID != localBucketID {
				writeError(w, http.StatusConflict, "bucket_identity_unverifiable", "无法验证上一次删除任务的存储桶身份，为避免误删已停止操作。")
				return
			}
			activeDeletion := localBucket.LifecycleState == r2.BucketDeleting &&
				(parentJob.Status == "pending" || parentJob.Status == "running")
			recoveringStuckFence := localBucket.LifecycleState == r2.BucketDeleting && parentJob.Status == "failed"
			switch {
			case activeDeletion:
				// Return the verified active job directly. This avoids a race where
				// it becomes failed before EnqueueUnique and a parentless retry is
				// created for a fence still owned by that failed job.
				a.record(request, "admin", "r2.bucket.delete-remote", "r2/remote-buckets/"+bucketName, "accepted", map[string]any{
					"account_id": input.AccountID, "jurisdiction": input.Jurisdiction,
					"mode": string(parentPayload.Mode), "job_id": parentJob.ID, "created": false,
				})
				writeJSON(w, http.StatusAccepted, map[string]any{"job": parentJob, "created": false})
				return
			case parentJob.Status == "failed" && (parentPayload.RemoteMutated || recoveringStuckFence):
				parentJobID = candidateParentID
			default:
				writeError(w, http.StatusConflict, "bucket_identity_unverifiable", "无法验证上一次删除任务的存储桶身份，为避免误删已停止操作。")
				return
			}
		}
	}

	expectedCreationDate := ""
	remoteMissingAtEnqueue := remoteMissing
	if parentJobID != "" {
		expectedCreationDate = parentPayload.ExpectedCreationDate
		remoteMissingAtEnqueue = parentPayload.RemoteMissingAtEnqueue
		if !remoteMissing {
			if parentPayload.RemoteMissingAtEnqueue {
				writeError(w, http.StatusConflict, "bucket_identity_changed", "上次删除时远端桶已不存在，但现在出现了同名桶；为避免误删已停止操作。")
				return
			}
			if remoteBucket.Name != "" && remoteBucket.Name != bucketName {
				writeError(w, http.StatusConflict, "bucket_identity_changed", "Cloudflare 返回的存储桶身份与上次任务不一致，为避免误删已停止操作。")
				return
			}
			if remoteBucket.Jurisdiction != "" && remoteBucket.Jurisdiction != input.Jurisdiction {
				writeError(w, http.StatusConflict, "bucket_identity_changed", "Cloudflare 返回的存储桶管辖区与上次任务不一致，为避免误删已停止操作。")
				return
			}
			normalizedExpected, expectedErr := normalizeR2CreationDate(expectedCreationDate)
			currentCreationDate, normalizeErr := normalizeR2CreationDate(remoteBucket.CreationDate)
			if expectedErr != nil || normalizeErr != nil || currentCreationDate != normalizedExpected {
				writeError(w, http.StatusConflict, "bucket_identity_changed", "远端同名存储桶已发生变化，为避免误删已停止操作。")
				return
			}
			expectedCreationDate = normalizedExpected
		}
	} else if !remoteMissing {
		if remoteBucket.Name != "" && remoteBucket.Name != bucketName {
			writeError(w, http.StatusConflict, "bucket_identity_changed", "Cloudflare 返回的存储桶身份与请求不一致，已停止删除。")
			return
		}
		if remoteBucket.Jurisdiction != "" && remoteBucket.Jurisdiction != input.Jurisdiction {
			writeError(w, http.StatusConflict, "bucket_identity_changed", "Cloudflare 返回的存储桶管辖区与请求不一致，已停止删除。")
			return
		}
		expectedCreationDate, err = normalizeR2CreationDate(remoteBucket.CreationDate)
		if err != nil {
			writeError(w, http.StatusConflict, "bucket_identity_unverifiable", "无法核验存储桶创建时间，为避免误删已停止操作。")
			return
		}
	}
	// The shared worker payload intentionally excludes credentials. The worker
	// resolves the account secret at execution time.
	payload := r2.BucketDeletionPayload{
		AccountID: input.AccountID, CloudflareAccountID: account.CloudflareAccountID,
		BucketName: bucketName, Jurisdiction: input.Jurisdiction,
		ExpectedCreationDate: expectedCreationDate, LocalBucketID: localBucketID,
		Mode: input.Mode, Stage: "queued", ParentJobID: parentJobID,
		RemoteMissingAtEnqueue: remoteMissingAtEnqueue,
	}
	if parentJobID != "" {
		payload.RemoteMutated = parentPayload.RemoteMutated
		payload.DeletedObjects = parentPayload.DeletedObjects
		payload.AbortedMultipart = parentPayload.AbortedMultipart
	}
	resourceKey := fmt.Sprintf("%s/%s/%s", input.AccountID, input.Jurisdiction, bucketName)
	job, created, err := a.deps.Jobs.EnqueueUniqueForAccount(
		request.Context(), input.AccountID, r2.BucketDeletionJobType, resourceKey, parentJobID, payload, 8,
	)
	if errors.Is(err, jobs.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "未找到指定账号。")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法创建存储桶删除任务。")
		return
	}
	a.record(request, "admin", "r2.bucket.delete-remote", "r2/remote-buckets/"+bucketName, "accepted", map[string]any{
		"account_id": input.AccountID, "jurisdiction": input.Jurisdiction,
		"mode": string(input.Mode), "job_id": job.ID, "created": created,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "created": created})
}

func normalizeR2CreationDate(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("creation date is missing")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

func remoteBucketKey(jurisdiction, name string) string {
	return jurisdiction + "\x00" + name
}

func isCloudflareNotFound(err error) bool {
	return accounts.IsR2BucketNotFound(err)
}

func writeR2BucketDeletionRemoteError(w http.ResponseWriter, err error) {
	var apiErr *accounts.CloudflareAPIError
	if !errors.As(err, &apiErr) {
		writeError(w, http.StatusBadGateway, "cloudflare_unavailable", "暂时无法连接 Cloudflare，请稍后重试。")
		return
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		writeError(w, http.StatusForbidden, "permission_denied", "Cloudflare API Token 无权读取或删除该存储桶，请授予 Workers R2 Storage Write 权限。")
	case http.StatusTooManyRequests:
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Cloudflare 请求过于频繁，请稍后重试。")
	case http.StatusLocked:
		writeError(w, http.StatusConflict, "bucket_locked", "存储桶当前被 Cloudflare 锁定，暂时无法删除。")
	default:
		if apiErr.Code == 9109 || apiErr.Code == 10000 {
			writeError(w, http.StatusForbidden, "permission_denied", "Cloudflare API Token 无权读取或删除该存储桶，请授予 Workers R2 Storage Write 权限。")
			return
		}
		writeError(w, http.StatusBadGateway, "cloudflare_unavailable", "Cloudflare 暂时无法完成存储桶身份核验，请稍后重试。")
	}
}
