package r2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type ObjectState string

const (
	StatePending   ObjectState = "pending"
	StateCommitted ObjectState = "committed"
	StateDeleting  ObjectState = "deleting"
	StateError     ObjectState = "error"
)

type Limits struct {
	StorageBytes        int64
	AccountStorageBytes int64
	ClassA              int64
	ClassB              int64
}

type PhysicalBucket struct {
	ID             string               `json:"id"`
	AccountID      string               `json:"account_id"`
	Name           string               `json:"name"`
	Writable       bool                 `json:"writable"`
	Adopted        bool                 `json:"adopted"`
	StorageBytes   int64                `json:"storage_bytes"`
	ReservedBytes  int64                `json:"reserved_storage_bytes"`
	ClassAOps      int64                `json:"class_a_ops"`
	ClassBOps      int64                `json:"class_b_ops"`
	LatencyMS      float64              `json:"latency_ms"`
	HealthStatus   string               `json:"health_status"`
	LifecycleState BucketLifecycleState `json:"lifecycle_state"`
	DeletionJobID  string               `json:"deletion_job_id,omitempty"`
	OverflowUntil  *time.Time           `json:"allow_overflow_until,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	UsageCheckedAt time.Time            `json:"usage_checked_at"`
}

type CreateBucketInput struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Adopted   bool   `json:"adopted"`
}

type Object struct {
	Key          string            `json:"key"`
	ObjectID     string            `json:"object_id"`
	BucketID     string            `json:"physical_bucket_id"`
	PhysicalKey  string            `json:"physical_key"`
	State        ObjectState       `json:"state"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	ContentType  string            `json:"content_type"`
	Metadata     map[string]string `json:"metadata"`
	LastModified time.Time         `json:"last_modified"`
	Error        string            `json:"error,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

type ListOptions struct {
	Prefix string
	After  string
	Limit  int
}

type ObjectList struct {
	Objects    []Object `json:"objects"`
	NextMarker string   `json:"next_marker,omitempty"`
}

type BucketObjectStats struct {
	StorageBytes int64
	ObjectCount  int64
}

type Store struct {
	db              *sql.DB
	limits          Limits
	policy          PlacementPolicy
	writeActivity   sync.Mutex
	writeActivities map[string]*keyWriteActivity
}

type keyWriteActivity struct {
	mutex      sync.Mutex
	references int
}

func NewStore(db *sql.DB, limits Limits) *Store {
	if limits.AccountStorageBytes <= 0 {
		limits.AccountStorageBytes = limits.StorageBytes
	}
	return &Store{
		db: db, limits: limits, policy: PlacementPolicy{SoftLimit: 1},
		writeActivities: make(map[string]*keyWriteActivity),
	}
}

func (s *Store) beginWriteActivity(key string) func() {
	if s == nil || key == "" {
		return func() {}
	}
	activity := s.referenceWriteActivity(key)
	activity.mutex.Lock()
	return func() {
		activity.mutex.Unlock()
		s.unreferenceWriteActivity(key, activity)
	}
}

func (s *Store) tryBeginWriteActivity(key string) (func(), bool) {
	if s == nil || key == "" {
		return func() {}, true
	}
	activity := s.referenceWriteActivity(key)
	if !activity.mutex.TryLock() {
		s.unreferenceWriteActivity(key, activity)
		return nil, false
	}
	return func() {
		activity.mutex.Unlock()
		s.unreferenceWriteActivity(key, activity)
	}, true
}

func (s *Store) referenceWriteActivity(key string) *keyWriteActivity {
	s.writeActivity.Lock()
	defer s.writeActivity.Unlock()
	if s.writeActivities == nil {
		s.writeActivities = make(map[string]*keyWriteActivity)
	}
	activity := s.writeActivities[key]
	if activity == nil {
		activity = &keyWriteActivity{}
		s.writeActivities[key] = activity
	}
	activity.references++
	return activity
}

func (s *Store) unreferenceWriteActivity(key string, activity *keyWriteActivity) {
	s.writeActivity.Lock()
	defer s.writeActivity.Unlock()
	activity.references--
	if activity.references == 0 && s.writeActivities[key] == activity {
		delete(s.writeActivities, key)
	}
}

func (s *Store) CreateBucket(ctx context.Context, input CreateBucketInput) (PhysicalBucket, error) {
	if input.AccountID == "" || input.Name == "" {
		return PhysicalBucket{}, errors.New("account_id and name are required")
	}
	now := time.Now()
	bucket := PhysicalBucket{
		ID: uuid.NewString(), AccountID: input.AccountID, Name: input.Name,
		Writable: true, Adopted: input.Adopted, HealthStatus: "healthy", LifecycleState: BucketActive,
		CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO r2_physical_buckets(
		id, account_id, bucket_name, writable, adopted, health_status, created_at, updated_at, usage_checked_at)
		VALUES(?, ?, ?, 1, ?, ?, ?, ?, ?)`, bucket.ID, bucket.AccountID, bucket.Name, bucket.Adopted,
		bucket.HealthStatus, now.Unix(), now.Unix(), 0)
	if err != nil {
		if strings.Contains(err.Error(), "r2_remote_bucket_deletion_active") {
			return PhysicalBucket{}, ErrBucketDeleting
		}
		return PhysicalBucket{}, fmt.Errorf("create physical bucket: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO r2_account_capacity(account_id, unmanaged_storage_bytes, usage_checked_at, updated_at)
		VALUES(?, 0, ?, ?) ON CONFLICT(account_id) DO NOTHING`, bucket.AccountID, now.Unix(), now.Unix())
	if err != nil {
		return PhysicalBucket{}, fmt.Errorf("initialize account capacity: %w", err)
	}
	return bucket, nil
}

func (s *Store) GetBucket(ctx context.Context, id string) (PhysicalBucket, error) {
	return scanBucket(s.db.QueryRowContext(ctx, bucketSelect+" WHERE id = ?", id))
}

func (s *Store) ListBuckets(ctx context.Context) ([]PhysicalBucket, error) {
	rows, err := s.db.QueryContext(ctx, bucketSelect+" ORDER BY account_id, bucket_name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var buckets []PhysicalBucket
	for rows.Next() {
		bucket, err := scanBucket(rows)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

func (s *Store) ListBucketObjectStats(ctx context.Context) (map[string]BucketObjectStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT physical_bucket_id, COALESCE(SUM(size), 0), COUNT(*)
		FROM r2_objects WHERE state = ? GROUP BY physical_bucket_id`, StateCommitted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := make(map[string]BucketObjectStats)
	for rows.Next() {
		var bucketID string
		var item BucketObjectStats
		if err := rows.Scan(&bucketID, &item.StorageBytes, &item.ObjectCount); err != nil {
			return nil, err
		}
		stats[bucketID] = item
	}
	return stats, rows.Err()
}

func (s *Store) DeleteBucket(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	bucket, err := scanBucket(tx.QueryRowContext(ctx, bucketSelect+" WHERE id = ?", id))
	if err != nil {
		return err
	}
	if bucket.LifecycleState != BucketActive {
		return ErrBucketDeleting
	}
	var referenced int
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM r2_objects WHERE physical_bucket_id = ?)
		OR EXISTS(SELECT 1 FROM r2_multipart_uploads WHERE physical_bucket_id = ?)
		OR EXISTS(SELECT 1 FROM r2_write_intents AS wi WHERE wi.target_bucket_id = ? OR EXISTS(
			SELECT 1 FROM r2_objects AS previous
			WHERE previous.object_id = wi.previous_object_id AND previous.physical_bucket_id = ?
		))
		OR EXISTS(SELECT 1 FROM r2_physical_cleanups WHERE physical_bucket_id = ?)`,
		id, id, id, id, id).Scan(&referenced); err != nil {
		return fmt.Errorf("check physical bucket references: %w", err)
	}
	if referenced != 0 {
		return ErrBucketInUse
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM r2_physical_buckets WHERE id = ? AND lifecycle_state = ?", id, BucketActive)
	if err != nil {
		return fmt.Errorf("delete physical bucket: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBucketNotFound
	}
	return tx.Commit()
}

func (s *Store) ReservePut(ctx context.Context, input ObjectInput) (Object, error) {
	if input.Key == "" || input.Size < 0 {
		return Object{}, errors.New("object key is required and size cannot be negative")
	}
	selected, err := s.selectBucket(ctx, input)
	if err != nil {
		return Object{}, err
	}
	now := time.Now()
	object := Object{
		Key: input.Key, ObjectID: uuid.NewString(), BucketID: selected.ID,
		PhysicalKey: input.Key, State: StatePending, Size: input.Size, ContentType: input.ContentType,
		Metadata: cloneMetadata(input.Metadata), LastModified: now, CreatedAt: now, UpdatedAt: now,
	}
	metadata, _ := json.Marshal(object.Metadata)
	_, err = s.db.ExecContext(ctx, `INSERT INTO r2_objects(
		object_key, object_id, physical_bucket_id, physical_key, state, size, etag, content_type,
		metadata_json, last_modified, error, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, '', ?, ?, ?, '', ?, ?)
		ON CONFLICT(object_key) DO UPDATE SET object_id = excluded.object_id,
		physical_bucket_id = excluded.physical_bucket_id, physical_key = excluded.physical_key,
		state = excluded.state, size = excluded.size, etag = '', content_type = excluded.content_type,
		metadata_json = excluded.metadata_json, last_modified = excluded.last_modified, error = '',
		updated_at = excluded.updated_at`, object.Key, object.ObjectID, object.BucketID, object.PhysicalKey,
		object.State, object.Size, object.ContentType, string(metadata), now.UnixNano(), now.UnixNano(), now.UnixNano())
	if err != nil {
		return Object{}, fmt.Errorf("reserve object: %w", err)
	}
	return object, nil
}

func (s *Store) selectBucket(ctx context.Context, input ObjectInput) (Candidate, error) {
	buckets, err := s.ListBuckets(ctx)
	if err != nil {
		return Candidate{}, err
	}
	rules, err := s.listRules(ctx)
	if err != nil {
		return Candidate{}, err
	}
	requestedSize := input.Size
	if requestedSize < 0 {
		requestedSize = 0
	}
	candidates := make([]Candidate, 0, len(buckets))
	for _, bucket := range buckets {
		candidates = append(candidates, Candidate{
			ID: bucket.ID, Healthy: bucket.HealthStatus == "healthy",
			Writable:     bucket.Writable && bucket.LifecycleState == BucketActive,
			StorageRatio: ratio(bucket.StorageBytes+requestedSize, s.limits.StorageBytes),
			ClassARatio:  ratio(bucket.ClassAOps+1, s.limits.ClassA), ClassBRatio: ratio(bucket.ClassBOps, s.limits.ClassB),
			LatencyRatio: latencyRatio(bucket.LatencyMS), AllowOverflow: bucket.OverflowUntil != nil && bucket.OverflowUntil.After(time.Now()),
		})
	}
	selected, err := s.policy.Select(input, candidates, rules)
	if err != nil {
		return Candidate{}, err
	}
	return selected, nil
}

func r2CredentialAccounts(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (map[string]bool, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT id FROM accounts
		WHERE r2_access_key_id_secret_id IS NOT NULL AND r2_secret_access_key_secret_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = true
	}
	return result, rows.Err()
}

func (s *Store) CommitPut(ctx context.Context, objectID, etag string, size int64) error {
	if !validObjectETag(etag) {
		return ErrObjectETagUnavailable
	}
	result, err := s.db.ExecContext(ctx, `UPDATE r2_objects SET state = ?, etag = ?, size = ?,
		last_modified = ?, error = '', updated_at = ? WHERE object_id = ? AND state = ?`,
		StateCommitted, etag, size, time.Now().UnixNano(), time.Now().UnixNano(), objectID, StatePending)
	return objectUpdateResult(result, err)
}

func (s *Store) FailPut(ctx context.Context, objectID, message string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_objects SET state = ?, error = ?, updated_at = ?
		WHERE object_id = ? AND state = ?`, StateError, message, time.Now().UnixNano(), objectID, StatePending)
	return objectUpdateResult(result, err)
}

