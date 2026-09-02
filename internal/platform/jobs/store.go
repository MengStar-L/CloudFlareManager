package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Job struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	ResourceKey string          `json:"resource_key,omitempty"`
	ParentJobID string          `json:"parent_job_id,omitempty"`
	Status      Status          `json:"status"`
	Payload     json.RawMessage `json:"payload"`
	Progress    float64         `json:"progress"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	Error       string          `json:"error,omitempty"`
	ErrorCode   string          `json:"error_code,omitempty"`
	LeaseUntil  *time.Time      `json:"lease_until,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Enqueue(ctx context.Context, jobType string, payload any, maxAttempts int) (Job, error) {
	job, _, err := s.enqueue(ctx, jobType, "", "", payload, maxAttempts)
	return job, err
}

func (s *Store) EnqueueUnique(
	ctx context.Context,
	jobType, resourceKey, parentJobID string,
	payload any,
	maxAttempts int,
) (Job, bool, error) {
	if resourceKey == "" {
		return Job{}, false, errors.New("job resource key is required")
	}
	return s.enqueue(ctx, jobType, resourceKey, parentJobID, payload, maxAttempts)
}

func (s *Store) enqueue(
	ctx context.Context,
	jobType, resourceKey, parentJobID string,
	payload any,
	maxAttempts int,
) (Job, bool, error) {
	if jobType == "" {
		return Job{}, false, errors.New("job type is required")
	}
	if maxAttempts <= 0 {
		maxAttempts = 4
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Job{}, false, fmt.Errorf("encode job payload: %w", err)
	}
	now := time.Now()
	job := Job{
		ID: uuid.NewString(), Type: jobType, ResourceKey: resourceKey, ParentJobID: parentJobID,
		Status: StatusPending, Payload: encoded, MaxAttempts: maxAttempts, CreatedAt: now, UpdatedAt: now,
	}
	var parent any
	if parentJobID != "" {
		parent = parentJobID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(
		id, type, resource_key, parent_job_id, status, payload_json, progress, attempts,
		max_attempts, error, error_code, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, 0, 0, ?, '', '', ?, ?)`, job.ID, job.Type, job.ResourceKey,
		parent, job.Status, string(job.Payload), job.MaxAttempts, job.CreatedAt.Unix(), job.UpdatedAt.Unix())
	if err != nil {
		if resourceKey != "" {
			existing, getErr := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobSelectColumns+` FROM jobs
				WHERE type = ? AND resource_key = ? AND status IN (?, ?)
				ORDER BY created_at, id LIMIT 1`, jobType, resourceKey, StatusPending, StatusRunning))
			if getErr == nil {
				if commitErr := tx.Commit(); commitErr != nil {
					return Job{}, false, commitErr
				}
				return existing, false, nil
			}
		}
		return Job{}, false, fmt.Errorf("enqueue job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func (s *Store) Claim(ctx context.Context, leaseDuration time.Duration) (*Job, error) {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `SELECT id FROM jobs
		WHERE (status = ? AND attempts < max_attempts AND (lease_until IS NULL OR lease_until <= ?)) OR
			(status = ? AND lease_until IS NOT NULL AND lease_until <= ?)
		ORDER BY created_at, id LIMIT 1`, StatusPending, now.Unix(), StatusRunning, now.Unix())
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, tx.Commit()
		}
		return nil, err
	}
	leaseUntil := now.Add(leaseDuration)
	// A reclaimed running job resumes the attempt that lost its lease. Capping
	// the counter keeps a crash during the final attempt recoverable.
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?,
		attempts = CASE WHEN attempts < max_attempts THEN attempts + 1 ELSE attempts END,
		lease_until = ?, updated_at = ? WHERE id = ?`, StatusRunning, leaseUntil.Unix(), now.Unix(), id)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, errors.New("job claim lost")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	job, err := s.Get(ctx, id)
	return &job, err
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobSelectColumns+` FROM jobs WHERE id = ?`, id))
}

func (s *Store) List(ctx context.Context, limit int, status Status) ([]Job, error) {
	return s.ListFiltered(ctx, limit, status, "", "")
}

