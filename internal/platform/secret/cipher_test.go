package secret

import (
	"bytes"
	"testing"
)

func TestCipherRoundTripAndAADBinding(t *testing.T) {
	t.Parallel()

	key := bytes.Repeat([]byte{0x42}, KeySize)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := c.Encrypt([]byte("token-value"), []byte("account:1"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := c.Decrypt(sealed, []byte("account:1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "token-value" {
		t.Fatalf("plain = %q", plain)
	}
	if _, err := c.Decrypt(sealed, []byte("account:2")); err == nil {
		t.Fatal("expected associated-data mismatch")
	}
}

func TestCipherRejectsInvalidKeySize(t *testing.T) {
	t.Parallel()

	if _, err := NewCipher([]byte("short")); err == nil {
		t.Fatal("expected invalid key size")
	}
}
