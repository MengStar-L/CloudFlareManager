package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type MultipartBackend interface {
	CreateMultipart(context.Context, Target, string, string, map[string]string) (string, error)
	UploadPart(context.Context, Target, string, string, int32, io.Reader, int64) (string, error)
	CompleteMultipart(context.Context, Target, string, string, []CompletedPart, PutOptions) (string, error)
	AbortMultipart(context.Context, Target, string, string) error
}

type CreateMultipartInput struct {
	Key         string
	ContentType string
	Metadata    map[string]string
}

type UploadPartRequest struct {
	Key         string
	UploadID    string
	PartNumber  int32
	Body        io.Reader
	Size        int64
	PayloadHash string
}

type CompleteMultipartRequest struct {
	Key      string
	UploadID string
	Parts    []CompletedPart
}

func (s Service) CreateMultipart(ctx context.Context, input CreateMultipartInput) (MultipartUpload, error) {
	backend, err := s.multipartBackend()
	if err != nil {
		return MultipartUpload{}, err
	}
	finishActivity := s.Index.beginWriteActivity(input.Key)
	defer finishActivity()
	if err := s.repairCurrentObjectETag(ctx, input.Key); err != nil {
		return MultipartUpload{}, err
	}
	upload, err := s.Index.BeginMultipart(ctx, ObjectInput{
		Key: input.Key, Size: -1, ContentType: input.ContentType, Metadata: input.Metadata,
	})
	if err != nil {
		return MultipartUpload{}, err
	}
	target, err := s.target(ctx, upload.BucketID)
	if err != nil {
		_ = s.Index.AbortClientMultipart(ctx, upload.ID)
		return MultipartUpload{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		_ = s.Index.AbortClientMultipart(ctx, upload.ID)
		return MultipartUpload{}, err
	}
	upstreamID, err := backend.CreateMultipart(ctx, target, upload.Key, upload.ContentType,
		upstreamWriteMetadata(upload.Metadata, upload.WriteIntentID))
	if err != nil {
		if isDefinitiveWriteRejection(err) {
			if cleanupErr := s.Index.AbortClientMultipart(ctx, upload.ID); cleanupErr != nil {
				return MultipartUpload{}, errors.Join(err, cleanupErr)
			}
			return MultipartUpload{}, err
		}
		_ = s.Index.FailMultipart(ctx, upload.ID)
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, "")
		return MultipartUpload{}, err
	}
	if err := s.Index.ActivateMultipart(ctx, upload.ID, upstreamID); err != nil {
		if abortErr := backend.AbortMultipart(ctx, target, upload.Key, upstreamID); abortErr == nil || isMultipartNotFound(abortErr) {
			_ = s.Index.AbortClientMultipart(ctx, upload.ID)
		} else {
			_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upstreamID)
		}
		return MultipartUpload{}, err
	}
	return s.Index.GetMultipart(ctx, upload.ID)
}

