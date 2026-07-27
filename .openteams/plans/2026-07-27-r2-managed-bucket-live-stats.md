# R2 Managed Bucket Live Stats Implementation Plan

**Goal:** Make managed R2 bucket rows show object count and storage bytes from the local committed-object index immediately after refresh or tab re-entry, while preserving Cloudflare Analytics for unmanaged buckets and account-wide quota totals.

**Architecture:** Add one read-only aggregate query to the R2 store and overlay its per-bucket results in `remoteBucketViews` after Cloudflare usage has been read. Keep account totals based on the remote analytics response, and make the React overview tab reload on every transition into that tab without adding polling.

**Tech Stack:** Go 1.24, SQLite, `net/http`/`httptest`, React 19, TypeScript, Vite

---

## File Map

- Modify `internal/modules/r2/store.go`: define the per-bucket aggregate result and query committed objects.
- Modify `internal/modules/r2/store_test.go`: verify aggregate behavior for committed, pending, replacing, deleting, and moved objects.
- Modify `internal/platform/httpapi/api.go`: merge local managed-bucket stats with Cloudflare bucket and usage data.
- Modify `internal/platform/httpapi/api_test.go`: verify managed/local and unmanaged/remote data sources, including analytics failure.
- Modify `web/src/pages/StoragePage.tsx`: reload the overview whenever the user enters its tab.
- Keep `.openteams/specs/2026-07-27-r2-managed-bucket-live-stats-design.html` and this plan with the implementation history.

### Task 1: Add Live Aggregate Queries to the R2 Store

**Files:**
- Modify: `internal/modules/r2/store_test.go`
- Modify: `internal/modules/r2/store.go:52-82`
- Modify: `internal/modules/r2/store.go:112-127`

- **Step 1: Write the failing store test**

Add this test to `internal/modules/r2/store_test.go`. It creates only the source bucket until all writes are committed so placement is deterministic, then exercises pending, replacement, deletion-state, and rebalance mapping semantics.

```go
func TestStoreListBucketObjectStatsReflectsCommittedIndex(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{13}, secret.KeySize))
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
	store := NewStore(db, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	source, err := store.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "source"})
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.ReservePut(context.Background(), ObjectInput{Key: "first.bin", Size: 12})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(context.Background(), first.ObjectID, "first-etag", 12); err != nil {
		t.Fatal(err)
	}
	second, err := store.ReservePut(context.Background(), ObjectInput{Key: "second.bin", Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(context.Background(), second.ObjectID, "second-etag", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReservePut(context.Background(), ObjectInput{Key: "pending.bin", Size: 99}); err != nil {
		t.Fatal(err)
	}

	stats, err := store.ListBucketObjectStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stats[source.ID]; got.StorageBytes != 17 || got.ObjectCount != 2 {
		t.Fatalf("source stats = %#v", got)
	}

	replacement, err := store.ReservePut(context.Background(), ObjectInput{Key: "first.bin", Size: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(context.Background(), replacement.ObjectID, "replacement-etag", 20); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDelete(context.Background(), "second.bin"); err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MoveObjectMapping(context.Background(), replacement.ObjectID, target.ID, "moved-etag"); err != nil {
		t.Fatal(err)
	}

	stats, err = store.ListBucketObjectStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stats[source.ID]; ok {
		t.Fatalf("source should have no committed objects: %#v", stats[source.ID])
	}
	if got := stats[target.ID]; got.StorageBytes != 20 || got.ObjectCount != 1 {
		t.Fatalf("target stats = %#v", got)
	}
}
```

- **Step 2: Run the test and verify it fails**

Run:

```powershell
go test ./internal/modules/r2 -run TestStoreListBucketObjectStatsReflectsCommittedIndex -count=1
```

Expected: compilation fails because `ListBucketObjectStats` and `BucketObjectStats` do not exist.

- **Step 3: Add the aggregate type and query**

Add near `ObjectList` in `internal/modules/r2/store.go`:

```go
type BucketObjectStats struct {
	StorageBytes int64
	ObjectCount  int64
}
```

Add after `ListBuckets`:

