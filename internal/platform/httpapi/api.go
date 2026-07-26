package httpapi

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/ai"
	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/d1"
	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/audit"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/auth"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/credentials"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/update"
	"github.com/google/uuid"
)

const sessionCookieName = "cf_r2_manager_session"

type Dependencies struct {
	DB           *sql.DB
	Auth         *auth.Store
	Accounts     *accounts.Store
	Jobs         *jobs.Store
	Audit        *audit.Store
	Credentials  *credentials.Store
	R2           *r2.Store
	Remote       accounts.RemoteClient
	Updater      *update.Updater
	D1           *d1.Client
	AI           *ai.Gateway
	AIManagement *ai.Management
	Version      string
	Static       http.Handler
}

type API struct {
	deps Dependencies
}

func New(deps Dependencies) http.Handler {
	api := &API{deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /readyz", api.ready)
	mux.HandleFunc("POST /api/v1/session", api.login)
	mux.Handle("GET /api/v1/session", api.protected(http.HandlerFunc(api.currentSession)))
	mux.Handle("DELETE /api/v1/session", api.protected(http.HandlerFunc(api.logout)))
	mux.Handle("GET /api/v1/accounts", api.protected(http.HandlerFunc(api.listAccounts)))
	mux.Handle("POST /api/v1/accounts", api.protected(http.HandlerFunc(api.createAccount)))
	mux.Handle("GET /api/v1/accounts/{id}", api.protected(http.HandlerFunc(api.getAccount)))
	mux.Handle("POST /api/v1/accounts/{id}/verify", api.protected(http.HandlerFunc(api.verifyAccount)))
	mux.Handle("DELETE /api/v1/accounts/{id}", api.protected(http.HandlerFunc(api.deleteAccount)))
	mux.Handle("GET /api/v1/jobs", api.protected(http.HandlerFunc(api.listJobs)))
	mux.Handle("GET /api/v1/audit", api.protected(http.HandlerFunc(api.listAudit)))
	mux.Handle("GET /api/v1/events", api.protected(http.HandlerFunc(api.events)))
	mux.Handle("GET /api/v1/credentials", api.protected(http.HandlerFunc(api.listCredentials)))
	mux.Handle("POST /api/v1/credentials", api.protected(http.HandlerFunc(api.createCredential)))
	mux.Handle("POST /api/v1/credentials/{id}/rotate", api.protected(http.HandlerFunc(api.rotateCredential)))
	mux.Handle("GET /api/v1/credentials/{id}/secret", api.protected(http.HandlerFunc(api.revealCredentialSecret)))
	mux.Handle("DELETE /api/v1/credentials/{id}", api.protected(http.HandlerFunc(api.revokeCredential)))
	mux.Handle("DELETE /api/v1/credentials/{id}/record", api.protected(http.HandlerFunc(api.deleteCredentialRecord)))
	mux.Handle("GET /api/v1/r2/buckets", api.protected(http.HandlerFunc(api.listR2Buckets)))
	mux.Handle("GET /api/v1/r2/remote-buckets", api.protected(http.HandlerFunc(api.listRemoteR2Buckets)))
	mux.Handle("POST /api/v1/r2/remote-buckets", api.protected(http.HandlerFunc(api.createRemoteR2Bucket)))
	mux.Handle("GET /api/v1/r2/overview", api.protected(http.HandlerFunc(api.r2Overview)))
	mux.Handle("GET /api/v1/system/update", api.protected(http.HandlerFunc(api.checkUpdate)))
	mux.Handle("POST /api/v1/system/update", api.protected(http.HandlerFunc(api.applyUpdate)))
	mux.Handle("POST /api/v1/r2/buckets", api.protected(http.HandlerFunc(api.createR2Bucket)))
	mux.Handle("DELETE /api/v1/r2/buckets/{id}", api.protected(http.HandlerFunc(api.deleteR2Bucket)))
	mux.Handle("POST /api/v1/r2/buckets/{id}/adopt", api.protected(http.HandlerFunc(api.adoptR2Bucket)))
	mux.Handle("POST /api/v1/r2/buckets/{id}/orphans/scan", api.protected(http.HandlerFunc(api.scanR2Orphans)))
	mux.Handle("GET /api/v1/r2/objects", api.protected(http.HandlerFunc(api.listR2Objects)))
	mux.Handle("GET /api/v1/r2/findings", api.protected(http.HandlerFunc(api.listR2Findings)))
	mux.Handle("POST /api/v1/r2/index/rebuild", api.protected(http.HandlerFunc(api.rebuildR2Index)))
	mux.Handle("POST /api/v1/r2/recovery", api.protected(http.HandlerFunc(api.recoverR2State)))
	mux.Handle("POST /api/v1/r2/rebalance", api.protected(http.HandlerFunc(api.rebalanceR2Objects)))
	mux.Handle("GET /api/v1/d1/databases", api.protected(http.HandlerFunc(api.listD1Databases)))
	mux.Handle("POST /api/v1/d1/databases", api.protected(http.HandlerFunc(api.createD1Database)))
	mux.Handle("DELETE /api/v1/d1/databases/{id}", api.protected(http.HandlerFunc(api.deleteD1Database)))
	mux.Handle("POST /api/v1/d1/databases/{id}/query", api.protected(http.HandlerFunc(api.queryD1Database)))
	mux.Handle("GET /api/v1/d1/databases/{id}/schema", api.protected(http.HandlerFunc(api.d1Schema)))
	mux.Handle("GET /api/v1/d1/databases/{id}/tables/{table}/rows", api.protected(http.HandlerFunc(api.d1TableRows)))
	mux.Handle("GET /api/v1/d1/databases/{id}/history", api.protected(http.HandlerFunc(api.d1History)))
	mux.Handle("GET /api/v1/d1/databases/{id}/insights", api.protected(http.HandlerFunc(api.d1Insights)))
	mux.Handle("POST /api/v1/d1/databases/{id}/backup", api.protected(http.HandlerFunc(api.backupD1Database)))
	mux.Handle("GET /api/v1/d1/databases/{id}/backups", api.protected(http.HandlerFunc(api.d1Backups)))
	mux.Handle("POST /api/v1/d1/databases/{id}/time-travel/restore", api.protected(http.HandlerFunc(api.restoreD1Database)))
	mux.Handle("GET /api/v1/ai/usage", api.protected(http.HandlerFunc(api.aiUsage)))
	mux.Handle("GET /api/v1/ai/logs", api.protected(http.HandlerFunc(api.aiLogs)))
	mux.Handle("GET /api/v1/ai/models", api.protected(http.HandlerFunc(api.aiModels)))
	mux.Handle("GET /api/v1/ai/gateways", api.protected(http.HandlerFunc(api.listAIGateways)))
	mux.Handle("POST /api/v1/ai/gateways", api.protected(http.HandlerFunc(api.createAIGateway)))
	mux.Handle("DELETE /api/v1/ai/gateways/{id}", api.protected(http.HandlerFunc(api.deleteAIGateway)))
	mux.Handle("GET /api/v1/ai/gateways/{id}/logs", api.protected(http.HandlerFunc(api.aiGatewayLogs)))
	mux.Handle("POST /api/v1/ai/playground", api.protected(http.HandlerFunc(api.aiPlayground)))
	if deps.Static != nil {
		mux.Handle("/", deps.Static)
	}
	return api.requestContext(mux)
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": a.deps.Version})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	if a.deps.DB == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := a.deps.DB.PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "database is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if a.deps.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "authentication is unavailable")
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if err := a.deps.Auth.Authenticate(r.Context(), input.Password); err != nil {
		a.record(r, "anonymous", "session.login", "session", "denied", nil)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid administrator credentials")
		return
	}
	session, err := a.deps.Auth.CreateSession(r.Context(), 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: session.Token, Path: "/", HttpOnly: true,
		Secure: forwardedHTTPS(r), SameSite: http.SameSiteStrictMode, Expires: session.ExpiresAt,
	})
	a.record(r, "admin", "session.login", "session", "success", nil)
	writeJSON(w, http.StatusOK, map[string]any{"csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt})
}

func (a *API) currentSession(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt,
	})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	if err := a.deps.Auth.RevokeSession(r.Context(), session.Token); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not revoke session")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	a.record(r, "admin", "session.logout", "session", "success", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.Accounts.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list accounts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": items})
}

