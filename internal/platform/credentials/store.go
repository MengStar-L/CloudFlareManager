package credentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
	"github.com/google/uuid"
)

type Kind string

const (
	KindS3     Kind = "s3"
	KindWebDAV Kind = "webdav"
	KindAI     Kind = "ai"
)

type Credential struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Name      string    `json:"name"`
	PublicID  string    `json:"public_id"`
	Scopes    []string  `json:"scopes"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Secret    string    `json:"secret,omitempty"`
	secretID  sql.NullString
}

type CreateInput struct {
	Kind     Kind     `json:"kind"`
	Name     string   `json:"name"`
	PublicID string   `json:"public_id,omitempty"`
	Scopes   []string `json:"scopes"`
}

type Store struct {
	db      *sql.DB
	secrets *secret.Repository
}

func NewStore(db *sql.DB, secrets *secret.Repository) *Store {
	return &Store{db: db, secrets: secrets}
}

func (s *Store) Create(ctx context.Context, input CreateInput) (Credential, error) {
	if input.Kind != KindS3 && input.Kind != KindWebDAV && input.Kind != KindAI {
		return Credential{}, errors.New("credential kind must be s3, webdav, or ai")
	}
	if strings.TrimSpace(input.Name) == "" {
		return Credential{}, errors.New("credential name is required")
	}
	if len(input.Scopes) == 0 {
		return Credential{}, errors.New("at least one credential scope is required")
	}
	publicID := input.PublicID
	if publicID == "" {
		var err error
		publicID, err = generatePublicID(input.Kind)
		if err != nil {
			return Credential{}, err
		}
	}
	secretValue, err := randomSecret()
	if err != nil {
		return Credential{}, err
	}
	id := uuid.NewString()
	secretID, err := s.secrets.Put(ctx, "credential:"+id, string(input.Kind), secretValue)
	if err != nil {
		return Credential{}, err
	}
	scopes, err := json.Marshal(input.Scopes)
	if err != nil {
		_ = s.secrets.Delete(ctx, secretID)
		return Credential{}, err
	}
	hash := sha256.Sum256([]byte(secretValue))
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO protocol_credentials(
		id, kind, name, public_id, secret_hash, scopes_json, disabled, created_at, updated_at, secret_id)
		VALUES(?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`, id, input.Kind, input.Name, publicID, hash[:], string(scopes),
		now.Unix(), now.Unix(), secretID)
	if err != nil {
		_ = s.secrets.Delete(ctx, secretID)
		return Credential{}, fmt.Errorf("create protocol credential: %w", err)
	}
	return Credential{
		ID: id, Kind: input.Kind, Name: input.Name, PublicID: publicID, Scopes: input.Scopes,
		CreatedAt: now, UpdatedAt: now, Secret: secretValue, secretID: sql.NullString{String: secretID, Valid: true},
	}, nil
}

