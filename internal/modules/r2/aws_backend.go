package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awstypes "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type AWSBackend struct {
	HTTPClient aws.HTTPClient
}

func (b AWSBackend) Put(ctx context.Context, target Target, key string, body io.Reader, size int64, contentType string, metadata map[string]string, options PutOptions) (string, error) {
	input := &awss3.PutObjectInput{
		Bucket: aws.String(target.Bucket), Key: aws.String(key), Body: body,
		ContentLength: aws.Int64(size), Metadata: metadata,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	if options.IfMatch != "" {
		input.IfMatch = aws.String(options.IfMatch)
	}
	if options.IfNoneMatch != "" {
		input.IfNoneMatch = aws.String(options.IfNoneMatch)
	}
	output, err := b.client(target).PutObject(ctx, input)
	if err != nil {
		return "", classifyAWSMutationError(err)
	}
	return strings.Trim(aws.ToString(output.ETag), `"`), nil
}

func (b AWSBackend) Get(ctx context.Context, target Target, key string, options GetOptions) (GetResult, error) {
	input := &awss3.GetObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)}
	if options.Range != "" {
		input.Range = aws.String(options.Range)
	}
	if options.IfMatch != "" {
		input.IfMatch = aws.String(options.IfMatch)
	}
	if options.IfNoneMatch != "" {
		input.IfNoneMatch = aws.String(options.IfNoneMatch)
	}
	if options.IfModifiedSince != nil {
		input.IfModifiedSince = options.IfModifiedSince
	}
	if options.IfUnmodifiedSince != nil {
		input.IfUnmodifiedSince = options.IfUnmodifiedSince
	}
	output, err := b.client(target).GetObject(ctx, input)
	if err != nil {
		return GetResult{}, classifyAWSMutationError(err)
	}
	return GetResult{
		Body: output.Body, Size: aws.ToInt64(output.ContentLength), ETag: strings.Trim(aws.ToString(output.ETag), `"`),
		ContentType: aws.ToString(output.ContentType), Metadata: userVisibleMetadata(output.Metadata),
		LastModified: aws.ToTime(output.LastModified), ContentRange: aws.ToString(output.ContentRange),
	}, nil
}

func (b AWSBackend) Delete(ctx context.Context, target Target, key string) error {
	_, err := b.client(target).DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(target.Bucket), Key: aws.String(key)})
	return classifyAWSMutationError(err)
}

func (b AWSBackend) CreateMultipart(ctx context.Context, target Target, key, contentType string, metadata map[string]string) (string, error) {
	input := &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(target.Bucket), Key: aws.String(key), Metadata: metadata,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	output, err := b.client(target).CreateMultipartUpload(ctx, input)
	if err != nil {
		return "", classifyAWSMutationError(err)
	}
	return aws.ToString(output.UploadId), nil
}

func (b AWSBackend) UploadPart(ctx context.Context, target Target, key, uploadID string, partNumber int32, body io.Reader, size int64) (string, error) {
	output, err := b.client(target).UploadPart(ctx, &awss3.UploadPartInput{
		Bucket: aws.String(target.Bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(partNumber), Body: body, ContentLength: aws.Int64(size),
	})
	if err != nil {
		return "", classifyAWSMutationError(err)
	}
	return strings.Trim(aws.ToString(output.ETag), `"`), nil
}

func (b AWSBackend) CompleteMultipart(ctx context.Context, target Target, key, uploadID string, parts []CompletedPart, options PutOptions) (string, error) {
	completed := make([]awstypes.CompletedPart, 0, len(parts))
	for _, part := range parts {
		etag := `"` + strings.Trim(part.ETag, `"`) + `"`
		completed = append(completed, awstypes.CompletedPart{PartNumber: aws.Int32(part.PartNumber), ETag: aws.String(etag)})
	}
	input := &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(target.Bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &awstypes.CompletedMultipartUpload{Parts: completed},
	}
	if options.IfMatch != "" {
		input.IfMatch = aws.String(options.IfMatch)
	}
	if options.IfNoneMatch != "" {
		input.IfNoneMatch = aws.String(options.IfNoneMatch)
	}
	output, err := b.client(target).CompleteMultipartUpload(ctx, input)
	if err != nil {
		return "", classifyAWSMutationError(err)
	}
	return strings.Trim(aws.ToString(output.ETag), `"`), nil
}

func (b AWSBackend) AbortMultipart(ctx context.Context, target Target, key, uploadID string) error {
	_, err := b.client(target).AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
		Bucket: aws.String(target.Bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	return classifyAWSMutationError(err)
}

func (b AWSBackend) Head(ctx context.Context, target Target, key string) (RemoteObject, error) {
	output, err := b.client(target).HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(target.Bucket), Key: aws.String(key),
	})
	if err != nil {
		return RemoteObject{}, err
	}
	return RemoteObject{
		Key: key, Size: aws.ToInt64(output.ContentLength), ETag: strings.Trim(aws.ToString(output.ETag), `"`),
		ContentType: aws.ToString(output.ContentType), Metadata: cloneMetadata(output.Metadata), LastModified: aws.ToTime(output.LastModified),
	}, nil
}

