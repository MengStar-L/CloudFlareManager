package r2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type ScanFinding struct {
	ID       string    `json:"id"`
	BucketID string    `json:"physical_bucket_id"`
	Key      string    `json:"physical_key"`
	Kind     string    `json:"kind"`
	Detail   string    `json:"detail,omitempty"`
	FoundAt  time.Time `json:"found_at"`
}

func (s *Store) AdoptObject(ctx context.Context, bucketID string, remote RemoteObject) (Object, error) {
	if bucketID == "" || remote.Key == "" || remote.Size < 0 {
		return Object{}, errors.New("bucket id, object key, and non-negative size are required")
	}
	now := time.Now()
	modified := remote.LastModified
	if modified.IsZero() {
		modified = now
	}
	object := Object{
		Key: remote.Key, ObjectID: uuid.NewString(), BucketID: bucketID, PhysicalKey: remote.Key,
		State: StateCommitted, Size: remote.Size, ETag: remote.ETag, ContentType: remote.ContentType,
		Metadata: cloneMetadata(remote.Metadata), LastModified: modified, CreatedAt: now, UpdatedAt: now,
	}
	metadata, err := json.Marshal(object.Metadata)
	if err != nil {
		return Object{}, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO r2_objects(
		object_key, object_id, physical_bucket_id, physical_key, state, size, etag, content_type,
		metadata_json, last_modified, error, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?) ON CONFLICT(object_key) DO NOTHING`,
		object.Key, object.ObjectID, object.BucketID, object.PhysicalKey, object.State, object.Size,
		object.ETag, object.ContentType, string(metadata), modified.UnixNano(), now.UnixNano(), now.UnixNano())
	if err != nil {
		return Object{}, fmt.Errorf("adopt object: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Object{}, ErrObjectConflict
	}
	return object, nil
}

func (s *Store) HasPhysicalMapping(ctx context.Context, bucketID, physicalKey string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_objects
		WHERE physical_bucket_id = ? AND physical_key = ?`, bucketID, physicalKey).Scan(&count)
	return count > 0, err
}

func (s *Store) ListObjectsByBucket(ctx context.Context, bucketID, after string, limit int) (ObjectList, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, objectSelect+` WHERE physical_bucket_id = ? AND state = ?
		AND object_key > ? ORDER BY object_key LIMIT ?`, bucketID, StateCommitted, after, limit+1)
	if err != nil {
		return ObjectList{}, err
	}
	defer rows.Close()
	var objects []Object
	for rows.Next() {
		object, err := scanObject(rows)
		if err != nil {
			return ObjectList{}, err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return ObjectList{}, err
	}
	result := ObjectList{Objects: objects}
	if len(objects) > limit {
		result.NextMarker = objects[limit-1].Key
		result.Objects = objects[:limit]
	}
	return result, nil
}

func (s *Store) ListObjectsByStates(ctx context.Context, states []ObjectState, after string, limit int) (ObjectList, error) {
	if len(states) == 0 {
		return ObjectList{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	query := objectSelect + " WHERE state IN ("
	args := make([]any, 0, len(states)+2)
	for index, state := range states {
		if index != 0 {
			query += ","
		}
		query += "?"
		args = append(args, state)
	}
	query += ") AND object_key > ? ORDER BY object_key LIMIT ?"
	args = append(args, after, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ObjectList{}, err
	}
	defer rows.Close()
	var objects []Object
	for rows.Next() {
		object, err := scanObject(rows)
		if err != nil {
			return ObjectList{}, err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return ObjectList{}, err
	}
	result := ObjectList{Objects: objects}
	if len(objects) > limit {
		result.NextMarker = objects[limit-1].Key
		result.Objects = objects[:limit]
	}
	return result, nil
}

func (s *Store) MoveObjectMapping(ctx context.Context, objectID, bucketID, etag string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_objects SET physical_bucket_id = ?, physical_key = object_key,
		etag = ?, last_modified = ?, updated_at = ? WHERE object_id = ? AND state = ?`,
		bucketID, etag, time.Now().UnixNano(), time.Now().UnixNano(), objectID, StateCommitted)
	return objectUpdateResult(result, err)
}

func (s *Store) FinishBucketScan(ctx context.Context, bucketID string, storageBytes int64, adopted bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_physical_buckets SET storage_bytes = ?, adopted = ?,
		health_status = 'healthy', updated_at = ? WHERE id = ?`, storageBytes, adopted, time.Now().Unix(), bucketID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBucketNotFound
	}
	return nil
}

func (s *Store) ClearScanFindings(ctx context.Context, bucketID, kind string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM r2_scan_findings WHERE physical_bucket_id = ? AND kind = ?`, bucketID, kind)
	return err
}

func (s *Store) RecordScanFinding(ctx context.Context, finding ScanFinding) error {
	if finding.ID == "" {
		finding.ID = uuid.NewString()
	}
	if finding.FoundAt.IsZero() {
		finding.FoundAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO r2_scan_findings(
		id, physical_bucket_id, physical_key, kind, detail, found_at) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(physical_bucket_id, physical_key, kind) DO UPDATE SET
		detail = excluded.detail, found_at = excluded.found_at`, finding.ID, finding.BucketID,
		finding.Key, finding.Kind, finding.Detail, finding.FoundAt.Unix())
	return err
}

func (s *Store) ListScanFindings(ctx context.Context, bucketID, kind string, limit int) ([]ScanFinding, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, physical_bucket_id, physical_key, kind, detail, found_at FROM r2_scan_findings WHERE 1 = 1`
	var args []any
	if bucketID != "" {
		query += " AND physical_bucket_id = ?"
		args = append(args, bucketID)
	}
	if kind != "" {
		query += " AND kind = ?"
		args = append(args, kind)
	}
	query += " ORDER BY found_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var findings []ScanFinding
	for rows.Next() {
		var finding ScanFinding
		var found int64
		if err := rows.Scan(&finding.ID, &finding.BucketID, &finding.Key, &finding.Kind, &finding.Detail, &found); err != nil {
			return nil, err
		}
		finding.FoundAt = time.Unix(found, 0)
		findings = append(findings, finding)
	}
	return findings, rows.Err()
}

func (s *Store) ListMultipartByStatus(ctx context.Context, status MultipartStatus, limit int) ([]MultipartUpload, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, multipartSelect+" WHERE status = ? ORDER BY created_at, id LIMIT ?", status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var uploads []MultipartUpload
	for rows.Next() {
		upload, err := scanMultipart(rows)
		if err != nil {
			return nil, err
		}
		uploads = append(uploads, upload)
	}
	return uploads, rows.Err()
}

var ErrObjectConflict = errors.New("logical object key already exists")
