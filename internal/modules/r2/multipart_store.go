package r2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type MultipartStatus string

const (
	MultipartInitiating MultipartStatus = "initiating"
	MultipartActive     MultipartStatus = "active"
	MultipartCompleting MultipartStatus = "completing"
	MultipartError      MultipartStatus = "error"
)

type MultipartUpload struct {
	ID           string            `json:"upload_id"`
	Key          string            `json:"key"`
	ObjectID     string            `json:"object_id"`
	BucketID     string            `json:"physical_bucket_id"`
	UpstreamID   string            `json:"-"`
	ContentType  string            `json:"content_type"`
	Metadata     map[string]string `json:"metadata"`
	Status       MultipartStatus   `json:"status"`
	CreatedAt    time.Time         `json:"created_at"`
	LastModified time.Time         `json:"last_modified"`
}

type MultipartPart struct {
	PartNumber   int32     `json:"part_number"`
	ETag         string    `json:"etag"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

type CompletedPart struct {
	PartNumber int32
	ETag       string
}

type MultipartPartList struct {
	Parts          []MultipartPart `json:"parts"`
	NextPartNumber int32           `json:"next_part_number,omitempty"`
}

type ListMultipartOptions struct {
	Prefix string
	After  string
	Limit  int
}

type MultipartUploadList struct {
	Uploads    []MultipartUpload `json:"uploads"`
	NextMarker string            `json:"next_marker,omitempty"`
}

type multipartAttributes struct {
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func (s *Store) BeginMultipart(ctx context.Context, input ObjectInput) (MultipartUpload, error) {
	if input.Key == "" {
		return MultipartUpload{}, errors.New("object key is required")
	}
	input.Size = -1
	selected, err := s.selectBucket(ctx, input)
	if err != nil {
		return MultipartUpload{}, err
	}
	now := time.Now()
	upload := MultipartUpload{
		ID: uuid.NewString(), Key: input.Key, ObjectID: uuid.NewString(), BucketID: selected.ID,
		ContentType: input.ContentType, Metadata: cloneMetadata(input.Metadata), Status: MultipartInitiating,
		CreatedAt: now, LastModified: now,
	}
	attributes, err := json.Marshal(multipartAttributes{ContentType: upload.ContentType, Metadata: upload.Metadata})
	if err != nil {
		return MultipartUpload{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO r2_multipart_uploads(
		id, object_key, object_id, physical_bucket_id, upstream_upload_id, metadata_json, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, '', ?, ?, ?, ?)`, upload.ID, upload.Key, upload.ObjectID, upload.BucketID,
		string(attributes), upload.Status, now.UnixNano(), now.UnixNano())
	if err != nil {
		return MultipartUpload{}, fmt.Errorf("begin multipart upload: %w", err)
	}
	return upload, nil
}