func (s *Store) GetObject(ctx context.Context, key string) (Object, error) {
	return scanObject(s.db.QueryRowContext(ctx, objectSelect+" WHERE object_key = ? AND state = ?", key, StateCommitted))
}

func (s *Store) GetObjectByID(ctx context.Context, objectID string) (Object, error) {
	return scanObject(s.db.QueryRowContext(ctx, objectSelect+" WHERE object_id = ?", objectID))
}

// BackfillObjectETag repairs legacy committed rows without overwriting a value
// written by another request or attaching the ETag to a replacement object.
func (s *Store) BackfillObjectETag(ctx context.Context, objectID, etag string) (Object, error) {
	object, err := s.GetObjectByID(ctx, objectID)
	if err != nil {
		return Object{}, err
	}
	return s.BackfillObjectMetadata(ctx, objectID, object.ETag, object.Size, etag, object.Size)
}

// BackfillObjectMetadata repairs a legacy committed row after the caller has
// verified the remote object's identity. The expected size fences concurrent
// replacement, and storage accounting changes atomically with the object row.
func (s *Store) BackfillObjectMetadata(
	ctx context.Context,
	objectID string,
	expectedETag string,
	expectedSize int64,
	etag string,
	size int64,
) (Object, error) {
	if objectID == "" {
		return Object{}, ErrObjectNotFound
	}
	if expectedSize < 0 || size < 0 {
		return Object{}, ErrConditionalRequestConflict
	}
	normalizedETag, valid := normalizeObjectETag(etag)
	if !valid {
		return Object{}, ErrObjectETagUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Object{}, err
	}
	defer tx.Rollback()
	current, err := scanObject(tx.QueryRowContext(ctx, objectSelect+" WHERE object_id = ? AND state = ?", objectID, StateCommitted))
	if err != nil {
		return Object{}, err
	}
	if validObjectETag(current.ETag) {
		currentETag, _ := normalizeObjectETag(current.ETag)
		if currentETag != normalizedETag || current.Size != size {
			return Object{}, ErrConditionalRequestConflict
		}
		if err := tx.Commit(); err != nil {
			return Object{}, err
		}
		return current, nil
	}
	if current.ETag != expectedETag || current.Size != expectedSize {
		return Object{}, ErrConditionalRequestConflict
	}
	now := time.Now()
	result, err := tx.ExecContext(ctx, `UPDATE r2_objects SET etag = ?, size = ?, updated_at = ?
		WHERE object_id = ? AND state = ? AND etag = ? AND size = ?`,
		normalizedETag, size, now.UnixNano(), objectID, StateCommitted, expectedETag, expectedSize)
	if err != nil {
		return Object{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Object{}, ErrConditionalRequestConflict
	}
	var indexedBytes, cleanupBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM r2_objects
		WHERE physical_bucket_id = ? AND state = ?`, current.BucketID, StateCommitted).Scan(&indexedBytes); err != nil {
		return Object{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM r2_physical_cleanups
		WHERE physical_bucket_id = ?`, current.BucketID).Scan(&cleanupBytes); err != nil {
		return Object{}, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE r2_physical_buckets
		SET storage_bytes = MAX(storage_bytes, ?), updated_at = ? WHERE id = ?`,
		indexedBytes+cleanupBytes, now.Unix(), current.BucketID)
	if err != nil {
		return Object{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Object{}, ErrBucketNotFound
	}
	object, err := scanObject(tx.QueryRowContext(ctx, objectSelect+" WHERE object_id = ? AND state = ?", objectID, StateCommitted))
	if err != nil {
		return Object{}, err
	}
	if !validObjectETag(object.ETag) {
		return Object{}, ErrObjectETagUnavailable
	}
	if err := tx.Commit(); err != nil {
		return Object{}, err
	}
	return object, nil
}

func (s *Store) BeginDelete(ctx context.Context, key string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_objects SET state = ?, updated_at = ?
		WHERE object_key = ? AND state = ?`, StateDeleting, time.Now().UnixNano(), key, StateCommitted)
	return objectUpdateResult(result, err)
}

