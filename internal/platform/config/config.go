package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DataDir        string    `yaml:"data_dir"`
	DatabasePath   string    `yaml:"database_path"`
	MasterKeyFile  string    `yaml:"master_key_file"`
	LogLevel       string    `yaml:"log_level"`
	TrustedProxies []string  `yaml:"trusted_proxies"`
	Listeners      Listeners `yaml:"listeners"`
	R2             R2Config  `yaml:"r2"`
	AI             AIConfig  `yaml:"ai"`
}

type Listeners struct {
	Admin   string `yaml:"admin"`
	S3      string `yaml:"s3"`
	WebDAV  string `yaml:"webdav"`
	AI      string `yaml:"ai"`
	Metrics string `yaml:"metrics"`
}

type R2Config struct {
	LogicalBucket    string `yaml:"logical_bucket"`
	TempDir          string `yaml:"temp_dir"`
	StorageSoftLimit int64  `yaml:"storage_soft_limit_bytes"`
	ClassASoftLimit  int64  `yaml:"class_a_soft_limit"`
	ClassBSoftLimit  int64  `yaml:"class_b_soft_limit"`
}

type AIConfig struct {
	NeuronSoftLimit  float64 `yaml:"neuron_soft_limit"`
	MaxRetryAccounts int     `yaml:"max_retry_accounts"`
}

func Default() Config {
	return Config{
		DataDir:        "/var/lib/cf-r2-manager",
		DatabasePath:   "/var/lib/cf-r2-manager/manager.db",
		MasterKeyFile:  "/var/lib/cf-r2-manager/master.key",
		LogLevel:       "info",
		TrustedProxies: []string{"127.0.0.1/32"},
		Listeners: Listeners{
			Admin: "127.0.0.1:8080", Metrics: "127.0.0.1:9090",
			S3: "127.0.0.1:9000", WebDAV: "127.0.0.1:9001", AI: "127.0.0.1:9002",
		},
		R2: R2Config{
			LogicalBucket: "storage", StorageSoftLimit: 9_000_000_000,
			ClassASoftLimit: 900_000, ClassBSoftLimit: 9_000_000,
		},
		AI: AIConfig{NeuronSoftLimit: 9_000, MaxRetryAccounts: 2},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var raw Config
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("merge config: %w", err)
	}
	if raw.DataDir != "" {
		if raw.DatabasePath == "" {
			cfg.DatabasePath = filepath.Join(raw.DataDir, "manager.db")
		}
		if raw.MasterKeyFile == "" {
			cfg.MasterKeyFile = filepath.Join(raw.DataDir, "master.key")
		}
		if raw.R2.TempDir == "" {
			cfg.R2.TempDir = filepath.Join(raw.DataDir, "tmp")
		}
	}
	if cfg.R2.TempDir == "" {
		cfg.R2.TempDir = filepath.Join(cfg.DataDir, "tmp")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.DataDir == "" || c.DatabasePath == "" || c.MasterKeyFile == "" {
		return errors.New("data_dir, database_path, and master_key_file are required")
	}
	if c.R2.LogicalBucket == "" {
		return errors.New("r2.logical_bucket is required")
	}
	for name, addr := range map[string]string{
		"admin": c.Listeners.Admin, "s3": c.Listeners.S3, "webdav": c.Listeners.WebDAV,
		"ai": c.Listeners.AI, "metrics": c.Listeners.Metrics,
	} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("invalid %s listener: %w", name, err)
		}
	}
	host, _, _ := net.SplitHostPort(c.Listeners.Metrics)
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("metrics listener must use a loopback address")
	}
	return nil
}
