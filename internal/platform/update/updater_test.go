package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionLess(t *testing.T) {
	t.Parallel()
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.10.0", "v0.9.0", false},
		{"v0.9.0", "v0.10.0", true},
		{"v1.0.0", "v1.0.1", true},
		{"dev", "v0.1.0", true},
		{"v0.1.0", "", false},
		{"v1.2.3-rc1", "v1.2.3", false},
	}
	for _, c := range cases {
		if got := versionLess(c.current, c.latest); got != c.want {
			t.Fatalf("versionLess(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestCheckAndApplySwapsExecutable(t *testing.T) {
	t.Parallel()

	newBinary := []byte("brand new binary bytes")
	digest := sha256.Sum256(newBinary)
	assetName := AssetName()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name":     "v9.9.9",
				"body":         "notes",
				"published_at": "2026-07-26T00:00:00Z",
				"assets": []map[string]any{
					{"name": assetName, "size": len(newBinary), "browser_download_url": server.URL + "/download/" + assetName},
					{"name": "checksums.txt", "size": 100, "browser_download_url": server.URL + "/download/checksums.txt"},
				},
			})
		case "/download/" + assetName:
			_, _ = w.Write(newBinary)
		case "/download/checksums.txt":
			_, _ = w.Write([]byte(hex.EncodeToString(digest[:]) + "  " + assetName + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exe := filepath.Join(t.TempDir(), "cf-r2-manager")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	updater := &Updater{
		Repo: "owner/repo", CurrentVersion: "v0.1.0",
		BaseURL: server.URL, Client: server.Client(), ExecutablePath: exe,
	}

	info, err := updater.Check(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !info.UpdateAvailable || info.LatestVersion != "v9.9.9" || info.AssetName != assetName {
		t.Fatalf("check = %#v", info)
	}

	version, err := updater.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "v9.9.9" {
		t.Fatalf("applied version = %q", version)
	}
	installed, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != string(newBinary) {
		t.Fatalf("executable content = %q", installed)
	}
	backup, err := os.ReadFile(exe + ".old")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "old binary" {
		t.Fatalf("backup content = %q", backup)
	}
	updater.CleanupOld()
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatalf("backup should be removed, stat err = %v", err)
	}
}

func TestApplyRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()

	assetName := AssetName()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]any{
					{"name": assetName, "size": 4, "browser_download_url": server.URL + "/download/" + assetName},
					{"name": "checksums.txt", "size": 100, "browser_download_url": server.URL + "/download/checksums.txt"},
				},
			})
		case "/download/" + assetName:
			_, _ = w.Write([]byte("evil"))
		case "/download/checksums.txt":
			_, _ = w.Write([]byte("deadbeef  " + assetName + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exe := filepath.Join(t.TempDir(), "cf-r2-manager")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := &Updater{
		Repo: "owner/repo", CurrentVersion: "v0.1.0",
		BaseURL: server.URL, Client: server.Client(), ExecutablePath: exe,
	}
	if _, err := updater.Apply(context.Background()); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	current, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "old binary" {
		t.Fatalf("executable should be untouched, content = %q", current)
	}
}
