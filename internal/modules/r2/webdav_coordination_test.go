package r2

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestServiceCoordinatesExternalWebDAVMutations(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	coordinator := &recordingWebDAVCoordinator{}
	service.WebDAVCoordinator = coordinator
	key := WebDAVMountPrefix("credential") + "folder/file.txt"

	if _, err := service.Put(context.Background(), PutRequest{Key: key, Body: strings.NewReader("data"), Size: 4}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	wantCreationPaths := []string{key, WebDAVMountPrefix("credential") + "folder/", WebDAVMountPrefix("credential")}
	if got := coordinator.guardCall(0); !reflect.DeepEqual(got, wantCreationPaths) {
		t.Fatalf("PUT guarded paths = %v, want %v", got, wantCreationPaths)
	}
	if !coordinator.guardReleased(0) {
		t.Fatal("PUT guard was not released")
	}

	coordinator.reset()
	coordinator.relevantRoots = []string{key}
	if err := service.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := coordinator.guardCall(0); !reflect.DeepEqual(got, wantCreationPaths) {
		t.Fatalf("DELETE guarded paths = %v, want %v", got, wantCreationPaths)
	}
	if got := coordinator.guardDeleteExact(0); !reflect.DeepEqual(got, []string{key}) {
		t.Fatalf("DELETE stale-lock cleanup = %v, want [%s]", got, key)
	}
	if !coordinator.guardReleased(0) {
		t.Fatal("DELETE guard was not released")
	}
}

func TestNestedWebDAVMutationSkipsReentrantGuardButCleansLocks(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	key := WebDAVMountPrefix("credential") + "nested.txt"
	if _, err := service.Put(context.Background(), PutRequest{Key: key, Body: strings.NewReader("data"), Size: 4}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	coordinator := &recordingWebDAVCoordinator{relevantRoots: []string{key}}
	service.WebDAVCoordinator = coordinator
	ctx := WithWebDAVMutationGuard(context.Background(), coordinator, service.webDAVTreeMutationPaths(key))
	if err := service.Delete(ctx, key); err != nil {
		t.Fatalf("nested Delete: %v", err)
	}
	if got := coordinator.guardCount(); got != 0 {
		t.Fatalf("nested Delete guard calls = %d, want 0", got)
	}
	if got := coordinator.directDeleteExactCall(0); !reflect.DeepEqual(got, []string{key}) {
		t.Fatalf("nested Delete stale-lock cleanup = %v, want [%s]", got, key)
	}
}

func TestDeletingLastWebDAVObjectPreservesMountRootLock(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	prefix := WebDAVMountPrefix("credential")
	key := prefix + "last.txt"
	if _, err := service.Put(context.Background(), PutRequest{Key: key, Body: strings.NewReader("data"), Size: 4}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	coordinator := &recordingWebDAVCoordinator{relevantRoots: []string{prefix}}
	service.WebDAVCoordinator = coordinator
	if err := service.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := coordinator.guardDeleteExact(0); len(got) != 0 {
		t.Fatalf("mount root lock cleanup = %v, want none", got)
	}
}

func TestDeleteRetriesLockCleanupAfterObjectIsAlreadyGone(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	key := WebDAVMountPrefix("credential") + "cleanup.txt"
	if _, err := service.Put(context.Background(), PutRequest{Key: key, Body: strings.NewReader("data"), Size: 4}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	cleanupFailure := errors.New("injected lock cleanup failure")
	coordinator := &recordingWebDAVCoordinator{relevantRoots: []string{key}, relevantErr: cleanupFailure}
	service.WebDAVCoordinator = coordinator
	if err := service.Delete(context.Background(), key); !errors.Is(err, cleanupFailure) {
		t.Fatalf("first Delete error = %v, want cleanup failure", err)
	}
	if _, err := service.Stat(context.Background(), key); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("object after first Delete = %v", err)
	}

	coordinator.mu.Lock()
	coordinator.relevantErr = nil
	coordinator.mu.Unlock()
	if err := service.Delete(context.Background(), key); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("retry Delete error = %v, want ErrObjectNotFound after cleanup", err)
	}
	if got := coordinator.guardDeleteExact(1); !reflect.DeepEqual(got, []string{key}) {
		t.Fatalf("retry stale-lock cleanup = %v, want [%s]", got, key)
	}
}

func TestServiceDoesNotCoordinateNonWebDAVKeys(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	coordinator := &recordingWebDAVCoordinator{}
	service.WebDAVCoordinator = coordinator
	if _, err := service.Put(context.Background(), PutRequest{Key: "s3/file.txt", Body: strings.NewReader("data"), Size: 4}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := service.Delete(context.Background(), "s3/file.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := coordinator.guardCount(); got != 0 {
		t.Fatalf("non-WebDAV guard calls = %d, want 0", got)
	}
}

func TestMoveFileHoldsOneTreeGuardAcrossCopyAndDelete(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	prefix := WebDAVMountPrefix("credential")
	source := prefix + "source.txt"
	destination := prefix + "destination.txt"
	if _, err := service.Put(context.Background(), PutRequest{Key: source, Body: strings.NewReader("data"), Size: 4}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	coordinator := &recordingWebDAVCoordinator{}
	service.WebDAVCoordinator = coordinator
	if err := service.MoveFile(context.Background(), source, destination, false); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	if got := coordinator.guardCount(); got != 1 {
		t.Fatalf("MoveFile guard calls = %d, want one tree guard", got)
	}
	wantPaths := []string{source, prefix, destination}
	if got := coordinator.guardCall(0); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("MoveFile guarded paths = %v, want %v", got, wantPaths)
	}
	if !coordinator.guardReleased(0) {
		t.Fatal("MoveFile tree guard was not released")
	}
}

func TestRecoveryHoldsWebDAVTreeGuard(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	key := WebDAVMountPrefix("credential") + "recover.bin"
	intent, err := service.Index.BeginWrite(context.Background(), BeginWriteInput{ObjectInput: ObjectInput{Key: key, Size: 4}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := service.target(context.Background(), intent.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), target, intent.Key, strings.NewReader("data"), 4, "", upstreamWriteMetadata(nil, intent.ID), PutOptions{IfNoneMatch: "*"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.MarkWriteUploading(context.Background(), intent.ID, ""); err != nil {
		t.Fatal(err)
	}

	coordinator := &recordingWebDAVCoordinator{}
	service.WebDAVCoordinator = coordinator
	if err := service.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := coordinator.guardCount(); got != 1 {
		t.Fatalf("recovery guard calls = %d, want one", got)
	}
	wantPaths := []string{key, WebDAVMountPrefix("credential")}
	if got := coordinator.guardCall(0); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("recovery guarded paths = %v, want %v", got, wantPaths)
	}
	if !coordinator.guardReleased(0) {
		t.Fatal("recovery guard was not released")
	}
}

type recordingWebDAVCoordinator struct {
	mu                sync.Mutex
	guards            []*recordingWebDAVGuard
	relevantRoots     []string
	relevantErr       error
	directDeleteExact [][]string
}

func (c *recordingWebDAVCoordinator) GuardExternalPaths(_ context.Context, paths []string) (WebDAVMutationGuard, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	guard := &recordingWebDAVGuard{paths: append([]string(nil), paths...)}
	c.guards = append(c.guards, guard)
	return guard, nil
}

func (c *recordingWebDAVCoordinator) RelevantLockRoots(context.Context, []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.relevantRoots...), c.relevantErr
}

func (c *recordingWebDAVCoordinator) DeletePaths(context.Context, []string) error { return nil }

func (c *recordingWebDAVCoordinator) DeleteExactPaths(_ context.Context, paths []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.directDeleteExact = append(c.directDeleteExact, append([]string(nil), paths...))
	return nil
}

func (c *recordingWebDAVCoordinator) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guards = nil
	c.directDeleteExact = nil
}

func (c *recordingWebDAVCoordinator) guardCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.guards)
}

func (c *recordingWebDAVCoordinator) guardCall(index int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.guards[index].paths...)
}

func (c *recordingWebDAVCoordinator) guardReleased(index int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.guards[index].released
}

func (c *recordingWebDAVCoordinator) guardDeleteExact(index int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.guards[index].deletedExact...)
}

func (c *recordingWebDAVCoordinator) directDeleteExactCall(index int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.directDeleteExact[index]...)
}

type recordingWebDAVGuard struct {
	paths        []string
	deletedExact []string
	released     bool
}

func (g *recordingWebDAVGuard) Release() { g.released = true }

func (g *recordingWebDAVGuard) DeletePaths(context.Context, []string) error { return nil }

func (g *recordingWebDAVGuard) DeleteExactPaths(_ context.Context, paths []string) error {
	g.deletedExact = append([]string(nil), paths...)
	return nil
}
