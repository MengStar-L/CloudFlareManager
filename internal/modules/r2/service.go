package r2

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
)

type Target struct {
	AccountID           string
	CloudflareAccountID string
	AccessKeyID         string
	SecretAccessKey     string
	Bucket              string
}

type GetOptions struct {
	Range             string
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   *time.Time
	IfUnmodifiedSince *time.Time
}

type GetResult struct {
	Body         io.ReadCloser
	Size         int64
	ETag         string
	ContentType  string
	Metadata     map[string]string
	LastModified time.Time
	ContentRange string
}

type Backend interface {
	Put(context.Context, Target, string, io.Reader, int64, string, map[string]string) (string, error)
	Get(context.Context, Target, string, GetOptions) (GetResult, error)
	Delete(context.Context, Target, string) error
}

type PutRequest struct {
	Key         string
	Body        io.Reader
	Size        int64
	ContentType string
	Metadata    map[string]string
	PayloadHash string
}

type Service struct {
	Index    *Store
	Accounts *accounts.Store
	Backend  Backend
	Usage    AccountUsageProvider
	TempDir  string
	// ChunkBytes 是服务端强制分片的块大小：超过该大小（或长度未知）的单次
	// PUT 会在服务端切块经 multipart 转发，本地磁盘峰值仅为单块大小。
	ChunkBytes int64
}

type AccountUsageProvider interface {
	R2BucketUsage(context.Context, string, string) (map[string]accounts.BucketUsage, error)
}

const defaultChunkBytes = 64 << 20

func (s Service) chunkSize() int64 {
	if s.ChunkBytes > 0 {
		return s.ChunkBytes
	}
	return defaultChunkBytes
}

func (s Service) Put(ctx context.Context, request PutRequest) (Object, error) {
	if s.Index == nil || s.Accounts == nil || s.Backend == nil {
		return Object{}, errors.New("R2 service is not configured")
	}
	if request.Body == nil {
		request.Body = &emptyReader{}
	}
	// 大对象或未知长度：不整体落盘，切块经 multipart 转发；
	// 校验失败时 Abort 丢弃已传分片，语义与整体校验一致。
	if backend, ok := s.Backend.(MultipartBackend); ok {
		if request.Size < 0 || request.Size > s.chunkSize() {
			return s.putChunked(ctx, backend, request)
		}
	}
	file, size, digest, err := s.spool(request.Body)
	if err != nil {
		return Object{}, err
	}
	defer func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}()
	if request.Size >= 0 && request.Size != size {
		return Object{}, fmt.Errorf("content length mismatch: expected %d bytes, received %d", request.Size, size)
	}
	if err := validatePayloadHash(request.PayloadHash, digest); err != nil {
		return Object{}, err
	}
	intent, err := s.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{
		Key: request.Key, Size: size, ContentType: request.ContentType, Metadata: request.Metadata,
	}, ExpectedClassA: 1})
	if err != nil {
		return Object{}, err
	}
	target, err := s.target(ctx, intent.BucketID)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return Object{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return Object{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return Object{}, err
	}
	if err := s.Index.MarkWriteUploading(ctx, intent.ID, ""); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return Object{}, err
	}
	etag, err := s.Backend.Put(ctx, target, intent.Key, file, size, request.ContentType, upstreamWriteMetadata(request.Metadata, intent.ID))
	if err != nil {
		return s.resolveAmbiguousWrite(ctx, intent, target, err)
	}
	if err := s.Index.MarkWriteCompleting(ctx, intent.ID, etag, size); err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
		return Object{}, err
	}
	return s.Index.CommitWrite(ctx, intent.ID, etag, size)
}

