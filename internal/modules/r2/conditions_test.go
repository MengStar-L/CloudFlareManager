package r2

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMutationConditions(t *testing.T) {
	modified := time.Date(2026, time.August, 29, 10, 11, 12, 900_000_000, time.UTC)
	object := &Object{ETag: "Current-ETag", LastModified: modified}
	before := modified.Add(-time.Second)
	after := modified.Add(time.Second)
	tests := []struct {
		name       string
		object     *Object
		conditions MutationConditions
		wantError  bool
	}{
		{name: "no conditions", object: object},
		{name: "if match wildcard exists", object: object, conditions: MutationConditions{IfMatch: &EntityTagSet{Wildcard: true}}},
		{name: "if match wildcard missing", conditions: MutationConditions{IfMatch: &EntityTagSet{Wildcard: true}}, wantError: true},
		{name: "if match exact", object: object, conditions: MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: "Current-ETag"}}}}},
		{name: "if match case sensitive", object: object, conditions: MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: "current-etag"}}}}, wantError: true},
		{name: "if match weak never matches", object: object, conditions: MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: "Current-ETag", Weak: true}}}}, wantError: true},
		{name: "if none wildcard missing", conditions: MutationConditions{IfNoneMatch: &EntityTagSet{Wildcard: true}}},
		{name: "if none wildcard exists", object: object, conditions: MutationConditions{IfNoneMatch: &EntityTagSet{Wildcard: true}}, wantError: true},
		{name: "if none weak comparison", object: object, conditions: MutationConditions{IfNoneMatch: &EntityTagSet{Tags: []EntityTag{{Value: "Current-ETag", Weak: true}}}}, wantError: true},
		{name: "if unmodified equal at seconds", object: object, conditions: MutationConditions{IfUnmodifiedSince: &modified}},
		{name: "if unmodified stale", object: object, conditions: MutationConditions{IfUnmodifiedSince: &before}, wantError: true},
		{name: "if unmodified future", object: object, conditions: MutationConditions{IfUnmodifiedSince: &after}},
		{name: "if match suppresses stale date", object: object, conditions: MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: "Current-ETag"}}}, IfUnmodifiedSince: &before}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkMutationConditions(test.object, test.conditions)
			if errors.Is(err, ErrPreconditionFailed) != test.wantError {
				t.Fatalf("error = %v, want precondition failure %v", err, test.wantError)
			}
		})
	}
}

func TestServiceConditionalPutReportsCreationAndUsesPhysicalGuards(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()

	created, err := service.PutConditional(ctx, PutRequest{
		Key: "catalog.json", Body: strings.NewReader("first"), Size: 5,
		Conditions: MutationConditions{IfNoneMatch: &EntityTagSet{Wildcard: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.Object.Key != "catalog.json" {
		t.Fatalf("created result = %#v", created)
	}
	if backend.putOptions.IfNoneMatch != "*" || backend.putOptions.IfMatch != "" {
		t.Fatalf("initial physical conditions = %#v", backend.putOptions)
	}

	updated, err := service.PutConditional(ctx, PutRequest{
		Key: "catalog.json", Body: strings.NewReader("second"), Size: 6,
		Conditions: MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: created.Object.ETag}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Created {
		t.Fatalf("updated result = %#v", updated)
	}
	if backend.putOptions.IfMatch != quoteETag(created.Object.ETag) || backend.putOptions.IfNoneMatch != "" {
		t.Fatalf("replacement physical conditions = %#v", backend.putOptions)
	}

	beforeCalls := backend.putCalls
	_, err = service.PutConditional(ctx, PutRequest{
		Key: "catalog.json", Body: strings.NewReader("stale"), Size: 5,
		Conditions: MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: "stale-etag"}}}},
	})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale PUT error = %v", err)
	}
	if backend.putCalls != beforeCalls {
		t.Fatalf("backend PUT calls = %d, want %d", backend.putCalls, beforeCalls)
	}
	intents, listErr := service.Index.ListWriteIntents(ctx, 10)
	if listErr != nil || len(intents) != 0 {
		t.Fatalf("write intents = %#v, error = %v", intents, listErr)
	}
}