func (a *API) createAccount(w http.ResponseWriter, r *http.Request) {
	var input accounts.CreateInput
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	account, err := a.deps.Accounts.Create(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_account", err.Error())
		return
	}
	job, err := a.deps.Jobs.Enqueue(r.Context(), accounts.CapabilityJobType, map[string]string{"account_id": account.ID}, 4)
	if err != nil {
		_ = a.deps.Accounts.Delete(r.Context(), account.ID)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not schedule capability detection")
		return
	}
	a.record(r, "admin", "account.create", "accounts/"+account.ID, "success", map[string]any{"job_id": job.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"account": account, "job": job})
}

func (a *API) getAccount(w http.ResponseWriter, r *http.Request) {
	account, err := a.deps.Accounts.Get(r.Context(), r.PathValue("id"), false)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load account")
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (a *API) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if a.deps.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "updater is not configured")
		return
	}
	info, err := a.deps.Updater.Check(r.Context(), r.URL.Query().Get("force") == "1")
	if err != nil {
		writeError(w, http.StatusBadGateway, "update_check_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (a *API) applyUpdate(w http.ResponseWriter, r *http.Request) {
	if a.deps.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "updater is not configured")
		return
	}
	version, err := a.deps.Updater.Apply(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "update_failed", err.Error())
		return
	}
	a.record(r, "admin", "system.update", "system", "success", map[string]any{"version": version})
	writeJSON(w, http.StatusOK, map[string]any{"status": "restarting", "version": version})
	// 先让响应发出去，再重启进程。
	go func() {
		time.Sleep(800 * time.Millisecond)
		a.deps.Updater.Restart()
	}()
}

