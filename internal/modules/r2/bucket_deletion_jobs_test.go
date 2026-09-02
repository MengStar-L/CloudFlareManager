package r2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/audit"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
)

const deletionTestCreationDate = "2026-08-30T10:11:12.000Z"

type deletionTestRemote struct {
	bucket            accounts.RemoteBucket
	objects           []accounts.RemoteObject
	exists            bool
	hiddenNotEmpty    bool
	getErr            error
	deleteErr         error
	deleteErrRemoves  bool
	objectDeleteErr   error
	objectErrRemoves  bool
	listErr           error
	getCalls          int
	deleteCalls       int
	objectDeleteCalls int
}

func (r *deletionTestRemote) GetR2Bucket(context.Context, string, string, string, string) (accounts.RemoteBucket, error) {
	r.getCalls++
	if r.getErr != nil {
		return accounts.RemoteBucket{}, r.getErr
	}
	if !r.exists {
		return accounts.RemoteBucket{}, &accounts.CloudflareAPIError{Operation: "get R2 bucket", StatusCode: 404, Code: 10006, Message: "not found"}
	}
	return r.bucket, nil
}

func (r *deletionTestRemote) ListR2Objects(context.Context, string, string, string, string, string, int) (accounts.RemoteObjectPage, error) {
	if r.listErr != nil {
		return accounts.RemoteObjectPage{}, r.listErr
	}
	return accounts.RemoteObjectPage{Objects: append([]accounts.RemoteObject(nil), r.objects...)}, nil
}

func (r *deletionTestRemote) DeleteR2Object(_ context.Context, _, _, _, _, key string) error {
	r.objectDeleteCalls++
	if r.objectDeleteErr != nil {
		if r.objectErrRemoves {
			r.exists = false
			r.objects = nil
		}
		return r.objectDeleteErr
	}
	for index, object := range r.objects {
		if object.Key == key {
			r.objects = append(r.objects[:index], r.objects[index+1:]...)
			return nil
		}
	}
	return nil
}

func (r *deletionTestRemote) DeleteR2Bucket(context.Context, string, string, string, string) error {
	r.deleteCalls++
	if r.deleteErr != nil {
		if r.deleteErrRemoves {
			r.exists = false
		}
		return r.deleteErr
	}
	if len(r.objects) != 0 || r.hiddenNotEmpty {
		return &accounts.CloudflareAPIError{Operation: "delete R2 bucket", StatusCode: 409, Code: 10008, Message: "not empty"}
	}
	r.exists = false
	return nil
}

type deletionTestBackend struct {
	remote       *deletionTestRemote
	multipart    []RemoteMultipart
	batches      int
	aborts       int
	listCalls    int
	failListCall int
	listErr      error
}

func (b *deletionTestBackend) Put(context.Context, Target, string, io.Reader, int64, string, map[string]string, PutOptions) (string, error) {
	return "", errors.New("unexpected put")
}

func (b *deletionTestBackend) Get(context.Context, Target, string, GetOptions) (GetResult, error) {
	return GetResult{}, ErrObjectNotFound
}

func (b *deletionTestBackend) Delete(context.Context, Target, string) error {
	return errors.New("unexpected delete")
}

func (b *deletionTestBackend) ListRemote(_ context.Context, _ Target, _, _ string, limit int32) (RemoteObjectList, error) {
	b.listCalls++
	if b.listErr != nil {
		return RemoteObjectList{}, b.listErr
	}
	if b.failListCall != 0 && b.listCalls == b.failListCall {
		return RemoteObjectList{}, ErrRateLimited
	}
	result := RemoteObjectList{}
	for _, object := range b.remote.objects {
		if int32(len(result.Objects)) == limit {
			break
		}
		result.Objects = append(result.Objects, RemoteObject{Key: object.Key, Size: object.Size})
	}
	return result, nil
}

type deletionTestAWSError struct {
	code   string
	status int
}

func (e deletionTestAWSError) Error() string       { return e.code }
func (e deletionTestAWSError) ErrorCode() string   { return e.code }
func (e deletionTestAWSError) HTTPStatusCode() int { return e.status }

func (b *deletionTestBackend) DeleteRemoteBatch(_ context.Context, _ Target, keys []string) (int, error) {
	b.batches++
	for _, key := range keys {
		_ = b.remote.DeleteR2Object(context.Background(), "", "", "", "", key)
	}
	return len(keys), nil
}

func (b *deletionTestBackend) ListRemoteMultipart(context.Context, Target, string, string, int32) (RemoteMultipartPage, error) {
	return RemoteMultipartPage{Uploads: append([]RemoteMultipart(nil), b.multipart...)}, nil
}

