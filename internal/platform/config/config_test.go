package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMigratesLegacyAdminListener(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("data_dir: " + filepath.ToSlash(dir) + "\nlisteners:\n  admin: 127.0.0.1:18080\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listeners.HTTP != "127.0.0.1:18080" {
		t.Fatalf("http listener = %q", cfg.Listeners.HTTP)
	}
	if cfg.Listeners.S3 != "" || cfg.Listeners.WebDAV != "" || cfg.Listeners.AI != "" {
		t.Fatalf("legacy protocol listeners should be disabled: %#v", cfg.Listeners)
	}
	if cfg.R2.LogicalBucket != "storage" {
		t.Fatalf("logical bucket = %q", cfg.R2.LogicalBucket)
	}
	if cfg.DatabasePath != filepath.Join(dir, "manager.db") {
		t.Fatalf("database path = %q", cfg.DatabasePath)
	}
}

func TestLoadPrefersHTTPListenerOverLegacyAdmin(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("data_dir: " + filepath.ToSlash(dir) + "\nlisteners:\n  http: 127.0.0.1:19090\n  admin: 127.0.0.1:18080\n  s3: 0.0.0.0:19091\n  webdav: 0.0.0.0:19092\n  ai: 0.0.0.0:19093\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listeners.HTTP != "127.0.0.1:19090" {
		t.Fatalf("http listener = %q", cfg.Listeners.HTTP)
	}
}

func TestValidateRejectsPublicMetricsListener(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Listeners.Metrics = "0.0.0.0:9090"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected public metrics listener to be rejected")
	}
}

func TestLoadInheritsAccountStorageLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("data_dir: " + filepath.ToSlash(dir) + "\nr2:\n  storage_soft_limit_bytes: 12345\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.R2.AccountStorageSoftLimit != 12345 {
		t.Fatalf("account storage limit = %d", cfg.R2.AccountStorageSoftLimit)
	}
}