func (s *Store) List(ctx context.Context, kind Kind) ([]Credential, error) {
	query := credentialSelect
	args := []any{}
	if kind != "" {
		query += " WHERE kind = ?"
		args = append(args, kind)
	}
	query += " ORDER BY created_at DESC, id DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var credentials []Credential
	for rows.Next() {
		credential, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	return credentials, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (Credential, error) {
	credential, _, err := s.getByIDWithHash(ctx, id)
	return credential, err
}

func (s *Store) Verify(ctx context.Context, kind Kind, publicID, suppliedSecret string) (Credential, error) {
	credential, hash, err := s.getWithHash(ctx, kind, publicID)
	if err != nil || credential.Disabled {
		return Credential{}, ErrInvalidCredential
	}
	got := sha256.Sum256([]byte(suppliedSecret))
	if subtle.ConstantTimeCompare(got[:], hash) != 1 {
		return Credential{}, ErrInvalidCredential
	}
	return credential, nil
}

func (s *Store) Secret(ctx context.Context, kind Kind, publicID string) (string, Credential, error) {
	credential, _, err := s.getWithHash(ctx, kind, publicID)
	if err != nil || credential.Disabled || !credential.secretID.Valid {
		return "", Credential{}, ErrInvalidCredential
	}
	value, err := s.secrets.Get(ctx, credential.secretID.String)
	if err != nil {
		return "", Credential{}, err
	}
	return value, credential, nil
}

// RevealSecret returns the plaintext secret for a credential record so the
// console can show it on demand. Revoked credentials keep their secret until
// the record is deleted, so reveal works for them too (they no longer
// authenticate anyway).
func (s *Store) RevealSecret(ctx context.Context, id string) (string, Credential, error) {
	credential, _, err := s.getByIDWithHash(ctx, id)
	if err != nil {
		return "", Credential{}, err
	}
	if !credential.secretID.Valid {
		return "", Credential{}, ErrInvalidCredential
	}
	value, err := s.secrets.Get(ctx, credential.secretID.String)
	if err != nil {
		return "", Credential{}, err
	}
	return value, credential, nil
}

func (s *Store) Revoke(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE protocol_credentials SET disabled = 1, updated_at = ? WHERE id = ?", time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return nil
}

// Delete permanently removes a credential record and its stored secret.
// Only revoked (disabled) credentials may be deleted, keeping revocation an
// explicit, auditable step before the record disappears.
func (s *Store) Delete(ctx context.Context, id string) error {
	credential, _, err := s.getByIDWithHash(ctx, id)
	if err != nil {
		return err
	}
	if !credential.Disabled {
		return ErrNotRevoked
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM protocol_credentials WHERE id = ? AND disabled = 1", id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	if credential.secretID.Valid {
		_ = s.secrets.Delete(ctx, credential.secretID.String)
	}
	return nil
}

func (s *Store) Rotate(ctx context.Context, id string) (Credential, error) {
	credential, _, err := s.getByIDWithHash(ctx, id)
	if err != nil {
		return Credential{}, err
	}
	secretValue, err := randomSecret()
	if err != nil {
		return Credential{}, err
	}
	newSecretID, err := s.secrets.Put(ctx, "credential:"+credential.ID, string(credential.Kind), secretValue)
	if err != nil {
		return Credential{}, err
	}
	hash := sha256.Sum256([]byte(secretValue))
	result, err := s.db.ExecContext(ctx, `UPDATE protocol_credentials SET secret_hash = ?, secret_id = ?,
		disabled = 0, updated_at = ? WHERE id = ?`, hash[:], newSecretID, time.Now().Unix(), id)
	if err != nil {
		_ = s.secrets.Delete(ctx, newSecretID)
		return Credential{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		_ = s.secrets.Delete(ctx, newSecretID)
		return Credential{}, ErrNotFound
	}
	if credential.secretID.Valid {
		_ = s.secrets.Delete(ctx, credential.secretID.String)
	}
	credential.Secret = secretValue
	credential.Disabled = false
	credential.UpdatedAt = time.Now()
	credential.secretID = sql.NullString{String: newSecretID, Valid: true}
	return credential, nil
}

func (s *Store) getWithHash(ctx context.Context, kind Kind, publicID string) (Credential, []byte, error) {
	var credential Credential
	var scopes string
	var hash []byte
	var created, updated int64
	err := s.db.QueryRowContext(ctx, credentialSelect+" WHERE kind = ? AND public_id = ?", kind, publicID).Scan(
		&credential.ID, &credential.Kind, &credential.Name, &credential.PublicID, &hash, &scopes,
		&credential.Disabled, &created, &updated, &credential.secretID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, nil, ErrNotFound
		}
		return Credential{}, nil, err
	}
	if err := json.Unmarshal([]byte(scopes), &credential.Scopes); err != nil {
		return Credential{}, nil, err
	}
	credential.CreatedAt, credential.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return credential, hash, nil
}

func (s *Store) getByIDWithHash(ctx context.Context, id string) (Credential, []byte, error) {
	var credential Credential
	var scopes string
	var hash []byte
	var created, updated int64
	err := s.db.QueryRowContext(ctx, credentialSelect+" WHERE id = ?", id).Scan(
		&credential.ID, &credential.Kind, &credential.Name, &credential.PublicID, &hash, &scopes,
		&credential.Disabled, &created, &updated, &credential.secretID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, nil, ErrNotFound
		}
		return Credential{}, nil, err
	}
	if err := json.Unmarshal([]byte(scopes), &credential.Scopes); err != nil {
		return Credential{}, nil, err
	}
	credential.CreatedAt, credential.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return credential, hash, nil
}

const credentialSelect = `SELECT id, kind, name, public_id, secret_hash, scopes_json, disabled,
	created_at, updated_at, secret_id FROM protocol_credentials`

type scanner interface {
	Scan(...any) error
}

func scanCredential(row scanner) (Credential, error) {
	var credential Credential
	var scopes string
	var hash []byte
	var created, updated int64
	if err := row.Scan(&credential.ID, &credential.Kind, &credential.Name, &credential.PublicID, &hash,
		&scopes, &credential.Disabled, &created, &updated, &credential.secretID); err != nil {
		return Credential{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &credential.Scopes); err != nil {
		return Credential{}, err
	}
	credential.CreatedAt, credential.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return credential, nil
}

func generatePublicID(kind Kind) (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	encoded := strings.TrimRight(base32.StdEncoding.EncodeToString(data), "=")
	switch kind {
	case KindS3:
		return "CFR2" + encoded, nil
	case KindAI:
		return "cfai_" + strings.ToLower(encoded), nil
	default:
		return "dav_" + strings.ToLower(encoded), nil
	}
}

func randomSecret() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

var (
	ErrNotFound          = errors.New("credential not found")
	ErrInvalidCredential = errors.New("invalid credential")
	ErrNotRevoked        = errors.New("credential is not revoked")
)
