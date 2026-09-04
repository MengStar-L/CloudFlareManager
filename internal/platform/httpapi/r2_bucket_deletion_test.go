package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/audit"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/auth"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

const (
	deletionTestPassword = "correct horse battery staple"
	deletionTestToken    = "cloudflare-api-token-must-stay-secret"
)

type r2DeletionAPIFixture struct {
	db           *sql.DB
	handler      http.Handler
	account      accounts.Account
	accountStore *accounts.Store
	index        *r2.Store
	jobs         *jobs.Store
	audit        *audit.Store
	cookie       *http.Cookie
	csrf         string
	remote       *httptest.Server
	remoteFn     func(http.ResponseWriter, *http.Request)
}

func newR2DeletionAPIFixture(t *testing.T, remoteFn func(http.ResponseWriter, *http.Request)) *r2DeletionAPIFixture {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	authStore := auth.NewStore(db)
	if err := authStore.InitializeAdmin(context.Background(), deletionTestPassword); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{22}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare-account", APIToken: deletionTestToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	index := r2.NewStore(db, r2.Limits{StorageBytes: 1 << 30, ClassA: 1000, ClassB: 1000})
	jobStore := jobs.NewStore(db)
	auditStore := audit.NewStore(db)
	fixture := &r2DeletionAPIFixture{
		db: db, account: account, accountStore: accountStore, index: index, jobs: jobStore,
		audit: auditStore, remoteFn: remoteFn,
	}
	fixture.remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixture.remoteFn != nil {
			fixture.remoteFn(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fixture.remote.Close)
	fixture.handler = New(Dependencies{
		DB: db, Auth: authStore, Accounts: accountStore, Jobs: jobStore, Audit: auditStore, R2: index,
		Remote: accounts.RemoteClient{BaseURL: fixture.remote.URL, Client: fixture.remote.Client()},
	})
	login := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/session", map[string]string{"password": deletionTestPassword}, nil)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}
	var loginBody struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	fixture.cookie, fixture.csrf = login.Result().Cookies()[0], loginBody.CSRF
	return fixture
}

func (f *r2DeletionAPIFixture) request(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", f.csrf)
	request.AddCookie(f.cookie)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func validBucketDeletionInput(accountID string, mode r2.BucketDeletionMode) map[string]string {
	return map[string]string{
		"account_id": accountID, "jurisdiction": "default", "mode": string(mode),
		"admin_password": deletionTestPassword, "confirmation_name": "gamesync",
	}
}

func writeDeletionIdentity(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"success":true,"result":{"name":"gamesync","creation_date":"2026-08-30T16:00:00+08:00","jurisdiction":"default"}}`))
}

func TestCreateR2BucketDeletionRequiresAdminPassword(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	input := validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly)
	input["admin_password"] = "wrong"
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions", input)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"confirmation_failed"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if items, err := fixture.jobs.List(context.Background(), 10, ""); err != nil || len(items) != 0 {
		t.Fatalf("jobs = %#v, err = %v", items, err)
	}
}

func TestCreateR2BucketDeletionRequiresExactName(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	input := validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyAndDelete)
	input["confirmation_name"] = "GameSync"
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions", input)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"confirmation_name_mismatch"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestCreateR2BucketDeletionRejectsUnsupportedJurisdiction(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	input := validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly)
	input["jurisdiction"] = "eu"
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions", input)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"unsupported_jurisdiction"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestCreateR2BucketDeletionSnapshotsRemoteIdentity(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyAndDelete))
	if response.Code != http.StatusAccepted {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	items, err := fixture.jobs.List(context.Background(), 10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("jobs = %#v, err = %v", items, err)
	}
	var payload r2.BucketDeletionPayload
	if err := json.Unmarshal(items[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ExpectedCreationDate != "2026-08-30T08:00:00Z" || payload.CloudflareAccountID != "cloudflare-account" {
		t.Fatalf("payload = %#v", payload)
	}
	combined := response.Body.String() + string(items[0].Payload)
	events, err := fixture.audit.List(context.Background(), 10, "r2.bucket.delete-remote")
	if err != nil || len(events) != 1 {
		t.Fatalf("audit = %#v, err = %v", events, err)
	}
	if events[0].Result != "accepted" {
		t.Fatalf("enqueue audit result = %q", events[0].Result)
	}
	auditJSON, _ := json.Marshal(events[0].Detail)
	combined += string(auditJSON)
	if strings.Contains(combined, deletionTestPassword) || strings.Contains(combined, deletionTestToken) {
		t.Fatal("response, job payload, or audit detail leaked a secret")
	}
}

func TestCreateR2BucketDeletionDoesNotEnqueueAfterAccountDeletedDuringRemoteCheck(t *testing.T) {
	t.Parallel()

	remoteStarted := make(chan struct{}, 1)
	releaseRemote := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseRemote)
		}
	}()
	fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, request *http.Request) {
		select {
		case remoteStarted <- struct{}{}:
		default:
		}
		<-releaseRemote
		writeDeletionIdentity(w, request)
	})

	data, err := json.Marshal(validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", fixture.csrf)
	request.AddCookie(fixture.cookie)
	response := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() {
		fixture.handler.ServeHTTP(response, request)
		close(requestDone)
	}()

	select {
	case <-remoteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("bucket deletion request did not reach the remote identity check")
	}
	if err := fixture.accountStore.Delete(context.Background(), fixture.account.ID); err != nil {
		t.Fatalf("delete account while remote identity check is in flight: %v", err)
	}
	close(releaseRemote)
	released = true
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("bucket deletion request did not finish after releasing the remote response")
	}

	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"not_found"`) {
		t.Fatalf("response = %d %s, want account not found", response.Code, response.Body.String())
	}
	items, err := fixture.jobs.List(context.Background(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("jobs after account deletion = %#v, want none", items)
	}
}

func TestCreateR2BucketDeletionReturnsExistingActiveJob(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	input := validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly)
	first := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions", input)
	second := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions", input)
	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("responses = %d/%d, second = %s", first.Code, second.Code, second.Body.String())
	}
	var firstBody, secondBody struct {
		Job     jobs.Job `json:"job"`
		Created bool     `json:"created"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &firstBody)
	_ = json.Unmarshal(second.Body.Bytes(), &secondBody)
	if !firstBody.Created || secondBody.Created || firstBody.Job.ID == "" || firstBody.Job.ID != secondBody.Job.ID {
		t.Fatalf("first = %#v, second = %#v", firstBody, secondBody)
	}
}

func TestCreateR2BucketDeletionReturnsLinkedRunningJobWithoutEnqueue(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	local, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{
		AccountID: fixture.account.ID, Name: "gamesync",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyAndDelete)
	first := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions", input)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first response = %d %s", first.Code, first.Body.String())
	}
	claimed, err := fixture.jobs.Claim(context.Background(), time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim first job = %#v, %v", claimed, err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE r2_physical_buckets
		SET lifecycle_state = ?, deletion_job_id = ? WHERE id = ?`, r2.BucketDeleting, claimed.ID, local.ID); err != nil {
		t.Fatal(err)
	}

	second := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions", input)
	if second.Code != http.StatusAccepted {
		t.Fatalf("second response = %d %s", second.Code, second.Body.String())
	}
	var body struct {
		Job     jobs.Job `json:"job"`
		Created bool     `json:"created"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Created || body.Job.ID != claimed.ID || body.Job.Status != jobs.StatusRunning {
		t.Fatalf("second response body = %#v", body)
	}
	items, err := fixture.jobs.List(context.Background(), 10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("jobs = %#v, err = %v", items, err)
	}
}

func TestCreateR2BucketDeletionRetryInheritsIdentityAndCountersWithFreshRoundBudget(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	local, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{
		AccountID: fixture.account.ID, Name: "gamesync",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyAndDelete))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first response = %d %s", first.Code, first.Body.String())
	}
	claimed, err := fixture.jobs.Claim(context.Background(), time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim first job = %#v, %v", claimed, err)
	}
	var parentPayload r2.BucketDeletionPayload
	if err := json.Unmarshal(claimed.Payload, &parentPayload); err != nil {
		t.Fatal(err)
	}
	parentPayload.RemoteMutated = true
	parentPayload.DeletedObjects = 12
	parentPayload.AbortedMultipart = 3
	parentPayload.DeleteRounds = 4
	if err := fixture.jobs.SetPayload(context.Background(), claimed.ID, parentPayload); err != nil {
		t.Fatal(err)
	}
	if err := fixture.jobs.FailPermanent(context.Background(), claimed.ID, "partial_delete_failed", "partial"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE r2_physical_buckets
		SET lifecycle_state = ?, deletion_job_id = ? WHERE id = ?`, r2.BucketDeleteFailed, claimed.ID, local.ID); err != nil {
		t.Fatal(err)
	}

	second := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyAndDelete))
	if second.Code != http.StatusAccepted {
		t.Fatalf("retry response = %d %s", second.Code, second.Body.String())
	}
	items, err := fixture.jobs.List(context.Background(), 10, "")
	if err != nil || len(items) != 2 {
		t.Fatalf("jobs = %#v, err = %v", items, err)
	}
	var retry jobs.Job
	for _, item := range items {
		if item.ParentJobID == claimed.ID {
			retry = item
			break
		}
	}
	if retry.ID == "" {
		t.Fatalf("retry job not found in %#v", items)
	}
	var retryPayload r2.BucketDeletionPayload
	if err := json.Unmarshal(retry.Payload, &retryPayload); err != nil {
		t.Fatal(err)
	}
	if retry.ParentJobID != claimed.ID || retryPayload.ParentJobID != claimed.ID ||
		retryPayload.ExpectedCreationDate != parentPayload.ExpectedCreationDate || !retryPayload.RemoteMutated ||
		retryPayload.DeletedObjects != 12 || retryPayload.AbortedMultipart != 3 || retryPayload.DeleteRounds != 0 {
		t.Fatalf("retry job = %#v, payload = %#v", retry, retryPayload)
	}
}