func (b *deletionTestBackend) AbortMultipart(_ context.Context, _ Target, key, uploadID string) error {
	for index, upload := range b.multipart {
		if upload.Key == key && upload.UploadID == uploadID {
			b.multipart = append(b.multipart[:index], b.multipart[index+1:]...)
			b.aborts++
			return nil
		}
	}
	return nil
}

func newBucketDeletionTest(t *testing.T, mode BucketDeletionMode, objects []accounts.RemoteObject) (BucketDeletionJobs, jobs.Job, *Store, *deletionTestRemote, *deletionTestBackend, PhysicalBucket) {
	t.Helper()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1 << 30, AccountStorageBytes: 1 << 30, ClassA: 10000, ClassB: 10000})
	account := createIntentAccount(t, accountStore, ctx, "delete")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	remote := &deletionTestRemote{exists: true, bucket: accounts.RemoteBucket{
		Name: "bucket", CreationDate: deletionTestCreationDate, Jurisdiction: "default",
	}, objects: append([]accounts.RemoteObject(nil), objects...)}
	backend := &deletionTestBackend{remote: remote}
	jobStore := jobs.NewStore(store.db)
	payload := BucketDeletionPayload{
		AccountID: account.ID, CloudflareAccountID: account.CloudflareAccountID,
		BucketName: bucket.Name, Jurisdiction: "default", ExpectedCreationDate: deletionTestCreationDate,
		LocalBucketID: bucket.ID, Mode: mode,
	}
	queued, err := jobStore.Enqueue(ctx, BucketDeletionJobType, payload, 4)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.Claim(ctx, time.Minute)
	if err != nil || claimed == nil || claimed.ID != queued.ID {
		t.Fatalf("claim job: %#v, %v", claimed, err)
	}
	handler := BucketDeletionJobs{
		Service: Service{Index: store, Accounts: accountStore, Backend: backend},
		Jobs:    jobStore, Remote: remote, Clear: backend, Audit: audit.NewStore(store.db),
	}
	return handler, *claimed, store, remote, backend, bucket
}

func TestBucketDeletionEmptyOnlyRejectsNonEmptyWithoutMutation(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly,
		[]accounts.RemoteObject{{Key: "keep.txt", Size: 4}})
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "bucket_not_empty", true)
	if backend.batches != 0 || remote.deleteCalls != 0 {
		t.Fatalf("remote mutations: batches=%d delete_bucket=%d", backend.batches, remote.deleteCalls)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketActive {
		t.Fatalf("bucket after rejection = %#v, %v", got, getErr)
	}
}