func userVisibleMetadata(metadata map[string]string) map[string]string {
	result := cloneMetadata(metadata)
	for key := range result {
		if strings.EqualFold(key, InternalWriteIDMetadata) {
			delete(result, key)
		}
	}
	return result
}

func (b AWSBackend) ListRemote(ctx context.Context, target Target, prefix, continuation string, limit int32) (RemoteObjectList, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	input := &awss3.ListObjectsV2Input{
		Bucket: aws.String(target.Bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(limit),
	}
	if continuation != "" {
		input.ContinuationToken = aws.String(continuation)
	}
	output, err := b.client(target).ListObjectsV2(ctx, input)
	if err != nil {
		return RemoteObjectList{}, err
	}
	result := RemoteObjectList{}
	for _, object := range output.Contents {
		result.Objects = append(result.Objects, RemoteObject{
			Key: aws.ToString(object.Key), Size: aws.ToInt64(object.Size), ETag: strings.Trim(aws.ToString(object.ETag), `"`),
			LastModified: aws.ToTime(object.LastModified),
		})
	}
	if aws.ToBool(output.IsTruncated) {
		result.ContinuationToken = aws.ToString(output.NextContinuationToken)
	}
	return result, nil
}

func (b AWSBackend) client(target Target) *awss3.Client {
	configuration := aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider(target.AccessKeyID, target.SecretAccessKey, ""),
		HTTPClient:  b.httpClient(),
	}
	return awss3.NewFromConfig(configuration, func(options *awss3.Options) {
		options.BaseEndpoint = aws.String("https://" + target.CloudflareAccountID + ".r2.cloudflarestorage.com")
		options.UsePathStyle = true
	})
}

func (b AWSBackend) httpClient() aws.HTTPClient {
	if b.HTTPClient != nil {
		return b.HTTPClient
	}
	return &http.Client{Timeout: 0, Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment, MaxIdleConns: 100, MaxIdleConnsPerHost: 20,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}

func classifyAWSMutationError(err error) error {
	if err == nil {
		return nil
	}
	var code string
	var apiError interface{ ErrorCode() string }
	if errors.As(err, &apiError) {
		code = apiError.ErrorCode()
	}
	var status int
	var statusError interface{ HTTPStatusCode() int }
	if errors.As(err, &statusError) {
		status = statusError.HTTPStatusCode()
	}
	switch {
	case status == http.StatusRequestedRangeNotSatisfiable || code == "InvalidRange" ||
		code == "RequestedRangeNotSatisfiable" || code == "RangeNotSatisfiable":
		return fmt.Errorf("%w: %w", ErrRangeNotSatisfiable, err)
	case status == http.StatusTooManyRequests || code == "SlowDown" || code == "TooManyRequests" || code == "Throttling":
		return fmt.Errorf("%w: %w", ErrRateLimited, err)
	case status == http.StatusPreconditionFailed || status == http.StatusConflict ||
		code == "PreconditionFailed" || code == "ConditionalRequestConflict":
		return fmt.Errorf("%w: %w", ErrConditionalRequestConflict, err)
	default:
		return err
	}
}