func TestCreateR2BucketDeletionRetryRejectsSameNameRecreatedBucket(t *testing.T) {
	t.Parallel()
	var creationDate atomic.Value
	creationDate.Store("2026-08-30T16:00:00+08:00")
	fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":{"name":"gamesync","creation_date":"` + creationDate.Load().(string) + `","jurisdiction":"default"}}`))
	})
	local, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{
		AccountID: fixture.account.ID, Name: "gamesync",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyAndDelete))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first response = %d %s", first.Code, first.Body.String())
	}
	claimed, err := fixture.jobs.Claim(context.Background(), time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim first job = %#v, %v", claimed, err)
	}
	var payload r2.BucketDeletionPayload
	if err := json.Unmarshal(claimed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.RemoteMutated = true
	if err := fixture.jobs.SetPayload(context.Background(), claimed.ID, payload); err != nil {
		t.Fatal(err)
	}
	if err := fixture.jobs.FailPermanent(context.Background(), claimed.ID, "partial_delete_failed", "partial"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE r2_physical_buckets
		SET lifecycle_state = ?, deletion_job_id = ? WHERE id = ?`, r2.BucketDeleteFailed, claimed.ID, local.ID); err != nil {
		t.Fatal(err)
	}
	creationDate.Store("2026-08-31T08:00:00Z")

	retry := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyAndDelete))
	if retry.Code != http.StatusConflict || !strings.Contains(retry.Body.String(), `"code":"bucket_identity_changed"`) {
		t.Fatalf("retry response = %d %s", retry.Code, retry.Body.String())
	}
	items, err := fixture.jobs.List(context.Background(), 10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("jobs after rejected retry = %#v, err = %v", items, err)
	}
}

func TestCreateR2BucketDeletionHandlesRemoteMissingManagedBucket(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10006,"message":"not found"}]}`))
	})
	local, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: fixture.account.ID, Name: "gamesync"})
	if err != nil {
		t.Fatal(err)
	}
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly))
	if response.Code != http.StatusAccepted {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	items, _ := fixture.jobs.List(context.Background(), 10, "")
	var payload r2.BucketDeletionPayload
	if len(items) != 1 || json.Unmarshal(items[0].Payload, &payload) != nil {
		t.Fatalf("jobs = %#v", items)
	}
	if !payload.RemoteMissingAtEnqueue || payload.LocalBucketID != local.ID || payload.ExpectedCreationDate != "" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestCreateR2BucketDeletionDoesNotTreatProxy404AsMissingBucket(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "proxy route not found", http.StatusNotFound)
	})
	local, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{
		AccountID: fixture.account.ID, Name: "gamesync",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly))
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"code":"cloudflare_unavailable"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if items, listErr := fixture.jobs.List(context.Background(), 10, ""); listErr != nil || len(items) != 0 {
		t.Fatalf("jobs = %#v, err = %v", items, listErr)
	}
	if got, getErr := fixture.index.GetBucket(context.Background(), local.ID); getErr != nil || got.LifecycleState != r2.BucketActive {
		t.Fatalf("local bucket = %#v, err = %v", got, getErr)
	}
}

