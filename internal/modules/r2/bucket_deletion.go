package r2

import (
	"context"
	"errors"
	"fmt"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
)

const BucketDeletionJobType = "r2.bucket.delete-remote"

type BucketDeletionMode string

const (
	BucketDeletionEmptyOnly      BucketDeletionMode = "empty_only"
	BucketDeletionEmptyAndDelete BucketDeletionMode = "empty_and_delete"
)

type BucketDeletionPayload struct {
	AccountID              string             `json:"account_id"`
	CloudflareAccountID    string             `json:"cloudflare_account_id"`
	BucketName             string             `json:"bucket_name"`
	Jurisdiction           string             `json:"jurisdiction"`
	ExpectedCreationDate   string             `json:"expected_creation_date"`
	LocalBucketID          string             `json:"local_bucket_id,omitempty"`
	Mode                   BucketDeletionMode `json:"mode"`
	Stage                  string             `json:"stage"`
	ParentJobID            string             `json:"parent_job_id,omitempty"`
	RemoteMissingAtEnqueue bool               `json:"remote_missing_at_enqueue"`
	RemoteMutated          bool               `json:"remote_mutated"`
	DeletedObjects         int64              `json:"deleted_objects"`
	AbortedMultipart       int64              `json:"aborted_multipart"`
	DeleteRounds           int                `json:"delete_rounds"`
}

type BucketDeletionRemote interface {
	GetR2Bucket(context.Context, string, string, string, string) (accounts.RemoteBucket, error)
	ListR2Objects(context.Context, string, string, string, string, string, int) (accounts.RemoteObjectPage, error)
	DeleteR2Object(context.Context, string, string, string, string, string) error
	DeleteR2Bucket(context.Context, string, string, string, string) error
}

func (s *Store) listBucketDeletionMultipart(ctx context.Context, bucketID string) ([]MultipartUpload, error) {
	if bucketID == "" {
		return nil, errors.New("bucket id is required")
	}
	rows, err := s.db.QueryContext(ctx, multipartSelect+` WHERE physical_bucket_id = ? ORDER BY object_key, id`, bucketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MultipartUpload
	for rows.Next() {
		upload, err := scanMultipart(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, upload)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list bucket multipart uploads: %w", err)
	}
	return result, nil
}

func (s *Store) listBucketDeletionWriteIntents(ctx context.Context, bucketID string) ([]WriteIntent, error) {
	if bucketID == "" {
		return nil, errors.New("bucket id is required")
	}
	rows, err := s.db.QueryContext(ctx, writeIntentSelect+` WHERE target_bucket_id = ? OR EXISTS(
		SELECT 1 FROM r2_objects AS previous
		WHERE previous.object_id = r2_write_intents.previous_object_id
			AND previous.physical_bucket_id = ?
	) ORDER BY created_at, id`, bucketID, bucketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []WriteIntent
	for rows.Next() {
		intent, err := scanWriteIntent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list bucket write intents: %w", err)
	}
	return result, nil
}