func (s *Store) ActivateMultipart(ctx context.Context, id, upstreamID string) error {
	if upstreamID == "" {
		return errors.New("upstream upload id is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE r2_multipart_uploads SET upstream_upload_id = ?, status = ?, updated_at = ?
		WHERE id = ? AND status = ?`, upstreamID, MultipartActive, time.Now().UnixNano(), id, MultipartInitiating)
	return multipartUpdateResult(result, err)
}

func (s *Store) FailMultipart(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_multipart_uploads SET status = ?, updated_at = ?
		WHERE id = ? AND status IN (?, ?)`, MultipartError, time.Now().UnixNano(), id, MultipartInitiating, MultipartCompleting)
	return multipartUpdateResult(result, err)
}

func (s *Store) GetMultipart(ctx context.Context, id string) (MultipartUpload, error) {
	return scanMultipart(s.db.QueryRowContext(ctx, multipartSelect+" WHERE id = ?", id))
}

func (s *Store) ListMultipart(ctx context.Context, options ListMultipartOptions) (MultipartUploadList, error) {
	if options.Limit <= 0 || options.Limit > 1000 {
		options.Limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, multipartSelect+` WHERE status IN (?, ?) AND object_key LIKE ? ESCAPE '\'
		AND object_key > ? ORDER BY object_key, id LIMIT ?`, MultipartActive, MultipartCompleting,
		escapeLike(options.Prefix)+"%", options.After, options.Limit+1)
	if err != nil {
		return MultipartUploadList{}, err
	}
	defer rows.Close()
	var uploads []MultipartUpload
	for rows.Next() {
		upload, err := scanMultipart(rows)
		if err != nil {
			return MultipartUploadList{}, err
		}
		uploads = append(uploads, upload)
	}
	if err := rows.Err(); err != nil {
		return MultipartUploadList{}, err
	}
	result := MultipartUploadList{Uploads: uploads}
	if len(uploads) > options.Limit {
		result.NextMarker = uploads[options.Limit-1].Key
		result.Uploads = uploads[:options.Limit]
	}
	return result, nil
}

func (s *Store) RecordMultipartPart(ctx context.Context, uploadID string, part MultipartPart) error {
	if part.PartNumber < 1 || part.PartNumber > 10000 || part.ETag == "" || part.Size < 0 {
		return ErrInvalidPart
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_multipart_uploads WHERE id = ? AND status = ?`,
		uploadID, MultipartActive).Scan(&active); err != nil {
		return err
	}
	if active != 1 {
		return ErrMultipartNotFound
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO r2_multipart_parts(upload_id, part_number, etag, size, updated_at)
		VALUES(?, ?, ?, ?, ?) ON CONFLICT(upload_id, part_number) DO UPDATE SET
		etag = excluded.etag, size = excluded.size, updated_at = excluded.updated_at`,
		uploadID, part.PartNumber, part.ETag, part.Size, now.UnixNano())
	return err
}

func (s *Store) ListMultipartParts(ctx context.Context, uploadID string, after int32, limit int) (MultipartPartList, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if _, err := s.GetMultipart(ctx, uploadID); err != nil {
		return MultipartPartList{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT part_number, etag, size, updated_at FROM r2_multipart_parts
		WHERE upload_id = ? AND part_number > ? ORDER BY part_number LIMIT ?`, uploadID, after, limit+1)
	if err != nil {
		return MultipartPartList{}, err
	}
	defer rows.Close()
	var parts []MultipartPart
	for rows.Next() {
		var part MultipartPart
		var modified int64
		if err := rows.Scan(&part.PartNumber, &part.ETag, &part.Size, &modified); err != nil {
			return MultipartPartList{}, err
		}
		part.LastModified = time.Unix(0, modified)
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		return MultipartPartList{}, err
	}
	result := MultipartPartList{Parts: parts}
	if len(parts) > limit {
		result.NextPartNumber = parts[limit-1].PartNumber
		result.Parts = parts[:limit]
	}
	return result, nil
}

func (s *Store) BeginCompleteMultipart(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_multipart_uploads SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`, MultipartCompleting, time.Now().UnixNano(), id, MultipartActive)
	return multipartUpdateResult(result, err)
}

func (s *Store) ResetMultipart(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_multipart_uploads SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`, MultipartActive, time.Now().UnixNano(), id, MultipartCompleting)
	return multipartUpdateResult(result, err)
}

func (s *Store) CommitMultipart(ctx context.Context, upload MultipartUpload, etag string, size int64) (Object, error) {
	now := time.Now()
	metadata, err := json.Marshal(cloneMetadata(upload.Metadata))
	if err != nil {
		return Object{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Object{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO r2_objects(
		object_key, object_id, physical_bucket_id, physical_key, state, size, etag, content_type,
		metadata_json, last_modified, error, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(object_key) DO UPDATE SET object_id = excluded.object_id,
		physical_bucket_id = excluded.physical_bucket_id, physical_key = excluded.physical_key,
		state = excluded.state, size = excluded.size, etag = excluded.etag,
		content_type = excluded.content_type, metadata_json = excluded.metadata_json,
		last_modified = excluded.last_modified, error = '', updated_at = excluded.updated_at`,
		upload.Key, upload.ObjectID, upload.BucketID, upload.Key, StateCommitted, size, etag,
		upload.ContentType, string(metadata), now.UnixNano(), now.UnixNano(), now.UnixNano())
	if err != nil {
		return Object{}, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM r2_multipart_uploads WHERE id = ? AND status = ?`, upload.ID, MultipartCompleting)
	if err != nil {
		return Object{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Object{}, ErrMultipartNotFound
	}
	if err := tx.Commit(); err != nil {
		return Object{}, err
	}
	return s.GetObject(ctx, upload.Key)
}

func (s *Store) DeleteMultipart(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM r2_multipart_uploads WHERE id = ?", id)
	return multipartUpdateResult(result, err)
}

const multipartSelect = `SELECT id, object_key, object_id, physical_bucket_id, upstream_upload_id,
	metadata_json, status, created_at, updated_at FROM r2_multipart_uploads`

func scanMultipart(row scanner) (MultipartUpload, error) {
	var upload MultipartUpload
	var attributesJSON string
	var created, updated int64
	if err := row.Scan(&upload.ID, &upload.Key, &upload.ObjectID, &upload.BucketID, &upload.UpstreamID,
		&attributesJSON, &upload.Status, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MultipartUpload{}, ErrMultipartNotFound
		}
		return MultipartUpload{}, err
	}
	var attributes multipartAttributes
	if err := json.Unmarshal([]byte(attributesJSON), &attributes); err != nil {
		return MultipartUpload{}, err
	}
	upload.ContentType = attributes.ContentType
	upload.Metadata = cloneMetadata(attributes.Metadata)
	upload.CreatedAt = time.Unix(0, created)
	upload.LastModified = time.Unix(0, updated)
	return upload, nil
}

func multipartUpdateResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrMultipartNotFound
	}
	return nil
}

var (
	ErrMultipartNotFound = errors.New("multipart upload not found")
	ErrInvalidPart       = errors.New("invalid multipart part")
	ErrInvalidPartOrder  = errors.New("multipart parts are not in ascending order")
)
