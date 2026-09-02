package r2

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type BucketLifecycleState string

const (
	BucketActive       BucketLifecycleState = "active"
	BucketDeleting     BucketLifecycleState = "deleting"
	BucketDeleteFailed BucketLifecycleState = "delete_failed"
)

var (
	ErrBucketDeleting = errors.New("bucket deletion is in progress")
	ErrBucketInUse    = errors.New("physical bucket is still referenced")
)

func (s *Store) GetBucketByAccountAndName(ctx context.Context, accountID, name string) (PhysicalBucket, error) {
	if accountID == "" || name == "" {
		return PhysicalBucket{}, errors.New("account id and bucket name are required")
	}
	return scanBucket(s.db.QueryRowContext(ctx, bucketSelect+" WHERE account_id = ? AND bucket_name = ?", accountID, name))
}

func (s *Store) EnsureBucketActive(ctx context.Context, bucketID string) error {
	return ensureBucketActiveQuery(ctx, s.db, bucketID)
}

type bucketLifecycleQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func ensureBucketActiveQuery(ctx context.Context, queryer bucketLifecycleQueryer, bucketID string) error {
	var state BucketLifecycleState
	if err := queryer.QueryRowContext(ctx, `SELECT lifecycle_state FROM r2_physical_buckets WHERE id = ?`, bucketID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBucketNotFound
		}
		return err
	}
	if state != BucketActive {
		return ErrBucketDeleting
	}
	return nil
}

