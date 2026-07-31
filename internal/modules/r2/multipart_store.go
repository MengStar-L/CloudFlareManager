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
	ID            string            `json:"upload_id"`
	WriteIntentID string            `json:"-"`
	Key           string            `json:"key"`
	ObjectID      string            `json:"object_id"`
	BucketID      string            `json:"physical_bucket_id"`
	UpstreamID    string            `json:"-"`
	ContentType   string            `json:"content_type"`
	Metadata      map[string]string `json:"metadata"`
	Status        MultipartStatus   `json:"status"`
	CreatedAt     time.Time         `json:"created_at"`
	LastModified  time.Time         `json:"last_modified"`
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
	intent, err := s.BeginWrite(ctx, BeginWriteInput{ObjectInput: input, ExpectedClassA: 1})
	if err != nil {
		return MultipartUpload{}, err
	}
	abortIntent := true
	defer func() {
		if abortIntent {
			_ = s.AbortWrite(context.Background(), intent.ID)
		}
	}()
	now := time.Now()
	upload := MultipartUpload{
		ID: uuid.NewString(), WriteIntentID: intent.ID, Key: input.Key, ObjectID: intent.ID, BucketID: intent.BucketID,
		ContentType: input.ContentType, Metadata: cloneMetadata(intent.Metadata), Status: MultipartInitiating,
		CreatedAt: now, LastModified: now,
	}
	attributes, err := json.Marshal(multipartAttributes{ContentType: upload.ContentType, Metadata: upload.Metadata})
	if err != nil {
		return MultipartUpload{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO r2_multipart_uploads(
		id, object_key, object_id, physical_bucket_id, upstream_upload_id, metadata_json, status, created_at, updated_at,
		write_intent_id) VALUES(?, ?, ?, ?, '', ?, ?, ?, ?, ?)`, upload.ID, upload.Key, upload.ObjectID, upload.BucketID,
		string(attributes), upload.Status, now.UnixNano(), now.UnixNano(), upload.WriteIntentID)
	if err != nil {
		return MultipartUpload{}, fmt.Errorf("begin multipart upload: %w", err)
	}
	abortIntent = false
	return upload, nil
}

func (s *Store) ActivateMultipart(ctx context.Context, id, upstreamID string) error {
	if upstreamID == "" {
		return errors.New("upstream upload id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var intentID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(write_intent_id, '') FROM r2_multipart_uploads
		WHERE id = ? AND status = ?`, id, MultipartInitiating).Scan(&intentID); err != nil || intentID == "" {
		return ErrMultipartNotFound
	}
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `UPDATE r2_multipart_uploads SET upstream_upload_id = ?, status = ?, updated_at = ?
		WHERE id = ?`, upstreamID, MultipartActive, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE r2_write_intents SET state = ?, upstream_upload_id = ?, updated_at = ?
		WHERE id = ?`, WriteUploading, upstreamID, now, intentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PrepareMultipartPart(ctx context.Context, uploadID string, partNumber int32, size int64) error {
	if partNumber < 1 || partNumber > 10000 || size < 0 {
		return ErrInvalidPart
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upload, err := scanMultipart(tx.QueryRowContext(ctx, multipartSelect+" WHERE id = ? AND status = ?", uploadID, MultipartActive))
	if err != nil || upload.WriteIntentID == "" {
		return ErrMultipartNotFound
	}
	intent, err := scanWriteIntent(tx.QueryRowContext(ctx, writeIntentSelect+" WHERE id = ?", upload.WriteIntentID))
	if err != nil {
		return err
	}
	var previousSize int64
	err = tx.QueryRowContext(ctx, `SELECT size FROM r2_multipart_parts WHERE upload_id = ? AND part_number = ?`,
		uploadID, partNumber).Scan(&previousSize)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var reservedPrevious, reservedRequested int64
	err = tx.QueryRowContext(ctx, `SELECT previous_size, requested_size FROM r2_multipart_part_reservations
		WHERE upload_id = ? AND part_number = ?`, uploadID, partNumber).Scan(&reservedPrevious, &reservedRequested)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	currentContribution := int64(0)
	if err == nil {
		previousSize = reservedPrevious
		currentContribution = maxInt64(reservedRequested-reservedPrevious, 0)
	}
	desiredContribution := maxInt64(size-previousSize, 0)
	adjustment := desiredContribution - currentContribution
	if adjustment > 0 {
		bucket, err := scanBucket(tx.QueryRowContext(ctx, bucketSelect+" WHERE id = ?", intent.BucketID))
		if err != nil {
			return err
		}
		var managed, reserved, unmanaged int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes), 0), COALESCE(SUM(reserved_storage_bytes), 0)
			FROM r2_physical_buckets WHERE account_id = ?`, bucket.AccountID).Scan(&managed, &reserved); err != nil {
			return err
		}
		_ = tx.QueryRowContext(ctx, `SELECT unmanaged_storage_bytes FROM r2_account_capacity WHERE account_id = ?`,
			bucket.AccountID).Scan(&unmanaged)
		overflow := bucket.OverflowUntil != nil && bucket.OverflowUntil.After(time.Now())
		if !overflow && (bucket.StorageBytes+bucket.ReservedBytes+adjustment > s.limits.StorageBytes ||
			managed+reserved+unmanaged+adjustment > s.limits.AccountStorageBytes) {
			return ErrQuotaExceeded
		}
	}
	now := time.Now()
	if adjustment != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET reserved_storage_bytes =
			MAX(reserved_storage_bytes + ?, 0), updated_at = ? WHERE id = ?`, adjustment, now.Unix(), intent.BucketID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE r2_write_intents SET reserved_bytes = MAX(reserved_bytes + ?, 0),
			updated_at = ? WHERE id = ?`, adjustment, now.UnixNano(), intent.ID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO r2_multipart_part_reservations(
		upload_id, part_number, previous_size, requested_size, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(upload_id, part_number) DO UPDATE SET requested_size = excluded.requested_size,
		updated_at = excluded.updated_at`, uploadID, partNumber, previousSize, size, now.UnixNano(), now.UnixNano())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitMultipartPart(ctx context.Context, uploadID string, part MultipartPart) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upload, err := scanMultipart(tx.QueryRowContext(ctx, multipartSelect+" WHERE id = ? AND status = ?", uploadID, MultipartActive))
	if err != nil || upload.WriteIntentID == "" {
		return ErrMultipartNotFound
	}
	var previousSize, requestedSize int64
	if err := tx.QueryRowContext(ctx, `SELECT previous_size, requested_size FROM r2_multipart_part_reservations
		WHERE upload_id = ? AND part_number = ?`, uploadID, part.PartNumber).Scan(&previousSize, &requestedSize); err != nil {
		return ErrInvalidPart
	}
	if requestedSize != part.Size {
		return ErrInvalidPart
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO r2_multipart_parts(upload_id, part_number, etag, size, updated_at)
		VALUES(?, ?, ?, ?, ?) ON CONFLICT(upload_id, part_number) DO UPDATE SET etag = excluded.etag,
		size = excluded.size, updated_at = excluded.updated_at`, uploadID, part.PartNumber, part.ETag, part.Size, now.UnixNano()); err != nil {
		return err
	}
	if adjustment := minInt64(part.Size-previousSize, 0); adjustment != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET reserved_storage_bytes =
			MAX(reserved_storage_bytes + ?, 0), updated_at = ? WHERE id = ?`, adjustment, now.Unix(), upload.BucketID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE r2_write_intents SET reserved_bytes = MAX(reserved_bytes + ?, 0),
			updated_at = ? WHERE id = ?`, adjustment, now.UnixNano(), upload.WriteIntentID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM r2_multipart_part_reservations
		WHERE upload_id = ? AND part_number = ?`, uploadID, part.PartNumber); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE r2_multipart_uploads SET updated_at = ? WHERE id = ?`, now.UnixNano(), uploadID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailMultipart(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_multipart_uploads SET status = ?, updated_at = ?
		WHERE id = ? AND status IN (?, ?)`, MultipartError, time.Now().UnixNano(), id, MultipartInitiating, MultipartCompleting)
	return multipartUpdateResult(result, err)
}

func (s *Store) GetMultipart(ctx context.Context, id string) (MultipartUpload, error) {
	return scanMultipart(s.db.QueryRowContext(ctx, multipartSelect+" WHERE id = ?", id))
}

func (s *Store) GetMultipartByWriteIntent(ctx context.Context, intentID string) (MultipartUpload, error) {
	return scanMultipart(s.db.QueryRowContext(ctx, multipartSelect+" WHERE write_intent_id = ?", intentID))
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

func (s *Store) ListExpiredMultipart(ctx context.Context, before time.Time, limit int) ([]MultipartUpload, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, multipartSelect+` WHERE status IN (?, ?, ?, ?) AND updated_at <= ?
		ORDER BY updated_at, id LIMIT ?`, MultipartInitiating, MultipartActive, MultipartCompleting,
		MultipartError, before.UnixNano(), limit)
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
	return result, rows.Err()
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
	var pending int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_multipart_part_reservations WHERE upload_id = ?`, id).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return ErrWriteInProgress
	}
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

