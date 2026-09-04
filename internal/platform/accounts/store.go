package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
	"github.com/google/uuid"
)

type Account struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	CloudflareAccountID string       `json:"cloudflare_account_id"`
	Enabled             bool         `json:"enabled"`
	HealthStatus        string       `json:"health_status"`
	HealthError         string       `json:"health_error,omitempty"`
	Capabilities        []Capability `json:"capabilities,omitempty"`
	APIToken            string       `json:"-"`
	R2AccessKeyID       string       `json:"-"`
	R2SecretAccessKey   string       `json:"-"`
	HasR2Credentials    bool         `json:"has_r2_credentials"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
	apiTokenSecretID    string
	r2AccessSecretID    sql.NullString
	r2SecretSecretID    sql.NullString
}

type Capability struct {
	Name      string    `json:"name"`
	Available bool      `json:"available"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

type CreateInput struct {
	Name                string `json:"name"`
	CloudflareAccountID string `json:"cloudflare_account_id"`
	APIToken            string `json:"api_token"`
	R2AccessKeyID       string `json:"r2_access_key_id,omitempty"`
	R2SecretAccessKey   string `json:"r2_secret_access_key,omitempty"`
}

type UpdateCredentialsInput struct {
	APIToken           *string `json:"api_token,omitempty"`
	R2AccessKeyID      *string `json:"r2_access_key_id,omitempty"`
	R2SecretAccessKey  *string `json:"r2_secret_access_key,omitempty"`
	ClearR2Credentials bool    `json:"clear_r2_credentials,omitempty"`
}

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

const (
	DeletionBlockerR2Bucket      = "r2_bucket"
	DeletionBlockerR2DeletionJob = "r2_bucket_deletion_job"
	deletionBlockerPreviewLimit  = 5
)

type DeletionBlockerItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
}

type DeletionBlocker struct {
	Kind      string                `json:"kind"`
	Count     int                   `json:"count"`
	Items     []DeletionBlockerItem `json:"items,omitempty"`
	Truncated bool                  `json:"truncated,omitempty"`
}

type AccountInUseError struct {
	Blockers []DeletionBlocker
}

func (e *AccountInUseError) Error() string { return ErrAccountInUse.Error() }
func (e *AccountInUseError) Unwrap() error { return ErrAccountInUse }

type Store struct {
	db      *sql.DB
	secrets *secret.Repository
	mu      sync.RWMutex
}

func NewStore(db *sql.DB, secrets *secret.Repository) *Store {
	return &Store{db: db, secrets: secrets}
}

