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
	TempDir  string
}

func (s Service) Put(ctx context.Context, request PutRequest) (Object, error) {
	if s.Index == nil || s.Accounts == nil || s.Backend == nil {
		return Object{}, errors.New("R2 service is not configured")
	}
	if request.Body == nil {
		request.Body = &emptyReader{}
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
	object, err := s.Index.ReservePut(ctx, ObjectInput{
		Key: request.Key, Size: size, ContentType: request.ContentType, Metadata: request.Metadata,
	})
	if err != nil {
		return Object{}, err
	}
	target, err := s.target(ctx, object.BucketID)
	if err != nil {
		_ = s.Index.FailPut(ctx, object.ObjectID, err.Error())
		return Object{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = s.Index.FailPut(ctx, object.ObjectID, err.Error())
		return Object{}, err
	}
	etag, err := s.Backend.Put(ctx, target, object.PhysicalKey, file, size, request.ContentType, request.Metadata)
	if err != nil {
		_ = s.Index.FailPut(ctx, object.ObjectID, classifyUpstreamError(err))
		return Object{}, err
	}
	if err := s.Index.CommitPut(ctx, object.ObjectID, etag, size); err != nil {
		return Object{}, err
	}
	return s.Index.GetObject(ctx, request.Key)
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
	return s.Backend.Get(ctx, target, object.PhysicalKey, options)
}

func (s Service) Stat(ctx context.Context, key string) (Object, error) {
	return s.Index.GetObject(ctx, key)
}

func (s Service) List(ctx context.Context, options ListOptions) (ObjectList, error) {
	return s.Index.ListObjects(ctx, options)
}

func (s Service) Delete(ctx context.Context, key string) error {
	object, err := s.Index.GetObject(ctx, key)
	if err != nil {
		return err
	}
	if err := s.Index.BeginDelete(ctx, key); err != nil {
		return err
	}
	target, err := s.target(ctx, object.BucketID)
	if err != nil {
		_ = s.Index.FailDelete(ctx, key, err.Error())
		return err
	}
	if err := s.Backend.Delete(ctx, target, object.PhysicalKey); err != nil {
		_ = s.Index.FailDelete(ctx, key, classifyUpstreamError(err))
		return err
	}
	return s.Index.CompleteDelete(ctx, key)
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
		CloudflareAccountID: account.CloudflareAccountID, AccessKeyID: account.R2AccessKeyID,
		SecretAccessKey: account.R2SecretAccessKey, Bucket: bucket.Name,
	}, nil
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

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

var ErrPayloadHashMismatch = errors.New("request payload hash does not match")
