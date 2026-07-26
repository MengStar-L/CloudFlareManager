package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesDefaultsAndOverrides(t *testing.T) {
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
	if cfg.Listeners.Admin != "127.0.0.1:18080" {
		t.Fatalf("admin listener = %q", cfg.Listeners.Admin)
	}
	if cfg.Listeners.S3 != "127.0.0.1:9000" {
		t.Fatalf("s3 default = %q", cfg.Listeners.S3)
	}
	if cfg.R2.LogicalBucket != "storage" {
		t.Fatalf("logical bucket = %q", cfg.R2.LogicalBucket)
	}
	if cfg.DatabasePath != filepath.Join(dir, "manager.db") {
		t.Fatalf("database path = %q", cfg.DatabasePath)
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