func (s *Store) FailDelete(ctx context.Context, key, message string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE r2_objects SET state = ?, error = ?, updated_at = ?
		WHERE object_key = ? AND state = ?`, StateError, message, time.Now().UnixNano(), key, StateDeleting)
	return objectUpdateResult(result, err)
}

func (s *Store) CompleteDelete(ctx context.Context, key string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM r2_objects WHERE object_key = ? AND state = ?", key, StateDeleting)
	return objectUpdateResult(result, err)
}

func (s *Store) ListObjects(ctx context.Context, options ListOptions) (ObjectList, error) {
	if options.Limit <= 0 || options.Limit > 1000 {
		options.Limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, objectSelect+` WHERE state = ? AND object_key LIKE ? ESCAPE '\'
		AND object_key > ? ORDER BY object_key LIMIT ?`, StateCommitted, escapeLike(options.Prefix)+"%", options.After, options.Limit+1)
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
	if len(objects) > options.Limit {
		result.NextMarker = objects[options.Limit-1].Key
		result.Objects = objects[:options.Limit]
	}
	return result, nil
}

func (s *Store) listRules(ctx context.Context) ([]PlacementRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT prefix, extension, content_type, min_size, max_size, target_bucket_id
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

const bucketSelect = `SELECT id, account_id, bucket_name, writable, adopted, storage_bytes,
	reserved_storage_bytes, class_a_ops, class_b_ops, latency_ms, health_status, allow_overflow_until,
	created_at, updated_at, usage_checked_at, lifecycle_state, deletion_job_id
	FROM r2_physical_buckets`

const objectSelect = `SELECT object_key, object_id, physical_bucket_id, physical_key, state, size,
	etag, content_type, metadata_json, last_modified, error, created_at, updated_at FROM r2_objects`

type scanner interface {
	Scan(...any) error
}

func scanBucket(row scanner) (PhysicalBucket, error) {
	var bucket PhysicalBucket
	var overflow sql.NullInt64
	var deletionJobID sql.NullString
	var created, updated, usageChecked int64
	if err := row.Scan(&bucket.ID, &bucket.AccountID, &bucket.Name, &bucket.Writable, &bucket.Adopted,
		&bucket.StorageBytes, &bucket.ReservedBytes, &bucket.ClassAOps, &bucket.ClassBOps, &bucket.LatencyMS, &bucket.HealthStatus,
		&overflow, &created, &updated, &usageChecked, &bucket.LifecycleState, &deletionJobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PhysicalBucket{}, ErrBucketNotFound
		}
		return PhysicalBucket{}, err
	}
	bucket.CreatedAt, bucket.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	if usageChecked > 0 {
		bucket.UsageCheckedAt = time.Unix(usageChecked, 0)
	}
	if overflow.Valid {
		value := time.Unix(overflow.Int64, 0)
		bucket.OverflowUntil = &value
	}
	bucket.DeletionJobID = deletionJobID.String
	return bucket, nil
}

func scanObject(row scanner) (Object, error) {
	var object Object
	var metadata string
	var modified, created, updated int64
	if err := row.Scan(&object.Key, &object.ObjectID, &object.BucketID, &object.PhysicalKey, &object.State,
		&object.Size, &object.ETag, &object.ContentType, &metadata, &modified, &object.Error, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Object{}, ErrObjectNotFound
		}
		return Object{}, err
	}
	if err := json.Unmarshal([]byte(metadata), &object.Metadata); err != nil {
		return Object{}, err
	}
	object.LastModified = time.Unix(0, modified)
	object.CreatedAt = time.Unix(0, created)
	object.UpdatedAt = time.Unix(0, updated)
	return object, nil
}

func objectUpdateResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrObjectNotFound
	}
	return nil
}

func ratio(value, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	return float64(value) / float64(limit)
}

func latencyRatio(milliseconds float64) float64 {
	if milliseconds <= 0 {
		return 0
	}
	if milliseconds >= 2000 {
		return 1
	}
	return milliseconds / 2000
}

func escapeLike(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return replacer.Replace(value)
}

func cloneMetadata(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func validObjectETag(value string) bool {
	_, valid := normalizeObjectETag(value)
	return valid
}

func normalizeObjectETag(value string) (string, bool) {
	normalized := strings.Trim(strings.TrimSpace(value), `"`)
	return normalized, normalized != "" && !strings.HasPrefix(strings.ToLower(normalized), "w/")
}

var (
	ErrBucketNotFound        = errors.New("physical bucket not found")
	ErrObjectNotFound        = errors.New("object not found")
	ErrObjectETagUnavailable = errors.New("object ETag is unavailable")
)
