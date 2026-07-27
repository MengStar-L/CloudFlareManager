package r2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	WebDAVNamespaceRoot             = ".cf-r2-manager/webdav/"
	webDAVNamespaceMigrationSetting = "r2.webdav_namespace.v1"
)

type WebDAVNamespaceMigration struct {
	TargetCredentialID string
	MigratedObjects    int64
	AlreadyComplete    bool
	Deferred           bool
}

func WebDAVMountPrefix(credentialID string) string {
	return WebDAVNamespaceRoot + credentialID + "/"
}

func IsWebDAVInternalKey(key string) bool {
	return strings.HasPrefix(key, WebDAVNamespaceRoot)
}

func WebDAVMountKey(credentialID, visible string) (string, error) {
	if !validCredentialPathID(credentialID) {
		return "", ErrInvalidPath
	}
	if visible == "" {
		return WebDAVMountPrefix(credentialID), nil
	}
	if err := validateLogicalPath(visible); err != nil {
		return "", err
	}
	return WebDAVMountPrefix(credentialID) + visible, nil
}

func WebDAVVisibleKey(credentialID, internal string) (string, bool) {
	if !validCredentialPathID(credentialID) {
		return "", false
	}
	prefix := WebDAVMountPrefix(credentialID)
	if !strings.HasPrefix(internal, prefix) {
		return "", false
	}
	return strings.TrimPrefix(internal, prefix), true
}

func validCredentialPathID(value string) bool {
	if value == "" || strings.ContainsAny(value, `/\`) || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (s *Store) EnsureWebDAVNamespaces(ctx context.Context, credentialIDsOldestFirst []string) (WebDAVNamespaceMigration, error) {
	if s == nil || s.db == nil {
		return WebDAVNamespaceMigration{}, errors.New("R2 store is not configured")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx, "SELECT value FROM system_settings WHERE key = ?", webDAVNamespaceMigrationSetting).Scan(&existing)
	if err == nil {
		var state struct {
			TargetCredentialID string `json:"target_credential_id"`
		}
		if json.Unmarshal([]byte(existing), &state) != nil {
			return WebDAVNamespaceMigration{}, errors.New("invalid WebDAV namespace migration setting")
		}
		return WebDAVNamespaceMigration{TargetCredentialID: state.TargetCredentialID, AlreadyComplete: true}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WebDAVNamespaceMigration{}, err
	}

	reservedPattern := escapeLike(WebDAVNamespaceRoot) + "%"
	for _, table := range []string{"r2_objects", "r2_multipart_uploads", "webdav_locks"} {
		var count int64
		query := "SELECT COUNT(*) FROM " + table + " WHERE object_key LIKE ? ESCAPE '\\'"
		if err := tx.QueryRowContext(ctx, query, reservedPattern).Scan(&count); err != nil {
			return WebDAVNamespaceMigration{}, err
		}
		if count > 0 {
			return WebDAVNamespaceMigration{}, ErrWebDAVNamespaceConflict
		}
	}

	var legacyObjects, legacyMultipart, legacyLocks int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM r2_objects").Scan(&legacyObjects); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM r2_multipart_uploads").Scan(&legacyMultipart); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM webdav_locks").Scan(&legacyLocks); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	legacyJobs, err := countLegacyFileJobs(ctx, tx)
	if err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if legacyObjects+legacyMultipart+legacyLocks+legacyJobs == 0 {
		if err := writeWebDAVMigrationSetting(ctx, tx, ""); err != nil {
			return WebDAVNamespaceMigration{}, err
		}
		return WebDAVNamespaceMigration{}, tx.Commit()
	}
	if len(credentialIDsOldestFirst) == 0 {
		return WebDAVNamespaceMigration{Deferred: true}, tx.Commit()
	}
	targetID := credentialIDsOldestFirst[0]
	if !validCredentialPathID(targetID) {
		return WebDAVNamespaceMigration{}, ErrInvalidPath
	}
	prefix := WebDAVMountPrefix(targetID)
	for _, table := range []string{"r2_objects", "r2_multipart_uploads", "webdav_locks"} {
		query := "UPDATE " + table + " SET object_key = ? || object_key"
		if _, err := tx.ExecContext(ctx, query, prefix); err != nil {
			return WebDAVNamespaceMigration{}, fmt.Errorf("migrate %s: %w", table, err)
		}
	}
	if err := migrateFileJobPaths(ctx, tx, targetID); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if err := writeWebDAVMigrationSetting(ctx, tx, targetID); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if err := tx.Commit(); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	return WebDAVNamespaceMigration{TargetCredentialID: targetID, MigratedObjects: legacyObjects}, nil
}

func countLegacyFileJobs(ctx context.Context, tx *sql.Tx) (int64, error) {
	var count int64
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs
		WHERE type IN (?, ?) AND status IN ('pending', 'running')`, FileMoveJobType, FileDeleteJobType).Scan(&count)
	return count, err
}

func migrateFileJobPaths(ctx context.Context, tx *sql.Tx, credentialID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, payload_json FROM jobs
		WHERE type IN (?, ?) AND status IN ('pending', 'running')`, FileMoveJobType, FileDeleteJobType)
	if err != nil {
		return err
	}
	defer rows.Close()
	type update struct {
		id      string
		payload []byte
	}
	var updates []update
	for rows.Next() {
		var id, encoded string
		if err := rows.Scan(&id, &encoded); err != nil {
			return err
		}
		var payload FileJobPayload
		if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
			return fmt.Errorf("decode file job %s: %w", id, err)
		}
		payload.Source, err = WebDAVMountKey(credentialID, payload.Source)
		if err != nil {
			return fmt.Errorf("migrate file job %s source: %w", id, err)
		}
		if payload.Destination != "" {
			payload.Destination, err = WebDAVMountKey(credentialID, payload.Destination)
			if err != nil {
				return fmt.Errorf("migrate file job %s destination: %w", id, err)
			}
		}
		value, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		updates = append(updates, update{id: id, payload: value})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range updates {
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET payload_json = ? WHERE id = ?", string(item.payload), item.id); err != nil {
			return err
		}
	}
	return nil
}

func writeWebDAVMigrationSetting(ctx context.Context, tx *sql.Tx, targetID string) error {
	value, err := json.Marshal(struct {
		TargetCredentialID string `json:"target_credential_id,omitempty"`
		CompletedAt        string `json:"completed_at"`
	}{TargetCredentialID: targetID, CompletedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO system_settings(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		webDAVNamespaceMigrationSetting, string(value), time.Now().Unix())
	return err
}

var ErrWebDAVNamespaceConflict = errors.New("reserved WebDAV namespace already contains data before migration")