func (s *Store) AbortClientMultipart(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upload, err := scanMultipart(tx.QueryRowContext(ctx, multipartSelect+" WHERE id = ?", id))
	if err != nil {
		return err
	}
	if upload.WriteIntentID != "" {
		intent, err := scanWriteIntent(tx.QueryRowContext(ctx, writeIntentSelect+" WHERE id = ?", upload.WriteIntentID))
		if err == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET reserved_storage_bytes =
				MAX(reserved_storage_bytes - ?, 0), updated_at = ? WHERE id = ?`,
				intent.ReservedBytes, time.Now().Unix(), intent.BucketID); err != nil {
				return err
			}
		} else if !errors.Is(err, ErrWriteIntentNotFound) {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM r2_multipart_uploads WHERE id = ?`, id); err != nil {
		return err
	}
	if upload.WriteIntentID != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM r2_write_intents WHERE id = ?`, upload.WriteIntentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const multipartSelect = `SELECT id, object_key, object_id, physical_bucket_id, upstream_upload_id,
	metadata_json, status, created_at, updated_at, COALESCE(write_intent_id, '') FROM r2_multipart_uploads`

func scanMultipart(row scanner) (MultipartUpload, error) {
	var upload MultipartUpload
	var attributesJSON string
	var created, updated int64
	if err := row.Scan(&upload.ID, &upload.Key, &upload.ObjectID, &upload.BucketID, &upload.UpstreamID,
		&attributesJSON, &upload.Status, &created, &updated, &upload.WriteIntentID); err != nil {
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

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
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