func TestServicePhysicalConflictReleasesWriteIntent(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	backend.putError = ErrConditionalRequestConflict

	_, err := service.PutConditional(context.Background(), PutRequest{Key: "conflict.txt", Body: strings.NewReader("data"), Size: 4})
	if !errors.Is(err, ErrConditionalRequestConflict) {
		t.Fatalf("PUT error = %v", err)
	}
	intents, listErr := service.Index.ListWriteIntents(context.Background(), 10)
	if listErr != nil || len(intents) != 0 {
		t.Fatalf("write intents = %#v, error = %v", intents, listErr)
	}
	buckets, statErr := service.Index.ListBuckets(context.Background())
	if statErr != nil || len(buckets) != 1 || buckets[0].ReservedBytes != 0 {
		t.Fatalf("buckets = %#v, error = %v", buckets, statErr)
	}
}

func TestDeleteConditionalChecksMissingAndCurrentResources(t *testing.T) {
	t.Parallel()
	service, _, _ := newChunkedTestService(t, 64)
	ctx := context.Background()

	if err := service.DeleteConditional(ctx, "missing.txt", MutationConditions{IfMatch: &EntityTagSet{Wildcard: true}}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("missing conditional DELETE error = %v", err)
	}
	object, err := service.Put(ctx, PutRequest{Key: "current.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteConditional(ctx, object.Key, MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: "stale"}}}}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale conditional DELETE error = %v", err)
	}
	intents, err := service.Index.ListWriteIntents(ctx, 10)
	if err != nil || len(intents) != 0 {
		t.Fatalf("write intents after stale DELETE = %#v, error = %v", intents, err)
	}
	if _, err := service.Stat(ctx, object.Key); err != nil {
		t.Fatalf("stale DELETE removed object: %v", err)
	}
	if err := service.DeleteConditional(ctx, object.Key, MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: object.ETag}}}}); err != nil {
		t.Fatalf("current conditional DELETE: %v", err)
	}
}

func TestCopyConditionalReportsDestinationCreation(t *testing.T) {
	t.Parallel()
	service, _, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	if _, err := service.Put(ctx, PutRequest{Key: "source.txt", Body: strings.NewReader("data"), Size: 4}); err != nil {
		t.Fatal(err)
	}

	created, err := service.CopyConditional(ctx, "source.txt", "destination.txt",
		MutationConditions{IfNoneMatch: &EntityTagSet{Wildcard: true}})
	if err != nil || !created.Created {
		t.Fatalf("created copy = %#v, error = %v", created, err)
	}
	updated, err := service.CopyConditional(ctx, "source.txt", "destination.txt", MutationConditions{})
	if err != nil || updated.Created {
		t.Fatalf("updated copy = %#v, error = %v", updated, err)
	}
	if _, err := service.CopyConditional(ctx, "source.txt", "destination.txt",
		MutationConditions{IfNoneMatch: &EntityTagSet{Wildcard: true}}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("no-overwrite copy error = %v", err)
	}
}

func TestConcurrentCreateOnlyReservationsAreSerialized(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := store.BeginWrite(ctx, BeginWriteInput{
				ObjectInput: ObjectInput{Key: "catalog.json", Size: 10},
				Conditions:  MutationConditions{IfNoneMatch: &EntityTagSet{Wildcard: true}},
			})
			errorsCh <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsCh)

	var succeeded, locked int
	for err := range errorsCh {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrWriteInProgress):
			locked++
		default:
			t.Fatalf("reservation error = %v", err)
		}
	}
	if succeeded != 1 || locked != 1 {
		t.Fatalf("succeeded=%d locked=%d", succeeded, locked)
	}
}

func TestBeginWriteConditionFailureHasNoSideEffects(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}

	_, err = store.BeginWrite(ctx, BeginWriteInput{
		ObjectInput: ObjectInput{Key: "new.txt", Size: 10},
		Conditions:  MutationConditions{IfMatch: &EntityTagSet{Wildcard: true}},
	})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("BeginWrite error = %v", err)
	}
	intents, err := store.ListWriteIntents(ctx, 10)
	if err != nil || len(intents) != 0 {
		t.Fatalf("write intents = %#v, error = %v", intents, err)
	}
	updated, err := store.GetBucket(ctx, bucket.ID)
	if err != nil || updated.ReservedBytes != 0 {
		t.Fatalf("bucket = %#v, error = %v", updated, err)
	}
}