func (a *API) verifyAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.deps.Accounts.Get(r.Context(), id, false); err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load account")
		return
	}
	job, err := a.deps.Jobs.Enqueue(r.Context(), accounts.CapabilityJobType, map[string]string{"account_id": id}, 4)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not schedule capability detection")
		return
	}
	a.record(r, "admin", "account.verify", "accounts/"+id, "success", map[string]any{"job_id": job.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (a *API) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.deps.Accounts.Delete(r.Context(), id); err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeError(w, http.StatusConflict, "account_in_use", "account cannot be removed while resources reference it")
		return
	}
	a.record(r, "admin", "account.delete", "accounts/"+id, "success", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	status := jobs.Status(r.URL.Query().Get("status"))
	items, err := a.deps.Jobs.List(r.Context(), queryLimit(r, 100), status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list jobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": items})
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.Audit.List(r.Context(), queryLimit(r, 100), r.URL.Query().Get("action"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list audit events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": items})
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusNotImplemented, "streaming_unavailable", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		items, err := a.deps.Jobs.List(r.Context(), 50, "")
		if err != nil {
			return
		}
		data, _ := json.Marshal(map[string]any{"jobs": items})
		_, _ = fmt.Fprintf(w, "event: jobs\ndata: %s\n\n", data)
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *API) listCredentials(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.Credentials.List(r.Context(), credentials.Kind(r.URL.Query().Get("kind")))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list protocol credentials")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": items})
}

func (a *API) createCredential(w http.ResponseWriter, r *http.Request) {
	var input credentials.CreateInput
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	credential, err := a.deps.Credentials.Create(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	a.record(r, "admin", "credential.create", "credentials/"+credential.ID, "success", map[string]any{"kind": credential.Kind})
	writeJSON(w, http.StatusCreated, credential)
}

func (a *API) rotateCredential(w http.ResponseWriter, r *http.Request) {
	credential, err := a.deps.Credentials.Rotate(r.Context(), r.PathValue("id"))
	if errors.Is(err, credentials.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "credential not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not rotate credential")
		return
	}
	a.record(r, "admin", "credential.rotate", "credentials/"+credential.ID, "success", nil)
	writeJSON(w, http.StatusOK, credential)
}

func (a *API) revealCredentialSecret(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	secret, credential, err := a.deps.Credentials.RevealSecret(r.Context(), id)
	if errors.Is(err, credentials.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "credential not found")
		return
	}
	if errors.Is(err, credentials.ErrInvalidCredential) {
		writeError(w, http.StatusConflict, "secret_unavailable", "credential has no stored secret")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not reveal credential secret")
		return
	}
	a.record(r, "admin", "credential.reveal", "credentials/"+id, "success", map[string]any{"kind": credential.Kind})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        credential.ID,
		"kind":      credential.Kind,
		"public_id": credential.PublicID,
		"secret":    secret,
	})
}

