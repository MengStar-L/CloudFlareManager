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

const InternalWriteIDMetadata = "cf-r2-manager-write-id"

type WriteIntentState string

const (
	WriteReserved   WriteIntentState = "reserved"
	WriteUploading  WriteIntentState = "uploading"
	WriteCompleting WriteIntentState = "completing"
	WriteAborting   WriteIntentState = "aborting"
	WriteRecovery   WriteIntentState = "recovery"
	WriteDeleting   WriteIntentState = "deleting"
)

type WriteOperation string

const (
	WriteOperationPut             WriteOperation = "put"
	WriteOperationDelete          WriteOperation = "delete"
	WriteOperationLegacyMultipart WriteOperation = "legacy-multipart"
)

type BeginWriteInput struct {
	ObjectInput
	ExpectedClassA    int64
	InternalMultipart bool
	TargetBucketID    string
}

type WriteIntent struct {
	ID                string
	Key               string
	BucketID          string
	PreviousObjectID  string
	ReservedBytes     int64
	DeclaredSize      int64
	ActualSize        int64
	ContentType       string
	Metadata          map[string]string
	State             WriteIntentState
	Operation         WriteOperation
	UpstreamUploadID  string
	ETag              string
	InternalMultipart bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (s *Store) BeginWrite(ctx context.Context, input BeginWriteInput) (WriteIntent, error) {
	if input.Key == "" || input.Size < -1 {
		return WriteIntent{}, errors.New("object key is required and size cannot be less than -1")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WriteIntent{}, err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM r2_write_intents WHERE object_key = ?", input.Key).Scan(&active); err != nil {
		return WriteIntent{}, err
	}
	if active != 0 {
		return WriteIntent{}, ErrWriteInProgress
	}
	var previous Object
	previous, err = scanObject(tx.QueryRowContext(ctx, objectSelect+" WHERE object_key = ? AND state = ?", input.Key, StateCommitted))
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		return WriteIntent{}, err
	}
	if errors.Is(err, ErrObjectNotFound) {
		previous = Object{}
	}
	selected, err := s.selectWriteBucket(ctx, tx, input)
	if err != nil {
		return WriteIntent{}, err
	}
	reserved := input.Size
	if reserved < 0 {
		reserved = 0
	}
	now := time.Now()
	intent := WriteIntent{
		ID: uuid.NewString(), Key: input.Key, BucketID: selected.ID, PreviousObjectID: previous.ObjectID,
		ReservedBytes: reserved, DeclaredSize: input.Size, ContentType: input.ContentType,
		Metadata: userVisibleMetadata(input.Metadata), State: WriteReserved, Operation: WriteOperationPut,
		InternalMultipart: input.InternalMultipart,
		CreatedAt:         now, UpdatedAt: now,
	}
	metadata, err := json.Marshal(intent.Metadata)
	if err != nil {
		return WriteIntent{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO r2_write_intents(
		id, object_key, target_bucket_id, previous_object_id, reserved_bytes, declared_size,
		actual_size, content_type, metadata_json, state, operation, upstream_upload_id, etag,
		internal_multipart, created_at, updated_at) VALUES(?, ?, ?, NULLIF(?, ''), ?, ?, 0, ?, ?, ?, ?, '', '', ?, ?, ?)`,
		intent.ID, intent.Key, intent.BucketID, intent.PreviousObjectID, intent.ReservedBytes,
		intent.DeclaredSize, intent.ContentType, string(metadata), intent.State, intent.Operation, intent.InternalMultipart,
		now.UnixNano(), now.UnixNano())
	if err != nil {
		return WriteIntent{}, fmt.Errorf("create write intent: %w", err)
	}
	if intent.ReservedBytes > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET reserved_storage_bytes = reserved_storage_bytes + ?,
			updated_at = ? WHERE id = ?`, intent.ReservedBytes, now.Unix(), intent.BucketID); err != nil {
			return WriteIntent{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return WriteIntent{}, err
	}
	return intent, nil
}

func (s *Store) BeginDeleteWrite(ctx context.Context, key string) (WriteIntent, Object, error) {
	if key == "" {
		return WriteIntent{}, Object{}, errors.New("object key is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WriteIntent{}, Object{}, err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_write_intents WHERE object_key = ?`, key).Scan(&active); err != nil {
		return WriteIntent{}, Object{}, err
	}
	if active != 0 {
		return WriteIntent{}, Object{}, ErrWriteInProgress
	}
	object, err := scanObject(tx.QueryRowContext(ctx, objectSelect+" WHERE object_key = ? AND state = ?", key, StateCommitted))
	if err != nil {
		return WriteIntent{}, Object{}, err
	}
	var maintenanceLocked int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_bucket_maintenance_locks
		WHERE physical_bucket_id = ?`, object.BucketID).Scan(&maintenanceLocked); err != nil {
		return WriteIntent{}, Object{}, err
	}
	if maintenanceLocked != 0 {
		return WriteIntent{}, Object{}, ErrBucketBusy
	}
	now := time.Now()
	intent := WriteIntent{
		ID: uuid.NewString(), Key: key, BucketID: object.BucketID, PreviousObjectID: object.ObjectID,
		DeclaredSize: 0, ContentType: object.ContentType, Metadata: cloneMetadata(object.Metadata),
		State: WriteDeleting, Operation: WriteOperationDelete, CreatedAt: now, UpdatedAt: now,
	}
	metadata, err := json.Marshal(intent.Metadata)
	if err != nil {
		return WriteIntent{}, Object{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO r2_write_intents(
		id, object_key, target_bucket_id, previous_object_id, reserved_bytes, declared_size,
		actual_size, content_type, metadata_json, state, operation, upstream_upload_id, etag,
		internal_multipart, created_at, updated_at) VALUES(?, ?, ?, ?, 0, 0, 0, ?, ?, ?, ?, '', '', 0, ?, ?)`,
		intent.ID, intent.Key, intent.BucketID, intent.PreviousObjectID, intent.ContentType, string(metadata),
		intent.State, intent.Operation, now.UnixNano(), now.UnixNano())
	if err != nil {
		return WriteIntent{}, Object{}, fmt.Errorf("create delete intent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WriteIntent{}, Object{}, err
	}
	return intent, object, nil
}

func (s *Store) selectWriteBucket(ctx context.Context, tx *sql.Tx, input BeginWriteInput) (Candidate, error) {
	rows, err := tx.QueryContext(ctx, bucketSelect+" ORDER BY account_id, bucket_name")
	if err != nil {
		return Candidate{}, err
	}
	var buckets []PhysicalBucket
	for rows.Next() {
		bucket, err := scanBucket(rows)
		if err != nil {
			rows.Close()
			return Candidate{}, err
		}
		buckets = append(buckets, bucket)
	}
	if err := rows.Close(); err != nil {
		return Candidate{}, err
	}
	rules, err := listRulesQuery(ctx, tx)
	if err != nil {
		return Candidate{}, err
	}
	type accountState struct {
		managed, reserved, unmanaged, classA, classB int64
	}
	accounts := make(map[string]accountState)
	candidates := make([]Candidate, 0, len(buckets))
	requested := input.Size
	if requested < 0 {
		requested = 0
	}
	for _, bucket := range buckets {
		state, ok := accounts[bucket.AccountID]
		if !ok {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes), 0),
				COALESCE(SUM(reserved_storage_bytes), 0) FROM r2_physical_buckets WHERE account_id = ?`,
				bucket.AccountID).Scan(&state.managed, &state.reserved); err != nil {
				return Candidate{}, err
			}
			_ = tx.QueryRowContext(ctx, `SELECT unmanaged_storage_bytes FROM r2_account_capacity
				WHERE account_id = ?`, bucket.AccountID).Scan(&state.unmanaged)
			_ = tx.QueryRowContext(ctx, `SELECT class_a_ops, class_b_ops FROM r2_account_usage_monthly
				WHERE account_id = ? AND usage_month = ?`, bucket.AccountID, usageMonth(time.Now())).Scan(&state.classA, &state.classB)
			accounts[bucket.AccountID] = state
		}
		var fenced int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_physical_cleanups
			WHERE physical_bucket_id = ? AND physical_key = ?`, bucket.ID, input.Key).Scan(&fenced); err != nil {
			return Candidate{}, err
		}
		var maintenanceLocked int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_bucket_maintenance_locks
			WHERE physical_bucket_id = ?`, bucket.ID).Scan(&maintenanceLocked); err != nil {
			return Candidate{}, err
		}
		bucketAvailable := s.limits.StorageBytes - bucket.StorageBytes - bucket.ReservedBytes
		accountAvailable := s.limits.AccountStorageBytes - state.managed - state.reserved - state.unmanaged
		available := bucketAvailable
		if accountAvailable < available {
			available = accountAvailable
		}
		candidate := Candidate{
			ID: bucket.ID, AccountID: bucket.AccountID, Healthy: bucket.HealthStatus == "healthy",
			Writable:            bucket.Writable && fenced == 0 && maintenanceLocked == 0 && !bucket.UsageCheckedAt.IsZero(),
			StorageRatio:        ratio(bucket.StorageBytes+bucket.ReservedBytes+requested, s.limits.StorageBytes),
			AccountStorageRatio: ratio(state.managed+state.reserved+state.unmanaged+requested, s.limits.AccountStorageBytes),
			ClassARatio:         ratio(state.classA+input.ExpectedClassA, s.limits.ClassA),
			ClassBRatio:         ratio(state.classB, s.limits.ClassB), LatencyRatio: latencyRatio(bucket.LatencyMS),
			AvailableBytes: available,
			AllowOverflow:  bucket.OverflowUntil != nil && bucket.OverflowUntil.After(time.Now()),
		}
		candidates = append(candidates, candidate)
	}
	if input.TargetBucketID != "" {
		for _, candidate := range candidates {
			if candidate.ID == input.TargetBucketID {
				if !s.policy.eligible(candidate) {
					return Candidate{}, ErrQuotaExceeded
				}
				return candidate, nil
			}
		}
		return Candidate{}, ErrBucketNotFound
	}
	return s.policy.Select(input.ObjectInput, candidates, rules)
}

func listRulesQuery(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]PlacementRule, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT prefix, extension, content_type, min_size, max_size, target_bucket_id
		FROM r2_placement_rules WHERE enabled = 1 ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []PlacementRule
	for rows.Next() {
		var rule PlacementRule
		if err := rows.Scan(&rule.Prefix, &rule.Extension, &rule.ContentType, &rule.MinSize, &rule.MaxSize, &rule.TargetID); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func (s *Store) GetWriteIntent(ctx context.Context, id string) (WriteIntent, error) {
	return scanWriteIntent(s.db.QueryRowContext(ctx, writeIntentSelect+" WHERE id = ?", id))
}

func (s *Store) ListWriteIntents(ctx context.Context, limit int) ([]WriteIntent, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, writeIntentSelect+" ORDER BY created_at, id LIMIT ?", limit)
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
	return result, rows.Err()
}

func (s *Store) EnsureWriteReservation(ctx context.Context, id string, total int64) (WriteIntent, error) {
	if total < 0 {
		return WriteIntent{}, errors.New("reservation cannot be negative")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WriteIntent{}, err
	}
	defer tx.Rollback()
	intent, err := scanWriteIntent(tx.QueryRowContext(ctx, writeIntentSelect+" WHERE id = ?", id))
	if err != nil {
		return WriteIntent{}, err
	}
	if total <= intent.ReservedBytes {
		return intent, tx.Commit()
	}
	delta := total - intent.ReservedBytes
	bucket, err := scanBucket(tx.QueryRowContext(ctx, bucketSelect+" WHERE id = ?", intent.BucketID))
	if err != nil {
		return WriteIntent{}, err
	}
	var managed, reserved, unmanaged int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes), 0), COALESCE(SUM(reserved_storage_bytes), 0)
		FROM r2_physical_buckets WHERE account_id = ?`, bucket.AccountID).Scan(&managed, &reserved); err != nil {
		return WriteIntent{}, err
	}
	_ = tx.QueryRowContext(ctx, "SELECT unmanaged_storage_bytes FROM r2_account_capacity WHERE account_id = ?", bucket.AccountID).Scan(&unmanaged)
	overflow := bucket.OverflowUntil != nil && bucket.OverflowUntil.After(time.Now())
	if !overflow && (bucket.StorageBytes+bucket.ReservedBytes+delta > s.limits.StorageBytes ||
		managed+reserved+unmanaged+delta > s.limits.AccountStorageBytes) {
		return WriteIntent{}, ErrQuotaExceeded
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET reserved_storage_bytes = reserved_storage_bytes + ?,
		updated_at = ? WHERE id = ?`, delta, now.Unix(), intent.BucketID); err != nil {
		return WriteIntent{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE r2_write_intents SET reserved_bytes = ?, updated_at = ? WHERE id = ?`,
		total, now.UnixNano(), id); err != nil {
		return WriteIntent{}, err
	}
	if err := tx.Commit(); err != nil {
		return WriteIntent{}, err
	}
	intent.ReservedBytes, intent.UpdatedAt = total, now
	return intent, nil
}

func (s *Store) MarkWriteUploading(ctx context.Context, id, upstreamID string) error {
	return s.updateWriteState(ctx, id, WriteUploading, upstreamID, "", 0)
}

func (s *Store) MarkWriteCompleting(ctx context.Context, id, etag string, size int64) error {
	return s.updateWriteState(ctx, id, WriteCompleting, "", etag, size)
}

func (s *Store) HoldWriteForRecovery(ctx context.Context, id, upstreamID string) error {
	return s.updateWriteState(ctx, id, WriteRecovery, upstreamID, "", 0)
}

func (s *Store) MarkWriteAborting(ctx context.Context, id, upstreamID string) error {
	return s.updateWriteState(ctx, id, WriteAborting, upstreamID, "", 0)
}

func (s *Store) updateWriteState(ctx context.Context, id string, state WriteIntentState, upstreamID, etag string, size int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_write_intents SET state = ?,
		upstream_upload_id = CASE WHEN ? = '' THEN upstream_upload_id ELSE ? END,
		etag = CASE WHEN ? = '' THEN etag ELSE ? END,
		actual_size = CASE WHEN ? = 0 THEN actual_size ELSE ? END, updated_at = ? WHERE id = ?`,
		state, upstreamID, upstreamID, etag, etag, size, size, time.Now().UnixNano(), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrWriteIntentNotFound
	}
	return nil
}

func (s *Store) AbortWrite(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	intent, err := scanWriteIntent(tx.QueryRowContext(ctx, writeIntentSelect+" WHERE id = ?", id))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET
		reserved_storage_bytes = MAX(reserved_storage_bytes - ?, 0), updated_at = ? WHERE id = ?`,
		intent.ReservedBytes, time.Now().Unix(), intent.BucketID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM r2_write_intents WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitWrite(ctx context.Context, id, etag string, size int64) (Object, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Object{}, err
	}
	defer tx.Rollback()
	intent, err := scanWriteIntent(tx.QueryRowContext(ctx, writeIntentSelect+" WHERE id = ?", id))
	if err != nil {
		return Object{}, err
	}
	if intent.Operation != WriteOperationPut && intent.Operation != WriteOperationLegacyMultipart {
		return Object{}, ErrWriteIntentNotFound
	}
	if size < 0 || size > intent.ReservedBytes {
		return Object{}, ErrQuotaExceeded
	}
	var previous Object
	previous, err = scanObject(tx.QueryRowContext(ctx, objectSelect+" WHERE object_key = ? AND state = ?", intent.Key, StateCommitted))
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		return Object{}, err
	}
	if errors.Is(err, ErrObjectNotFound) {
		previous = Object{}
	}
	if previous.ObjectID != intent.PreviousObjectID {
		return Object{}, ErrWriteInProgress
	}
	now := time.Now()
	if previous.ObjectID != "" && previous.BucketID != intent.BucketID {
		_, err = tx.ExecContext(ctx, `INSERT INTO r2_physical_cleanups(
			id, object_key, physical_bucket_id, physical_key, expected_etag, size, status, error, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, 'pending', '', ?, ?)`, uuid.NewString(), previous.Key,
			previous.BucketID, previous.PhysicalKey, previous.ETag, previous.Size, now.UnixNano(), now.UnixNano())
		if err != nil {
			return Object{}, err
		}
	}
	delta := size
	if previous.ObjectID != "" && previous.BucketID == intent.BucketID {
		delta -= previous.Size
	}
	if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET storage_bytes = MAX(storage_bytes + ?, 0),
		reserved_storage_bytes = MAX(reserved_storage_bytes - ?, 0), updated_at = ? WHERE id = ?`,
		delta, intent.ReservedBytes, now.Unix(), intent.BucketID); err != nil {
		return Object{}, err
	}
	metadata, err := json.Marshal(intent.Metadata)
	if err != nil {
		return Object{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO r2_objects(
		object_key, object_id, physical_bucket_id, physical_key, state, size, etag, content_type,
		metadata_json, last_modified, error, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(object_key) DO UPDATE SET object_id = excluded.object_id,
		physical_bucket_id = excluded.physical_bucket_id, physical_key = excluded.physical_key,
		state = excluded.state, size = excluded.size, etag = excluded.etag, content_type = excluded.content_type,
		metadata_json = excluded.metadata_json, last_modified = excluded.last_modified, error = '', updated_at = excluded.updated_at`,
		intent.Key, intent.ID, intent.BucketID, intent.Key, StateCommitted, size, etag, intent.ContentType,
		string(metadata), now.UnixNano(), now.UnixNano(), now.UnixNano())
	if err != nil {
		return Object{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM r2_multipart_uploads WHERE write_intent_id = ?", intent.ID); err != nil {
		return Object{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM r2_write_intents WHERE id = ?", intent.ID); err != nil {
		return Object{}, err
	}
	if err := tx.Commit(); err != nil {
		return Object{}, err
	}
	return s.GetObject(ctx, intent.Key)
}

func (s *Store) CommitDeleteWrite(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	intent, err := scanWriteIntent(tx.QueryRowContext(ctx, writeIntentSelect+" WHERE id = ?", id))
	if err != nil {
		return err
	}
	if intent.Operation != WriteOperationDelete {
		return ErrWriteIntentNotFound
	}
	object, err := scanObject(tx.QueryRowContext(ctx, objectSelect+" WHERE object_key = ? AND state = ?", intent.Key, StateCommitted))
	if err != nil {
		return err
	}
	if object.ObjectID != intent.PreviousObjectID || object.BucketID != intent.BucketID {
		return ErrWriteInProgress
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM r2_objects WHERE object_key = ? AND object_id = ?`, object.Key, object.ObjectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE r2_physical_buckets SET storage_bytes = MAX(storage_bytes - ?, 0),
		updated_at = ? WHERE id = ?`, object.Size, time.Now().Unix(), object.BucketID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM r2_write_intents WHERE id = ?`, intent.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) HasActiveWriteForBucket(ctx context.Context, bucketID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM r2_write_intents WHERE target_bucket_id = ?", bucketID).Scan(&count)
	return count > 0, err
}

func (s *Store) AcquireBucketMaintenance(ctx context.Context, bucketID, operation string) error {
	if bucketID == "" || operation == "" {
		return errors.New("bucket id and maintenance operation are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_write_intents WHERE target_bucket_id = ?`, bucketID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return ErrBucketBusy
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO r2_bucket_maintenance_locks(physical_bucket_id, operation, created_at)
		VALUES(?, ?, ?)`, bucketID, operation, time.Now().Unix()); err != nil {
		return ErrBucketBusy
	}
	return tx.Commit()
}

func (s *Store) ReleaseBucketMaintenance(ctx context.Context, bucketID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM r2_bucket_maintenance_locks WHERE physical_bucket_id = ?`, bucketID)
	return err
}

func (s *Store) ClearBucketMaintenanceLocks(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM r2_bucket_maintenance_locks`)
	return err
}

func scanWriteIntent(row scanner) (WriteIntent, error) {
	var intent WriteIntent
	var previous sql.NullString
	var metadata string
	var created, updated int64
	if err := row.Scan(&intent.ID, &intent.Key, &intent.BucketID, &previous, &intent.ReservedBytes,
		&intent.DeclaredSize, &intent.ActualSize, &intent.ContentType, &metadata, &intent.State,
		&intent.Operation, &intent.UpstreamUploadID, &intent.ETag, &intent.InternalMultipart, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WriteIntent{}, ErrWriteIntentNotFound
		}
		return WriteIntent{}, err
	}
	intent.PreviousObjectID = previous.String
	if err := json.Unmarshal([]byte(metadata), &intent.Metadata); err != nil {
		return WriteIntent{}, err
	}
	intent.CreatedAt, intent.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
	return intent, nil
}

const writeIntentSelect = `SELECT id, object_key, target_bucket_id, previous_object_id, reserved_bytes,
	declared_size, actual_size, content_type, metadata_json, state, operation, upstream_upload_id, etag,
	internal_multipart, created_at, updated_at FROM r2_write_intents`

var (
	ErrWriteInProgress     = errors.New("object write is already in progress")
	ErrWriteIntentNotFound = errors.New("write intent not found")
	ErrBucketBusy          = errors.New("physical bucket has active writes")
)
