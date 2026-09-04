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
	ExpectedObjectID  string
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

type PutOptions struct {
	IfMatch     string
	IfNoneMatch string
}

type Backend interface {
	Put(context.Context, Target, string, io.Reader, int64, string, map[string]string, PutOptions) (string, error)
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
	Conditions  MutationConditions
}

type PutResult struct {
	Object  Object
	Created bool
}

type Service struct {
	Index             *Store
	Accounts          *accounts.Store
	Backend           Backend
	Usage             AccountUsageProvider
	WebDAVCoordinator WebDAVMutationCoordinator
	TempDir           string
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
	result, err := s.PutConditional(ctx, request)
	return result.Object, err
}

func (s Service) PutConditional(ctx context.Context, request PutRequest) (PutResult, error) {
	if s.Index == nil || s.Accounts == nil || s.Backend == nil {
		return PutResult{}, errors.New("R2 service is not configured")
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
		return PutResult{}, err
	}
	defer func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}()
	if request.Size >= 0 && request.Size != size {
		return PutResult{}, fmt.Errorf("content length mismatch: expected %d bytes, received %d", request.Size, size)
	}
	if err := validatePayloadHash(request.PayloadHash, digest); err != nil {
		return PutResult{}, err
	}
	paths, err := s.webDAVCreationMutationPaths(ctx, request.Key)
	if err != nil {
		return PutResult{}, err
	}
	ctx, guard, err := s.beginWebDAVMutation(ctx, paths)
	if err != nil {
		return PutResult{}, err
	}
	if guard != nil {
		defer guard.Release()
	}
	finishActivity := s.Index.beginWriteActivity(request.Key)
	defer finishActivity()
	currentPaths, err := s.webDAVCreationMutationPaths(ctx, request.Key)
	if err != nil {
		return PutResult{}, err
	}
	if err := s.validateWebDAVMutationScope(ctx, currentPaths); err != nil {
		return PutResult{}, err
	}
	if err := s.repairCurrentObjectETag(ctx, request.Key); err != nil {
		return PutResult{}, err
	}
	intent, err := s.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{
		Key: request.Key, Size: size, ContentType: request.ContentType, Metadata: request.Metadata,
	}, ExpectedClassA: 1, Conditions: request.Conditions})
	if err != nil {
		return PutResult{}, err
	}
	created := intent.PreviousObjectID == ""
	target, err := s.target(ctx, intent.BucketID)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	putOptions, err := s.physicalPutOptions(ctx, intent)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	if err := s.Index.MarkWriteUploading(ctx, intent.ID, ""); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	etag, err := s.Backend.Put(ctx, target, intent.Key, file, size, request.ContentType,
		upstreamWriteMetadata(request.Metadata, intent.ID), putOptions)
	if err != nil {
		if isDefinitiveWriteRejection(err) {
			_ = s.Index.AbortWrite(ctx, intent.ID)
			return PutResult{}, err
		}
		object, resolveErr := s.resolveAmbiguousWrite(ctx, intent, target, err)
		return PutResult{Object: object, Created: created}, resolveErr
	}
	if err := s.Index.MarkWriteCompleting(ctx, intent.ID, etag, size); err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, "")
		return PutResult{}, err
	}
	object, err := s.Index.CommitWrite(ctx, intent.ID, etag, size)
	return PutResult{Object: object, Created: created}, err
}