func (s Service) UploadPart(ctx context.Context, request UploadPartRequest) (MultipartPart, error) {
	backend, err := s.multipartBackend()
	if err != nil {
		return MultipartPart{}, err
	}
	finishActivity := s.Index.beginWriteActivity(request.Key)
	defer finishActivity()
	if request.PartNumber < 1 || request.PartNumber > 10000 {
		return MultipartPart{}, ErrInvalidPart
	}
	upload, err := s.activeMultipart(ctx, request.Key, request.UploadID)
	if err != nil {
		return MultipartPart{}, err
	}
	if request.Body == nil {
		request.Body = &emptyReader{}
	}
	file, size, digest, err := s.spool(request.Body)
	if err != nil {
		return MultipartPart{}, err
	}
	defer removeStagedFile(file)
	if request.Size >= 0 && request.Size != size {
		return MultipartPart{}, fmt.Errorf("content length mismatch: expected %d bytes, received %d", request.Size, size)
	}
	if err := validatePayloadHash(request.PayloadHash, digest); err != nil {
		return MultipartPart{}, err
	}
	target, err := s.target(ctx, upload.BucketID)
	if err != nil {
		return MultipartPart{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return MultipartPart{}, err
	}
	if err := s.Index.PrepareMultipartPart(ctx, upload.ID, request.PartNumber, size); err != nil {
		return MultipartPart{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		return MultipartPart{}, err
	}
	etag, err := backend.UploadPart(ctx, target, upload.Key, upload.UpstreamID, request.PartNumber, file, size)
	if err != nil {
		return MultipartPart{}, err
	}
	part := MultipartPart{PartNumber: request.PartNumber, ETag: strings.Trim(etag, `"`), Size: size}
	if err := s.Index.CommitMultipartPart(ctx, upload.ID, part); err != nil {
		return MultipartPart{}, err
	}
	parts, err := s.Index.ListMultipartParts(ctx, upload.ID, request.PartNumber-1, 1)
	if err != nil {
		return MultipartPart{}, err
	}
	if len(parts.Parts) != 1 {
		return MultipartPart{}, ErrInvalidPart
	}
	return parts.Parts[0], nil
}

func (s Service) ListParts(ctx context.Context, key, uploadID string, after int32, limit int) (MultipartPartList, error) {
	if _, err := s.activeMultipart(ctx, key, uploadID); err != nil {
		return MultipartPartList{}, err
	}
	return s.Index.ListMultipartParts(ctx, uploadID, after, limit)
}

func (s Service) CompleteMultipart(ctx context.Context, request CompleteMultipartRequest) (Object, error) {
	backend, err := s.multipartBackend()
	if err != nil {
		return Object{}, err
	}
	finishActivity := s.Index.beginWriteActivity(request.Key)
	defer finishActivity()
	upload, err := s.activeMultipart(ctx, request.Key, request.UploadID)
	if err != nil {
		return Object{}, err
	}
	stored, err := s.allMultipartParts(ctx, upload.ID)
	if err != nil {
		return Object{}, err
	}
	size, normalized, err := validateCompletedParts(request.Parts, stored)
	if err != nil {
		return Object{}, err
	}
	if err := s.Index.BeginCompleteMultipart(ctx, upload.ID); err != nil {
		return Object{}, err
	}
	restore := func(cause error) error {
		if resetErr := s.Index.ResetMultipart(ctx, upload.ID); resetErr != nil {
			_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
			return errors.Join(cause, resetErr)
		}
		return cause
	}
	target, err := s.target(ctx, upload.BucketID)
	if err != nil {
		return Object{}, restore(err)
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		return Object{}, restore(err)
	}
	if err := s.Index.MarkWriteCompleting(ctx, upload.WriteIntentID, "", size); err != nil {
		return Object{}, restore(err)
	}
	intent, err := s.Index.GetWriteIntent(ctx, upload.WriteIntentID)
	if err != nil {
		return Object{}, restore(err)
	}
	putOptions, err := s.physicalPutOptions(ctx, intent)
	if err != nil {
		return Object{}, restore(err)
	}
	etag, err := backend.CompleteMultipart(ctx, target, upload.Key, upload.UpstreamID, normalized, putOptions)
	if err != nil {
		if errors.Is(err, ErrConditionalRequestConflict) {
			cleanupCtx := context.WithoutCancel(ctx)
			markErr := s.Index.MarkWriteAborting(cleanupCtx, upload.WriteIntentID, upload.UpstreamID)
			if cleanupErr := s.Index.AbortClientMultipart(cleanupCtx, upload.ID); cleanupErr != nil {
				return Object{}, errors.Join(err, markErr, cleanupErr)
			}
			return Object{}, err
		}
		if isMultipartNotFound(err) {
			return s.resolveConsumedMultipartComplete(ctx, backend, upload, target, err)
		}
		if isDefinitiveWriteRejection(err) {
			return Object{}, restore(err)
		}
		return s.resolveMultipartComplete(ctx, upload, target, err)
	}
	etag = strings.Trim(etag, `"`)
	if err := s.Index.MarkWriteCompleting(ctx, upload.WriteIntentID, etag, size); err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return Object{}, err
	}
	return s.Index.CommitWrite(ctx, upload.WriteIntentID, etag, size)
}

func (s Service) resolveConsumedMultipartComplete(
	ctx context.Context,
	backend MultipartBackend,
	upload MultipartUpload,
	target Target,
	cause error,
) (Object, error) {
	cleanupCtx := context.WithoutCancel(ctx)
	if err := s.Index.MarkWriteConsumed(cleanupCtx, upload.WriteIntentID, upload.UpstreamID); err != nil {
		return Object{}, errors.Join(cause, err)
	}
	intent, err := s.Index.GetWriteIntent(cleanupCtx, upload.WriteIntentID)
	if err != nil {
		return Object{}, errors.Join(cause, err)
	}
	maintenance, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		return Object{}, cause
	}
	_ = s.Index.RecordOperation(cleanupCtx, target.AccountID, OperationClassB)
	remote, headErr := maintenance.Head(cleanupCtx, target, upload.Key)
	state, classifyErr := s.classifyWriteIntentHead(cleanupCtx, intent, remote, headErr)
	if classifyErr != nil || state == remoteWriteAmbiguous {
		if classifyErr != nil {
			return Object{}, errors.Join(cause, classifyErr)
		}
		return Object{}, errors.Join(cause, ErrWriteRecoveryAmbiguous)
	}
	if state == remoteWritePublished {
		object, commitErr := s.Index.CommitWrite(cleanupCtx, upload.WriteIntentID, remote.ETag, remote.Size)
		if commitErr != nil {
			return Object{}, errors.Join(cause, commitErr)
		}
		return object, nil
	}
	if err := s.Index.MarkWriteAborting(cleanupCtx, upload.WriteIntentID, upload.UpstreamID); err != nil {
		return Object{}, errors.Join(cause, err)
	}
	remote, state, abortErr := s.abortMultipartThenClassify(
		cleanupCtx, backend, maintenance, target, intent, upload.Key, upload.UpstreamID,
	)
	if abortErr != nil {
		return Object{}, errors.Join(cause, abortErr)
	}
	if state == remoteWritePublished {
		object, commitErr := s.Index.CommitWrite(cleanupCtx, upload.WriteIntentID, remote.ETag, remote.Size)
		if commitErr != nil {
			return Object{}, errors.Join(cause, commitErr)
		}
		return object, nil
	}
	if state == remoteWriteAmbiguous {
		return Object{}, errors.Join(cause, ErrWriteRecoveryAmbiguous)
	}
	if cleanupErr := s.Index.AbortClientMultipart(cleanupCtx, upload.ID); cleanupErr != nil {
		return Object{}, errors.Join(cause, cleanupErr)
	}
	return Object{}, cause
}