func (s Service) putChunked(ctx context.Context, backend MultipartBackend, request PutRequest) (Object, error) {
	expectedOps := int64(1)
	if request.Size >= 0 {
		expectedOps = 2 + (request.Size+s.chunkSize()-1)/s.chunkSize()
	}
	intent, err := s.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{
		Key: request.Key, Size: request.Size, ContentType: request.ContentType, Metadata: request.Metadata,
	}, ExpectedClassA: expectedOps, InternalMultipart: true})
	if err != nil {
		return Object{}, err
	}
	target, err := s.target(ctx, intent.BucketID)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return Object{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return Object{}, err
	}
	uploadID, err := backend.CreateMultipart(ctx, target, intent.Key, request.ContentType, upstreamWriteMetadata(request.Metadata, intent.ID))
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return Object{}, err
	}
	if err := s.Index.MarkWriteUploading(ctx, intent.ID, uploadID); err != nil {
		if abortErr := backend.AbortMultipart(ctx, target, intent.Key, uploadID); abortErr == nil {
			_ = s.Index.AbortWrite(ctx, intent.ID)
		} else {
			_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		}
		return Object{}, err
	}
	abort := func(reason error) {
		_ = s.Index.MarkWriteAborting(ctx, intent.ID, uploadID)
		if err := backend.AbortMultipart(ctx, target, intent.Key, uploadID); err == nil {
			_ = s.Index.AbortWrite(ctx, intent.ID)
		}
	}

	hash := sha256.New()
	source := io.TeeReader(request.Body, hash)
	var (
		parts      []CompletedPart
		total      int64
		partNumber int32
	)
	for {
		file, partSize, _, err := s.spool(io.LimitReader(source, s.chunkSize()))
		if err != nil {
			abort(err)
			return Object{}, err
		}
		if partSize == 0 && partNumber > 0 {
			removeStagedFile(file)
			break
		}
		partNumber++
		if _, err := s.Index.EnsureWriteReservation(ctx, intent.ID, total+partSize); err != nil {
			removeStagedFile(file)
			abort(err)
			return Object{}, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			removeStagedFile(file)
			abort(err)
			return Object{}, err
		}
		if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
			removeStagedFile(file)
			abort(err)
			return Object{}, err
		}
		etag, err := backend.UploadPart(ctx, target, intent.Key, uploadID, partNumber, file, partSize)
		removeStagedFile(file)
		if err != nil {
			abort(err)
			return Object{}, err
		}
		parts = append(parts, CompletedPart{PartNumber: partNumber, ETag: etag})
		total += partSize
		if partSize < s.chunkSize() {
			break
		}
	}
	if request.Size >= 0 && request.Size != total {
		err := fmt.Errorf("content length mismatch: expected %d bytes, received %d", request.Size, total)
		abort(err)
		return Object{}, err
	}
	if err := validatePayloadHash(request.PayloadHash, hash.Sum(nil)); err != nil {
		abort(err)
		return Object{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		abort(err)
		return Object{}, err
	}
	if err := s.Index.MarkWriteCompleting(ctx, intent.ID, "", total); err != nil {
		abort(err)
		return Object{}, err
	}
	etag, err := backend.CompleteMultipart(ctx, target, intent.Key, uploadID, parts)
	if err != nil {
		return s.resolveInternalMultipartComplete(ctx, backend, intent, target, uploadID, err)
	}
	if err := s.Index.MarkWriteCompleting(ctx, intent.ID, etag, total); err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return Object{}, err
	}
	return s.Index.CommitWrite(ctx, intent.ID, etag, total)
}

func (s Service) Get(ctx context.Context, key string, options GetOptions) (GetResult, error) {
	object, err := s.Index.GetObject(ctx, key)
	if err != nil {
		return GetResult{}, err
	}
	target, err := s.target(ctx, object.BucketID)
	if err != nil {
		return GetResult{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassB); err != nil {
		return GetResult{}, err
	}
	return s.Backend.Get(ctx, target, object.PhysicalKey, options)
}

func (s Service) Stat(ctx context.Context, key string) (Object, error) {
	return s.Index.GetObject(ctx, key)
}

func (s Service) List(ctx context.Context, options ListOptions) (ObjectList, error) {
	return s.Index.ListObjects(ctx, options)
}

func (s Service) Delete(ctx context.Context, key string) error {
	intent, object, err := s.Index.BeginDeleteWrite(ctx, key)
	if err != nil {
		return err
	}
	target, err := s.target(ctx, object.BucketID)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return err
	}
	if err := s.Backend.Delete(ctx, target, object.PhysicalKey); err != nil {
		return s.resolveAmbiguousDelete(ctx, intent, object, target, err)
	}
	return s.Index.CommitDeleteWrite(ctx, intent.ID)
}

func (s Service) resolveAmbiguousDelete(ctx context.Context, intent WriteIntent, object Object, target Target, cause error) error {
	backend, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
		return cause
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	_, err := backend.Head(ctx, target, object.PhysicalKey)
	if isRemoteNotFound(err) {
		return s.Index.CommitDeleteWrite(ctx, intent.ID)
	}
	if err == nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
	} else {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
	}
	return cause
}

