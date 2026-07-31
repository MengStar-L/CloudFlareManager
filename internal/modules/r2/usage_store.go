package r2

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
)

type OperationClass string

const (
	OperationClassA OperationClass = "class_a"
	OperationClassB OperationClass = "class_b"
)

type AccountUsage struct {
	AccountID      string    `json:"account_id"`
	Month          string    `json:"usage_month"`
	ManagedBytes   int64     `json:"managed_bytes"`
	UnmanagedBytes int64     `json:"unmanaged_bytes"`
	ReservedBytes  int64     `json:"reserved_bytes"`
	StorageLimit   int64     `json:"account_storage_soft_limit_bytes"`
	ClassAOps      int64     `json:"class_a_ops"`
	ClassALimit    int64     `json:"class_a_soft_limit"`
	ClassBOps      int64     `json:"class_b_ops"`
	ClassBLimit    int64     `json:"class_b_soft_limit"`
	UsageCheckedAt time.Time `json:"usage_checked_at"`
}

func usageMonth(value time.Time) string {
	return value.UTC().Format("2006-01")
}

func (s *Store) ConsumeOperation(ctx context.Context, accountID string, class OperationClass) error {
	return s.recordOperation(ctx, accountID, class, true)
}

func (s *Store) RecordOperation(ctx context.Context, accountID string, class OperationClass) error {
	return s.recordOperation(ctx, accountID, class, false)
}

func (s *Store) recordOperation(ctx context.Context, accountID string, class OperationClass, enforce bool) error {
	if accountID == "" {
		return errors.New("account id is required")
	}
	month := usageMonth(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var classA, classB int64
	err = tx.QueryRowContext(ctx, `SELECT class_a_ops, class_b_ops FROM r2_account_usage_monthly
		WHERE account_id = ? AND usage_month = ?`, accountID, month).Scan(&classA, &classB)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	switch class {
	case OperationClassA:
		if enforce && classA+1 > s.limits.ClassA {
			return ErrQuotaExceeded
		}
		classA++
	case OperationClassB:
		if enforce && classB+1 > s.limits.ClassB {
			return ErrQuotaExceeded
		}
		classB++
	default:
		return fmt.Errorf("unknown operation class %q", class)
	}
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `INSERT INTO r2_account_usage_monthly(
		account_id, usage_month, class_a_ops, class_b_ops, updated_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(account_id, usage_month) DO UPDATE SET class_a_ops = excluded.class_a_ops,
		class_b_ops = excluded.class_b_ops, updated_at = excluded.updated_at`, accountID, month, classA, classB, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetAccountUsage(ctx context.Context, accountID string) (AccountUsage, error) {
	usage := AccountUsage{
		AccountID: accountID, Month: usageMonth(time.Now()), StorageLimit: s.limits.AccountStorageBytes,
		ClassALimit: s.limits.ClassA, ClassBLimit: s.limits.ClassB,
	}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes), 0),
		COALESCE(SUM(reserved_storage_bytes), 0) FROM r2_physical_buckets WHERE account_id = ?`,
		accountID).Scan(&usage.ManagedBytes, &usage.ReservedBytes)
	if err != nil {
		return AccountUsage{}, err
	}
	var checked int64
	err = s.db.QueryRowContext(ctx, `SELECT unmanaged_storage_bytes, usage_checked_at
		FROM r2_account_capacity WHERE account_id = ?`, accountID).Scan(&usage.UnmanagedBytes, &checked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AccountUsage{}, err
	}
	if checked > 0 {
		usage.UsageCheckedAt = time.Unix(checked, 0)
	}
	err = s.db.QueryRowContext(ctx, `SELECT class_a_ops, class_b_ops FROM r2_account_usage_monthly
		WHERE account_id = ? AND usage_month = ?`, accountID, usage.Month).Scan(&usage.ClassAOps, &usage.ClassBOps)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AccountUsage{}, err
	}
	return usage, nil
}

func (s *Store) SetUnmanagedStorage(ctx context.Context, accountID string, bytes int64, checkedAt time.Time) error {
	if accountID == "" || bytes < 0 {
		return errors.New("account id and non-negative storage are required")
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO r2_account_capacity(
		account_id, unmanaged_storage_bytes, usage_checked_at, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET unmanaged_storage_bytes = excluded.unmanaged_storage_bytes,
		usage_checked_at = excluded.usage_checked_at, updated_at = excluded.updated_at`,
		accountID, bytes, checkedAt.Unix(), time.Now().Unix())
	return err
}