func TestCreateR2BucketDeletionCanRecoverFailedJobStuckDeleting(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	local, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{
		AccountID: fixture.account.ID, Name: "gamesync",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first response = %d %s", first.Code, first.Body.String())
	}
	claimed, err := fixture.jobs.Claim(context.Background(), time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim first job = %#v, %v", claimed, err)
	}
	if err := fixture.jobs.FailPermanent(context.Background(), claimed.ID, "local_finalize_failed", "local state update failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE r2_physical_buckets
		SET lifecycle_state = ?, deletion_job_id = ? WHERE id = ?`, r2.BucketDeleting, claimed.ID, local.ID); err != nil {
		t.Fatal(err)
	}

	second := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly))
	if second.Code != http.StatusAccepted {
		t.Fatalf("recovery response = %d %s", second.Code, second.Body.String())
	}
	items, err := fixture.jobs.List(context.Background(), 10, "")
	if err != nil || len(items) != 2 {
		t.Fatalf("jobs = %#v, err = %v", items, err)
	}
	var retry jobs.Job
	for _, item := range items {
		if item.ParentJobID == claimed.ID {
			retry = item
		}
	}
	if retry.ID == "" {
		t.Fatalf("recovery job not found in %#v", items)
	}
}

func TestCreateR2BucketDeletionRejectsUnverifiableIdentity(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"result":{"name":"gamesync","jurisdiction":"default"}}`))
	})
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"bucket_identity_unverifiable"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestCreateR2BucketDeletionMapsCloudflarePermissionCodeToChineseError(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"authentication error"}]}`))
	})
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/remote-buckets/gamesync/deletions",
		validBucketDeletionInput(fixture.account.ID, r2.BucketDeletionEmptyOnly))
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"permission_denied"`) ||
		!strings.Contains(response.Body.String(), "Cloudflare API Token 无权读取或删除") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestDeleteLocalR2BucketExplainsNonEmptyFailureInChinese(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	bucket, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: fixture.account.ID, Name: "gamesync"})
	if err != nil {
		t.Fatal(err)
	}
	object, err := fixture.index.ReservePut(context.Background(), r2.ObjectInput{Key: "save.dat", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.index.CommitPut(context.Background(), object.ObjectID, "etag", 1); err != nil {
		t.Fatal(err)
	}
	response := fixture.request(t, http.MethodDelete, "/api/v1/r2/buckets/"+bucket.ID, nil)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"bucket_not_empty"`) ||
		!strings.Contains(response.Body.String(), "请先删除桶内所有文件") || !strings.Contains(response.Body.String(), "一键清空并删除桶") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestCreateLocalR2BucketRejectsActiveRemoteDeletionInChinese(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, writeDeletionIdentity)
	resourceKey := fixture.account.ID + "/default/gamesync"
	if _, created, err := fixture.jobs.EnqueueUnique(context.Background(), r2.BucketDeletionJobType,
		resourceKey, "", map[string]string{"bucket_name": "gamesync"}, 4); err != nil || !created {
		t.Fatalf("enqueue remote deletion job: created=%v, err=%v", created, err)
	}
	response := fixture.request(t, http.MethodPost, "/api/v1/r2/buckets", r2.CreateBucketInput{
		AccountID: fixture.account.ID, Name: "gamesync",
	})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"bucket_deleting"`) ||
		!strings.Contains(response.Body.String(), "远端删除任务正在运行") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestRemoteBucketViewsExposeDeletionLifecycle(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/r2/buckets"):
			if r.Header.Get("cf-r2-jurisdiction") != "default" {
				_, _ = w.Write([]byte(`{"success":true,"result":[],"result_info":{"cursor":""}}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"result":[{"name":"gamesync","creation_date":"2026-08-30T08:00:00Z","jurisdiction":"default"}],"result_info":{"cursor":""}}`))
		case r.URL.Path == "/graphql":
			_, _ = w.Write([]byte(`{"data":{"viewer":{"accounts":[]}}}`))
		default:
			http.NotFound(w, r)
		}
	})
	bucket, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: fixture.account.ID, Name: "gamesync"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := fixture.jobs.Enqueue(context.Background(), r2.BucketDeletionJobType, map[string]string{"bucket_name": "gamesync"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`UPDATE r2_physical_buckets SET lifecycle_state = ?, deletion_job_id = ? WHERE id = ?`,
		r2.BucketDeleting, job.ID, bucket.ID); err != nil {
		t.Fatal(err)
	}
	// remoteBucketViews only needs the already decrypted account used by the fixture.
	fullAccount := fixture.account
	fullAccount.APIToken = deletionTestToken
	views, _, err := (&API{deps: Dependencies{R2: fixture.index, Jobs: fixture.jobs,
		Remote: accounts.RemoteClient{BaseURL: fixture.remote.URL, Client: fixture.remote.Client()}}}).remoteBucketViews(context.Background(), fullAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Jurisdiction != "default" || views[0].LifecycleState != r2.BucketDeleting ||
		views[0].DeletionJobID != job.ID || views[0].DeletionStatus != jobs.StatusPending {
		t.Fatalf("views = %#v", views)
	}
}

func TestRemoteBucketViewsKeepsJurisdictionsSeparate(t *testing.T) {
	t.Parallel()
	fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			_, _ = w.Write([]byte(`{"data":{"viewer":{"accounts":[{"r2StorageAdaptiveGroups":[{"dimensions":{"bucketName":"gamesync"},"max":{"payloadSize":99,"metadataSize":5,"objectCount":7}}]}]}}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/r2/buckets") {
			jurisdiction := r.Header.Get("cf-r2-jurisdiction")
			if jurisdiction == "default" || jurisdiction == "eu" {
				_, _ = w.Write([]byte(`{"success":true,"result":[{"name":"gamesync","creation_date":"2026-08-30T08:00:00Z"}],"result_info":{"cursor":""}}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"result":[],"result_info":{"cursor":""}}`))
			return
		}
		http.NotFound(w, r)
	})
	bucket, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: fixture.account.ID, Name: "gamesync"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.index.AdoptObject(context.Background(), bucket.ID, r2.RemoteObject{Key: "save.dat", Size: 3}); err != nil {
		t.Fatal(err)
	}
	fullAccount := fixture.account
	fullAccount.APIToken = deletionTestToken
	views, _, err := (&API{deps: Dependencies{R2: fixture.index, Jobs: fixture.jobs,
		Remote: accounts.RemoteClient{BaseURL: fixture.remote.URL, Client: fixture.remote.Client()}}}).remoteBucketViews(context.Background(), fullAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("views = %#v", views)
	}
	byJurisdiction := map[string]remoteBucketView{}
	for _, view := range views {
		byJurisdiction[view.Jurisdiction] = view
	}
	defaultView, euView := byJurisdiction["default"], byJurisdiction["eu"]
	if !defaultView.Managed || defaultView.PayloadBytes == nil || *defaultView.PayloadBytes != 3 {
		t.Fatalf("default view = %#v", defaultView)
	}
	if euView.Managed || euView.PayloadBytes != nil || euView.ObjectCount != nil {
		t.Fatalf("eu view must not inherit default usage or membership: %#v", euView)
	}
}