func TestBucketDeletionEmptyOnlyRestoresActiveAfterHiddenMultipartConflict(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	remote.hiddenNotEmpty = true
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "bucket_not_empty", true)
	if backend.aborts != 0 || remote.objectDeleteCalls != 0 || remote.deleteCalls != 1 {
		t.Fatalf("unexpected remote mutations: aborts=%d objects=%d bucket_calls=%d",
			backend.aborts, remote.objectDeleteCalls, remote.deleteCalls)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketActive || got.DeletionJobID != "" {
		t.Fatalf("bucket after definite conflict = %#v, %v", got, getErr)
	}
	persisted, getErr := handler.Jobs.Get(context.Background(), job.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	var payload BucketDeletionPayload
	if err := json.Unmarshal(persisted.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RemoteMutated {
		t.Fatal("definite bucket-not-empty response must clear the initial write-ahead marker")
	}
}

func TestBucketDeletionEmptyOnlyPreservesPriorMutationFenceAfterConflict(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	remote.hiddenNotEmpty = true
	var payload BucketDeletionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.RemoteMutated = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = encoded
	if err := handler.Jobs.SetPayload(context.Background(), job.ID, payload); err != nil {
		t.Fatal(err)
	}
	err = handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "bucket_not_empty", true)
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketDeleteFailed || got.DeletionJobID != job.ID {
		t.Fatalf("bucket after prior remote mutation = %#v, %v", got, getErr)
	}
}

func TestBucketDeletionEmptyOnlyDeletesEmptyBucket(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if remote.exists || remote.getCalls < 2 {
		t.Fatalf("remote state exists=%v get_calls=%d", remote.exists, remote.getCalls)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
	events, err := handler.Audit.List(context.Background(), 10, BucketDeletionJobType)
	if err != nil || len(events) != 1 || events[0].Result != "success" || events[0].RequestID != job.ID {
		t.Fatalf("success audit = %#v, %v", events, err)
	}
}

func TestBucketDeletionDiscoversLocallyEnrolledBucketBeforeFencing(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	var payload BucketDeletionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.LocalBucketID = ""
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = encoded
	if err := handler.Jobs.SetPayload(context.Background(), job.ID, payload); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if remote.exists {
		t.Fatal("remote bucket still exists")
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
	persisted, err := handler.Jobs.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(persisted.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.LocalBucketID != bucket.ID {
		t.Fatalf("discovered local bucket id = %q, want %q", payload.LocalBucketID, bucket.ID)
	}
}

func TestBucketDeletionEmptyOnlyDoesNotFinalizeOnTransientListNotFound(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	remote.listErr = &accounts.CloudflareAPIError{
		Operation: "list R2 objects", StatusCode: http.StatusNotFound, Code: 10006, Message: "not found",
	}
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "cloudflare_unavailable", false)
	if remote.deleteCalls != 0 {
		t.Fatalf("delete calls after contradictory list/get result = %d", remote.deleteCalls)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketDeleting || got.DeletionJobID != job.ID {
		t.Fatalf("bucket after contradictory list/get result = %#v, %v", got, getErr)
	}
}

func TestBucketDeletionDoesNotTreatUnstructured404AsMissingBucket(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete,
		[]accounts.RemoteObject{{Key: "keep.txt", Size: 4}})
	remote.getErr = &accounts.CloudflareAPIError{
		Operation: "get R2 bucket", StatusCode: http.StatusNotFound, Message: "proxy not found",
	}
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "cloudflare_unavailable", false)
	if backend.batches != 0 || remote.objectDeleteCalls != 0 || remote.deleteCalls != 0 {
		t.Fatalf("remote mutations after unstructured 404: batches=%d objects=%d bucket=%d",
			backend.batches, remote.objectDeleteCalls, remote.deleteCalls)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketActive {
		t.Fatalf("bucket after unstructured 404 = %#v, %v", got, getErr)
	}
}

func TestBucketDeletionEmptyAndDeleteUsesBatchAndAbortsMultipart(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete,
		[]accounts.RemoteObject{{Key: "one"}, {Key: "two"}})
	backend.multipart = []RemoteMultipart{{Key: "large", UploadID: "upload-1"}}
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if backend.batches != 1 || backend.aborts != 1 || remote.exists {
		t.Fatalf("batch=%d abort=%d exists=%v", backend.batches, backend.aborts, remote.exists)
	}
	if remote.getCalls < 5 {
		t.Fatalf("identity was only checked %d times", remote.getCalls)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
}

func TestBucketDeletionPermissionFailureKeepsRemoteAndMarksManagedBucketFailed(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete,
		[]accounts.RemoteObject{{Key: "keep.txt", Size: 4}})
	backend.listErr = deletionTestAWSError{code: "AccessDenied", status: http.StatusForbidden}
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "permission_denied", true)
	if !remote.exists || remote.deleteCalls != 0 || backend.batches != 0 {
		t.Fatalf("remote state exists=%v delete_calls=%d batches=%d", remote.exists, remote.deleteCalls, backend.batches)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketDeleteFailed || got.DeletionJobID != job.ID {
		t.Fatalf("bucket after permission failure = %#v, %v", got, getErr)
	}
	events, auditErr := handler.Audit.List(context.Background(), 10, BucketDeletionJobType)
	if auditErr != nil || len(events) != 1 || events[0].Result != "failure" || events[0].Detail["error_code"] != "permission_denied" {
		t.Fatalf("failure audit = %#v, %v", events, auditErr)
	}
}

func TestBucketDeletionFallsBackToRESTObjects(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete,
		[]accounts.RemoteObject{{Key: "one"}, {Key: "two"}})
	handler.Clear = nil
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if backend.batches != 0 || remote.objectDeleteCalls != 2 || remote.exists {
		t.Fatalf("batch=%d rest_deletes=%d exists=%v", backend.batches, remote.objectDeleteCalls, remote.exists)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
}

func TestBucketDeletionRESTReverifiesBucketMissingDuringObjectDelete(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete,
		[]accounts.RemoteObject{{Key: "one"}})
	handler.Clear = nil
	remote.objectDeleteErr = &accounts.CloudflareAPIError{
		Operation: "delete R2 object", StatusCode: http.StatusNotFound, Code: 10006, Message: "bucket not found",
	}
	remote.objectErrRemoves = true
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if remote.getCalls < 3 {
		t.Fatalf("bucket absence was not reverified, get calls = %d", remote.getCalls)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
}

func TestBucketDeletionEmptyAndDeleteResumesAfterPartialClear(t *testing.T) {
	t.Parallel()
	objects := make([]accounts.RemoteObject, 1001)
	for index := range objects {
		objects[index].Key = fmt.Sprintf("object-%04d", index)
	}
	handler, job, store, remote, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete, objects)
	backend.failListCall = 2
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "rate_limited", false)
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketDeleting {
		t.Fatalf("bucket during retry = %#v, %v", got, getErr)
	}
	resumed, err := handler.Jobs.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), resumed); err != nil {
		t.Fatal(err)
	}
	if backend.batches != 2 || remote.exists {
		t.Fatalf("batches=%d exists=%v", backend.batches, remote.exists)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
}

