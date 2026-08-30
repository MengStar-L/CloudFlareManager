package r2

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestBeginWriteRejectsWebDAVNamespaceConflictsAgainstCommittedObjects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		existing    string
		conflicting string
	}{
		{name: "file ancestor blocks child", existing: "folder", conflicting: "folder/child.txt"},
		{name: "file blocks directory marker", existing: "folder", conflicting: "folder/"},
		{name: "descendant blocks file", existing: "folder/child.txt", conflicting: "folder"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, bucketID, ctx := newWebDAVIntentTestStore(t)
			prefix := WebDAVMountPrefix("credential")
			commitIntentObject(t, store, prefix+test.existing)
			before, err := store.GetBucket(ctx, bucketID)
			if err != nil {
				t.Fatal(err)
			}

			_, err = store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: prefix + test.conflicting, Size: 7}})
			if !errors.Is(err, ErrFileConflict) {
				t.Fatalf("BeginWrite error = %v, want %v", err, ErrFileConflict)
			}
			assertWebDAVIntentState(t, store, bucketID, before.ReservedBytes)
		})
	}
}

func TestBeginWriteRejectsWebDAVNamespaceConflictsAgainstActiveIntents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		existing    string
		conflicting string
	}{
		{name: "file ancestor blocks child", existing: "folder", conflicting: "folder/child.txt"},
		{name: "file blocks directory marker", existing: "folder", conflicting: "folder/"},
		{name: "descendant blocks file", existing: "folder/child.txt", conflicting: "folder"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, bucketID, ctx := newWebDAVIntentTestStore(t)
			prefix := WebDAVMountPrefix("credential")
			existing, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: prefix + test.existing, Size: 3}})
			if err != nil {
				t.Fatal(err)
			}

			_, err = store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: prefix + test.conflicting, Size: 7}})
			if !errors.Is(err, ErrFileConflict) {
				t.Fatalf("BeginWrite error = %v, want %v", err, ErrFileConflict)
			}
			assertWebDAVIntentState(t, store, bucketID, 3, existing.ID)
		})
	}
}

func TestConcurrentWebDAVFileAndChildReservationsAreSerialized(t *testing.T) {
	t.Parallel()
	store, bucketID, ctx := newWebDAVIntentTestStore(t)
	prefix := WebDAVMountPrefix("credential")
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var group sync.WaitGroup
	for _, key := range []string{prefix + "folder", prefix + "folder/child.txt"} {
		key := key
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: key, Size: 3}})
			errorsCh <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsCh)

	var succeeded, conflicted int
	for err := range errorsCh {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrFileConflict):
			conflicted++
		default:
			t.Fatalf("reservation error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	intents, err := store.ListWriteIntents(ctx, 10)
	if err != nil || len(intents) != 1 {
		t.Fatalf("write intents = %#v, error = %v", intents, err)
	}
	assertWebDAVIntentState(t, store, bucketID, 3, intents[0].ID)
}

func TestConcurrentWebDAVFileAndDirectoryMarkerReservationsAreSerialized(t *testing.T) {
	t.Parallel()
	store, bucketID, ctx := newWebDAVIntentTestStore(t)
	prefix := WebDAVMountPrefix("credential")
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var group sync.WaitGroup
	for _, key := range []string{prefix + "folder", prefix + "folder/"} {
		key := key
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: key, Size: 3}})
			errorsCh <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsCh)

	var succeeded, conflicted int
	for err := range errorsCh {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrFileConflict):
			conflicted++
		default:
			t.Fatalf("reservation error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	intents, err := store.ListWriteIntents(ctx, 10)
	if err != nil || len(intents) != 1 {
		t.Fatalf("write intents = %#v, error = %v", intents, err)
	}
	assertWebDAVIntentState(t, store, bucketID, 3, intents[0].ID)
}

func TestBeginWriteAllowsWebDAVDirectoryMarkersWithChildren(t *testing.T) {
	t.Parallel()
	store, bucketID, ctx := newWebDAVIntentTestStore(t)
	prefix := WebDAVMountPrefix("credential")
	marker, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: prefix + "folder/", Size: 2}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: prefix + "folder/child.txt", Size: 3}})
	if err != nil {
		t.Fatalf("child alongside directory marker: %v", err)
	}
	assertWebDAVIntentState(t, store, bucketID, 5, marker.ID, child.ID)
}

func TestBeginWriteLeavesS3FileAndChildKeysIndependent(t *testing.T) {
	t.Parallel()
	store, bucketID, ctx := newWebDAVIntentTestStore(t)
	file, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "folder", Size: 2}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "folder/child.txt", Size: 3}})
	if err != nil {
		t.Fatalf("S3 child alongside file key: %v", err)
	}
	assertWebDAVIntentState(t, store, bucketID, 5, file.ID, child.ID)
}

func newWebDAVIntentTestStore(t *testing.T) (*Store, string, context.Context) {
	t.Helper()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	return store, bucket.ID, ctx
}

func commitIntentObject(t *testing.T, store *Store, key string) {
	t.Helper()
	intent, err := store.BeginWrite(t.Context(), BeginWriteInput{ObjectInput: ObjectInput{Key: key, Size: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitWrite(t.Context(), intent.ID, "etag", 1); err != nil {
		t.Fatal(err)
	}
}

func assertWebDAVIntentState(t *testing.T, store *Store, bucketID string, wantReserved int64, wantIntentIDs ...string) {
	t.Helper()
	intents, err := store.ListWriteIntents(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != len(wantIntentIDs) {
		t.Fatalf("write intents = %#v, want IDs %v", intents, wantIntentIDs)
	}
	wantIDs := make(map[string]bool, len(wantIntentIDs))
	for _, id := range wantIntentIDs {
		wantIDs[id] = true
	}
	for _, intent := range intents {
		if !wantIDs[intent.ID] {
			t.Fatalf("unexpected write intent %s; want IDs %v", intent.ID, wantIntentIDs)
		}
	}
	bucket, err := store.GetBucket(t.Context(), bucketID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.ReservedBytes != wantReserved {
		t.Fatalf("reserved bytes = %d, want %d", bucket.ReservedBytes, wantReserved)
	}
}