func (s Service) AbortMultipart(ctx context.Context, key, uploadID string) error {
	backend, err := s.multipartBackend()
	if err != nil {
		return err
	}
	finishActivity := s.Index.beginWriteActivity(key)
	defer finishActivity()
	upload, err := s.Index.GetMultipart(ctx, uploadID)
	if err != nil || upload.Key != key {
		return ErrMultipartNotFound
	}
	target, err := s.target(ctx, upload.BucketID)
	if err != nil {
		return err
	}
	if upload.UpstreamID == "" {
		return s.Index.AbortClientMultipart(ctx, upload.ID)
	}
	if upload.WriteIntentID == "" {
		return s.settleUnboundMultipart(ctx, backend, target, upload, upload.Key, ErrWriteRecoveryAmbiguous)
	}
	intent, err := s.Index.GetWriteIntent(ctx, upload.WriteIntentID)
	if err != nil {
		return err
	}
	maintenance, err := s.maintenanceBackend()
	if err != nil {
		return err
	}
	if err := s.Index.MarkWriteAborting(ctx, upload.WriteIntentID, upload.UpstreamID); err != nil {
		return err
	}
	remote, state, err := s.abortMultipartThenClassify(
		ctx, backend, maintenance, target, intent, upload.Key, upload.UpstreamID,
	)
	if err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return err
	}
	switch state {
	case remoteWritePublished:
		if _, err := s.Index.CommitWrite(ctx, upload.WriteIntentID, remote.ETag, remote.Size); err != nil {
			_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
			return err
		}
		return nil
	case remoteWritePrevious, remoteWriteAbsent:
		return s.Index.AbortClientMultipart(ctx, upload.ID)
	default:
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return ErrWriteRecoveryAmbiguous
	}
}