func TestBucketDeletionStopsWhenIdentityChanges(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete,
		[]accounts.RemoteObject{{Key: "one"}})
	remote.bucket.CreationDate = "2026-08-31T10:11:12Z"
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "bucket_identity_changed", true)
	if backend.batches != 0 {
		t.Fatalf("batch deletes = %d", backend.batches)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketActive {
		t.Fatalf("bucket after mismatch = %#v, %v", got, getErr)
	}
}

func TestBucketDeletionTreatsVerifiedRemoteMissingAsSuccess(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete, nil)
	remote.exists = false
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
}

func TestBucketDeletionRemoteMissingSettlesLocalActivityBeforeFinalize(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete, nil)
	remote.exists = false
	now := time.Now().UnixNano()
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO r2_write_intents(
		id, object_key, target_bucket_id, reserved_bytes, declared_size, actual_size,
		content_type, metadata_json, state, operation, upstream_upload_id, etag,
		internal_multipart, created_at, updated_at)
		VALUES('active-intent', 'pending.bin', ?, 0, 0, 0, '', '{}', 'uploading',
		'put', '', '', 0, ?, ?)`, bucket.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO r2_multipart_uploads(
		id, object_key, object_id, physical_bucket_id, upstream_upload_id, metadata_json,
		status, created_at, updated_at, write_intent_id)
		VALUES('active-upload', 'pending.bin', 'pending-object', ?, 'upstream-upload', '{}',
		'active', ?, ?, 'active-intent')`, bucket.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, getErr := store.GetBucket(context.Background(), bucket.ID); !errors.Is(getErr, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", getErr)
	}
	var intents int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM r2_write_intents
		WHERE target_bucket_id = ?`, bucket.ID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 0 {
		t.Fatalf("active intents after remote-missing settlement = %d", intents)
	}
	var uploads int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM r2_multipart_uploads
		WHERE physical_bucket_id = ?`, bucket.ID).Scan(&uploads); err != nil {
		t.Fatal(err)
	}
	if uploads != 0 {
		t.Fatalf("multipart uploads after remote-missing settlement = %d", uploads)
	}
}