func (s *Store) Create(ctx context.Context, input CreateInput) (Account, error) {
	if input.Name == "" || input.CloudflareAccountID == "" || input.APIToken == "" {
		return Account{}, errors.New("name, cloudflare_account_id, and api_token are required")
	}
	id := uuid.NewString()
	scope := "account:" + id
	apiID, err := s.secrets.Put(ctx, scope, "cloudflare_api_token", input.APIToken)
	if err != nil {
		return Account{}, err
	}
	cleanup := []string{apiID}
	var accessID, secretID sql.NullString
	if input.R2AccessKeyID != "" || input.R2SecretAccessKey != "" {
		if input.R2AccessKeyID == "" || input.R2SecretAccessKey == "" {
			s.secrets.Delete(ctx, apiID)
			return Account{}, errors.New("both R2 access key and secret access key are required")
		}
		value, err := s.secrets.Put(ctx, scope, "r2_access_key_id", input.R2AccessKeyID)
		if err != nil {
			s.secrets.Delete(ctx, apiID)
			return Account{}, err
		}
		cleanup = append(cleanup, value)
		accessID = sql.NullString{String: value, Valid: true}
		value, err = s.secrets.Put(ctx, scope, "r2_secret_access_key", input.R2SecretAccessKey)
		if err != nil {
			for _, secretID := range cleanup {
				s.secrets.Delete(ctx, secretID)
			}
			return Account{}, err
		}
		cleanup = append(cleanup, value)
		secretID = sql.NullString{String: value, Valid: true}
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `INSERT INTO accounts(
		id, name, cloudflare_account_id, api_token_secret_id, r2_access_key_id_secret_id,
		r2_secret_access_key_secret_id, enabled, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, 1, ?, ?)`, id, input.Name, input.CloudflareAccountID, apiID, accessID, secretID, now, now)
	if err != nil {
		for _, secretID := range cleanup {
			s.secrets.Delete(ctx, secretID)
		}
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	return s.Get(ctx, id, false)
}

func (s *Store) Get(ctx context.Context, id string, includeSecrets bool) (Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.get(ctx, id, includeSecrets)
}

func (s *Store) get(ctx context.Context, id string, includeSecrets bool) (Account, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT id, name, cloudflare_account_id, enabled,
		health_status, health_error, api_token_secret_id, r2_access_key_id_secret_id,
		r2_secret_access_key_secret_id, created_at, updated_at FROM accounts WHERE id = ?`, id))
	if err != nil {
		return Account{}, err
	}
	account.Capabilities, err = s.listCapabilities(ctx, id)
	if err != nil {
		return Account{}, err
	}
	if includeSecrets {
		if account.APIToken, err = s.secrets.Get(ctx, account.apiTokenSecretID); err != nil {
			return Account{}, err
		}
		if account.r2AccessSecretID.Valid {
			if account.R2AccessKeyID, err = s.secrets.Get(ctx, account.r2AccessSecretID.String); err != nil {
				return Account{}, err
			}
		}
		if account.r2SecretSecretID.Valid {
			if account.R2SecretAccessKey, err = s.secrets.Get(ctx, account.r2SecretSecretID.String); err != nil {
				return Account{}, err
			}
		}
	}
	return account, nil
}

func (s *Store) List(ctx context.Context) ([]Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, cloudflare_account_id, enabled,
		health_status, health_error, api_token_secret_id, r2_access_key_id_secret_id,
		r2_secret_access_key_secret_id, created_at, updated_at FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		account.Capabilities, err = s.listCapabilities(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, account)
	}
	return result, rows.Err()
}

func (s *Store) UpdateCredentials(ctx context.Context, id string, input UpdateCredentialsInput) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, err := s.get(ctx, id, false)
	if err != nil {
		return Account{}, err
	}
	replaceAPI := input.APIToken != nil
	replaceR2 := input.R2AccessKeyID != nil || input.R2SecretAccessKey != nil
	if !replaceAPI && !replaceR2 && !input.ClearR2Credentials {
		return Account{}, &ValidationError{Message: "at least one credential change is required"}
	}
	if replaceAPI && strings.TrimSpace(*input.APIToken) == "" {
		return Account{}, &ValidationError{Message: "api_token cannot be empty"}
	}
	if input.ClearR2Credentials && replaceR2 {
		return Account{}, &ValidationError{Message: "R2 credentials cannot be replaced and removed at the same time"}
	}
	if replaceR2 {
		if input.R2AccessKeyID == nil || input.R2SecretAccessKey == nil ||
			strings.TrimSpace(*input.R2AccessKeyID) == "" || strings.TrimSpace(*input.R2SecretAccessKey) == "" {
			return Account{}, &ValidationError{Message: "both R2 access key and secret access key are required"}
		}
	}

	scope := "account:" + id
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	var newAPISecretID string
	var newR2AccessSecretID, newR2SecretSecretID sql.NullString
	if replaceAPI {
		newAPISecretID, err = s.secrets.PutTx(ctx, tx, scope, "cloudflare_api_token", *input.APIToken)
		if err != nil {
			return Account{}, err
		}
	}
	if replaceR2 {
		value, putErr := s.secrets.PutTx(ctx, tx, scope, "r2_access_key_id", *input.R2AccessKeyID)
		if putErr != nil {
			return Account{}, putErr
		}
		newR2AccessSecretID = sql.NullString{String: value, Valid: true}
		value, putErr = s.secrets.PutTx(ctx, tx, scope, "r2_secret_access_key", *input.R2SecretAccessKey)
		if putErr != nil {
			return Account{}, putErr
		}
		newR2SecretSecretID = sql.NullString{String: value, Valid: true}
	}

	sets := make([]string, 0, 7)
	args := make([]any, 0, 8)
	if replaceAPI {
		sets = append(sets, "api_token_secret_id = ?", "health_status = 'unknown'", "health_error = ''")
		args = append(args, newAPISecretID)
	}
	if replaceR2 {
		sets = append(sets, "r2_access_key_id_secret_id = ?", "r2_secret_access_key_secret_id = ?")
		args = append(args, newR2AccessSecretID, newR2SecretSecretID)
	} else if input.ClearR2Credentials {
		sets = append(sets, "r2_access_key_id_secret_id = NULL", "r2_secret_access_key_secret_id = NULL")
	}
	updatedAt := time.Now().Unix()
	sets = append(sets, "updated_at = ?")
	args = append(args, updatedAt, id)

	result, err := tx.ExecContext(ctx, "UPDATE accounts SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return Account{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Account{}, ErrNotFound
	}
	if replaceAPI {
		if _, err := tx.ExecContext(ctx, "DELETE FROM account_capabilities WHERE account_id = ?", id); err != nil {
			return Account{}, err
		}
	}
	if replaceAPI {
		if err := s.secrets.DeleteTx(ctx, tx, account.apiTokenSecretID); err != nil {
			return Account{}, err
		}
	}
	if replaceR2 || input.ClearR2Credentials {
		for _, secretID := range []sql.NullString{account.r2AccessSecretID, account.r2SecretSecretID} {
			if secretID.Valid {
				if err := s.secrets.DeleteTx(ctx, tx, secretID.String); err != nil {
					return Account{}, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}

	if replaceAPI {
		account.apiTokenSecretID = newAPISecretID
		account.HealthStatus = "unknown"
		account.HealthError = ""
		account.Capabilities = nil
	}
	if replaceR2 || input.ClearR2Credentials {
		account.r2AccessSecretID = newR2AccessSecretID
		account.r2SecretSecretID = newR2SecretSecretID
		account.HasR2Credentials = replaceR2
	}
	account.UpdatedAt = time.Unix(updatedAt, 0)
	return account, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	account, err := scanAccount(tx.QueryRowContext(ctx, `SELECT id, name, cloudflare_account_id, enabled,
		health_status, health_error, api_token_secret_id, r2_access_key_id_secret_id,
		r2_secret_access_key_secret_id, created_at, updated_at FROM accounts WHERE id = ?`, id))
	if err != nil {
		return err
	}
	blockers, err := accountDeletionBlockers(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("check account deletion blockers: %w", err)
	}
	if len(blockers) != 0 {
		return &AccountInUseError{Blockers: blockers}
	}

	// AI request logs are account-owned history, like the other account usage
	// tables. The original schema omitted ON DELETE CASCADE for this one table.
	if _, err := tx.ExecContext(ctx, "DELETE FROM ai_request_logs WHERE account_id = ?", id); err != nil {
		return fmt.Errorf("delete account AI request logs: %w", err)
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	for _, secretID := range []sql.NullString{{String: account.apiTokenSecretID, Valid: true}, account.r2AccessSecretID, account.r2SecretSecretID} {
		if secretID.Valid {
			if err := s.secrets.DeleteTx(ctx, tx, secretID.String); err != nil {
				return fmt.Errorf("delete account secret: %w", err)
			}
		}
	}
	return tx.Commit()
}

func accountDeletionBlockers(ctx context.Context, tx *sql.Tx, accountID string) ([]DeletionBlocker, error) {
	blockers := make([]DeletionBlocker, 0, 2)

	var bucketCount int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM r2_physical_buckets WHERE account_id = ?", accountID).Scan(&bucketCount); err != nil {
		return nil, err
	}
	if bucketCount > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT id, bucket_name, lifecycle_state
			FROM r2_physical_buckets WHERE account_id = ? ORDER BY bucket_name, id LIMIT ?`,
			accountID, deletionBlockerPreviewLimit)
		if err != nil {
			return nil, err
		}
		items := make([]DeletionBlockerItem, 0, min(bucketCount, deletionBlockerPreviewLimit))
		for rows.Next() {
			var item DeletionBlockerItem
			if err := rows.Scan(&item.ID, &item.Name, &item.Status); err != nil {
				rows.Close()
				return nil, err
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		blockers = append(blockers, DeletionBlocker{
			Kind: DeletionBlockerR2Bucket, Count: bucketCount, Items: items,
			Truncated: bucketCount > len(items),
		})
	}

	const activeDeletionWhere = `type = 'r2.bucket.delete-remote'
		AND status IN ('pending', 'running')
		AND substr(resource_key, 1, length(?) + 1) = ? || '/'`
	var deletionJobCount int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE "+activeDeletionWhere, accountID, accountID).Scan(&deletionJobCount); err != nil {
		return nil, err
	}
	if deletionJobCount > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT id, resource_key, status FROM jobs WHERE `+activeDeletionWhere+`
			ORDER BY created_at, id LIMIT ?`, accountID, accountID, deletionBlockerPreviewLimit)
		if err != nil {
			return nil, err
		}
		items := make([]DeletionBlockerItem, 0, min(deletionJobCount, deletionBlockerPreviewLimit))
		for rows.Next() {
			var item DeletionBlockerItem
			var resourceKey string
			if err := rows.Scan(&item.ID, &resourceKey, &item.Status); err != nil {
				rows.Close()
				return nil, err
			}
			item.Name = resourceKey
			if separator := strings.LastIndexByte(resourceKey, '/'); separator >= 0 && separator+1 < len(resourceKey) {
				item.Name = resourceKey[separator+1:]
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		blockers = append(blockers, DeletionBlocker{
			Kind: DeletionBlockerR2DeletionJob, Count: deletionJobCount, Items: items,
			Truncated: deletionJobCount > len(items),
		})
	}
	return blockers, nil
}

func (s *Store) SetCapabilities(ctx context.Context, id string, capabilities []Capability) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replaceCapabilities(ctx, tx, id, capabilities); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetHealth(ctx context.Context, id, status, message string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if status == "" {
		return errors.New("health status is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET health_status = ?, health_error = ?, updated_at = ?
		WHERE id = ?`, status, message, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) setHealthIfAPITokenCurrent(ctx context.Context, id, apiTokenSecretID, status, message string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if status == "" {
		return false, errors.New("health status is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET health_status = ?, health_error = ?, updated_at = ?
		WHERE id = ? AND api_token_secret_id = ?`, status, message, time.Now().Unix(), id, apiTokenSecretID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *Store) setVerificationResultIfAPITokenCurrent(
	ctx context.Context,
	id, apiTokenSecretID string,
	capabilities []Capability,
	health, message string,
) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if health == "" {
		return false, errors.New("health status is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var currentSecretID string
	if err := tx.QueryRowContext(ctx, "SELECT api_token_secret_id FROM accounts WHERE id = ?", id).Scan(&currentSecretID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	if currentSecretID != apiTokenSecretID {
		return false, nil
	}
	if err := replaceCapabilities(ctx, tx, id, capabilities); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET health_status = ?, health_error = ?, updated_at = ?
		WHERE id = ? AND api_token_secret_id = ?`, health, message, time.Now().Unix(), id, apiTokenSecretID)
	if err != nil {
		return false, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return false, nil
	}
	return true, tx.Commit()
}

func replaceCapabilities(ctx context.Context, tx *sql.Tx, id string, capabilities []Capability) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_capabilities WHERE account_id = ?", id); err != nil {
		return err
	}
	for _, capability := range capabilities {
		checkedAt := capability.CheckedAt
		if checkedAt.IsZero() {
			checkedAt = time.Now()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_capabilities(account_id, capability, available, detail, checked_at)
			VALUES(?, ?, ?, ?, ?)`, id, capability.Name, capability.Available, capability.Detail, checkedAt.Unix()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) listCapabilities(ctx context.Context, id string) ([]Capability, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT capability, available, detail, checked_at
		FROM account_capabilities WHERE account_id = ? ORDER BY capability`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Capability
	for rows.Next() {
		var capability Capability
		var checked int64
		if err := rows.Scan(&capability.Name, &capability.Available, &capability.Detail, &checked); err != nil {
			return nil, err
		}
		capability.CheckedAt = time.Unix(checked, 0)
		result = append(result, capability)
	}
	return result, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanAccount(row scanner) (Account, error) {
	var account Account
	var created, updated int64
	if err := row.Scan(&account.ID, &account.Name, &account.CloudflareAccountID, &account.Enabled,
		&account.HealthStatus, &account.HealthError, &account.apiTokenSecretID,
		&account.r2AccessSecretID, &account.r2SecretSecretID, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Account{}, ErrNotFound
		}
		return Account{}, err
	}
	account.CreatedAt, account.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	account.HasR2Credentials = account.r2AccessSecretID.Valid && account.r2SecretSecretID.Valid
	return account, nil
}

var (
	ErrNotFound     = errors.New("account not found")
	ErrAccountInUse = errors.New("account is still referenced by managed resources")
)
