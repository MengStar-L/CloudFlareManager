package r2

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRepairWebDAVNamespaceV1CommitsPublishedOrdinaryPutAtNamespacedLogicalKey(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	const credentialID = "gamesync-id"
	const rawKey = "saves/slot.dat"

	previous, err := service.Put(ctx, PutRequest{Key: rawKey, Body: strings.NewReader("old"), Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{
		Key: rawKey, Size: 4, ContentType: "application/octet-stream",
	}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := service.target(ctx, intent.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	backend := service.Backend.(*memoryBackend)
	if _, err := backend.Put(ctx, target, rawKey, strings.NewReader("next"), 4, intent.ContentType,
		upstreamWriteMetadata(nil, intent.ID), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.MarkWriteUploading(ctx, intent.ID, ""); err != nil {
		t.Fatal(err)
	}
	applySplitWebDAVNamespaceV1(t, service, credentialID, time.Now().Unix(), true, false)

	if err := service.RepairWebDAVNamespaceV1(ctx); err != nil {
		t.Fatal(err)
	}
	logicalKey := WebDAVMountPrefix(credentialID) + rawKey
	object, err := service.Index.GetObject(ctx, logicalKey)
	if err != nil {
		t.Fatal(err)
	}
	if object.ObjectID != intent.ID || object.PhysicalKey != rawKey || object.Size != 4 || object.ETag != "etag" {
		t.Fatalf("repaired object = %#v", object)
	}
	if _, err := service.Index.GetObject(ctx, rawKey); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("raw logical mapping error = %v", err)
	}
	if _, err := service.Index.GetWriteIntent(ctx, intent.ID); !errors.Is(err, ErrWriteIntentNotFound) {
		t.Fatalf("write intent error = %v", err)
	}
	bucket, err := service.Index.GetBucket(ctx, previous.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.StorageBytes != 4 || bucket.ReservedBytes != 0 {
		t.Fatalf("bucket accounting = %#v", bucket)
	}
	assertWebDAVNamespaceV2Marker(t, service, credentialID, true)
}

func TestRepairWebDAVNamespaceV1PreservesAmbiguousOrdinaryPut(t *testing.T) {
	t.Run("remote version does not match", func(t *testing.T) {
		service, _, cleanup := newFileManagerFixture(t)
		defer cleanup()
		ctx := context.Background()
		const credentialID = "gamesync-id"
		const rawKey = "saves/changed.dat"

		if _, err := service.Put(ctx, PutRequest{Key: rawKey, Body: strings.NewReader("old"), Size: 3}); err != nil {
			t.Fatal(err)
		}
		intent, err := service.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: rawKey, Size: 4}})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Index.MarkWriteUploading(ctx, intent.ID, ""); err != nil {
			t.Fatal(err)
		}
		backend := service.Backend.(*memoryBackend)
		physicalKey := "physical/" + rawKey
		backend.objects[physicalKey] = []byte("different")
		backend.metadata[physicalKey] = map[string]string{InternalWriteIDMetadata: "another-write"}
		backend.etags[physicalKey] = "different-etag"
		applySplitWebDAVNamespaceV1(t, service, credentialID, time.Now().Unix(), true, false)

		err = service.RepairWebDAVNamespaceV1(ctx)
		if !errors.Is(err, ErrWebDAVNamespaceRepairAmbiguous) {
			t.Fatalf("repair error = %v", err)
		}
		assertWriteIntentKey(t, service, intent.ID, rawKey)
		assertWebDAVNamespaceV2Marker(t, service, credentialID, false)
	})

	t.Run("intent created in migration second", func(t *testing.T) {
		service, _, cleanup := newFileManagerFixture(t)
		defer cleanup()
		ctx := context.Background()
		const credentialID = "gamesync-id"
		const rawKey = "saves/same-second.dat"

		intent, err := service.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: rawKey, Size: 1}})
		if err != nil {
			t.Fatal(err)
		}
		applySplitWebDAVNamespaceV1(t, service, credentialID, intent.CreatedAt.Unix(), false, false)

		err = service.RepairWebDAVNamespaceV1(ctx)
		if !errors.Is(err, ErrWebDAVNamespaceRepairAmbiguous) {
			t.Fatalf("repair error = %v", err)
		}
		assertWriteIntentKey(t, service, intent.ID, rawKey)
		assertWebDAVNamespaceV2Marker(t, service, credentialID, false)
	})
}

