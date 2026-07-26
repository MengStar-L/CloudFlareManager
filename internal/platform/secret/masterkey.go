package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const systemdCredentialName = "master-key"

func ResolveMasterKeyPath(configured string) string {
	if directory := os.Getenv("CREDENTIALS_DIRECTORY"); directory != "" {
		path := filepath.Join(directory, systemdCredentialName)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return configured
}

func LoadMasterKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("master key path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	if len(data) == KeySize {
		return data, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(decoded) != KeySize {
		return nil, fmt.Errorf("master key must contain exactly %d raw bytes or their base64 encoding", KeySize)
	}
	return decoded, nil
}

func WriteMasterKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("master key path is required")
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("master key already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create master key directory: %w", err)
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write master key: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("set master key permissions: %w", err)
	}
	return key, nil
}
