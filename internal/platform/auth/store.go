package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

type Store struct{ db *sql.DB }

type Session struct {
	Token     string    `json:"-"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) IsInitialized(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_users").Scan(&count)
	return count != 0, err
}

func (s *Store) InitializeAdmin(ctx context.Context, password string) error {
	initialized, err := s.IsInitialized(ctx)
	if err != nil {
		return err
	}
	if initialized {
		return ErrAlreadyInitialized
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, "INSERT INTO admin_users(id, password_hash, created_at, updated_at) VALUES(1, ?, ?, ?)", hash, now, now)
	return err
}

func (s *Store) Authenticate(ctx context.Context, password string) error {
	var hash string
	if err := s.db.QueryRowContext(ctx, "SELECT password_hash FROM admin_users WHERE id = 1").Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnauthenticated
		}
		return err
	}
	if !VerifyPassword(hash, password) {
		return ErrUnauthenticated
	}
	return nil
}

func (s *Store) CreateSession(ctx context.Context, ttl time.Duration) (Session, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	token, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken(24)
	if err != nil {
		return Session{}, err
	}
	expires := time.Now().Add(ttl)
	hash := sha256.Sum256([]byte(token))
	_, err = s.db.ExecContext(ctx, "INSERT INTO sessions(token_hash, csrf_token, expires_at, created_at) VALUES(?, ?, ?, ?)", hash[:], csrf, expires.Unix(), time.Now().Unix())
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, CSRFToken: csrf, ExpiresAt: expires}, nil
}

func (s *Store) ValidateSession(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrUnauthenticated
	}
	hash := sha256.Sum256([]byte(token))
	var session Session
	var expires int64
	if err := s.db.QueryRowContext(ctx, "SELECT csrf_token, expires_at FROM sessions WHERE token_hash = ?", hash[:]).Scan(&session.CSRFToken, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrUnauthenticated
		}
		return Session{}, err
	}
	session.ExpiresAt = time.Unix(expires, 0)
	if time.Now().After(session.ExpiresAt) {
		_ = s.RevokeSession(ctx, token)
		return Session{}, ErrUnauthenticated
	}
	session.Token = token
	return session, nil
}

func (s *Store) RevokeSession(ctx context.Context, token string) error {
	hash := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", hash[:])
	return err
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var ErrAlreadyInitialized = errors.New("administrator is already initialized")