func (a *API) revokeCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.deps.Credentials.Revoke(r.Context(), id); err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "credential not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "could not revoke credential")
		return
	}
	a.record(r, "admin", "credential.revoke", "credentials/"+id, "success", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deleteCredentialRecord(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := a.deps.Credentials.Delete(r.Context(), id)
	if errors.Is(err, credentials.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "credential not found")
		return
	}
	if errors.Is(err, credentials.ErrNotRevoked) {
		writeError(w, http.StatusConflict, "not_revoked", "credential must be revoked before its record can be deleted")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not delete credential record")
		return
	}
	a.record(r, "admin", "credential.delete", "credentials/"+id, "success", nil)
	w.WriteHeader(http.StatusNoContent)
}

// r2FreeTierBytes is the storage included in Cloudflare R2's free tier
// (10 GB-month of standard storage).
const r2FreeTierBytes int64 = 10_000_000_000

type remoteBucketView struct {
	Name          string `json:"name"`
	CreationDate  string `json:"creation_date,omitempty"`
	PayloadBytes  *int64 `json:"payload_bytes,omitempty"`
	MetadataBytes *int64 `json:"metadata_bytes,omitempty"`
	ObjectCount   *int64 `json:"object_count,omitempty"`
	Managed       bool   `json:"managed"`
	BucketID      string `json:"bucket_id,omitempty"`
	HealthStatus  string `json:"health_status,omitempty"`
	RemoteMissing bool   `json:"remote_missing,omitempty"`
}

// remoteBucketViews merges the Cloudflare bucket list, per-bucket usage, and
// local array membership for one account.
func (a *API) remoteBucketViews(ctx context.Context, account accounts.Account) ([]remoteBucketView, map[string]any, error) {
	remote, err := a.deps.Remote.R2Buckets(ctx, account.CloudflareAccountID, account.APIToken)
	if err != nil {
		return nil, nil, err
	}
	local, err := a.deps.R2.ListBuckets(ctx)
	if err != nil {
		return nil, nil, err
	}
	managed := make(map[string]struct {
		id     string
		health string
	})
	for _, bucket := range local {
		if bucket.AccountID == account.ID {
			managed[bucket.Name] = struct {
				id     string
				health string
			}{bucket.ID, bucket.HealthStatus}
		}
	}
	// 用量拉取失败不应拖垮整个列表：桶照常展示，仅缺少大小信息。
	usage, usageErr := a.deps.Remote.R2BucketUsage(ctx, account.CloudflareAccountID, account.APIToken)

	views := make([]remoteBucketView, 0, len(remote)+2)
	seen := make(map[string]bool, len(remote))
	var totalBytes int64
	for _, bucket := range remote {
		seen[bucket.Name] = true
		view := remoteBucketView{Name: bucket.Name, CreationDate: bucket.CreationDate}
		if entry, ok := managed[bucket.Name]; ok {
			view.Managed, view.BucketID, view.HealthStatus = true, entry.id, entry.health
		}
		if usageErr == nil {
			stats := usage[bucket.Name]
			payload, metadata, objects := stats.PayloadBytes, stats.MetadataBytes, stats.ObjectCount
			view.PayloadBytes, view.MetadataBytes, view.ObjectCount = &payload, &metadata, &objects
			totalBytes += payload
		}
		views = append(views, view)
	}
	// 本地登记过、但远端已经不存在的桶：保留展示并标记异常。
	for _, bucket := range local {
		if bucket.AccountID == account.ID && !seen[bucket.Name] {
			views = append(views, remoteBucketView{
				Name: bucket.Name, Managed: true, BucketID: bucket.ID,
				HealthStatus: bucket.HealthStatus, RemoteMissing: true,
			})
		}
	}

	summary := map[string]any{
		"free_tier_bytes": r2FreeTierBytes,
	}
	if usageErr == nil {
		remaining := r2FreeTierBytes - totalBytes
		if remaining < 0 {
			remaining = 0
		}
		summary["total_bytes"] = totalBytes
		summary["remaining_bytes"] = remaining
	} else {
		summary["usage_error"] = usageErr.Error()
	}
	return views, summary, nil
}

func (a *API) listRemoteR2Buckets(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "account_id is required")
		return
	}
	account, err := a.deps.Accounts.Get(r.Context(), accountID, true)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load account")
		return
	}
	views, summary, err := a.remoteBucketViews(r.Context(), account)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": views, "usage": summary})
}