func (s Service) Copy(ctx context.Context, source, destination string) (Object, error) {
	object, err := s.Index.GetObject(ctx, source)
	if err != nil {
		return Object{}, err
	}
	result, err := s.Get(ctx, source, GetOptions{})
	if err != nil {
		return Object{}, err
	}
	defer result.Body.Close()
	return s.Put(ctx, PutRequest{
		Key: destination, Body: result.Body, Size: object.Size, ContentType: object.ContentType,
		Metadata: object.Metadata, PayloadHash: "UNSIGNED-PAYLOAD",
	})
}

func (s Service) target(ctx context.Context, bucketID string) (Target, error) {
	bucket, err := s.Index.GetBucket(ctx, bucketID)
	if err != nil {
		return Target{}, err
	}
	account, err := s.Accounts.Get(ctx, bucket.AccountID, true)
	if err != nil {
		return Target{}, err
	}
	if account.R2AccessKeyID == "" || account.R2SecretAccessKey == "" {
		return Target{}, errors.New("account does not have R2 S3 credentials")
	}
	return Target{
		AccountID:           account.ID,
		CloudflareAccountID: account.CloudflareAccountID, AccessKeyID: account.R2AccessKeyID,
		SecretAccessKey: account.R2SecretAccessKey, Bucket: bucket.Name,
	}, nil
}

func (s Service) resolveAmbiguousWrite(ctx context.Context, intent WriteIntent, target Target, cause error) (Object, error) {
	backend, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, intent.UpstreamUploadID)
		return Object{}, cause
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, err := backend.Head(ctx, target, intent.Key)
	if err == nil && remote.Metadata[InternalWriteIDMetadata] == intent.ID {
		object, commitErr := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
		if commitErr == nil {
			return object, nil
		}
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, intent.UpstreamUploadID)
		return Object{}, commitErr
	}
	if err == nil || isRemoteNotFound(err) {
		_ = s.Index.AbortWrite(ctx, intent.ID)
	} else {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, intent.UpstreamUploadID)
	}
	return Object{}, cause
}

func (s Service) resolveInternalMultipartComplete(ctx context.Context, multipartBackend MultipartBackend, intent WriteIntent, target Target, uploadID string, cause error) (Object, error) {
	backend, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return Object{}, cause
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, err := backend.Head(ctx, target, intent.Key)
	if err == nil && remote.Metadata[InternalWriteIDMetadata] == intent.ID {
		object, commitErr := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
		if commitErr == nil {
			return object, nil
		}
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return Object{}, commitErr
	}
	if err != nil && !isRemoteNotFound(err) {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return Object{}, cause
	}
	_ = s.Index.MarkWriteAborting(ctx, intent.ID, uploadID)
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
	if err := multipartBackend.AbortMultipart(ctx, target, intent.Key, uploadID); err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return Object{}, cause
	}
	_ = s.Index.AbortWrite(ctx, intent.ID)
	return Object{}, cause
}

func upstreamWriteMetadata(metadata map[string]string, writeID string) map[string]string {
	result := userVisibleMetadata(metadata)
	result[InternalWriteIDMetadata] = writeID
	return result
}

func (s Service) spool(source io.Reader) (*os.File, int64, []byte, error) {
	directory := s.TempDir
	if directory == "" {
		directory = os.TempDir()
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, 0, nil, err
	}
	file, err := os.CreateTemp(directory, ".r2-upload-*")
	if err != nil {
		return nil, 0, nil, err
	}
	_ = file.Chmod(0o600)
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), source)
	if err != nil {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
		return nil, 0, nil, fmt.Errorf("stage upload: %w", err)
	}
	return file, size, hash.Sum(nil), nil
}

func validatePayloadHash(expected string, actual []byte) error {
	if expected == "" || expected == "UNSIGNED-PAYLOAD" || expected == "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" {
		return nil
	}
	want, err := hex.DecodeString(expected)
	if err != nil || len(want) != sha256.Size || subtle.ConstantTimeCompare(want, actual) != 1 {
		return ErrPayloadHashMismatch
	}
	return nil
}

func classifyUpstreamError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func isRemoteNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrObjectNotFound) {
		return true
	}
	var apiError interface{ ErrorCode() string }
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NoSuchObject", "NotFound", "404":
			return true
		}
	}
	var statusError interface{ HTTPStatusCode() int }
	return errors.As(err, &statusError) && statusError.HTTPStatusCode() == 404
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

var ErrPayloadHashMismatch = errors.New("request payload hash does not match")