func (s Service) settleUnboundMultipart(
	ctx context.Context,
	backend MultipartBackend,
	target Target,
	upload MultipartUpload,
	physicalKey string,
	ambiguous error,
) error {
	if upload.UpstreamID == "" {
		return s.Index.DeleteMultipart(ctx, upload.ID)
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
	abortErr := backend.AbortMultipart(ctx, target, physicalKey, upload.UpstreamID)
	if abortErr == nil {
		return s.Index.DeleteMultipart(ctx, upload.ID)
	}
	if !isMultipartNotFound(abortErr) {
		return abortErr
	}
	maintenance, err := s.maintenanceBackend()
	if err != nil {
		return fmt.Errorf("%w: inspect unbound multipart %s after NoSuchUpload: %v", ambiguous, upload.ID, err)
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	_, headErr := maintenance.Head(ctx, target, physicalKey)
	if isRemoteNotFound(headErr) {
		return s.Index.DeleteMultipart(ctx, upload.ID)
	}
	if headErr != nil {
		return fmt.Errorf("%w: inspect unbound multipart %s after NoSuchUpload: %v", ambiguous, upload.ID, headErr)
	}
	return fmt.Errorf("%w: remote object %q exists after NoSuchUpload for unbound multipart %s", ambiguous, physicalKey, upload.ID)
}

func (s Service) resolveMultipartComplete(ctx context.Context, upload MultipartUpload, target Target, cause error) (Object, error) {
	backend, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return Object{}, cause
	}
	intent, intentErr := s.Index.GetWriteIntent(ctx, upload.WriteIntentID)
	if intentErr != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return Object{}, errors.Join(cause, intentErr)
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, headErr := backend.Head(ctx, target, upload.Key)
	state, classifyErr := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
	if classifyErr != nil || state == remoteWriteAmbiguous {
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		if classifyErr != nil {
			return Object{}, errors.Join(cause, classifyErr)
		}
		return Object{}, errors.Join(cause, ErrWriteRecoveryAmbiguous)
	}
	if state == remoteWritePublished {
		object, commitErr := s.Index.CommitWrite(ctx, upload.WriteIntentID, remote.ETag, remote.Size)
		if commitErr == nil {
			return object, nil
		}
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return Object{}, commitErr
	}
	if state == remoteWritePrevious || state == remoteWriteAbsent {
		if resetErr := s.Index.ResetMultipart(ctx, upload.ID); resetErr != nil {
			_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
			return Object{}, errors.Join(cause, resetErr)
		}
	}
	return Object{}, cause
}

func (s Service) ListMultipart(ctx context.Context, options ListMultipartOptions) (MultipartUploadList, error) {
	return s.Index.ListMultipart(ctx, options)
}

func (s Service) ExpireMultipart(ctx context.Context, maxAge time.Duration, limit int) (int, error) {
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	cutoff := time.Now().Add(-maxAge)
	uploads, err := s.Index.ListExpiredMultipart(ctx, cutoff, limit)
	if err != nil {
		return 0, err
	}
	expired := 0
	var expiryErr error
	for _, snapshot := range uploads {
		finishActivity, acquired := s.Index.tryBeginWriteActivity(snapshot.Key)
		if !acquired {
			continue
		}
		upload, err := s.Index.GetMultipart(ctx, snapshot.ID)
		if errors.Is(err, ErrMultipartNotFound) {
			finishActivity()
			continue
		}
		if err != nil {
			finishActivity()
			expiryErr = errors.Join(expiryErr, fmt.Errorf("reload expired multipart %s: %w", snapshot.ID, err))
			continue
		}
		if upload.Key != snapshot.Key || upload.LastModified.After(cutoff) || !expirableMultipartStatus(upload.Status) {
			finishActivity()
			continue
		}
		didExpire, err := s.expireMultipartUpload(ctx, upload)
		if err != nil {
			deferErr := s.Index.DeferMultipartExpiry(context.WithoutCancel(ctx), upload.ID)
			finishActivity()
			itemErr := err
			if deferErr != nil {
				itemErr = errors.Join(itemErr, fmt.Errorf("defer expiry retry: %w", deferErr))
			}
			expiryErr = errors.Join(expiryErr, fmt.Errorf("expire multipart %s: %w", upload.ID, itemErr))
			continue
		}
		finishActivity()
		if didExpire {
			expired++
		}
	}
	return expired, expiryErr
}

func expirableMultipartStatus(status MultipartStatus) bool {
	return status == MultipartInitiating || status == MultipartActive ||
		status == MultipartCompleting || status == MultipartError
}

func (s Service) expireMultipartUpload(ctx context.Context, upload MultipartUpload) (bool, error) {
	if upload.Status == MultipartCompleting {
		maintenance, err := s.maintenanceBackend()
		if err != nil {
			return false, err
		}
		target, err := s.target(ctx, upload.BucketID)
		if err != nil {
			return false, err
		}
		intent, err := s.Index.GetWriteIntent(ctx, upload.WriteIntentID)
		if err != nil {
			return false, err
		}
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
		remote, headErr := maintenance.Head(ctx, target, upload.Key)
		state, classifyErr := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
		if classifyErr != nil {
			return false, classifyErr
		}
		if state == remoteWriteAmbiguous {
			return false, ErrWriteRecoveryAmbiguous
		}
		if state == remoteWritePublished {
			_, err := s.Index.CommitWrite(ctx, upload.WriteIntentID, remote.ETag, remote.Size)
			return err == nil, err
		}
	}
	if upload.UpstreamID == "" {
		return true, s.Index.AbortClientMultipart(ctx, upload.ID)
	}
	backend, err := s.multipartBackend()
	if err != nil {
		return false, err
	}
	target, err := s.target(ctx, upload.BucketID)
	if err != nil {
		return false, err
	}
	_ = s.Index.MarkWriteAborting(ctx, upload.WriteIntentID, upload.UpstreamID)
	maintenance, err := s.maintenanceBackend()
	if err != nil {
		return false, err
	}
	intent, err := s.Index.GetWriteIntent(ctx, upload.WriteIntentID)
	if err != nil {
		return false, err
	}
	remote, state, err := s.abortMultipartThenClassify(ctx, backend, maintenance, target, intent, upload.Key, upload.UpstreamID)
	if err != nil {
		return false, err
	}
	if state == remoteWritePublished {
		_, err := s.Index.CommitWrite(ctx, upload.WriteIntentID, remote.ETag, remote.Size)
		return err == nil, err
	}
	if state == remoteWriteAmbiguous {
		return false, ErrWriteRecoveryAmbiguous
	}
	return true, s.Index.AbortClientMultipart(ctx, upload.ID)
}

func (s Service) abortMultipartThenClassify(
	ctx context.Context,
	backend MultipartBackend,
	maintenance MaintenanceBackend,
	target Target,
	intent WriteIntent,
	key string,
	uploadID string,
) (RemoteObject, remoteWriteState, error) {
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
	if err := backend.AbortMultipart(ctx, target, key, uploadID); err != nil && !isMultipartNotFound(err) {
		return RemoteObject{}, remoteWriteAmbiguous, err
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, headErr := maintenance.Head(ctx, target, intent.Key)
	state, err := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
	return remote, state, err
}

func (s Service) remoteMatchesWriteIntent(ctx context.Context, writeIntentID string, remote RemoteObject) (bool, error) {
	intent, err := s.Index.GetWriteIntent(ctx, writeIntentID)
	if err != nil {
		return false, err
	}
	state, err := s.classifyWriteIntentRemote(ctx, intent, &remote)
	if err != nil {
		return false, err
	}
	if state == remoteWriteAmbiguous {
		return false, fmt.Errorf("%w: intent %s", ErrWriteRecoveryAmbiguous, intent.ID)
	}
	return state == remoteWritePublished, nil
}

type remoteWriteState uint8

const (
	remoteWritePublished remoteWriteState = iota
	remoteWritePrevious
	remoteWriteAbsent
	remoteWriteAmbiguous
)

func (s Service) classifyWriteIntentHead(
	ctx context.Context,
	intent WriteIntent,
	remote RemoteObject,
	headErr error,
) (remoteWriteState, error) {
	if headErr == nil {
		return s.classifyWriteIntentRemote(ctx, intent, &remote)
	}
	if isRemoteNotFound(headErr) {
		return s.classifyWriteIntentRemote(ctx, intent, nil)
	}
	return remoteWriteAmbiguous, headErr
}

func (s Service) classifyWriteIntentRemote(
	ctx context.Context,
	intent WriteIntent,
	remote *RemoteObject,
) (remoteWriteState, error) {
	var previous Object
	hasPrevious := intent.PreviousObjectID != ""
	if hasPrevious {
		var err error
		previous, err = s.Index.GetObjectByID(ctx, intent.PreviousObjectID)
		if err != nil {
			return remoteWriteAmbiguous, fmt.Errorf("read previous object for write recovery: %w", err)
		}
	}
	previousAtTarget := hasPrevious && previous.BucketID == intent.BucketID && previous.PhysicalKey == intent.Key
	if remote == nil {
		if previousAtTarget {
			return remoteWriteAmbiguous, nil
		}
		return remoteWriteAbsent, nil
	}

	writeID := remote.Metadata[InternalWriteIDMetadata]
	if writeID == intent.ID {
		return remoteWritePublished, nil
	}
	if writeID != "" {
		if previousAtTarget && writeID == previous.ObjectID {
			return remoteWritePrevious, nil
		}
		return remoteWriteAmbiguous, nil
	}
	publishedMatch := objectETagsEqual(intent.ETag, remote.ETag) && intent.ActualSize == remote.Size
	previousMatch := previousAtTarget && objectETagsEqual(previous.ETag, remote.ETag) && previous.Size == remote.Size
	if publishedMatch && previousMatch {
		return remoteWriteAmbiguous, nil
	}
	if publishedMatch {
		return remoteWritePublished, nil
	}
	if previousMatch {
		return remoteWritePrevious, nil
	}
	if !previousAtTarget {
		return remoteWriteAmbiguous, nil
	}
	if intent.Operation == WriteOperationLegacyMultipart {
		if previous.Size != remote.Size ||
			(validObjectETag(previous.ETag) && validObjectETag(remote.ETag) && !objectETagsEqual(previous.ETag, remote.ETag)) {
			return remoteWritePublished, nil
		}
	}
	return remoteWriteAmbiguous, nil
}

func objectETagsEqual(left, right string) bool {
	normalizedLeft, leftValid := normalizeObjectETag(left)
	normalizedRight, rightValid := normalizeObjectETag(right)
	return leftValid && rightValid && normalizedLeft == normalizedRight
}

func (s Service) multipartBackend() (MultipartBackend, error) {
	if s.Index == nil || s.Accounts == nil || s.Backend == nil {
		return nil, errors.New("R2 service is not configured")
	}
	backend, ok := s.Backend.(MultipartBackend)
	if !ok {
		return nil, errors.New("R2 backend does not support multipart uploads")
	}
	return backend, nil
}

func (s Service) activeMultipart(ctx context.Context, key, uploadID string) (MultipartUpload, error) {
	upload, err := s.Index.GetMultipart(ctx, uploadID)
	if err != nil || upload.Key != key || upload.Status != MultipartActive {
		return MultipartUpload{}, ErrMultipartNotFound
	}
	return upload, nil
}

func (s Service) allMultipartParts(ctx context.Context, uploadID string) ([]MultipartPart, error) {
	var result []MultipartPart
	var after int32
	for {
		page, err := s.Index.ListMultipartParts(ctx, uploadID, after, 1000)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Parts...)
		if page.NextPartNumber == 0 {
			return result, nil
		}
		after = page.NextPartNumber
	}
}

func validateCompletedParts(requested []CompletedPart, stored []MultipartPart) (int64, []CompletedPart, error) {
	if len(requested) == 0 {
		return 0, nil, ErrInvalidPart
	}
	byNumber := make(map[int32]MultipartPart, len(stored))
	for _, part := range stored {
		byNumber[part.PartNumber] = part
	}
	normalized := make([]CompletedPart, 0, len(requested))
	var previous int32
	var size int64
	for _, part := range requested {
		if part.PartNumber <= previous {
			return 0, nil, ErrInvalidPartOrder
		}
		storedPart, ok := byNumber[part.PartNumber]
		if !ok || !strings.EqualFold(strings.Trim(part.ETag, `"`), strings.Trim(storedPart.ETag, `"`)) {
			return 0, nil, ErrInvalidPart
		}
		previous = part.PartNumber
		size += storedPart.Size
		normalized = append(normalized, CompletedPart{PartNumber: part.PartNumber, ETag: storedPart.ETag})
	}
	return size, normalized, nil
}

func removeStagedFile(file *os.File) {
	if file == nil {
		return
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
}
