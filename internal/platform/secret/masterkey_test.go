package secret

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadMasterKeySupportsRawAndBase64(t *testing.T) {
	t.Parallel()

	want := bytes.Repeat([]byte{9}, KeySize)
	for name, contents := range map[string][]byte{
		"raw":    want,
		"base64": []byte(base64.StdEncoding.EncodeToString(want) + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "master.key")
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadMasterKey(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("master key mismatch")
			}
		})
	}
}

func TestWriteMasterKeyCreatesPrivateFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "keys", "master.key")
	key, err := WriteMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeySize {
		t.Fatalf("key length = %d", len(key))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("master key permissions = %o", info.Mode().Perm())
	}
}
