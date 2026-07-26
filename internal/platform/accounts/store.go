package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

type Store struct {
	db      *sql.DB
	secrets *secret.Repository
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

func (s *Store) Delete(ctx context.Context, id string) error {
	account, err := s.Get(ctx, id, false)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id); err != nil {
		return err
	}
	for _, secretID := range []sql.NullString{{String: account.apiTokenSecretID, Valid: true}, account.r2AccessSecretID, account.r2SecretSecretID} {
		if secretID.Valid {
			_ = s.secrets.Delete(ctx, secretID.String)
		}
	}
	return nil
}

func (s *Store) SetCapabilities(ctx context.Context, id string, capabilities []Capability) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	return tx.Commit()
}

func (s *Store) SetHealth(ctx context.Context, id, status, message string) error {
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
	return account, nil
}

var ErrNotFound = errors.New("account not found")