func (s *Store) ApplyAccountCapacity(ctx context.Context, accountID string, observed map[string]int64, unmanaged int64, checkedAt time.Time) error {
	if accountID == "" || unmanaged < 0 {
		return errors.New("account id and non-negative unmanaged storage are required")
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for bucketID, remoteBytes := range observed {
		if remoteBytes < 0 {
			return errors.New("observed bucket storage cannot be negative")
		}
		var indexedBytes, cleanupBytes int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM r2_objects
			WHERE physical_bucket_id = ? AND state = ?`, bucketID, StateCommitted).Scan(&indexedBytes); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM r2_physical_cleanups
			WHERE physical_bucket_id = ?`, bucketID).Scan(&cleanupBytes); err != nil {
			return err
		}
		minimum := indexedBytes + cleanupBytes
		if remoteBytes < minimum {
			remoteBytes = minimum
		}
		result, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET storage_bytes = ?, usage_checked_at = ?,
			updated_at = ? WHERE id = ? AND account_id = ?`, remoteBytes, checkedAt.Unix(), time.Now().Unix(), bucketID, accountID)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrBucketNotFound
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO r2_account_capacity(
		account_id, unmanaged_storage_bytes, usage_checked_at, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET unmanaged_storage_bytes = excluded.unmanaged_storage_bytes,
		usage_checked_at = excluded.usage_checked_at, updated_at = excluded.updated_at`,
		accountID, unmanaged, checkedAt.Unix(), time.Now().Unix())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s Service) SyncAccountCapacity(ctx context.Context) error {
	if s.Index == nil || s.Accounts == nil {
		return errors.New("R2 service is not configured")
	}
	provider := s.Usage
	if provider == nil {
		provider = accounts.RemoteClient{}
	}
	accountList, err := s.Accounts.List(ctx)
	if err != nil {
		return err
	}
	buckets, err := s.Index.ListBuckets(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, publicAccount := range accountList {
		if !publicAccount.Enabled {
			continue
		}
		account, err := s.Accounts.Get(ctx, publicAccount.ID, true)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		usage, err := provider.R2BucketUsage(ctx, account.CloudflareAccountID, account.APIToken)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		managedNames := make(map[string]PhysicalBucket)
		var locked []string
		for _, bucket := range buckets {
			if bucket.AccountID != account.ID {
				continue
			}
			managedNames[bucket.Name] = bucket
			if err := s.Index.AcquireBucketMaintenance(ctx, bucket.ID, "account-capacity-sync"); err != nil {
				for _, id := range locked {
					_ = s.Index.ReleaseBucketMaintenance(context.Background(), id)
				}
				if firstErr == nil {
					firstErr = err
				}
				locked = nil
				break
			}
			locked = append(locked, bucket.ID)
		}
		if locked == nil && len(managedNames) != 0 {
			continue
		}
		observed := make(map[string]int64, len(managedNames))
		var unmanaged int64
		for name, item := range usage {
			bytes := item.PayloadBytes + item.MetadataBytes
			if bucket, ok := managedNames[name]; ok {
				observed[bucket.ID] = bytes
			} else {
				unmanaged += bytes
			}
		}
		for _, bucket := range managedNames {
			if _, ok := observed[bucket.ID]; !ok {
				observed[bucket.ID] = 0
			}
		}
		err = s.Index.ApplyAccountCapacity(ctx, account.ID, observed, unmanaged, time.Now())
		for _, id := range locked {
			_ = s.Index.ReleaseBucketMaintenance(context.Background(), id)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