func TestRepairWebDAVNamespaceV1DoesNotDeleteChangedRemoteVersion(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	const credentialID = "gamesync-id"
	const rawKey = "saves/delete.dat"

	object, err := service.Put(ctx, PutRequest{Key: rawKey, Body: strings.NewReader("keep"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	intent, _, err := service.Index.BeginDeleteWrite(ctx, rawKey)
	if err != nil {
		t.Fatal(err)
	}
	backend := service.Backend.(*memoryBackend)
	physicalKey := "physical/" + rawKey
	backend.metadata[physicalKey] = map[string]string{InternalWriteIDMetadata: "newer-write"}
	applySplitWebDAVNamespaceV1(t, service, credentialID, time.Now().Unix(), true, false)

	err = service.RepairWebDAVNamespaceV1(ctx)
	if !errors.Is(err, ErrWebDAVNamespaceRepairAmbiguous) {
		t.Fatalf("repair error = %v", err)
	}
	if _, exists := backend.objects[physicalKey]; !exists {
		t.Fatal("changed remote object was deleted")
	}
	logicalKey := WebDAVMountPrefix(credentialID) + rawKey
	indexed, err := service.Index.GetObject(ctx, logicalKey)
	if err != nil || indexed.ObjectID != object.ObjectID || indexed.PhysicalKey != rawKey {
		t.Fatalf("indexed object = %#v, error = %v", indexed, err)
	}
	assertWriteIntentKey(t, service, intent.ID, rawKey)
	assertWebDAVNamespaceV2Marker(t, service, credentialID, false)
}

func TestRepairWebDAVNamespaceV1AbortsClientMultipartWithRawKey(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	const credentialID = "gamesync-id"
	const rawKey = "uploads/save.bin"

	base := service.Backend.(*memoryBackend)
	recorder := &namespaceRepairAbortRecorder{memoryBackend: base}
	service.Backend = recorder
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: rawKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadPart(ctx, UploadPartRequest{
		Key: rawKey, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("part"), Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	applySplitWebDAVNamespaceV1(t, service, credentialID, time.Now().Unix(), false, true)

	if err := service.RepairWebDAVNamespaceV1(ctx); err != nil {
		t.Fatal(err)
	}
	if len(recorder.abortKeys) != 1 || recorder.abortKeys[0] != rawKey {
		t.Fatalf("multipart abort keys = %#v", recorder.abortKeys)
	}
	if _, err := service.Index.GetMultipart(ctx, upload.ID); !errors.Is(err, ErrMultipartNotFound) {
		t.Fatalf("multipart error = %v", err)
	}
	if _, err := service.Index.GetWriteIntent(ctx, upload.WriteIntentID); !errors.Is(err, ErrWriteIntentNotFound) {
		t.Fatalf("write intent error = %v", err)
	}
	bucket, err := service.Index.GetBucket(ctx, upload.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.ReservedBytes != 0 {
		t.Fatalf("reserved bytes = %d", bucket.ReservedBytes)
	}
	assertWebDAVNamespaceV2Marker(t, service, credentialID, true)
}

func TestRepairWebDAVNamespaceV1WritesV2MarkerWithoutIntents(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	const credentialID = "gamesync-id"

	applySplitWebDAVNamespaceV1(t, service, credentialID, time.Now().Unix(), false, false)
	if err := service.RepairWebDAVNamespaceV1(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWebDAVNamespaceV2Marker(t, service, credentialID, true)
}

func TestRepairWebDAVNamespaceV1EmptyTargetAllowsModernIntents(t *testing.T) {
	t.Run("namespaced active multipart", func(t *testing.T) {
		service, _, cleanup := newFileManagerFixture(t)
		defer cleanup()
		ctx := context.Background()

		applySplitWebDAVNamespaceV1(t, service, "", time.Now().Add(-time.Minute).Unix(), false, false)
		upload, err := service.CreateMultipart(ctx, CreateMultipartInput{
			Key: WebDAVMountPrefix("modern-webdav") + "upload.bin",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.RepairWebDAVNamespaceV1(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Index.GetMultipart(ctx, upload.ID); err != nil {
			t.Fatalf("modern multipart was changed: %v", err)
		}
		assertWebDAVNamespaceV2Marker(t, service, "", true)
	})

	t.Run("raw S3 intent created after marker", func(t *testing.T) {
		service, _, cleanup := newFileManagerFixture(t)
		defer cleanup()
		ctx := context.Background()

		applySplitWebDAVNamespaceV1(t, service, "", time.Now().Add(-time.Minute).Unix(), false, false)
		intent, err := service.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "s3/new.bin", Size: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.RepairWebDAVNamespaceV1(ctx); err != nil {
			t.Fatal(err)
		}
		assertWriteIntentKey(t, service, intent.ID, intent.Key)
		assertWebDAVNamespaceV2Marker(t, service, "", true)
	})
}

func TestRepairWebDAVNamespaceV1EmptyTargetPreservesLegacyRawIntent(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	intent, err := service.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "legacy.bin", Size: 1}})
	if err != nil {
		t.Fatal(err)
	}
	applySplitWebDAVNamespaceV1(t, service, "", intent.CreatedAt.Unix(), false, false)

	err = service.RepairWebDAVNamespaceV1(ctx)
	if !errors.Is(err, ErrWebDAVNamespaceRepairAmbiguous) {
		t.Fatalf("repair error = %v", err)
	}
	assertWriteIntentKey(t, service, intent.ID, intent.Key)
	assertWebDAVNamespaceV2Marker(t, service, "", false)
}

func TestRepairWebDAVNamespaceV1CleansUnboundMultipartWithRawKey(t *testing.T) {
	t.Run("NoSuchUpload completes local cleanup", func(t *testing.T) {
		service, recorder, upload := prepareSplitUnboundMultipart(t)
		recorder.abortError = testRemoteError{code: "NoSuchUpload", status: 404}

		if err := service.RepairWebDAVNamespaceV1(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(recorder.abortKeys) != 1 || recorder.abortKeys[0] != "legacy/unbound.bin" {
			t.Fatalf("multipart abort keys = %#v", recorder.abortKeys)
		}
		if len(recorder.headKeys) != 1 || recorder.headKeys[0] != "legacy/unbound.bin" {
			t.Fatalf("multipart HEAD keys = %#v", recorder.headKeys)
		}
		if _, err := service.Index.GetMultipart(context.Background(), upload.ID); !errors.Is(err, ErrMultipartNotFound) {
			t.Fatalf("multipart error = %v", err)
		}
		assertWebDAVNamespaceV2Marker(t, service, "gamesync-id", true)
	})

	t.Run("NoSuchUpload with a raw remote object preserves local state", func(t *testing.T) {
		service, recorder, upload := prepareSplitUnboundMultipart(t)
		recorder.abortError = testRemoteError{code: "NoSuchUpload", status: 404}
		target, err := service.target(context.Background(), upload.BucketID)
		if err != nil {
			t.Fatal(err)
		}
		physicalKey := target.Bucket + "/legacy/unbound.bin"
		recorder.objects[physicalKey] = []byte("published")
		if recorder.etags == nil {
			recorder.etags = make(map[string]string)
		}
		recorder.etags[physicalKey] = "published-etag"

		err = service.RepairWebDAVNamespaceV1(context.Background())
		if !errors.Is(err, ErrWebDAVNamespaceRepairAmbiguous) {
			t.Fatalf("repair error = %v", err)
		}
		if len(recorder.headKeys) != 1 || recorder.headKeys[0] != "legacy/unbound.bin" {
			t.Fatalf("multipart HEAD keys = %#v", recorder.headKeys)
		}
		if _, err := service.Index.GetMultipart(context.Background(), upload.ID); err != nil {
			t.Fatalf("multipart was removed despite remote object: %v", err)
		}
		assertWebDAVNamespaceV2Marker(t, service, "gamesync-id", false)
	})

	t.Run("NoSuchUpload with an uncertain HEAD preserves local state", func(t *testing.T) {
		service, recorder, upload := prepareSplitUnboundMultipart(t)
		recorder.abortError = testRemoteError{code: "NoSuchUpload", status: 404}
		recorder.headError = errors.New("HEAD unavailable")

		err := service.RepairWebDAVNamespaceV1(context.Background())
		if !errors.Is(err, ErrWebDAVNamespaceRepairAmbiguous) || !strings.Contains(err.Error(), "HEAD unavailable") {
			t.Fatalf("repair error = %v", err)
		}
		if _, err := service.Index.GetMultipart(context.Background(), upload.ID); err != nil {
			t.Fatalf("multipart was removed after uncertain HEAD: %v", err)
		}
		assertWebDAVNamespaceV2Marker(t, service, "gamesync-id", false)
	})

	t.Run("abort failure preserves local state and marker", func(t *testing.T) {
		service, recorder, upload := prepareSplitUnboundMultipart(t)
		recorder.abortError = errors.New("abort unavailable")

		err := service.RepairWebDAVNamespaceV1(context.Background())
		if err == nil || !strings.Contains(err.Error(), "abort unavailable") {
			t.Fatalf("repair error = %v", err)
		}
		if _, err := service.Index.GetMultipart(context.Background(), upload.ID); err != nil {
			t.Fatalf("multipart was removed after failed abort: %v", err)
		}
		assertWebDAVNamespaceV2Marker(t, service, "gamesync-id", false)
	})
}

func TestRepairWebDAVNamespaceV1RejectsInvalidV2Marker(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := service.Index.db.ExecContext(ctx,
		"INSERT INTO system_settings(key, value, updated_at) VALUES(?, ?, ?)",
		webDAVNamespaceRepairSetting, "{", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	err := service.RepairWebDAVNamespaceV1(ctx)
	if err == nil || !strings.Contains(err.Error(), "invalid WebDAV namespace repair setting") {
		t.Fatalf("repair error = %v", err)
	}
}

func TestRepairWebDAVNamespaceV1LeavesPostMigrationS3IntentRaw(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	const credentialID = "gamesync-id"
	const rawKey = "s3/native.bin"

	applySplitWebDAVNamespaceV1(t, service, credentialID, time.Now().Add(-time.Minute).Unix(), false, false)
	intent, err := service.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: rawKey, Size: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RepairWebDAVNamespaceV1(ctx); err != nil {
		t.Fatal(err)
	}
	assertWriteIntentKey(t, service, intent.ID, rawKey)
	if _, err := service.Index.GetObject(ctx, WebDAVMountPrefix(credentialID)+rawKey); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("unexpected namespaced object error = %v", err)
	}
	assertWebDAVNamespaceV2Marker(t, service, credentialID, true)
}

type namespaceRepairAbortRecorder struct {
	*memoryBackend
	abortKeys []string
	headKeys  []string
	headError error
}

func prepareSplitUnboundMultipart(t *testing.T) (Service, *namespaceRepairAbortRecorder, MultipartUpload) {
	t.Helper()
	service, _, cleanup := newFileManagerFixture(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	base := service.Backend.(*memoryBackend)
	recorder := &namespaceRepairAbortRecorder{memoryBackend: base}
	service.Backend = recorder
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "legacy/unbound.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx,
		"UPDATE r2_multipart_uploads SET write_intent_id = NULL WHERE id = ?", upload.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.AbortWrite(ctx, upload.WriteIntentID); err != nil {
		t.Fatal(err)
	}
	applySplitWebDAVNamespaceV1(t, service, "gamesync-id", time.Now().Unix(), false, true)
	return service, recorder, upload
}

func (b *namespaceRepairAbortRecorder) AbortMultipart(
	ctx context.Context,
	target Target,
	key string,
	uploadID string,
) error {
	b.abortKeys = append(b.abortKeys, key)
	return b.memoryBackend.AbortMultipart(ctx, target, key, uploadID)
}

func (b *namespaceRepairAbortRecorder) Head(ctx context.Context, target Target, key string) (RemoteObject, error) {
	b.headKeys = append(b.headKeys, key)
	if b.headError != nil {
		return RemoteObject{}, b.headError
	}
	return b.memoryBackend.Head(ctx, target, key)
}

func applySplitWebDAVNamespaceV1(
	t *testing.T,
	service Service,
	credentialID string,
	updatedAt int64,
	prefixObjects bool,
	prefixMultipart bool,
) {
	t.Helper()
	ctx := context.Background()
	prefix := WebDAVMountPrefix(credentialID)
	if prefixObjects {
		if _, err := service.Index.db.ExecContext(ctx, "UPDATE r2_objects SET object_key = ? || object_key", prefix); err != nil {
			t.Fatal(err)
		}
	}
	if prefixMultipart {
		if _, err := service.Index.db.ExecContext(ctx, "UPDATE r2_multipart_uploads SET object_key = ? || object_key", prefix); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := encodeWebDAVNamespaceState(credentialID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx,
		"INSERT INTO system_settings(key, value, updated_at) VALUES(?, ?, ?)",
		webDAVNamespaceMigrationSetting, encoded, updatedAt); err != nil {
		t.Fatal(err)
	}
}

func assertWriteIntentKey(t *testing.T, service Service, intentID, wantKey string) {
	t.Helper()
	intent, err := service.Index.GetWriteIntent(context.Background(), intentID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Key != wantKey {
		t.Fatalf("write intent key = %q, want %q", intent.Key, wantKey)
	}
}

func assertWebDAVNamespaceV2Marker(t *testing.T, service Service, wantTarget string, wantPresent bool) {
	t.Helper()
	var encoded string
	err := service.Index.db.QueryRowContext(context.Background(),
		"SELECT value FROM system_settings WHERE key = ?", webDAVNamespaceRepairSetting).Scan(&encoded)
	if !wantPresent {
		if err == nil {
			t.Fatal("WebDAV namespace v2 marker was written")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeWebDAVNamespaceState(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if state.TargetCredentialID != wantTarget {
		t.Fatalf("v2 target = %q, want %q", state.TargetCredentialID, wantTarget)
	}
}
