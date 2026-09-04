package secret

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Repository struct {
	db     *sql.DB
	cipher *Cipher
}

func NewRepository(db *sql.DB, cipher *Cipher) *Repository {
	return &Repository{db: db, cipher: cipher}
}

func (r *Repository) Put(ctx context.Context, scope, kind, value string) (string, error) {
	return r.put(ctx, r.db, scope, kind, value)
}

func (r *Repository) PutTx(ctx context.Context, tx *sql.Tx, scope, kind, value string) (string, error) {
	return r.put(ctx, tx, scope, kind, value)
}

func (r *Repository) put(ctx context.Context, executor dbExecutor, scope, kind, value string) (string, error) {
	id := uuid.NewString()
	sealed, err := r.cipher.Encrypt([]byte(value), aad(scope, kind, id))
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	if _, err := executor.ExecContext(ctx, `INSERT INTO encrypted_secrets(id, scope, kind, ciphertext, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)`, id, scope, kind, sealed, now, now); err != nil {
		return "", fmt.Errorf("store encrypted secret: %w", err)
	}
	return id, nil
}

func (r *Repository) Get(ctx context.Context, id string) (string, error) {
	var scope, kind string
	var sealed []byte
	if err := r.db.QueryRowContext(ctx, "SELECT scope, kind, ciphertext FROM encrypted_secrets WHERE id = ?", id).Scan(&scope, &kind, &sealed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	plain, err := r.cipher.Decrypt(sealed, aad(scope, kind, id))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM encrypted_secrets WHERE id = ?", id)
	return err
}

func (r *Repository) DeleteTx(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM encrypted_secrets WHERE id = ?", id)
	return err
}

type dbExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func aad(scope, kind, id string) []byte {
	return []byte(scope + "\x00" + kind + "\x00" + id)
}

var ErrNotFound = errors.New("secret not found")