func (s Service) putChunked(ctx context.Context, backend MultipartBackend, request PutRequest) (PutResult, error) {
	expectedOps := int64(1)
	if request.Size >= 0 {
		expectedOps = 2 + (request.Size+s.chunkSize()-1)/s.chunkSize()
	}
	paths, err := s.webDAVCreationMutationPaths(ctx, request.Key)
	if err != nil {
		return PutResult{}, err
	}
	ctx, guard, err := s.beginWebDAVMutation(ctx, paths)
	if err != nil {
		return PutResult{}, err
	}
	if guard != nil {
		defer guard.Release()
	}
	finishActivity := s.Index.beginWriteActivity(request.Key)
	defer finishActivity()
	currentPaths, err := s.webDAVCreationMutationPaths(ctx, request.Key)
	if err != nil {
		return PutResult{}, err
	}
	if err := s.validateWebDAVMutationScope(ctx, currentPaths); err != nil {
		return PutResult{}, err
	}
	if err := s.repairCurrentObjectETag(ctx, request.Key); err != nil {
		return PutResult{}, err
	}
	intent, err := s.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{
		Key: request.Key, Size: request.Size, ContentType: request.ContentType, Metadata: request.Metadata,
	}, ExpectedClassA: expectedOps, InternalMultipart: true, Conditions: request.Conditions})
	if err != nil {
		return PutResult{}, err
	}
	created := intent.PreviousObjectID == ""
	target, err := s.target(ctx, intent.BucketID)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	putOptions, err := s.physicalPutOptions(ctx, intent)
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	uploadID, err := backend.CreateMultipart(ctx, target, intent.Key, request.ContentType, upstreamWriteMetadata(request.Metadata, intent.ID))
	if err != nil {
		_ = s.Index.AbortWrite(ctx, intent.ID)
		return PutResult{}, err
	}
	if err := s.Index.MarkWriteUploading(ctx, intent.ID, uploadID); err != nil {
		if abortErr := backend.AbortMultipart(ctx, target, intent.Key, uploadID); abortErr == nil || isMultipartNotFound(abortErr) {
			_ = s.Index.AbortWrite(ctx, intent.ID)
		} else {
			_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		}
		return PutResult{}, err
	}
	abort := func(reason error) {
		_ = s.Index.MarkWriteAborting(ctx, intent.ID, uploadID)
		if err := backend.AbortMultipart(ctx, target, intent.Key, uploadID); err == nil || isMultipartNotFound(err) {
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
			return PutResult{}, err
		}
		if partSize == 0 && partNumber > 0 {
			removeStagedFile(file)
			break
		}
		partNumber++
		if _, err := s.Index.EnsureWriteReservation(ctx, intent.ID, total+partSize); err != nil {
			removeStagedFile(file)
			abort(err)
			return PutResult{}, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			removeStagedFile(file)
			abort(err)
			return PutResult{}, err
		}
		if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
			removeStagedFile(file)
			abort(err)
			return PutResult{}, err
		}
		etag, err := backend.UploadPart(ctx, target, intent.Key, uploadID, partNumber, file, partSize)
		removeStagedFile(file)
		if err != nil {
			abort(err)
			return PutResult{}, err
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
		return PutResult{}, err
	}
	if err := validatePayloadHash(request.PayloadHash, hash.Sum(nil)); err != nil {
		abort(err)
		return PutResult{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassA); err != nil {
		abort(err)
		return PutResult{}, err
	}
	if err := s.Index.MarkWriteCompleting(ctx, intent.ID, "", total); err != nil {
		abort(err)
		return PutResult{}, err
	}
	etag, err := backend.CompleteMultipart(ctx, target, intent.Key, uploadID, parts, putOptions)
	if err != nil {
		if isDefinitiveWriteRejection(err) {
			abort(err)
			return PutResult{}, err
		}
		object, resolveErr := s.resolveInternalMultipartComplete(ctx, backend, intent, target, uploadID, err)
		return PutResult{Object: object, Created: created}, resolveErr
	}
	if err := s.Index.MarkWriteCompleting(ctx, intent.ID, etag, total); err != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return PutResult{}, err
	}
	object, err := s.Index.CommitWrite(ctx, intent.ID, etag, total)
	return PutResult{Object: object, Created: created}, err
}

