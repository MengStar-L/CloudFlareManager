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
	CompleteMultipart(context.Context, Target, string, string, []CompletedPart) (string, error)
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
		_ = s.Index.FailMultipart(ctx, upload.ID)
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, "")
		return MultipartUpload{}, err
	}
	if err := s.Index.ActivateMultipart(ctx, upload.ID, upstreamID); err != nil {
		if abortErr := backend.AbortMultipart(ctx, target, upload.Key, upstreamID); abortErr == nil {
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
	target, err := s.target(ctx, upload.BucketID)
	if err != nil {
		_ = s.Index.ResetMultipart(ctx, upload.ID)
		return Object{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		_ = s.Index.ResetMultipart(ctx, upload.ID)
		return Object{}, err
	}
	if err := s.Index.MarkWriteCompleting(ctx, upload.WriteIntentID, "", size); err != nil {
		_ = s.Index.ResetMultipart(ctx, upload.ID)
		return Object{}, err
	}
	etag, err := backend.CompleteMultipart(ctx, target, upload.Key, upload.UpstreamID, normalized)
	if err != nil {
		return s.resolveMultipartComplete(ctx, upload, target, err)
	}
	etag = strings.Trim(etag, `"`)
	if err := s.Index.MarkWriteCompleting(ctx, upload.WriteIntentID, etag, size); err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return Object{}, err
	}
	return s.Index.CommitWrite(ctx, upload.WriteIntentID, etag, size)
}

func (s Service) AbortMultipart(ctx context.Context, key, uploadID string) error {
	backend, err := s.multipartBackend()
	if err != nil {
		return err
	}
	upload, err := s.Index.GetMultipart(ctx, uploadID)
	if err != nil || upload.Key != key {
		return ErrMultipartNotFound
	}
	target, err := s.target(ctx, upload.BucketID)
	if err != nil {
		return err
	}
	if upload.WriteIntentID != "" {
		_ = s.Index.MarkWriteAborting(ctx, upload.WriteIntentID, upload.UpstreamID)
	}
	if upload.UpstreamID != "" {
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
		if err := backend.AbortMultipart(ctx, target, upload.Key, upload.UpstreamID); err != nil {
			return err
		}
	}
	return s.Index.AbortClientMultipart(ctx, upload.ID)
}

func (s Service) resolveMultipartComplete(ctx context.Context, upload MultipartUpload, target Target, cause error) (Object, error) {
	backend, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return Object{}, cause
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, err := backend.Head(ctx, target, upload.Key)
	matches := false
	if err == nil {
		matches, err = s.remoteMatchesWriteIntent(ctx, upload.WriteIntentID, remote)
		if err != nil {
			_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
			return Object{}, cause
		}
	}
	if matches {
		object, commitErr := s.Index.CommitWrite(ctx, upload.WriteIntentID, remote.ETag, remote.Size)
		if commitErr == nil {
			return object, nil
		}
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
		return Object{}, commitErr
	}
	if err == nil || isRemoteNotFound(err) {
		_ = s.Index.ResetMultipart(ctx, upload.ID)
		_ = s.Index.MarkWriteUploading(ctx, upload.WriteIntentID, upload.UpstreamID)
	} else {
		_ = s.Index.HoldWriteForRecovery(ctx, upload.WriteIntentID, upload.UpstreamID)
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
	uploads, err := s.Index.ListExpiredMultipart(ctx, time.Now().Add(-maxAge), limit)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, upload := range uploads {
		if upload.Status == MultipartCompleting {
			maintenance, err := s.maintenanceBackend()
			if err != nil {
				return expired, err
			}
			target, err := s.target(ctx, upload.BucketID)
			if err != nil {
				return expired, err
			}
			_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
			remote, headErr := maintenance.Head(ctx, target, upload.Key)
			matches := false
			if headErr == nil {
				matches, headErr = s.remoteMatchesWriteIntent(ctx, upload.WriteIntentID, remote)
			}
			if matches {
				if _, err := s.Index.CommitWrite(ctx, upload.WriteIntentID, remote.ETag, remote.Size); err != nil {
					return expired, err
				}
				expired++
				continue
			}
			if headErr != nil && !isRemoteNotFound(headErr) {
				return expired, headErr
			}
		}
		if upload.UpstreamID == "" {
			if err := s.Index.AbortClientMultipart(ctx, upload.ID); err != nil {
				return expired, err
			}
			expired++
			continue
		}
		backend, err := s.multipartBackend()
		if err != nil {
			return expired, err
		}
		target, err := s.target(ctx, upload.BucketID)
		if err != nil {
			return expired, err
		}
		_ = s.Index.MarkWriteAborting(ctx, upload.WriteIntentID, upload.UpstreamID)
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
		if err := backend.AbortMultipart(ctx, target, upload.Key, upload.UpstreamID); err != nil {
			return expired, err
		}
		if err := s.Index.AbortClientMultipart(ctx, upload.ID); err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}

func (s Service) remoteMatchesWriteIntent(ctx context.Context, writeIntentID string, remote RemoteObject) (bool, error) {
	intent, err := s.Index.GetWriteIntent(ctx, writeIntentID)
	if err != nil {
		return false, err
	}
	if remote.Metadata[InternalWriteIDMetadata] == intent.ID {
		return true, nil
	}
	if intent.Operation != WriteOperationLegacyMultipart {
		return false, nil
	}
	if intent.PreviousObjectID == "" {
		return true, nil
	}
	previous, err := s.Index.GetObjectByID(ctx, intent.PreviousObjectID)
	if err != nil {
		return false, fmt.Errorf("read previous object for legacy multipart: %w", err)
	}
	if previous.BucketID != intent.BucketID || previous.PhysicalKey != intent.Key {
		return true, nil
	}
	previousETag := strings.Trim(previous.ETag, `"`)
	remoteETag := strings.Trim(remote.ETag, `"`)
	if previousETag != "" && remoteETag != "" {
		return !strings.EqualFold(previousETag, remoteETag), nil
	}
	if previous.Size != remote.Size {
		return true, nil
	}
	return false, errors.New("legacy multipart remote version cannot be distinguished from the previous object")
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