func TestBucketDeletionRetryTakesOverFailedParentFenceAndLock(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete, nil)
	const parentJobID = "failed-parent-job"
	insertLifecycleTestJob(t, store, parentJobID)
	if _, err := store.db.ExecContext(context.Background(), `UPDATE jobs SET status = 'failed'
		WHERE id = ?`, parentJobID); err != nil {
		t.Fatal(err)
	}
	var payload BucketDeletionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.ParentJobID = parentJobID
	job.ParentJobID = parentJobID
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = encoded
	if _, err := store.db.ExecContext(context.Background(), `UPDATE jobs
		SET parent_job_id = ?, payload_json = ? WHERE id = ?`, parentJobID, string(encoded), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE r2_physical_buckets
		SET lifecycle_state = ?, deletion_job_id = ? WHERE id = ?`, BucketDeleting, parentJobID, bucket.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO r2_bucket_maintenance_locks(
		physical_bucket_id, operation, created_at) VALUES(?, ?, ?)`,
		bucket.ID, "delete:"+parentJobID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	remote.exists = false

	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket after recovered finalization error = %v", err)
	}
	var locks int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM r2_bucket_maintenance_locks
		WHERE physical_bucket_id = ?`, bucket.ID).Scan(&locks); err != nil {
		t.Fatal(err)
	}
	if locks != 0 {
		t.Fatalf("maintenance locks after recovered finalization = %d", locks)
	}
}

func TestBucketDeletionRetrySucceedsAfterLocalFinalization(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete, nil)
	remote.exists = false
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
	resumed, err := handler.Jobs.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), resumed); err != nil {
		t.Fatalf("retry after local finalization = %v", err)
	}
}

func TestBucketDeletionStopsAfterMaximumClearRounds(t *testing.T) {
	t.Parallel()
	handler, job, store, _, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete,
		[]accounts.RemoteObject{{Key: "continuously-recreated"}})
	var payload BucketDeletionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.DeleteRounds = bucketDeletionMaxRounds
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = encoded
	err = handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "external_writes_detected", true)
	if backend.batches != 0 {
		t.Fatalf("delete batches after maximum rounds = %d", backend.batches)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketDeleteFailed {
		t.Fatalf("bucket after maximum rounds = %#v, %v", got, getErr)
	}
}

func TestBucketDeletionAllowsEmptyConfirmationAtMaximumClearRounds(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete, nil)
	var payload BucketDeletionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.DeleteRounds = bucketDeletionMaxRounds
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = encoded
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if remote.exists {
		t.Fatal("empty bucket was not deleted at the round boundary")
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
}

func TestBucketDeletionAmbiguousDeleteKeepsManagedBucketFenced(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	remote.deleteErr = &accounts.CloudflareAPIError{
		Operation: "delete R2 bucket", StatusCode: http.StatusBadGateway, Code: 10001, Message: "upstream timeout",
	}
	job.Attempts = job.MaxAttempts
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "cloudflare_unavailable", false)
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketDeleteFailed || got.DeletionJobID != job.ID {
		t.Fatalf("bucket after ambiguous delete = %#v, %v", got, getErr)
	}
	persisted, getErr := handler.Jobs.Get(context.Background(), job.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	var payload BucketDeletionPayload
	if err := json.Unmarshal(persisted.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.RemoteMutated {
		t.Fatal("ambiguous delete must retain the remote mutation marker")
	}
}

func TestBucketDeletionDoesNotFinalizeOnContradictoryDeleteNotFound(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	remote.deleteErr = &accounts.CloudflareAPIError{
		Operation: "delete R2 bucket", StatusCode: http.StatusNotFound, Code: 10006, Message: "not found",
	}
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "cloudflare_unavailable", false)
	if remote.deleteCalls != 1 || remote.getCalls < 3 || !remote.exists {
		t.Fatalf("remote state exists=%v get_calls=%d delete_calls=%d", remote.exists, remote.getCalls, remote.deleteCalls)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketDeleting || got.DeletionJobID != job.ID {
		t.Fatalf("bucket after contradictory delete/get result = %#v, %v", got, getErr)
	}
}

func TestBucketDeletionFinalizesAfterDeleteNotFoundIsVerified(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	remote.deleteErr = &accounts.CloudflareAPIError{
		Operation: "delete R2 bucket", StatusCode: http.StatusNotFound, Code: 10006, Message: "not found",
	}
	remote.deleteErrRemoves = true
	if err := handler.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if remote.exists || remote.getCalls < 3 {
		t.Fatalf("remote state exists=%v get_calls=%d", remote.exists, remote.getCalls)
	}
	if _, err := store.GetBucket(context.Background(), bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("local bucket error = %v", err)
	}
}

func TestBucketDeletionRejectsMismatchedParentMetadata(t *testing.T) {
	t.Parallel()
	handler, job, store, remote, _, bucket := newBucketDeletionTest(t, BucketDeletionEmptyOnly, nil)
	job.ParentJobID = "different-parent"
	err := handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "bucket_identity_unverifiable", true)
	if remote.deleteCalls != 0 {
		t.Fatalf("delete calls = %d", remote.deleteCalls)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketActive {
		t.Fatalf("bucket after metadata mismatch = %#v, %v", got, getErr)
	}
}

func TestBucketDeletionRemoteMissingAtEnqueueRejectsRecreatedBucket(t *testing.T) {
	t.Parallel()
	handler, job, store, _, backend, bucket := newBucketDeletionTest(t, BucketDeletionEmptyAndDelete, nil)
	var payload BucketDeletionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.ExpectedCreationDate = ""
	payload.RemoteMissingAtEnqueue = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = encoded
	err = handler.Handle(context.Background(), job)
	assertBucketDeletionFailure(t, err, "bucket_identity_changed", true)
	if backend.batches != 0 {
		t.Fatalf("batch deletes = %d", backend.batches)
	}
	got, getErr := store.GetBucket(context.Background(), bucket.ID)
	if getErr != nil || got.LifecycleState != BucketActive {
		t.Fatalf("bucket after recreation = %#v, %v", got, getErr)
	}
}

func assertBucketDeletionFailure(t *testing.T, err error, code string, permanent bool) {
	t.Helper()
	var failure *jobs.HandlerError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %T %v", err, err)
	}
	if failure.Code != code || failure.Permanent != permanent {
		t.Fatalf("failure = %#v", failure)
	}
}