func (s *Store) ListFiltered(ctx context.Context, limit int, status Status, jobType, resourceKeyPrefix string) ([]Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + jobSelectColumns + ` FROM jobs WHERE 1 = 1`
	args := []any{}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	if jobType != "" {
		query += " AND type = ?"
		args = append(args, jobType)
	}
	if resourceKeyPrefix != "" {
		query += " AND substr(resource_key, 1, length(?)) = ?"
		args = append(args, resourceKeyPrefix, resourceKeyPrefix)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func (s *Store) SetProgress(ctx context.Context, id string, progress float64) error {
	if progress < 0 || progress > 1 {
		return errors.New("job progress must be between 0 and 1")
	}
	return s.update(ctx, `UPDATE jobs SET progress = ?, updated_at = ? WHERE id = ? AND status = ?`,
		progress, time.Now().Unix(), id, StatusRunning)
}

// SetPayload persists resumable job state while a worker owns the job.
func (s *Store) SetPayload(ctx context.Context, id string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode job payload: %w", err)
	}
	return s.update(ctx, `UPDATE jobs SET payload_json = ?, updated_at = ? WHERE id = ? AND status = ?`,
		string(encoded), time.Now().Unix(), id, StatusRunning)
}

func (s *Store) RenewLease(ctx context.Context, id string, duration time.Duration) error {
	if duration <= 0 {
		return errors.New("lease duration must be positive")
	}
	now := time.Now()
	return s.update(ctx, `UPDATE jobs SET lease_until = ?, updated_at = ? WHERE id = ? AND status = ?`,
		now.Add(duration).Unix(), now.Unix(), id, StatusRunning)
}

func (s *Store) Complete(ctx context.Context, id string) error {
	return s.update(ctx, `UPDATE jobs SET status = ?, progress = 1, error = '', error_code = '', lease_until = NULL,
		updated_at = ? WHERE id = ? AND status = ?`, StatusSucceeded, time.Now().Unix(), id, StatusRunning)
}

func (s *Store) Fail(ctx context.Context, id, code, message string, retryAt time.Time) error {
	job, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	status := StatusPending
	var lease any = retryAt.Unix()
	if job.Attempts >= job.MaxAttempts {
		status = StatusFailed
		lease = nil
	}
	return s.update(ctx, `UPDATE jobs SET status = ?, error = ?, error_code = ?, lease_until = ?, updated_at = ?
		WHERE id = ? AND status = ?`, status, message, code, lease, time.Now().Unix(), id, StatusRunning)
}

func (s *Store) FailPermanent(ctx context.Context, id, code, message string) error {
	return s.update(ctx, `UPDATE jobs SET status = ?, error = ?, error_code = ?, lease_until = NULL,
		updated_at = ? WHERE id = ? AND status = ?`, StatusFailed, message, code, time.Now().Unix(), id, StatusRunning)
}

func (s *Store) update(ctx context.Context, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return nil
}

type scanner interface {
	Scan(...any) error
}

const jobSelectColumns = `id, type, resource_key, parent_job_id, status, payload_json, progress,
	attempts, max_attempts, error, error_code, lease_until, created_at, updated_at`

func scanJob(row scanner) (Job, error) {
	var job Job
	var payload string
	var parent sql.NullString
	var lease sql.NullInt64
	var created, updated int64
	if err := row.Scan(&job.ID, &job.Type, &job.ResourceKey, &parent, &job.Status, &payload, &job.Progress,
		&job.Attempts, &job.MaxAttempts, &job.Error, &job.ErrorCode, &lease, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNotFound
		}
		return Job{}, err
	}
	job.Payload = json.RawMessage(payload)
	if parent.Valid {
		job.ParentJobID = parent.String
	}
	job.CreatedAt = time.Unix(created, 0)
	job.UpdatedAt = time.Unix(updated, 0)
	if lease.Valid {
		value := time.Unix(lease.Int64, 0)
		job.LeaseUntil = &value
	}
	return job, nil
}

var ErrNotFound = errors.New("job not found")
