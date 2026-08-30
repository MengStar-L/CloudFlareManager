package webdavprotocol

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
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
	db               *sql.DB
	mu               sync.Mutex
	activeMutations  map[uint64][]string
	nextMutationID   uint64
	mutationReleased chan struct{}
}

type lockMutationGuard struct {
	store *LockStore
	id    uint64
}

func NewLockStore(db *sql.DB) *LockStore {
	return &LockStore{
		db: db, activeMutations: make(map[uint64][]string), mutationReleased: make(chan struct{}),
	}
}

func (s *LockStore) Create(ctx context.Context, key, owner, depth string, ttl time.Duration) (Lock, error) {
	if ttl <= 0 || ttl > time.Hour {
		ttl = time.Hour
	}
	if depth == "" {
		depth = "infinity"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.waitForOverlappingMutation(ctx, Lock{Key: key, Depth: depth}); err != nil {
		return Lock{}, err
	}
	now := time.Now()
	locks, err := s.activeLocks(ctx, now.Unix())
	if err != nil {
		return Lock{}, err
	}
	for _, existing := range locks {
		if lockCovers(existing, key) || lockCovers(Lock{Key: key, Depth: depth}, existing.Key) {
			return Lock{}, ErrLocked
		}
	}
	lock := Lock{Token: "opaquelocktoken:" + uuid.NewString(), Key: key, Owner: owner, Depth: depth, ExpiresAt: now.Add(ttl)}
	_, err = s.db.ExecContext(ctx, `INSERT INTO webdav_locks(token, object_key, owner, depth, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`, lock.Token, lock.Key, lock.Owner, lock.Depth, lock.ExpiresAt.Unix(), now.Unix())
	return lock, err
}

func (s *LockStore) Refresh(ctx context.Context, token string, ttl time.Duration) (Lock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ttl <= 0 || ttl > time.Hour {
		ttl = time.Hour
	}
	now := time.Now()
	if err := s.deleteExpired(ctx, now.Unix()); err != nil {
		return Lock{}, err
	}
	expires := now.Add(ttl)
	result, err := s.db.ExecContext(ctx, "UPDATE webdav_locks SET expires_at = ? WHERE token = ? AND expires_at > ?", expires.Unix(), token, now.Unix())
	if err != nil {
		return Lock{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Lock{}, ErrLockNotFound
	}
	return s.Get(ctx, token)
}

func (s *LockStore) Get(ctx context.Context, token string) (Lock, error) {
	now := time.Now().Unix()
	if err := s.deleteExpired(ctx, now); err != nil {
		return Lock{}, err
	}
	var lock Lock
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT token, object_key, owner, depth, expires_at FROM webdav_locks
		WHERE token = ? AND expires_at > ?`, token, now).Scan(&lock.Token, &lock.Key, &lock.Owner, &lock.Depth, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Lock{}, ErrLockNotFound
	}
	lock.ExpiresAt = time.Unix(expires, 0)
	return lock, err
}

func (s *LockStore) Delete(ctx context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delete(ctx, token)
}

func (s *LockStore) delete(ctx context.Context, token string) error {
	if err := s.deleteExpired(ctx, time.Now().Unix()); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM webdav_locks WHERE token = ?", token)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrLockNotFound
	}
	return nil
}

// DeletePaths removes locks rooted at any supplied key or one of its
// descendants. Sibling keys that merely share a string prefix are preserved.
func (s *LockStore) DeletePaths(ctx context.Context, roots []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deletePaths(ctx, roots)
}

// DeleteExactPaths removes only locks rooted exactly at one of the supplied
// keys. It is used after a partially successful tree mutation, where locks on
// failed descendants must remain valid.
func (s *LockStore) DeleteExactPaths(ctx context.Context, roots []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteExactPaths(ctx, roots)
}

// RelevantLockRoots returns active lock roots whose mapped resource might
// disappear when one of paths is deleted. Callers can then revalidate only
// actual lock roots instead of probing every ancestor of every deleted key.
func (s *LockStore) RelevantLockRoots(ctx context.Context, paths []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	locks, err := s.activeLocks(ctx, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var roots []string
	for _, lock := range locks {
		for _, candidate := range paths {
			if !pathAtOrBelow(candidate, lock.Key) {
				continue
			}
			if _, ok := seen[lock.Key]; !ok {
				seen[lock.Key] = struct{}{}
				roots = append(roots, lock.Key)
			}
			break
		}
	}
	return roots, nil
}

func (s *LockStore) deletePaths(ctx context.Context, roots []string) error {
	locks, err := s.activeLocks(ctx, time.Now().Unix())
	if err != nil {
		return err
	}
	var tokens []string
	for _, lock := range locks {
		for _, root := range roots {
			if pathAtOrBelow(lock.Key, root) {
				tokens = append(tokens, lock.Token)
				break
			}
		}
	}
	if len(tokens) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, token := range tokens {
		if _, err := tx.ExecContext(ctx, "DELETE FROM webdav_locks WHERE token = ?", token); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *LockStore) deleteExactPaths(ctx context.Context, roots []string) error {
	rootSet := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		rootSet[root] = struct{}{}
	}
	locks, err := s.activeLocks(ctx, time.Now().Unix())
	if err != nil {
		return err
	}
	var tokens []string
	for _, lock := range locks {
		if _, ok := rootSet[lock.Key]; ok {
			tokens = append(tokens, lock.Token)
		}
	}
	if len(tokens) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, token := range tokens {
		if _, err := tx.ExecContext(ctx, "DELETE FROM webdav_locks WHERE token = ?", token); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *LockStore) Check(ctx context.Context, key, providedToken string) error {
	return s.CheckPaths(ctx, []string{key}, []string{providedToken})
}

// CheckPaths verifies that every active lock covering an affected key has
// its token in providedTokens.
func (s *LockStore) CheckPaths(ctx context.Context, keys, providedTokens []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkPaths(ctx, keys, providedTokens)
}

func (s *LockStore) checkPaths(ctx context.Context, keys, providedTokens []string) error {
	locks, err := s.activeLocks(ctx, time.Now().Unix())
	if err != nil {
		return err
	}
	provided := make(map[string]struct{}, len(providedTokens))
	for _, token := range providedTokens {
		provided[token] = struct{}{}
	}
	for _, lock := range locks {
		for _, key := range keys {
			if !lockCovers(lock, key) {
				continue
			}
			if _, ok := provided[lock.Token]; !ok {
				return ErrLocked
			}
			break
		}
	}
	return nil
}

// GuardPaths registers an in-flight mutation at the same linearization point
// as its lock check. New overlapping locks wait for the guard to be released;
// unrelated paths remain concurrent during remote object I/O.
func (s *LockStore) GuardPaths(ctx context.Context, lockKeys, mutationKeys, providedTokens []string, validate func() error) (*lockMutationGuard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.waitForMutationPaths(ctx, mutationKeys); err != nil {
		return nil, err
	}
	if err := s.checkPaths(ctx, lockKeys, providedTokens); err != nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
	}
	s.nextMutationID++
	id := s.nextMutationID
	s.activeMutations[id] = append([]string(nil), mutationKeys...)
	return &lockMutationGuard{store: s, id: id}, nil
}

func (s *LockStore) GuardExternalPaths(ctx context.Context, mutationKeys []string) (r2.WebDAVMutationGuard, error) {
	return s.GuardPaths(ctx, nil, mutationKeys, nil, nil)
}

func (g *lockMutationGuard) Release() {
	if g == nil || g.store == nil {
		return
	}
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	if _, ok := g.store.activeMutations[g.id]; !ok {
		return
	}
	delete(g.store.activeMutations, g.id)
	close(g.store.mutationReleased)
	g.store.mutationReleased = make(chan struct{})
}

func (g *lockMutationGuard) DeletePaths(ctx context.Context, roots []string) error {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	return g.store.deletePaths(ctx, roots)
}

func (g *lockMutationGuard) DeleteExactPaths(ctx context.Context, roots []string) error {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	return g.store.deleteExactPaths(ctx, roots)
}

func (g *lockMutationGuard) Delete(ctx context.Context, token string) error {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	return g.store.delete(ctx, token)
}

func (s *LockStore) waitForOverlappingMutation(ctx context.Context, candidate Lock) error {
	for s.mutationOverlaps(candidate) {
		changed := s.mutationReleased
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			s.mu.Lock()
			return ctx.Err()
		}
		s.mu.Lock()
	}
	return nil
}

func (s *LockStore) waitForMutationPaths(ctx context.Context, keys []string) error {
	for s.mutationPathsOverlap(keys) {
		changed := s.mutationReleased
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			s.mu.Lock()
			return ctx.Err()
		}
		s.mu.Lock()
	}
	return nil
}

func (s *LockStore) mutationOverlaps(candidate Lock) bool {
	for _, keys := range s.activeMutations {
		for _, key := range keys {
			if lockCovers(candidate, key) || pathAtOrBelow(candidate.Key, key) {
				return true
			}
		}
	}
	return false
}

func (s *LockStore) mutationPathsOverlap(keys []string) bool {
	for _, active := range s.activeMutations {
		for _, left := range active {
			for _, right := range keys {
				if pathAtOrBelow(left, right) || pathAtOrBelow(right, left) {
					return true
				}
			}
		}
	}
	return false
}

// TokenCovers reports whether token identifies an active lock whose scope
// contains key. Missing and expired tokens return ErrLockNotFound.
func (s *LockStore) TokenCovers(ctx context.Context, token, key string) (bool, error) {
	lock, err := s.Get(ctx, token)
	if err != nil {
		return false, err
	}
	return lockCovers(lock, key), nil
}

func (s *LockStore) activeLocks(ctx context.Context, now int64) ([]Lock, error) {
	if err := s.deleteExpired(ctx, now); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT token, object_key, owner, depth, expires_at FROM webdav_locks
		WHERE expires_at > ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locks []Lock
	for rows.Next() {
		var lock Lock
		var expires int64
		if err := rows.Scan(&lock.Token, &lock.Key, &lock.Owner, &lock.Depth, &expires); err != nil {
			return nil, err
		}
		lock.ExpiresAt = time.Unix(expires, 0)
		locks = append(locks, lock)
	}
	return locks, rows.Err()
}

func (s *LockStore) deleteExpired(ctx context.Context, now int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM webdav_locks WHERE expires_at <= ?", now)
	return err
}

func lockCovers(lock Lock, key string) bool {
	if sameLockPath(key, lock.Key) {
		return true
	}
	return lock.Depth == "infinity" && pathAtOrBelow(key, lock.Key)
}

func pathAtOrBelow(key, root string) bool {
	return sameLockPath(key, root) || root == "" || strings.HasPrefix(key, strings.TrimSuffix(root, "/")+"/")
}

func sameLockPath(left, right string) bool {
	return strings.TrimSuffix(left, "/") == strings.TrimSuffix(right, "/")
}

var (
	ErrLocked       = errors.New("resource is locked")
	ErrLockNotFound = errors.New("lock not found")
)
