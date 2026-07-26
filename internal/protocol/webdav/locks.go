package webdavprotocol

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Lock struct {
	Token     string    `json:"token"`
	Key       string    `json:"key"`
	Owner     string    `json:"owner"`
	Depth     string    `json:"depth"`
	ExpiresAt time.Time `json:"expires_at"`
}

type LockStore struct {
	db *sql.DB
}

func NewLockStore(db *sql.DB) *LockStore {
	return &LockStore{db: db}
}

func (s *LockStore) Create(ctx context.Context, key, owner, depth string, ttl time.Duration) (Lock, error) {
	if ttl <= 0 || ttl > time.Hour {
		ttl = time.Hour
	}
	if depth == "" {
		depth = "infinity"
	}
	if err := s.Check(ctx, key, ""); !errors.Is(err, ErrLocked) && err != nil {
		return Lock{}, err
	} else if errors.Is(err, ErrLocked) {
		return Lock{}, err
	}
	lock := Lock{Token: "opaquelocktoken:" + uuid.NewString(), Key: key, Owner: owner, Depth: depth, ExpiresAt: time.Now().Add(ttl)}
	_, err := s.db.ExecContext(ctx, `INSERT INTO webdav_locks(token, object_key, owner, depth, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`, lock.Token, lock.Key, lock.Owner, lock.Depth, lock.ExpiresAt.Unix(), time.Now().Unix())
	return lock, err
}

func (s *LockStore) Refresh(ctx context.Context, token string, ttl time.Duration) (Lock, error) {
	if ttl <= 0 || ttl > time.Hour {
		ttl = time.Hour
	}
	expires := time.Now().Add(ttl)
	result, err := s.db.ExecContext(ctx, "UPDATE webdav_locks SET expires_at = ? WHERE token = ? AND expires_at > ?", expires.Unix(), token, time.Now().Unix())
	if err != nil {
		return Lock{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Lock{}, ErrLockNotFound
	}
	return s.Get(ctx, token)
}

func (s *LockStore) Get(ctx context.Context, token string) (Lock, error) {
	var lock Lock
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT token, object_key, owner, depth, expires_at FROM webdav_locks
		WHERE token = ? AND expires_at > ?`, token, time.Now().Unix()).Scan(&lock.Token, &lock.Key, &lock.Owner, &lock.Depth, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Lock{}, ErrLockNotFound
	}
	lock.ExpiresAt = time.Unix(expires, 0)
	return lock, err
}

func (s *LockStore) Delete(ctx context.Context, token string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM webdav_locks WHERE token = ?", token)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrLockNotFound
	}
	return nil
}

func (s *LockStore) Check(ctx context.Context, key, providedToken string) error {
	_, _ = s.db.ExecContext(ctx, "DELETE FROM webdav_locks WHERE expires_at <= ?", time.Now().Unix())
	rows, err := s.db.QueryContext(ctx, `SELECT token, object_key, depth FROM webdav_locks
		WHERE expires_at > ?`, time.Now().Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var token, lockedKey, depth string
		if err := rows.Scan(&token, &lockedKey, &depth); err != nil {
			return err
		}
		matches := key == lockedKey || (depth == "infinity" && (lockedKey == "" || strings.HasPrefix(key, strings.TrimSuffix(lockedKey, "/")+"/")))
		if matches && token != providedToken {
			return ErrLocked
		}
	}
	return rows.Err()
}

var (
	ErrLocked       = errors.New("resource is locked")
	ErrLockNotFound = errors.New("lock not found")
)
