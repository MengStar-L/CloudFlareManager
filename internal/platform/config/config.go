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
	HTTP string `yaml:"http"`
	// Admin, S3, WebDAV, and AI are retained for loading pre-unified configs.
	// Only Admin is used, as a fallback when HTTP is not explicitly configured.
	Admin   string `yaml:"admin"`
	S3      string `yaml:"s3"`
	WebDAV  string `yaml:"webdav"`
	AI      string `yaml:"ai"`
	Metrics string `yaml:"metrics"`
}

type R2Config struct {
	LogicalBucket           string `yaml:"logical_bucket"`
	TempDir                 string `yaml:"temp_dir"`
	StorageSoftLimit        int64  `yaml:"storage_soft_limit_bytes"`
	AccountStorageSoftLimit int64  `yaml:"account_storage_soft_limit_bytes"`
	ClassASoftLimit         int64  `yaml:"class_a_soft_limit"`
	ClassBSoftLimit         int64  `yaml:"class_b_soft_limit"`
	// UploadChunkBytes 是服务端强制分片的块大小；超过该值（或长度未知）的
	// 单次 PUT 会切块经 multipart 转发，本地磁盘峰值仅为单块大小。
	UploadChunkBytes int64 `yaml:"upload_chunk_bytes"`
}

type AIConfig struct {
	NeuronSoftLimit  float64 `yaml:"neuron_soft_limit"`
	MaxRetryAccounts int     `yaml:"max_retry_accounts"`
}

func Default() Config {
	return Config{
		DataDir:        "/opt/CloudFlareManager/data",
		DatabasePath:   "/opt/CloudFlareManager/data/manager.db",
		MasterKeyFile:  "/opt/CloudFlareManager/data/master.key",
		LogLevel:       "info",
		TrustedProxies: []string{"127.0.0.1/32"},
		Listeners: Listeners{
			HTTP: "0.0.0.0:14325", Metrics: "127.0.0.1:14329",
		},
		R2: R2Config{
			LogicalBucket: "storage", StorageSoftLimit: 9_000_000_000, AccountStorageSoftLimit: 9_000_000_000,
			ClassASoftLimit: 900_000, ClassBSoftLimit: 9_000_000,
			UploadChunkBytes: 64 << 20,
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
	if raw.Listeners.HTTP == "" && raw.Listeners.Admin != "" {
		cfg.Listeners.HTTP = raw.Listeners.Admin
	}
	if raw.R2.AccountStorageSoftLimit <= 0 {
		cfg.R2.AccountStorageSoftLimit = cfg.R2.StorageSoftLimit
	}
	// R2 multipart 分片除末块外最小 5 MiB；过小的配置会在完成分片时被拒绝。
	if cfg.R2.UploadChunkBytes <= 0 {
		cfg.R2.UploadChunkBytes = 64 << 20
	} else if cfg.R2.UploadChunkBytes < 5<<20 {
		cfg.R2.UploadChunkBytes = 5 << 20
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
	if c.R2.StorageSoftLimit <= 0 || c.R2.AccountStorageSoftLimit <= 0 || c.R2.ClassASoftLimit <= 0 || c.R2.ClassBSoftLimit <= 0 {
		return errors.New("R2 soft limits must be positive")
	}
	for name, addr := range map[string]string{
		"http": c.Listeners.HTTP, "metrics": c.Listeners.Metrics,
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
