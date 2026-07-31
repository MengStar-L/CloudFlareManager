package r2

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PhysicalCleanup struct {
	ID           string
	ObjectKey    string
	BucketID     string
	PhysicalKey  string
	ExpectedETag string
	Size         int64
	Status       string
	Error        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (s *Store) ListPhysicalCleanups(ctx context.Context, limit int) ([]PhysicalCleanup, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, cleanupSelect+` WHERE status IN ('pending', 'processing')
		ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PhysicalCleanup
	for rows.Next() {
		cleanup, err := scanPhysicalCleanup(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, cleanup)
	}
	return result, rows.Err()
}

func (s *Store) ClaimPhysicalCleanup(ctx context.Context, id string) (PhysicalCleanup, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PhysicalCleanup{}, err
	}
	defer tx.Rollback()
	cleanup, err := scanPhysicalCleanup(tx.QueryRowContext(ctx, cleanupSelect+" WHERE id = ?", id))
	if err != nil {
		return PhysicalCleanup{}, err
	}
	if cleanup.Status != "pending" && cleanup.Status != "processing" {
		return PhysicalCleanup{}, ErrCleanupNotFound
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_cleanups SET status = 'processing', updated_at = ? WHERE id = ?`,
		now.UnixNano(), id); err != nil {
		return PhysicalCleanup{}, err
	}
	if err := tx.Commit(); err != nil {
		return PhysicalCleanup{}, err
	}
	cleanup.Status, cleanup.UpdatedAt = "processing", now
	return cleanup, nil
}

func (s *Store) RetryPhysicalCleanup(ctx context.Context, id string, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE r2_physical_cleanups SET status = 'pending', error = ?, updated_at = ?
		WHERE id = ?`, message, time.Now().UnixNano(), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrCleanupNotFound
	}
	return nil
}

func (s *Store) CompletePhysicalCleanup(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cleanup, err := scanPhysicalCleanup(tx.QueryRowContext(ctx, cleanupSelect+" WHERE id = ?", id))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET storage_bytes = MAX(storage_bytes - ?, 0),
		updated_at = ? WHERE id = ?`, cleanup.Size, time.Now().Unix(), cleanup.BucketID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM r2_physical_cleanups WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Service) ProcessPhysicalCleanups(ctx context.Context, limit int) (int, error) {
	backend, err := s.maintenanceBackend()
	if err != nil {
		return 0, err
	}
	items, err := s.Index.ListPhysicalCleanups(ctx, limit)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, listed := range items {
		cleanup, err := s.Index.ClaimPhysicalCleanup(ctx, listed.ID)
		if err != nil {
			return completed, err
		}
		if err := s.Index.AcquireBucketMaintenance(ctx, cleanup.BucketID, "physical-cleanup"); err != nil {
			_ = s.Index.RetryPhysicalCleanup(ctx, cleanup.ID, err)
			return completed, err
		}
		locked := true
		release := func() {
			if locked {
				_ = s.Index.ReleaseBucketMaintenance(context.Background(), cleanup.BucketID)
				locked = false
			}
		}
		target, err := s.target(ctx, cleanup.BucketID)
		if err != nil {
			release()
			_ = s.Index.RetryPhysicalCleanup(ctx, cleanup.ID, err)
			return completed, err
		}
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
		remote, err := backend.Head(ctx, target, cleanup.PhysicalKey)
		if isRemoteNotFound(err) {
			if err := s.Index.CompletePhysicalCleanup(ctx, cleanup.ID); err != nil {
				release()
				return completed, err
			}
			release()
			completed++
			continue
		}
		if err != nil {
			release()
			_ = s.Index.RetryPhysicalCleanup(ctx, cleanup.ID, err)
			return completed, err
		}
		if cleanup.ExpectedETag == "" {
			err := fmt.Errorf("cleanup ETag is missing for %s", cleanup.PhysicalKey)
			release()
			_ = s.Index.RetryPhysicalCleanup(ctx, cleanup.ID, err)
			return completed, err
		}
		if !strings.EqualFold(strings.Trim(remote.ETag, `"`), strings.Trim(cleanup.ExpectedETag, `"`)) {
			err := fmt.Errorf("cleanup ETag mismatch for %s", cleanup.PhysicalKey)
			release()
			_ = s.Index.RetryPhysicalCleanup(ctx, cleanup.ID, err)
			return completed, err
		}
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
		if err := s.Backend.Delete(ctx, target, cleanup.PhysicalKey); err != nil {
			release()
			_ = s.Index.RetryPhysicalCleanup(ctx, cleanup.ID, err)
			return completed, err
		}
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
		if _, err := backend.Head(ctx, target, cleanup.PhysicalKey); !isRemoteNotFound(err) {
			if err == nil {
				err = errors.New("cleanup delete was not visible upstream")
			}
			release()
			_ = s.Index.RetryPhysicalCleanup(ctx, cleanup.ID, err)
			return completed, err
		}
		if err := s.Index.CompletePhysicalCleanup(ctx, cleanup.ID); err != nil {
			release()
			return completed, err
		}
		release()
		completed++
	}
	return completed, nil
}

const cleanupSelect = `SELECT id, object_key, physical_bucket_id, physical_key, expected_etag,
	size, status, error, created_at, updated_at FROM r2_physical_cleanups`

func scanPhysicalCleanup(row scanner) (PhysicalCleanup, error) {
	var cleanup PhysicalCleanup
	var created, updated int64
	if err := row.Scan(&cleanup.ID, &cleanup.ObjectKey, &cleanup.BucketID, &cleanup.PhysicalKey,
		&cleanup.ExpectedETag, &cleanup.Size, &cleanup.Status, &cleanup.Error, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PhysicalCleanup{}, ErrCleanupNotFound
		}
		return PhysicalCleanup{}, err
	}
	cleanup.CreatedAt, cleanup.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
	return cleanup, nil
}

var ErrCleanupNotFound = errors.New("physical cleanup not found")