// r2Overview aggregates every configured account's buckets, usage, and free
// quota into one response for the cross-account overview.
func (a *API) r2Overview(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.Accounts.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list accounts")
		return
	}
	type accountBucketsView struct {
		AccountID   string             `json:"account_id"`
		AccountName string             `json:"account_name"`
		Buckets     []remoteBucketView `json:"buckets"`
		Usage       map[string]any     `json:"usage,omitempty"`
		Error       string             `json:"error,omitempty"`
	}
	overview := make([]accountBucketsView, 0, len(items))
	for _, item := range items {
		entry := accountBucketsView{AccountID: item.ID, AccountName: item.Name}
		account, err := a.deps.Accounts.Get(r.Context(), item.ID, true)
		if err != nil {
			entry.Error = "could not load account credentials"
			overview = append(overview, entry)
			continue
		}
		views, summary, err := a.remoteBucketViews(r.Context(), account)
		if err != nil {
			entry.Error = err.Error()
			overview = append(overview, entry)
			continue
		}
		entry.Buckets, entry.Usage = views, summary
		overview = append(overview, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": overview})
}

func (a *API) createRemoteR2Bucket(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountID string `json:"account_id"`
		Name      string `json:"name"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if input.AccountID == "" || input.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "account_id and name are required")
		return
	}
	account, err := a.deps.Accounts.Get(r.Context(), input.AccountID, true)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load account")
		return
	}
	bucket, err := a.deps.Remote.CreateR2Bucket(r.Context(), account.CloudflareAccountID, account.APIToken, input.Name)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	a.record(r, "admin", "r2.bucket.create-remote", "r2/remote-buckets/"+bucket.Name, "success", map[string]any{"account_id": input.AccountID})
	writeJSON(w, http.StatusCreated, map[string]any{"bucket": bucket})
}

func (a *API) listR2Buckets(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.R2.ListBuckets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list physical buckets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": items})
}

func (a *API) createR2Bucket(w http.ResponseWriter, r *http.Request) {
	var input r2.CreateBucketInput
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	bucket, err := a.deps.R2.CreateBucket(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_bucket", err.Error())
		return
	}
	a.record(r, "admin", "r2.bucket.create", "r2/buckets/"+bucket.ID, "success", nil)
	writeJSON(w, http.StatusCreated, bucket)
}

func (a *API) deleteR2Bucket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.deps.R2.DeleteBucket(r.Context(), id); err != nil {
		if errors.Is(err, r2.ErrBucketNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "physical bucket not found")
			return
		}
		writeError(w, http.StatusConflict, "bucket_in_use", "physical bucket is still referenced")
		return
	}
	a.record(r, "admin", "r2.bucket.delete", "r2/buckets/"+id, "success", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listR2Objects(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.R2.ListObjects(r.Context(), r2.ListOptions{
		Prefix: r.URL.Query().Get("prefix"), After: r.URL.Query().Get("after"), Limit: queryLimit(r, 200),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list unified objects")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *API) adoptR2Bucket(w http.ResponseWriter, r *http.Request) {
	a.enqueueR2BucketJob(w, r, r2.AdoptBucketJobType, "r2.bucket.adopt")
}

func (a *API) scanR2Orphans(w http.ResponseWriter, r *http.Request) {
	a.enqueueR2BucketJob(w, r, r2.OrphanScanJobType, "r2.bucket.orphans.scan")
}

func (a *API) enqueueR2BucketJob(w http.ResponseWriter, r *http.Request, jobType, action string) {
	id := r.PathValue("id")
	if _, err := a.deps.R2.GetBucket(r.Context(), id); err != nil {
		if errors.Is(err, r2.ErrBucketNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "physical bucket not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load physical bucket")
		return
	}
	job, err := a.deps.Jobs.Enqueue(r.Context(), jobType, map[string]string{"bucket_id": id}, 3)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not schedule R2 maintenance job")
		return
	}
	a.record(r, "admin", action, "r2/buckets/"+id, "success", map[string]any{"job_id": job.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (a *API) listR2Findings(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.R2.ListScanFindings(r.Context(), r.URL.Query().Get("bucket_id"), r.URL.Query().Get("kind"), queryLimit(r, 200))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list R2 scan findings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": items})
}

func (a *API) rebuildR2Index(w http.ResponseWriter, r *http.Request) {
	job, err := a.deps.Jobs.Enqueue(r.Context(), r2.RebuildIndexJobType, map[string]string{"source": "admin"}, 2)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not schedule index rebuild")
		return
	}
	a.record(r, "admin", "r2.index.rebuild", "r2/index", "success", map[string]any{"job_id": job.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (a *API) recoverR2State(w http.ResponseWriter, r *http.Request) {
	job, err := a.deps.Jobs.Enqueue(r.Context(), r2.RecoverStateJobType, map[string]string{"source": "admin"}, 3)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not schedule state recovery")
		return
	}
	a.record(r, "admin", "r2.state.recover", "r2/objects", "success", map[string]any{"job_id": job.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (a *API) rebalanceR2Objects(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SourceBucketID string `json:"source_bucket_id"`
		TargetBucketID string `json:"target_bucket_id"`
		Prefix         string `json:"prefix,omitempty"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if input.SourceBucketID == "" || input.TargetBucketID == "" || input.SourceBucketID == input.TargetBucketID {
		writeError(w, http.StatusBadRequest, "invalid_rebalance", "distinct source_bucket_id and target_bucket_id are required")
		return
	}
	if _, err := a.deps.R2.GetBucket(r.Context(), input.SourceBucketID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_rebalance", "source bucket does not exist")
		return
	}
	if _, err := a.deps.R2.GetBucket(r.Context(), input.TargetBucketID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_rebalance", "target bucket does not exist")
		return
	}
	job, err := a.deps.Jobs.Enqueue(r.Context(), r2.RebalanceJobType, input, 3)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not schedule rebalance")
		return
	}
	a.record(r, "admin", "r2.objects.rebalance", "r2/objects", "success", map[string]any{"job_id": job.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (a *API) listD1Databases(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	items, err := a.deps.D1.ListDatabases(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": items})
}

func (a *API) createD1Database(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountID string `json:"account_id"`
		Name      string `json:"name"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	database, err := a.deps.D1.CreateDatabase(r.Context(), input.AccountID, input.Name)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	a.record(r, "admin", "d1.database.create", "d1/databases/"+database.UUID, "success", map[string]any{"account_id": input.AccountID})
	writeJSON(w, http.StatusCreated, database)
}

func (a *API) deleteD1Database(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountID     string `json:"account_id"`
		AdminPassword string `json:"admin_password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if err := a.deps.Auth.Authenticate(r.Context(), input.AdminPassword); err != nil {
		writeError(w, http.StatusForbidden, "confirmation_failed", "administrator password confirmation failed")
		return
	}
	if err := a.deps.D1.DeleteDatabase(r.Context(), input.AccountID, r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	a.record(r, "admin", "d1.database.delete", "d1/databases/"+r.PathValue("id"), "success", map[string]any{"account_id": input.AccountID})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) queryD1Database(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountID     string `json:"account_id"`
		SQL           string `json:"sql"`
		Params        []any  `json:"params,omitempty"`
		AdminPassword string `json:"admin_password,omitempty"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	class := d1.ClassifySQL(input.SQL)
	if class != d1.SQLRead {
		if err := a.deps.Auth.Authenticate(r.Context(), input.AdminPassword); err != nil {
			writeError(w, http.StatusForbidden, "write_confirmation_required", "non-read-only SQL requires administrator password confirmation")
			return
		}
	}
	results, err := a.deps.D1.Query(r.Context(), input.AccountID, r.PathValue("id"), d1.QueryInput{SQL: input.SQL, Params: input.Params})
	if err != nil {
		a.record(r, "admin", "d1.query", "d1/databases/"+r.PathValue("id"), "error", map[string]any{"class": int(class)})
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	a.record(r, "admin", "d1.query", "d1/databases/"+r.PathValue("id"), "success", map[string]any{"class": int(class)})
	writeJSON(w, http.StatusOK, map[string]any{"class": class, "results": results})
}

func (a *API) d1History(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	items, err := a.deps.D1.History(r.Context(), accountID, r.PathValue("id"), queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list D1 query history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": items})
}

func (a *API) d1Schema(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	items, err := a.deps.D1.Schema(r.Context(), accountID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema": items})
}

func (a *API) d1TableRows(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	offset, err := strconv.Atoi(defaultQuery(r.URL.Query().Get("offset"), "0"))
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid_offset", "offset must be a non-negative integer")
		return
	}
	page, err := a.deps.D1.TableRows(r.Context(), accountID, r.PathValue("id"), r.PathValue("table"), queryLimit(r, 50), offset)
	if errors.Is(err, d1.ErrTableNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "D1 table or view not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) d1Insights(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	items, err := a.deps.D1.Insights(r.Context(), accountID, r.PathValue("id"), queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not analyze D1 query history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"insights": items, "notice": "Insights are derived from locally observed query history.",
	})
}

func (a *API) backupD1Database(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	backup, err := a.deps.D1.BackupDatabase(r.Context(), input.AccountID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "backup_failed", err.Error())
		return
	}
	a.record(r, "admin", "d1.database.backup", "d1/databases/"+r.PathValue("id"), "success", map[string]any{"object_key": backup.R2ObjectKey})
	writeJSON(w, http.StatusCreated, backup)
}

func (a *API) d1Backups(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	items, err := a.deps.D1.ListBackups(r.Context(), accountID, r.PathValue("id"), queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list D1 backups")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": items})
}

func (a *API) restoreD1Database(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountID     string `json:"account_id"`
		Bookmark      string `json:"bookmark"`
		AdminPassword string `json:"admin_password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if err := a.deps.Auth.Authenticate(r.Context(), input.AdminPassword); err != nil {
		writeError(w, http.StatusForbidden, "confirmation_failed", "administrator password confirmation failed")
		return
	}
	result, err := a.deps.D1.TimeTravelRestore(r.Context(), input.AccountID, r.PathValue("id"), input.Bookmark)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	a.record(r, "admin", "d1.time_travel.restore", "d1/databases/"+r.PathValue("id"), "success", map[string]any{"account_id": input.AccountID})
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (a *API) aiUsage(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.AI.Usage(r.Context(), r.URL.Query().Get("account_id"), r.URL.Query().Get("date"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load AI usage")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"usage":  items,
		"notice": "Neuron values are local estimates and are not Cloudflare billing data.",
	})
}

func (a *API) aiLogs(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.AI.Logs(r.Context(), queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load AI request logs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": items})
}

func (a *API) aiModels(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	items, err := a.deps.AIManagement.ListModels(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": items})
}

func (a *API) listAIGateways(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	items, err := a.deps.AIManagement.ListGateways(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gateways": items})
}

func (a *API) createAIGateway(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountID string         `json:"account_id"`
		Gateway   map[string]any `json:"gateway"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	gateway, err := a.deps.AIManagement.CreateGateway(r.Context(), input.AccountID, input.Gateway)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	a.record(r, "admin", "ai.gateway.create", "ai/gateways", "success", map[string]any{"account_id": input.AccountID})
	writeJSON(w, http.StatusCreated, gateway)
}

func (a *API) deleteAIGateway(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	if err := a.deps.AIManagement.DeleteGateway(r.Context(), accountID, r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	a.record(r, "admin", "ai.gateway.delete", "ai/gateways/"+r.PathValue("id"), "success", map[string]any{"account_id": accountID})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) aiGatewayLogs(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_required", "account_id is required")
		return
	}
	logs, err := a.deps.AIManagement.GatewayLogs(r.Context(), accountID, r.PathValue("id"), queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

func (a *API) aiPlayground(w http.ResponseWriter, r *http.Request) {
	request := r.Clone(r.Context())
	request.URL.Path = "/v1/chat/completions"
	a.deps.AI.Forward(w, request, "admin")
}

func (a *API) protected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.deps.Auth == nil {
			writeError(w, http.StatusServiceUnavailable, "not_configured", "authentication is unavailable")
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "administrator session is required")
			return
		}
		session, err := a.deps.Auth.ValidateSession(r.Context(), cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "administrator session is invalid")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			provided := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(provided), []byte(session.CSRFToken)) != 1 {
				writeError(w, http.StatusForbidden, "csrf_failed", "CSRF token is missing or invalid")
				return
			}
		}
		ctx := context.WithValue(r.Context(), sessionContextKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) record(r *http.Request, actor, action, resource, result string, detail map[string]any) {
	if a.deps.Audit == nil {
		return
	}
	_, _ = a.deps.Audit.Record(r.Context(), audit.Event{
		Actor: actor, Protocol: "admin", Action: action, Resource: resource, Result: result,
		RequestID: requestIDFromContext(r.Context()), Detail: detail,
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func queryLimit(r *http.Request, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func defaultQuery(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func forwardedHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

type sessionContextKey struct{}
type requestIDContextKey struct{}

func sessionFromContext(ctx context.Context) auth.Session {
	session, _ := ctx.Value(sessionContextKey{}).(auth.Session)
	return session
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}