```go
func (s *Store) ListBucketObjectStats(ctx context.Context) (map[string]BucketObjectStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT physical_bucket_id, COALESCE(SUM(size), 0), COUNT(*)
		FROM r2_objects WHERE state = ? GROUP BY physical_bucket_id`, StateCommitted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := make(map[string]BucketObjectStats)
	for rows.Next() {
		var bucketID string
		var item BucketObjectStats
		if err := rows.Scan(&bucketID, &item.StorageBytes, &item.ObjectCount); err != nil {
			return nil, err
		}
		stats[bucketID] = item
	}
	return stats, rows.Err()
}
```

- **Step 4: Format and rerun the focused test**

Run:

```powershell
gofmt -w internal/modules/r2/store.go internal/modules/r2/store_test.go
go test ./internal/modules/r2 -run TestStoreListBucketObjectStatsReflectsCommittedIndex -count=1
```

Expected: PASS.

- **Step 5: Commit the store change**

```powershell
git add internal/modules/r2/store.go internal/modules/r2/store_test.go
git commit -m "Add live per-bucket object statistics"
```

### Task 2: Overlay Managed Bucket Rows in the HTTP API

**Files:**
- Modify: `internal/platform/httpapi/api_test.go`
- Modify: `internal/platform/httpapi/api.go:441-507`

- **Step 1: Add a Cloudflare response helper for API tests**

Add the `r2` import and this helper to `internal/platform/httpapi/api_test.go`:

```go
func newR2StatsRemote(t *testing.T, analyticsStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/cloudflare/r2/buckets":
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
```

- **Step 2: Add the API fixture and failing managed/unmanaged merge test**

Add a fixture that builds a real SQLite R2 index. It creates a populated managed bucket, an empty managed bucket, and a managed bucket that is absent from the remote list:

```go
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
		R2: index,
		Remote: accounts.RemoteClient{BaseURL: remote.URL, Client: remote.Client()},
	}}, account
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
```

- **Step 3: Run the merge test and verify it fails**

Run:

```powershell
go test ./internal/platform/httpapi -run TestRemoteBucketViewsUsesLocalStatsForManagedBuckets -count=1
```

Expected: FAIL because the managed row reports Cloudflare's delayed `0` bytes and `0` objects instead of `12` and `1`.

- **Step 4: Load local stats and overlay managed rows**

In `remoteBucketViews`, load local statistics immediately after `ListBuckets`:

```go
	localStats, err := a.deps.R2.ListBucketObjectStats(ctx)
	if err != nil {
		return nil, nil, err
	}
```

Keep the existing remote usage assignment and `totalBytes += payload` unchanged so account totals remain remote. After that block, overlay managed rows:

```go
		if entry, ok := managed[bucket.Name]; ok {
			stats := localStats[entry.id]
			payload, objects := stats.StorageBytes, stats.ObjectCount
			view.PayloadBytes, view.ObjectCount = &payload, &objects
		}
```

When appending a locally managed bucket that is missing remotely, also include explicit local values:

```go
			stats := localStats[bucket.ID]
			payload, objects := stats.StorageBytes, stats.ObjectCount
			views = append(views, remoteBucketView{
				Name: bucket.Name, Managed: true, BucketID: bucket.ID,
				HealthStatus: bucket.HealthStatus, RemoteMissing: true,
				PayloadBytes: &payload, ObjectCount: &objects,
			})
```

- **Step 5: Add the analytics-failure regression test**

Add this second test using the same fixture with a failing GraphQL response:

```go
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
```

- **Step 6: Format and run the API tests**

Run:

```powershell
gofmt -w internal/platform/httpapi/api.go internal/platform/httpapi/api_test.go
go test ./internal/platform/httpapi -count=1
```

Expected: PASS.

- **Step 7: Commit the API merge**

```powershell
git add internal/platform/httpapi/api.go internal/platform/httpapi/api_test.go
git commit -m "Use local stats for managed R2 buckets"
```

### Task 3: Reload the Overview on Tab Entry

**Files:**
- Modify: `web/src/pages/StoragePage.tsx:101-110`

- **Step 1: Change the tab-entry effect**

Replace the one-time cache guard:

```tsx
useEffect(() => { if (tab === "overview" && overview === null) void loadOverview(); }, [tab]);
```

with:

```tsx
useEffect(() => { if (tab === "overview") void loadOverview(); }, [tab]);
```

This preserves the existing explicit refresh handler and adds a request only when the user transitions into the overview. Do not add a timer or interval.

- **Step 2: Compile the frontend**

Run:

```powershell
npm --prefix web run build
```

Expected: TypeScript and Vite complete successfully and produce `web/dist`.

- **Step 3: Commit the refresh behavior**

```powershell
git add web/src/pages/StoragePage.tsx
git commit -m "Refresh R2 overview on tab entry"
```

### Task 4: Run Full Regression and Record the Reviewed Design

**Files:**
- Add: `.openteams/specs/2026-07-27-r2-managed-bucket-live-stats-design.html`
- Add: `.openteams/plans/2026-07-27-r2-managed-bucket-live-stats.md`

- **Step 1: Run focused R2 and API tests**

```powershell
go test ./internal/modules/r2 ./internal/platform/httpapi -count=1
```

Expected: PASS for both packages.

- **Step 2: Run the full backend suite and build**

```powershell
go test ./... -count=1
go build ./...
```

Expected: all packages pass and all commands build successfully.

- **Step 3: Rebuild the frontend**

```powershell
npm --prefix web run build
```

Expected: successful TypeScript and Vite build.

- **Step 4: Check patch hygiene**

```powershell
git diff --check
git status --short
```

Expected: no whitespace errors; only the reviewed design, implementation plan, and intended source/test files are changed.

- **Step 5: Commit the reviewed documentation**

```powershell
git add .openteams/specs/2026-07-27-r2-managed-bucket-live-stats-design.html .openteams/plans/2026-07-27-r2-managed-bucket-live-stats.md
git commit -m "Document live R2 bucket statistics"
```

- **Step 6: Production acceptance after release**

Upload or replace one object through WebDAV, then use the existing refresh button or leave and re-enter “桶概览”. Verify the managed bucket row changes immediately, the object count remains correct for replacement, unmanaged bucket values still match Cloudflare Analytics, and no repeated background requests occur while the tab remains open.
