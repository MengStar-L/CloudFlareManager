package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/audit"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/auth"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestSessionAndAccountAPI(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	authStore := auth.NewStore(db)
	if err := authStore.InitializeAdmin(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{1}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	jobStore := jobs.NewStore(db)
	auditStore := audit.NewStore(db)
	handler := New(Dependencies{
		DB: db, Auth: authStore,
		Accounts: accountStore,
		Jobs:     jobStore, Audit: auditStore, Version: "test",
	})

	login := performJSON(t, handler, http.MethodPost, "/api/v1/session", map[string]string{"password": "correct horse battery staple"}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}
	var loginBody struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || loginBody.CSRFToken == "" {
		t.Fatalf("login response is missing session data")
	}

	withoutCSRF := performJSON(t, handler, http.MethodPost, "/api/v1/accounts", map[string]string{
		"name": "primary", "cloudflare_account_id": "account", "api_token": "token",
	}, cookies[0])
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", withoutCSRF.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", bytes.NewBufferString(`{
		"name":"primary","cloudflare_account_id":"account","api_token":"token"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	request.AddCookie(cookies[0])
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	var createdPayload struct {
		Account accounts.Account `json:"account"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &createdPayload); err != nil {
		t.Fatal(err)
	}
	if createdPayload.Account.ID == "" {
		t.Fatal("create response is missing account id")
	}

	missingUpdateCSRF := performJSON(t, handler, http.MethodPatch,
		"/api/v1/accounts/"+createdPayload.Account.ID+"/credentials", map[string]string{"api_token": "new-token"}, cookies[0])
	if missingUpdateCSRF.Code != http.StatusForbidden {
		t.Fatalf("credential update without CSRF status = %d", missingUpdateCSRF.Code)
	}
	updateRequest := httptest.NewRequest(http.MethodPatch, "/api/v1/accounts/"+createdPayload.Account.ID+"/credentials",
		bytes.NewBufferString(`{"api_token":"new-token","r2_access_key_id":"new-access","r2_secret_access_key":"new-secret"}`))
	updateRequest.Header.Set("Content-Type", "application/json")
	updateRequest.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	updateRequest.AddCookie(cookies[0])
	updateResponse := httptest.NewRecorder()
	handler.ServeHTTP(updateResponse, updateRequest)
	if updateResponse.Code != http.StatusAccepted {
		t.Fatalf("credential update status = %d, body = %s", updateResponse.Code, updateResponse.Body.String())
	}
	for _, secretValue := range []string{"new-token", "new-access", "new-secret"} {
		if bytes.Contains(updateResponse.Body.Bytes(), []byte(secretValue)) {
			t.Fatalf("credential update response leaked %q: %s", secretValue, updateResponse.Body.String())
		}
	}
	var updatedPayload struct {
		Account               accounts.Account `json:"account"`
		Job                   jobs.Job         `json:"job"`
		VerificationScheduled bool             `json:"verification_scheduled"`
	}
	if err := json.Unmarshal(updateResponse.Body.Bytes(), &updatedPayload); err != nil {
		t.Fatal(err)
	}
	if updatedPayload.Account.ID != createdPayload.Account.ID ||
		updatedPayload.Account.CloudflareAccountID != createdPayload.Account.CloudflareAccountID ||
		!updatedPayload.Account.HasR2Credentials || !updatedPayload.VerificationScheduled {
		t.Fatalf("credential update response = %#v", updatedPayload)
	}
	var jobPayload map[string]string
	if err := json.Unmarshal(updatedPayload.Job.Payload, &jobPayload); err != nil {
		t.Fatal(err)
	}
	if updatedPayload.Job.Type != accounts.CapabilityJobType || jobPayload["account_id"] != createdPayload.Account.ID {
		t.Fatalf("capability job = %#v payload=%#v", updatedPayload.Job, jobPayload)
	}
	withSecrets, err := accountStore.Get(context.Background(), createdPayload.Account.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if withSecrets.APIToken != "new-token" || withSecrets.R2AccessKeyID != "new-access" || withSecrets.R2SecretAccessKey != "new-secret" {
		t.Fatalf("stored updated credentials = %#v", withSecrets)
	}
	events, err := auditStore.List(context.Background(), 10, "account.credentials.update")
	if err != nil {
		t.Fatal(err)
	}
	encodedEvents, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || bytes.Contains(encodedEvents, []byte("new-token")) ||
		bytes.Contains(encodedEvents, []byte("new-access")) || bytes.Contains(encodedEvents, []byte("new-secret")) {
		t.Fatalf("credential update audit events = %s", encodedEvents)
	}

	identityRequest := httptest.NewRequest(http.MethodPatch, "/api/v1/accounts/"+createdPayload.Account.ID+"/credentials",
		bytes.NewBufferString(`{"cloudflare_account_id":"different-account","api_token":"another-token"}`))
	identityRequest.Header.Set("Content-Type", "application/json")
	identityRequest.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	identityRequest.AddCookie(cookies[0])
	identityResponse := httptest.NewRecorder()
	handler.ServeHTTP(identityResponse, identityRequest)
	if identityResponse.Code != http.StatusBadRequest {
		t.Fatalf("identity-changing update status = %d, body = %s", identityResponse.Code, identityResponse.Body.String())
	}

	var jobsBeforeR2Only int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = ?`, accounts.CapabilityJobType).Scan(&jobsBeforeR2Only); err != nil {
		t.Fatal(err)
	}
	r2OnlyRequest := httptest.NewRequest(http.MethodPatch, "/api/v1/accounts/"+createdPayload.Account.ID+"/credentials",
		bytes.NewBufferString(`{"r2_access_key_id":"second-access","r2_secret_access_key":"second-secret"}`))
	r2OnlyRequest.Header.Set("Content-Type", "application/json")
	r2OnlyRequest.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	r2OnlyRequest.AddCookie(cookies[0])
	r2OnlyResponse := httptest.NewRecorder()
	handler.ServeHTTP(r2OnlyResponse, r2OnlyRequest)
	if r2OnlyResponse.Code != http.StatusOK {
		t.Fatalf("R2-only credential update status = %d, body = %s", r2OnlyResponse.Code, r2OnlyResponse.Body.String())
	}
	var r2OnlyPayload struct {
		VerificationScheduled bool `json:"verification_scheduled"`
	}
	if err := json.Unmarshal(r2OnlyResponse.Body.Bytes(), &r2OnlyPayload); err != nil {
		t.Fatal(err)
	}
	var jobsAfterR2Only int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = ?`, accounts.CapabilityJobType).Scan(&jobsAfterR2Only); err != nil {
		t.Fatal(err)
	}
	if r2OnlyPayload.VerificationScheduled || jobsAfterR2Only != jobsBeforeR2Only {
		t.Fatalf("R2-only update scheduled API capability detection: before=%d after=%d response=%s",
			jobsBeforeR2Only, jobsAfterR2Only, r2OnlyResponse.Body.String())
	}

	clearR2Request := httptest.NewRequest(http.MethodPatch, "/api/v1/accounts/"+createdPayload.Account.ID+"/credentials",
		bytes.NewBufferString(`{"clear_r2_credentials":true}`))
	clearR2Request.Header.Set("Content-Type", "application/json")
	clearR2Request.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	clearR2Request.AddCookie(cookies[0])
	clearR2Response := httptest.NewRecorder()
	handler.ServeHTTP(clearR2Response, clearR2Request)
	if clearR2Response.Code != http.StatusOK {
		t.Fatalf("clear R2 credentials status = %d, body = %s", clearR2Response.Code, clearR2Response.Body.String())
	}
	var clearR2Payload struct {
		Account               accounts.Account `json:"account"`
		VerificationScheduled bool             `json:"verification_scheduled"`
	}
	if err := json.Unmarshal(clearR2Response.Body.Bytes(), &clearR2Payload); err != nil {
		t.Fatal(err)
	}
	if clearR2Payload.Account.HasR2Credentials || clearR2Payload.VerificationScheduled {
		t.Fatalf("clear R2 credentials response = %#v", clearR2Payload)
	}
	withSecrets, err = accountStore.Get(context.Background(), createdPayload.Account.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if withSecrets.APIToken != "new-token" || withSecrets.R2AccessKeyID != "" || withSecrets.R2SecretAccessKey != "" {
		t.Fatalf("clear R2 credentials changed the wrong stored values: %#v", withSecrets)
	}
	clearEvents, err := auditStore.List(context.Background(), 10, "account.credentials.update")
	if err != nil {
		t.Fatal(err)
	}
	foundRemovalAudit := false
	for _, event := range clearEvents {
		if event.Detail["r2_credentials_action"] == "removed" {
			foundRemovalAudit = true
			break
		}
	}
	if !foundRemovalAudit {
		t.Fatalf("clear R2 credentials audit event not found: %#v", clearEvents)
	}

	var secretsBeforeMissing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM encrypted_secrets`).Scan(&secretsBeforeMissing); err != nil {
		t.Fatal(err)
	}
	missingRequest := httptest.NewRequest(http.MethodPatch, "/api/v1/accounts/missing/credentials",
		bytes.NewBufferString(`{"api_token":"unused-token"}`))
	missingRequest.Header.Set("Content-Type", "application/json")
	missingRequest.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	missingRequest.AddCookie(cookies[0])
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingRequest)
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf("missing account credential update status = %d, body = %s", missingResponse.Code, missingResponse.Body.String())
	}
	var secretsAfterMissing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM encrypted_secrets`).Scan(&secretsAfterMissing); err != nil {
		t.Fatal(err)
	}
	if secretsAfterMissing != secretsBeforeMissing {
		t.Fatalf("missing account update created secrets: before=%d after=%d", secretsBeforeMissing, secretsAfterMissing)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	listRequest.AddCookie(cookies[0])
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || !bytes.Contains(listResponse.Body.Bytes(), []byte(`"name":"primary"`)) {
		t.Fatalf("list response = %d %s", listResponse.Code, listResponse.Body.String())
	}
}

func TestDeleteAccountReturnsStructuredActionableBlockers(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{16}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cf-account", APIToken: "api-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO r2_physical_buckets(id, account_id, bucket_name, created_at, updated_at)
		VALUES('bucket-id', ?, 'important-files', 1, 1)`, account.ID); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodDelete, "/api/v1/accounts/"+account.ID, nil)
	request.SetPathValue("id", account.ID)
	response := httptest.NewRecorder()
	(&API{deps: Dependencies{Accounts: accountStore}}).deleteAccount(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Blockers []accounts.DeletionBlocker `json:"blockers"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "account_in_use" || payload.Error.Message == "" || len(payload.Error.Details.Blockers) != 1 {
		t.Fatalf("response payload = %#v", payload)
	}
	blocker := payload.Error.Details.Blockers[0]
	if blocker.Kind != accounts.DeletionBlockerR2Bucket || blocker.Count != 1 ||
		len(blocker.Items) != 1 || blocker.Items[0].ID != "bucket-id" || blocker.Items[0].Name != "important-files" {
		t.Fatalf("blocker = %#v", blocker)
	}
}

func TestDeleteAccountDoesNotMisreportInternalErrorsAsResourceConflicts(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{17}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodDelete, "/api/v1/accounts/account-id", nil)
	request.SetPathValue("id", "account-id")
	response := httptest.NewRecorder()
	(&API{deps: Dependencies{Accounts: accountStore}}).deleteAccount(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "internal_error" || payload.Error.Message != "暂时无法删除账号，请稍后重试。" {
		t.Fatalf("response payload = %#v", payload)
	}
}

func TestHealthEndpointsDoNotRequireAuthentication(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := New(Dependencies{DB: db})
	for _, path := range []string{"/healthz", "/readyz"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
	}
}

func TestListJobsFiltersBeforeApplyingLimit(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	authStore := auth.NewStore(db)
	if err := authStore.InitializeAdmin(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(db)
	wanted, created, err := jobStore.EnqueueUnique(context.Background(), "r2.bucket.delete-remote", "account/default/gamesync", "", nil, 4)
	if err != nil || !created {
		t.Fatalf("enqueue wanted job: created=%v err=%v", created, err)
	}
	if _, err := db.Exec(`UPDATE jobs SET created_at = 1 WHERE id = ?`, wanted.ID); err != nil {
		t.Fatal(err)
	}
	for range 501 {
		if _, err := jobStore.Enqueue(context.Background(), "unrelated", nil, 1); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(Dependencies{DB: db, Auth: authStore, Jobs: jobStore})
	login := performJSON(t, handler, http.MethodPost, "/api/v1/session", map[string]string{"password": "correct horse battery staple"}, nil)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/jobs?limit=1&type=r2.bucket.delete-remote&resource_key_prefix=account%2Fdefault%2F", nil)
	request.AddCookie(login.Result().Cookies()[0])
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Jobs []jobs.Job `json:"jobs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Jobs) != 1 || payload.Jobs[0].ID != wanted.ID {
		t.Fatalf("filtered jobs = %#v, want %q", payload.Jobs, wanted.ID)
	}
}

func TestSystemEndpointsRequireAuthenticationAndUseRequestOrigin(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	authStore := auth.NewStore(db)
	if err := authStore.InitializeAdmin(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{DB: db, Auth: authStore, LogicalBucket: "unified-storage"})

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/v1/system/endpoints", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticated.Code)
	}

	login := performJSON(t, handler, http.MethodPost, "/api/v1/session", map[string]string{"password": "correct horse battery staple"}, nil)
	cookies := login.Result().Cookies()
	if login.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("login status = %d, cookies = %d", login.Code, len(cookies))
	}
	request := httptest.NewRequest(http.MethodGet, "http://manager.example.com/api/v1/system/endpoints", nil)
	request.Host = "cloud.example.com"
	request.Header.Set("X-Forwarded-Proto", "https")
	request.AddCookie(cookies[0])
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"panel_url": "https://cloud.example.com/", "s3_endpoint": "https://cloud.example.com",
		"s3_bucket": "unified-storage", "webdav_url": "https://cloud.example.com/",
		"ai_base_url": "https://cloud.example.com/v1",
	}
	for key, value := range want {
		if payload[key] != value {
			t.Fatalf("%s = %q, want %q", key, payload[key], value)
		}
	}
}

func TestRemoteBucketViewsUsesLocalStatsForManagedBuckets(t *testing.T) {
	t.Parallel()
	api, account := newR2StatsFixture(t, http.StatusOK)
	views, summary, err := api.remoteBucketViews(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]remoteBucketView, len(views))
	for _, view := range views {
		byName[view.Name] = view
	}
	if got := byName["managed"]; got.PayloadBytes == nil || *got.PayloadBytes != 12 || got.ObjectCount == nil || *got.ObjectCount != 1 {
		t.Fatalf("managed view = %#v", got)
	}
	if got := byName["empty"]; got.PayloadBytes == nil || *got.PayloadBytes != 0 || got.ObjectCount == nil || *got.ObjectCount != 0 {
		t.Fatalf("empty managed view = %#v", got)
	}
	if got := byName["external"]; got.PayloadBytes == nil || *got.PayloadBytes != 33 || got.ObjectCount == nil || *got.ObjectCount != 3 {
		t.Fatalf("external view = %#v", got)
	}
	if got := byName["missing"]; !got.RemoteMissing || got.PayloadBytes == nil || *got.PayloadBytes != 5 || got.ObjectCount == nil || *got.ObjectCount != 1 {
		t.Fatalf("remote-missing managed view = %#v", got)
	}
	if got := summary["total_bytes"]; got != int64(42) {
		t.Fatalf("remote total_bytes = %#v", got)
	}
}

func TestRemoteBucketViewsKeepsManagedStatsWhenAnalyticsFails(t *testing.T) {
	t.Parallel()
	api, account := newR2StatsFixture(t, http.StatusBadGateway)
	views, summary, err := api.remoteBucketViews(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]remoteBucketView, len(views))
	for _, view := range views {
		byName[view.Name] = view
	}
	managed := byName["managed"]
	if managed.PayloadBytes == nil || *managed.PayloadBytes != 12 || managed.ObjectCount == nil || *managed.ObjectCount != 1 {
		t.Fatalf("managed view during analytics failure = %#v", managed)
	}
	if external := byName["external"]; external.PayloadBytes != nil || external.ObjectCount != nil {
		t.Fatalf("external view should not invent analytics values: %#v", external)
	}
	if _, ok := summary["usage_error"]; !ok {
		t.Fatalf("summary should report analytics failure: %#v", summary)
	}
}

func newR2StatsFixture(t *testing.T, analyticsStatus int) (*API, accounts.Account) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{14}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err = accountStore.Get(context.Background(), account.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	index := r2.NewStore(db, r2.Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	managed, err := index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: account.ID, Name: "managed"})
	if err != nil {
		t.Fatal(err)
	}
	object, err := index.ReservePut(context.Background(), r2.ObjectInput{Key: "upload.bin", Size: 12})
	if err != nil {
		t.Fatal(err)
	}
	if object.BucketID != managed.ID {
		t.Fatalf("object bucket = %s", object.BucketID)
	}
	if err := index.CommitPut(context.Background(), object.ObjectID, "etag", 12); err != nil {
		t.Fatal(err)
	}
	if _, err := index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: account.ID, Name: "empty"}); err != nil {
		t.Fatal(err)
	}
	missing, err := index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: account.ID, Name: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.AdoptObject(context.Background(), missing.ID, r2.RemoteObject{Key: "missing.bin", Size: 5}); err != nil {
		t.Fatal(err)
	}
	remote := newR2StatsRemote(t, analyticsStatus)
	t.Cleanup(remote.Close)
	return &API{deps: Dependencies{
		R2:     index,
		Remote: accounts.RemoteClient{BaseURL: remote.URL, Client: remote.Client()},
	}}, account
}

func newR2StatsRemote(t *testing.T, analyticsStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/cloudflare/r2/buckets":
			if r.Header.Get("cf-r2-jurisdiction") != "default" {
				_, _ = w.Write([]byte(`{"success":true,"result":[],"result_info":{"cursor":""}}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"result":[{"name":"managed"},{"name":"empty"},{"name":"external"}],"result_info":{"cursor":""}}`))
		case "/graphql":
			if analyticsStatus != http.StatusOK {
				w.WriteHeader(analyticsStatus)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"viewer":{"accounts":[{"r2StorageAdaptiveGroups":[{"dimensions":{"bucketName":"managed"},"max":{"payloadSize":0,"metadataSize":7,"objectCount":0}},{"dimensions":{"bucketName":"empty"},"max":{"payloadSize":9,"metadataSize":1,"objectCount":1}},{"dimensions":{"bucketName":"external"},"max":{"payloadSize":33,"metadataSize":4,"objectCount":3}}]}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func performJSON(t *testing.T, handler http.Handler, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