func (s *Store) BeginBucketDeletion(ctx context.Context, bucketID, jobID, parentJobID string) (PhysicalBucket, error) {
	if bucketID == "" || jobID == "" {
		return PhysicalBucket{}, errors.New("bucket id and job id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PhysicalBucket{}, err
	}
	defer tx.Rollback()
	bucket, err := scanBucket(tx.QueryRowContext(ctx, bucketSelect+" WHERE id = ?", bucketID))
	if err != nil {
		return PhysicalBucket{}, err
	}
	var existingLock string
	lockErr := tx.QueryRowContext(ctx, `SELECT operation FROM r2_bucket_maintenance_locks
		WHERE physical_bucket_id = ?`, bucketID).Scan(&existingLock)
	if lockErr != nil && !errors.Is(lockErr, sql.ErrNoRows) {
		return PhysicalBucket{}, lockErr
	}
	hasLock := lockErr == nil
	transferLock := false
	switch bucket.LifecycleState {
	case BucketActive:
		if parentJobID != "" {
			return PhysicalBucket{}, ErrBucketDeleting
		}
		if hasLock {
			return PhysicalBucket{}, ErrBucketBusy
		}
	case BucketDeleteFailed, BucketDeleting:
		if parentJobID == "" || bucket.DeletionJobID != parentJobID {
			return PhysicalBucket{}, ErrBucketDeleting
		}
		var failedParent int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM jobs WHERE id = ? AND type = ? AND status = 'failed'
		)`, parentJobID, BucketDeletionJobType).Scan(&failedParent); err != nil {
			return PhysicalBucket{}, err
		}
		if failedParent == 0 {
			return PhysicalBucket{}, ErrBucketDeleting
		}
		if hasLock {
			if existingLock != "delete:"+parentJobID {
				return PhysicalBucket{}, ErrBucketBusy
			}
			transferLock = true
		}
	default:
		return PhysicalBucket{}, ErrBucketDeleting
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets
		SET lifecycle_state = ?, deletion_job_id = ?, updated_at = ?
		WHERE id = ? AND lifecycle_state = ? AND COALESCE(deletion_job_id, '') = ?`,
		BucketDeleting, jobID, now, bucketID, bucket.LifecycleState, bucket.DeletionJobID)
	if err != nil {
		return PhysicalBucket{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return PhysicalBucket{}, ErrBucketDeleting
	}
	if transferLock {
		result, err = tx.ExecContext(ctx, `UPDATE r2_bucket_maintenance_locks
			SET operation = ?, created_at = ? WHERE physical_bucket_id = ? AND operation = ?`,
			"delete:"+jobID, now, bucketID, "delete:"+parentJobID)
		if err != nil {
			return PhysicalBucket{}, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return PhysicalBucket{}, ErrBucketBusy
		}
	}
	if err := tx.Commit(); err != nil {
		return PhysicalBucket{}, err
	}
	bucket.LifecycleState = BucketDeleting
	bucket.DeletionJobID = jobID
	return bucket, nil
}

func (s *Store) MarkBucketDeletionFailed(ctx context.Context, bucketID, jobID string) error {
	if bucketID == "" || jobID == "" {
		return errors.New("bucket id and job id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets
		SET lifecycle_state = ?, updated_at = ?
		WHERE id = ? AND lifecycle_state = ? AND deletion_job_id = ?`,
		BucketDeleteFailed, time.Now().Unix(), bucketID, BucketDeleting, jobID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBucketDeleting
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM r2_bucket_maintenance_locks WHERE physical_bucket_id = ?`, bucketID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RestoreBucketActive(ctx context.Context, bucketID, jobID string) error {
	if bucketID == "" || jobID == "" {
		return errors.New("bucket id and job id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets
		SET lifecycle_state = ?, deletion_job_id = NULL, updated_at = ?
		WHERE id = ? AND lifecycle_state = ? AND deletion_job_id = ?`,
		BucketActive, time.Now().Unix(), bucketID, BucketDeleting, jobID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBucketDeleting
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM r2_bucket_maintenance_locks WHERE physical_bucket_id = ?`, bucketID); err != nil {
		return err
	}
	return tx.Commit()
}

func deletionBlockingActivityQuery() string {
	return `SELECT EXISTS(
		SELECT 1 FROM r2_write_intents AS wi
		WHERE wi.target_bucket_id = ? OR EXISTS(
			SELECT 1 FROM r2_objects AS previous
			WHERE previous.object_id = wi.previous_object_id
				AND previous.physical_bucket_id = ?
		)
	) OR EXISTS(
		SELECT 1 FROM r2_multipart_uploads WHERE physical_bucket_id = ?
	)`
}

func (s *Store) HasDeletionBlockingActivity(ctx context.Context, bucketID string) (bool, error) {
	var active int
	err := s.db.QueryRowContext(ctx, deletionBlockingActivityQuery(), bucketID, bucketID, bucketID).Scan(&active)
	return active != 0, err
}

func (s *Store) ListWriteIntentsByBucket(ctx context.Context, bucketID string, limit int) ([]WriteIntent, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, writeIntentSelect+` AS intent
		WHERE intent.target_bucket_id = ? OR EXISTS(
			SELECT 1 FROM r2_objects AS previous
			WHERE previous.object_id = intent.previous_object_id
				AND previous.physical_bucket_id = ?
		)
		ORDER BY intent.created_at, intent.id LIMIT ?`, bucketID, bucketID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intents []WriteIntent
	for rows.Next() {
		intent, err := scanWriteIntent(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (s *Store) ListMultipartByBucket(ctx context.Context, bucketID string, limit int) ([]MultipartUpload, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, multipartSelect+` WHERE physical_bucket_id = ?
		ORDER BY created_at, id LIMIT ?`, bucketID, limit)
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

// SettleBucketForDeletion lets already-running writes finish, aborts idle
// multipart uploads, and recovers any remaining intents. New work is already
// fenced by lifecycle_state before this method is called.
func (s Service) SettleBucketForDeletion(ctx context.Context, bucketID string) (bool, error) {
	bucket, err := s.Index.GetBucket(ctx, bucketID)
	if err != nil {
		return false, err
	}
	if bucket.LifecycleState != BucketDeleting {
		return false, ErrBucketDeleting
	}
	uploads, err := s.Index.ListMultipartByBucket(ctx, bucketID, 10000)
	if err != nil {
		return false, err
	}
	for _, upload := range uploads {
		if err := s.AbortMultipart(ctx, upload.Key, upload.ID); err != nil && !errors.Is(err, ErrMultipartNotFound) {
			return false, err
		}
	}
	backend, err := s.maintenanceBackend()
	if err != nil {
		return false, err
	}
	intents, err := s.Index.ListWriteIntentsByBucket(ctx, bucketID, 10000)
	if err != nil {
		return false, err
	}
	for _, intent := range intents {
		if err := s.recoverWriteIntent(ctx, backend, intent); err != nil {
			return false, err
		}
	}
	active, err := s.Index.HasDeletionBlockingActivity(ctx, bucketID)
	return !active, err
}

func (s *Store) AcquireBucketDeletionMaintenance(ctx context.Context, bucketID, jobID string) error {
	if bucketID == "" || jobID == "" {
		return errors.New("bucket id and job id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owned int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM r2_physical_buckets
		WHERE id = ? AND lifecycle_state = ? AND deletion_job_id = ?
	)`, bucketID, BucketDeleting, jobID).Scan(&owned); err != nil {
		return err
	}
	if owned == 0 {
		return ErrBucketDeleting
	}
	var active int
	if err := tx.QueryRowContext(ctx, deletionBlockingActivityQuery(), bucketID, bucketID, bucketID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return ErrBucketBusy
	}
	operation := "delete:" + jobID
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT operation FROM r2_bucket_maintenance_locks WHERE physical_bucket_id = ?`, bucketID).Scan(&existing)
	switch {
	case err == nil && existing == operation:
		return tx.Commit()
	case err == nil:
		return ErrBucketBusy
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO r2_bucket_maintenance_locks(
		physical_bucket_id, operation, created_at) VALUES(?, ?, ?)`, bucketID, operation, time.Now().Unix()); err != nil {
		return ErrBucketBusy
	}
	return tx.Commit()
}

func (s *Store) FinalizeDeletedBucket(ctx context.Context, bucketID, jobID string) error {
	if bucketID == "" || jobID == "" {
		return errors.New("bucket id and job id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owned int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM r2_physical_buckets
		WHERE id = ? AND lifecycle_state = ? AND deletion_job_id = ?
	)`, bucketID, BucketDeleting, jobID).Scan(&owned); err != nil {
		return err
	}
	if owned == 0 {
		return ErrBucketDeleting
	}
	var active int
	if err := tx.QueryRowContext(ctx, deletionBlockingActivityQuery(), bucketID, bucketID, bucketID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return ErrBucketBusy
	}
	var operation string
	if err := tx.QueryRowContext(ctx, `SELECT operation FROM r2_bucket_maintenance_locks
		WHERE physical_bucket_id = ?`, bucketID).Scan(&operation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBucketBusy
		}
		return err
	}
	if operation != "delete:"+jobID {
		return ErrBucketBusy
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM webdav_locks WHERE expires_at <= ?`, []any{time.Now().Unix()}},
		{`DELETE FROM r2_multipart_uploads WHERE physical_bucket_id = ?`, []any{bucketID}},
		{`DELETE FROM r2_write_intents WHERE target_bucket_id = ? OR EXISTS(
			SELECT 1 FROM r2_objects AS previous
			WHERE previous.object_id = r2_write_intents.previous_object_id
				AND previous.physical_bucket_id = ?)`, []any{bucketID, bucketID}},
		{`DELETE FROM r2_physical_cleanups WHERE physical_bucket_id = ?`, []any{bucketID}},
		{`DELETE FROM r2_objects WHERE physical_bucket_id = ?`, []any{bucketID}},
		{`DELETE FROM r2_scan_findings WHERE physical_bucket_id = ?`, []any{bucketID}},
		{`DELETE FROM r2_placement_rules WHERE target_bucket_id = ?`, []any{bucketID}},
		{`DELETE FROM r2_bucket_maintenance_locks WHERE physical_bucket_id = ?`, []any{bucketID}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("finalize deleted bucket: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM r2_physical_buckets
		WHERE id = ? AND lifecycle_state = ? AND deletion_job_id = ?`, bucketID, BucketDeleting, jobID)
	if err != nil {
		return fmt.Errorf("finalize deleted bucket: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBucketDeleting
	}
	return tx.Commit()
}
