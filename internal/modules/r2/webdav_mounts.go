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
	webDAVNamespaceRepairSetting    = "r2.webdav_namespace.v2"
)

type webDAVNamespaceState struct {
	TargetCredentialID string `json:"target_credential_id"`
	CompletedAt        string `json:"completed_at"`
}

type webDAVNamespaceRepairState struct {
	TargetCredentialID string
	V1UpdatedAt        int64
	Required           bool
}

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
		state, decodeErr := decodeWebDAVNamespaceState(existing)
		if decodeErr != nil {
			return WebDAVNamespaceMigration{}, errors.New("invalid WebDAV namespace migration setting")
		}
		return WebDAVNamespaceMigration{TargetCredentialID: state.TargetCredentialID, AlreadyComplete: true}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WebDAVNamespaceMigration{}, err
	}

	reservedPattern := escapeLike(WebDAVNamespaceRoot) + "%"
	for _, table := range []string{"r2_objects", "r2_multipart_uploads", "r2_write_intents", "webdav_locks"} {
		var count int64
		query := "SELECT COUNT(*) FROM " + table + " WHERE object_key LIKE ? ESCAPE '\\'"
		if err := tx.QueryRowContext(ctx, query, reservedPattern).Scan(&count); err != nil {
			return WebDAVNamespaceMigration{}, err
		}
		if count > 0 {
			return WebDAVNamespaceMigration{}, ErrWebDAVNamespaceConflict
		}
	}

	var legacyObjects, legacyMultipart, legacyIntents, legacyLocks int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM r2_objects").Scan(&legacyObjects); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM r2_multipart_uploads").Scan(&legacyMultipart); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM r2_write_intents").Scan(&legacyIntents); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM webdav_locks").Scan(&legacyLocks); err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	legacyJobs, err := countLegacyFileJobs(ctx, tx)
	if err != nil {
		return WebDAVNamespaceMigration{}, err
	}
	if legacyMultipart+legacyIntents != 0 {
		return WebDAVNamespaceMigration{Deferred: true}, tx.Commit()
	}
	if legacyObjects+legacyLocks+legacyJobs == 0 {
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
	for _, table := range []string{"r2_objects", "webdav_locks"} {
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

// RepairWebDAVNamespaceV1 resolves the split logical/physical state created
// by v0.5.1, which prefixed objects and multipart uploads but not their write
// intents. It runs once, before ordinary startup recovery.
func (s Service) RepairWebDAVNamespaceV1(ctx context.Context) error {
	state, err := s.Index.webDAVNamespaceRepairState(ctx)
	if err != nil || !state.Required {
		return err
	}
	if state.TargetCredentialID == "" {
		ambiguous, err := s.Index.hasWebDAVNamespaceV1RawIntent(ctx, state.V1UpdatedAt)
		if err != nil {
			return err
		}
		if ambiguous {
			return fmt.Errorf("%w: v1 recorded an empty namespace while a raw write intent exists", ErrWebDAVNamespaceRepairAmbiguous)
		}
		return s.Index.completeWebDAVNamespaceRepair(ctx, "")
	}
	if !validCredentialPathID(state.TargetCredentialID) {
		return ErrInvalidPath
	}
	prefix := WebDAVMountPrefix(state.TargetCredentialID)
	for {
		intents, err := s.Index.ListWriteIntents(ctx, 10000)
		if err != nil {
			return err
		}
		repaired := 0
		for _, intent := range intents {
			logicalKey, upload, needsRepair, err := s.namespaceRepairIntent(ctx, state, prefix, intent)
			if err != nil {
				return err
			}
			if !needsRepair {
				continue
			}
			if err := s.repairWebDAVNamespaceIntent(ctx, intent, logicalKey, upload); err != nil {
				return err
			}
			repaired++
		}
		if repaired == 0 {
			break
		}
	}
	if err := s.repairWebDAVNamespaceUnboundMultipart(ctx, state.TargetCredentialID); err != nil {
		return err
	}
	return s.Index.completeWebDAVNamespaceRepair(ctx, state.TargetCredentialID)
}

func (s Service) repairWebDAVNamespaceUnboundMultipart(ctx context.Context, credentialID string) error {
	for {
		uploads, err := s.Index.listWebDAVNamespaceV1UnboundMultipart(ctx, WebDAVMountPrefix(credentialID), 1000)
		if err != nil {
			return err
		}
		if len(uploads) == 0 {
			return nil
		}
		for _, upload := range uploads {
			rawKey, ok := WebDAVVisibleKey(credentialID, upload.Key)
			if !ok || rawKey == "" {
				return fmt.Errorf("%w: unbound multipart %s has an incompatible logical key", ErrWebDAVNamespaceRepairAmbiguous, upload.ID)
			}
			if upload.UpstreamID == "" {
				if err := s.Index.DeleteMultipart(ctx, upload.ID); err != nil {
					return err
				}
				continue
			}
			target, err := s.target(ctx, upload.BucketID)
			if err != nil {
				return err
			}
			backend, err := s.multipartBackend()
			if err != nil {
				return err
			}
			if err := s.settleUnboundMultipart(ctx, backend, target, upload, rawKey, ErrWebDAVNamespaceRepairAmbiguous); err != nil {
				return err
			}
		}
	}
}

func (s Service) namespaceRepairIntent(
	ctx context.Context,
	state webDAVNamespaceRepairState,
	prefix string,
	intent WriteIntent,
) (string, *MultipartUpload, bool, error) {
	if IsWebDAVInternalKey(intent.Key) {
		return "", nil, false, nil
	}
	upload, uploadErr := s.Index.GetMultipartByWriteIntent(ctx, intent.ID)
	if uploadErr != nil && !errors.Is(uploadErr, ErrMultipartNotFound) {
		return "", nil, false, uploadErr
	}
	if uploadErr == nil {
		if upload.Key == intent.Key {
			return "", nil, false, nil
		}
		if upload.Key != prefix+intent.Key {
			return "", nil, false, fmt.Errorf("%w: multipart %s has incompatible logical key", ErrWebDAVNamespaceRepairAmbiguous, upload.ID)
		}
		return upload.Key, &upload, true, nil
	}
	if intent.PreviousObjectID != "" {
		previous, err := s.Index.GetObjectByID(ctx, intent.PreviousObjectID)
		if err != nil {
			return "", nil, false, err
		}
		if previous.Key == intent.Key {
			return "", nil, false, nil
		}
		if previous.Key != prefix+intent.Key {
			return "", nil, false, fmt.Errorf("%w: intent %s has incompatible previous object key", ErrWebDAVNamespaceRepairAmbiguous, intent.ID)
		}
		return previous.Key, nil, true, nil
	}
	createdSecond := intent.CreatedAt.Unix()
	if createdSecond < state.V1UpdatedAt {
		logicalKey, err := WebDAVMountKey(state.TargetCredentialID, intent.Key)
		return logicalKey, nil, err == nil, err
	}
	if createdSecond == state.V1UpdatedAt {
		return "", nil, false, fmt.Errorf("%w: intent %s was created in the v1 migration second", ErrWebDAVNamespaceRepairAmbiguous, intent.ID)
	}
	return "", nil, false, nil
}

func (s Service) repairWebDAVNamespaceIntent(
	ctx context.Context,
	intent WriteIntent,
	logicalKey string,
	upload *MultipartUpload,
) error {
	if intent.Operation == WriteOperationDelete {
		return s.repairWebDAVNamespaceDelete(ctx, intent, logicalKey)
	}
	target, err := s.target(ctx, intent.BucketID)
	if err != nil {
		return err
	}
	upstreamID := intent.UpstreamUploadID
	if upload != nil && upload.UpstreamID != "" {
		upstreamID = upload.UpstreamID
	}
	if intent.State == WriteReserved && upstreamID == "" {
		return s.abortNamespaceRepairIntent(ctx, intent, upload)
	}
	backend, err := s.maintenanceBackend()
	if err != nil {
		return err
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, headErr := backend.Head(ctx, target, intent.Key)
	state, classifyErr := s.classifyWriteIntentHead(ctx, intent, remote, headErr)
	if classifyErr != nil {
		return classifyErr
	}
	if state == remoteWritePublished {
		_, err := s.Index.commitNamespaceRepairWrite(ctx, intent.ID, logicalKey, intent.Key, remote.ETag, remote.Size)
		return err
	}
	if upstreamID == "" {
		if state == remoteWritePrevious || state == remoteWriteAbsent {
			return s.abortNamespaceRepairIntent(ctx, intent, upload)
		}
		return fmt.Errorf("%w: intent %s remote version cannot be identified", ErrWebDAVNamespaceRepairAmbiguous, intent.ID)
	}
	if err := s.abortNamespaceRepairUpstream(ctx, intent, target, upstreamID); err != nil {
		return err
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, headErr = backend.Head(ctx, target, intent.Key)
	state, classifyErr = s.classifyWriteIntentHead(ctx, intent, remote, headErr)
	if classifyErr != nil {
		return classifyErr
	}
	if state == remoteWritePublished {
		_, err := s.Index.commitNamespaceRepairWrite(ctx, intent.ID, logicalKey, intent.Key, remote.ETag, remote.Size)
		return err
	}
	if state == remoteWritePrevious || state == remoteWriteAbsent {
		return s.abortNamespaceRepairIntent(ctx, intent, upload)
	}
	return fmt.Errorf("%w: intent %s remained ambiguous after multipart abort", ErrWebDAVNamespaceRepairAmbiguous, intent.ID)
}

func (s Service) abortNamespaceRepairUpstream(
	ctx context.Context,
	intent WriteIntent,
	target Target,
	upstreamID string,
) error {
	if upstreamID == "" {
		return nil
	}
	backend, err := s.multipartBackend()
	if err != nil {
		return err
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
	err = backend.AbortMultipart(ctx, target, intent.Key, upstreamID)
	if err != nil && !isMultipartNotFound(err) {
		return err
	}
	return nil
}

func (s Service) abortNamespaceRepairIntent(
	ctx context.Context,
	intent WriteIntent,
	upload *MultipartUpload,
) error {
	if upload != nil {
		return s.Index.AbortClientMultipart(ctx, upload.ID)
	}
	return s.Index.AbortWrite(ctx, intent.ID)
}

func (s Service) repairWebDAVNamespaceDelete(ctx context.Context, intent WriteIntent, logicalKey string) error {
	object, err := s.Index.GetObjectByID(ctx, intent.PreviousObjectID)
	if err != nil {
		return err
	}
	if object.Key != logicalKey {
		return ErrWebDAVNamespaceRepairAmbiguous
	}
	target, err := s.target(ctx, object.BucketID)
	if err != nil {
		return err
	}
	backend, err := s.maintenanceBackend()
	if err != nil {
		return err
	}
	_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
	remote, headErr := backend.Head(ctx, target, object.PhysicalKey)
	if headErr == nil {
		if !remoteMatchesObjectVersion(object, remote) {
			return fmt.Errorf("%w: delete intent %s remote version changed", ErrWebDAVNamespaceRepairAmbiguous, intent.ID)
		}
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassA)
		deleteErr := s.Backend.Delete(ctx, target, object.PhysicalKey)
		_ = s.Index.RecordOperation(ctx, target.AccountID, OperationClassB)
		_, confirmErr := backend.Head(ctx, target, object.PhysicalKey)
		if !isRemoteNotFound(confirmErr) {
			if confirmErr != nil {
				return confirmErr
			}
			if deleteErr != nil {
				return deleteErr
			}
			return errors.New("namespace repair could not confirm remote deletion")
		}
	} else if !isRemoteNotFound(headErr) {
		return headErr
	}
	if err := s.Index.commitNamespaceRepairDelete(ctx, intent.ID, logicalKey); err != nil {
		return err
	}
	return s.cleanupDeletedWebDAVLocks(context.WithoutCancel(ctx), logicalKey, nil)
}

func remoteMatchesObjectVersion(object Object, remote RemoteObject) bool {
	if writeID := remote.Metadata[InternalWriteIDMetadata]; writeID != "" {
		return writeID == object.ObjectID
	}
	return object.Size == remote.Size && objectETagsEqual(object.ETag, remote.ETag)
}

func (s *Store) hasWebDAVNamespaceV1RawIntent(ctx context.Context, v1UpdatedAt int64) (bool, error) {
	cutoff := time.Unix(v1UpdatedAt, 0).Add(time.Second).UnixNano()
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM r2_write_intents
		WHERE object_key NOT LIKE ? ESCAPE '\' AND created_at < ?
	)`, escapeLike(WebDAVNamespaceRoot)+"%", cutoff).Scan(&found)
	return found != 0, err
}

func (s *Store) listWebDAVNamespaceV1UnboundMultipart(
	ctx context.Context,
	prefix string,
	limit int,
) ([]MultipartUpload, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, multipartSelect+` WHERE COALESCE(write_intent_id, '') = ''
		AND object_key LIKE ? ESCAPE '\' ORDER BY created_at, id LIMIT ?`, escapeLike(prefix)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	uploads := make([]MultipartUpload, 0, limit)
	for rows.Next() {
		upload, err := scanMultipart(rows)
		if err != nil {
			return nil, err
		}
		uploads = append(uploads, upload)
	}
	return uploads, rows.Err()
}

func (s *Store) webDAVNamespaceRepairState(ctx context.Context) (webDAVNamespaceRepairState, error) {
	var repairMarker string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM system_settings WHERE key = ?", webDAVNamespaceRepairSetting).Scan(&repairMarker); err == nil {
		if _, decodeErr := decodeWebDAVNamespaceState(repairMarker); decodeErr != nil {
			return webDAVNamespaceRepairState{}, errors.New("invalid WebDAV namespace repair setting")
		}
		return webDAVNamespaceRepairState{}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return webDAVNamespaceRepairState{}, err
	}
	var encoded string
	var updatedAt int64
	if err := s.db.QueryRowContext(ctx, "SELECT value, updated_at FROM system_settings WHERE key = ?", webDAVNamespaceMigrationSetting).Scan(&encoded, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return webDAVNamespaceRepairState{}, nil
		}
		return webDAVNamespaceRepairState{}, err
	}
	state, err := decodeWebDAVNamespaceState(encoded)
	if err != nil {
		return webDAVNamespaceRepairState{}, err
	}
	return webDAVNamespaceRepairState{
		TargetCredentialID: state.TargetCredentialID,
		V1UpdatedAt:        updatedAt,
		Required:           true,
	}, nil
}

func (s *Store) completeWebDAVNamespaceRepair(ctx context.Context, targetID string) error {
	value, err := encodeWebDAVNamespaceState(targetID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO system_settings(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		webDAVNamespaceRepairSetting, value, time.Now().Unix())
	return err
}

func decodeWebDAVNamespaceState(value string) (webDAVNamespaceState, error) {
	var state webDAVNamespaceState
	if err := json.Unmarshal([]byte(value), &state); err != nil {
		return webDAVNamespaceState{}, err
	}
	return state, nil
}

func encodeWebDAVNamespaceState(targetID string) (string, error) {
	value, err := json.Marshal(webDAVNamespaceState{
		TargetCredentialID: targetID,
		CompletedAt:        time.Now().UTC().Format(time.RFC3339),
	})
	return string(value), err
}

// PrepareWebDAVNamespaceMigration settles interrupted writes and aborts
// resumable client uploads while their legacy logical keys still equal their
// upstream physical keys. The one-time namespace migration can then safely
// change only committed logical keys.
func (s Service) PrepareWebDAVNamespaceMigration(ctx context.Context) error {
	if err := s.RecoverInterruptedBeforeServing(ctx); err != nil {
		return err
	}
	for _, status := range []MultipartStatus{MultipartInitiating, MultipartActive, MultipartCompleting, MultipartError} {
		for {
			uploads, err := s.Index.ListMultipartByStatus(ctx, status, 1000)
			if err != nil {
				return err
			}
			if len(uploads) == 0 {
				break
			}
			for _, upload := range uploads {
				if upload.UpstreamID == "" {
					if err := s.Index.AbortClientMultipart(ctx, upload.ID); err != nil {
						return err
					}
					continue
				}
				if err := s.AbortMultipart(ctx, upload.Key, upload.ID); err != nil {
					return err
				}
			}
		}
	}
	if err := s.RecoverInterruptedBeforeServing(ctx); err != nil {
		return err
	}
	intents, err := s.Index.ListWriteIntents(ctx, 1)
	if err != nil {
		return err
	}
	if len(intents) != 0 {
		return ErrWriteInProgress
	}
	return nil
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
	value, err := encodeWebDAVNamespaceState(targetID)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, key := range []string{webDAVNamespaceMigrationSetting, webDAVNamespaceRepairSetting} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO system_settings(key, value, updated_at) VALUES(?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, now); err != nil {
			return err
		}
	}
	return nil
}

var (
	ErrWebDAVNamespaceConflict        = errors.New("reserved WebDAV namespace already contains data before migration")
	ErrWebDAVNamespaceRepairAmbiguous = errors.New("WebDAV namespace v1 repair is ambiguous")
)
