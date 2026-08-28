package auth

import (
	"bytes"
	"errors"
	"testing"
)

func testKey(fill byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	return key
}

func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher(testKey(1))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	plaintext := []byte("gho_a-github-token")
	sealed, err := c.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("sealed value still contains the plaintext")
	}

	opened, err := c.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Errorf("Open returned %q, want %q", opened, plaintext)
	}
}

func TestCipherSealIsRandomized(t *testing.T) {
	c, _ := NewCipher(testKey(1))
	first, _ := c.Seal([]byte("same"))
	second, _ := c.Seal([]byte("same"))
	if bytes.Equal(first, second) {
		t.Error("two seals of the same plaintext are identical: the nonce is not random")
	}
}

func TestCipherOpenRejectsTamperedAndForeignValues(t *testing.T) {
	c, _ := NewCipher(testKey(1))
	sealed, _ := c.Seal([]byte("secret"))

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := c.Open(tampered); !errors.Is(err, ErrSealed) {
		t.Errorf("Open(tampered) error = %v, want ErrSealed", err)
	}

	if _, err := c.Open([]byte("short")); !errors.Is(err, ErrSealed) {
		t.Errorf("Open(short) error = %v, want ErrSealed", err)
	}

	other, _ := NewCipher(testKey(2))
	if _, err := other.Open(sealed); !errors.Is(err, ErrSealed) {
		t.Errorf("Open with another key: error = %v, want ErrSealed", err)
	}
}

func TestNewCipherRejectsShortKey(t *testing.T) {
	if _, err := NewCipher([]byte("too short")); err == nil {
		t.Fatal("NewCipher accepted a short key")
	}
}