func (s Service) Get(ctx context.Context, key string, options GetOptions) (GetResult, error) {
	if options.ExpectedObjectID != "" {
		finishActivity := s.Index.beginWriteActivity(key)
		defer finishActivity()
	}
	object, err := s.Stat(ctx, key)
	if err != nil {
		return GetResult{}, err
	}
	if options.ExpectedObjectID != "" && object.ObjectID != options.ExpectedObjectID {
		return GetResult{}, ErrConditionalRequestConflict
	}
	if options.IfMatch == "" {
		options.IfMatch = quoteETag(object.ETag)
	}
	target, err := s.target(ctx, object.BucketID)
	if err != nil {
		return GetResult{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassB); err != nil {
		return GetResult{}, err
	}
	options.ExpectedObjectID = ""
	return s.Backend.Get(ctx, target, object.PhysicalKey, options)
}

func (s Service) Stat(ctx context.Context, key string) (Object, error) {
	object, err := s.Index.GetObject(ctx, key)
	if err != nil {
		return Object{}, err
	}
	if err := s.Index.EnsureBucketActive(ctx, object.BucketID); err != nil {
		return Object{}, err
	}
	return s.objectWithETag(ctx, object)
}

func (s Service) List(ctx context.Context, options ListOptions) (ObjectList, error) {
	result, err := s.Index.ListObjects(ctx, options)
	if err != nil {
		return ObjectList{}, err
	}
	for index, object := range result.Objects {
		if err := s.Index.EnsureBucketActive(ctx, object.BucketID); err != nil {
			if errors.Is(err, ErrBucketDeleting) {
				continue
			}
			return ObjectList{}, err
		}
		if validObjectETag(object.ETag) {
			continue
		}
		repaired, err := s.objectWithETag(ctx, object)
		if err != nil {
			return ObjectList{}, err
		}
		result.Objects[index] = repaired
	}
	return result, nil
}

func (s Service) Delete(ctx context.Context, key string) error {
	return s.DeleteConditional(ctx, key, MutationConditions{})
}

func (s Service) DeleteConditional(ctx context.Context, key string, conditions MutationConditions) error {
	paths, err := s.webDAVDeletionMutationPaths(ctx, key)
	if err != nil {
		return err
	}
	ctx, guard, err := s.beginWebDAVMutation(ctx, paths)
	if err != nil {
		return err
	}
	if guard != nil {
		defer guard.Release()
	}
	finishActivity := s.Index.beginWriteActivity(key)
	defer finishActivity()
	currentPaths, err := s.webDAVDeletionMutationPaths(ctx, key)
	if err != nil {
		return err
	}
	if err := s.validateWebDAVMutationScope(ctx, currentPaths); err != nil {
		return err
	}
	if err := s.repairCurrentObjectETag(ctx, key); err != nil {
		return err
	}
	intent, object, err := s.Index.BeginDeleteWriteConditional(ctx, key, conditions)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			if cleanupErr := s.cleanupDeletedWebDAVLocks(context.WithoutCancel(ctx), key, guard); cleanupErr != nil {
				return cleanupErr
			}
		}
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
		if err := s.resolveAmbiguousDelete(ctx, intent, object, target, err); err != nil {
			return err
		}
	} else if err := s.Index.CommitDeleteWrite(ctx, intent.ID); err != nil {
		return err
	}
	// The object is already gone. Finish local lock reconciliation even if the
	// client disconnected; a later idempotent DELETE retries it if this fails.
	return s.cleanupDeletedWebDAVLocks(context.WithoutCancel(ctx), key, guard)
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
	result, err := s.CopyConditional(ctx, source, destination, MutationConditions{})
	return result.Object, err
}

func (s Service) CopyConditional(ctx context.Context, source, destination string, conditions MutationConditions) (PutResult, error) {
	object, err := s.Stat(ctx, source)
	if err != nil {
		return PutResult{}, err
	}
	result, err := s.Get(ctx, source, GetOptions{
		IfMatch: quoteETag(object.ETag), ExpectedObjectID: object.ObjectID,
	})
	if err != nil {
		return PutResult{}, err
	}
	defer result.Body.Close()
	return s.PutConditional(ctx, PutRequest{
		Key: destination, Body: result.Body, Size: object.Size, ContentType: object.ContentType,
		Metadata: object.Metadata, PayloadHash: "UNSIGNED-PAYLOAD", Conditions: conditions,
	})
}

func (s Service) physicalPutOptions(ctx context.Context, intent WriteIntent) (PutOptions, error) {
	if intent.PreviousObjectID == "" {
		return PutOptions{IfNoneMatch: "*"}, nil
	}
	previous, err := s.Index.GetObjectByID(ctx, intent.PreviousObjectID)
	if err != nil {
		return PutOptions{}, err
	}
	previous, err = s.objectWithETag(ctx, previous)
	if err != nil {
		return PutOptions{}, err
	}
	if previous.BucketID != intent.BucketID || previous.PhysicalKey != intent.Key {
		return PutOptions{IfNoneMatch: "*"}, nil
	}
	return PutOptions{IfMatch: quoteETag(previous.ETag)}, nil
}

func (s Service) repairCurrentObjectETag(ctx context.Context, key string) error {
	object, err := s.Index.GetObject(ctx, key)
	if errors.Is(err, ErrObjectNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.objectWithETag(ctx, object)
	return err
}

func (s Service) objectWithETag(ctx context.Context, object Object) (Object, error) {
	if validObjectETag(object.ETag) {
		return object, nil
	}
	backend, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		return Object{}, ErrObjectETagUnavailable
	}
	target, err := s.target(ctx, object.BucketID)
	if err != nil {
		return Object{}, err
	}
	if err := s.Index.ConsumeOperation(ctx, target.AccountID, OperationClassB); err != nil {
		return Object{}, err
	}
	remote, err := backend.Head(ctx, target, object.PhysicalKey)
	if err != nil {
		return Object{}, err
	}
	if !validObjectETag(remote.ETag) {
		return Object{}, ErrObjectETagUnavailable
	}
	writeID := remote.Metadata[InternalWriteIDMetadata]
	if writeID != "" && writeID != object.ObjectID {
		return Object{}, ErrConditionalRequestConflict
	}
	if remote.Size != object.Size && writeID != object.ObjectID {
		return Object{}, ErrConditionalRequestConflict
	}
	return s.Index.BackfillObjectMetadata(ctx, object.ObjectID, object.ETag, object.Size, remote.ETag, remote.Size)
}

func isDefinitiveWriteRejection(err error) bool {
	return errors.Is(err, ErrConditionalRequestConflict) || errors.Is(err, ErrRateLimited)
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
		return Target{}, ErrR2CredentialsRequired
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
	remote, headErr := backend.Head(ctx, target, intent.Key)
	state, classifyErr := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
	if classifyErr != nil || state == remoteWriteAmbiguous {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, intent.UpstreamUploadID)
		if classifyErr != nil {
			return Object{}, errors.Join(cause, classifyErr)
		}
		return Object{}, errors.Join(cause, ErrWriteRecoveryAmbiguous)
	}
	if state == remoteWritePublished {
		object, commitErr := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
		if commitErr == nil {
			return object, nil
		}
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, intent.UpstreamUploadID)
		return Object{}, commitErr
	}
	if state == remoteWritePrevious || state == remoteWriteAbsent {
		_ = s.Index.AbortWrite(ctx, intent.ID)
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
	remote, headErr := backend.Head(ctx, target, intent.Key)
	state, classifyErr := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
	if classifyErr != nil || state == remoteWriteAmbiguous {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		if classifyErr != nil {
			return Object{}, errors.Join(cause, classifyErr)
		}
		return Object{}, errors.Join(cause, ErrWriteRecoveryAmbiguous)
	}
	if state == remoteWritePublished {
		object, commitErr := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
		if commitErr == nil {
			return object, nil
		}
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return Object{}, commitErr
	}
	_ = s.Index.MarkWriteAborting(ctx, intent.ID, uploadID)
	remote, state, abortErr := s.abortMultipartThenClassify(ctx, multipartBackend, backend, target, intent, intent.Key, uploadID)
	if abortErr != nil {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return Object{}, errors.Join(cause, abortErr)
	}
	if state == remoteWritePublished {
		object, commitErr := s.Index.CommitWrite(ctx, intent.ID, remote.ETag, remote.Size)
		if commitErr != nil {
			_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
			return Object{}, errors.Join(cause, commitErr)
		}
		return object, nil
	}
	if state == remoteWriteAmbiguous {
		_ = s.Index.HoldWriteForRecovery(ctx, intent.ID, uploadID)
		return Object{}, errors.Join(cause, ErrWriteRecoveryAmbiguous)
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
