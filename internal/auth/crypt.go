package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// Cipher seals secrets held in the database — today the users' GitHub OAuth
// tokens — with AES-256-GCM under the server key.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a 32 byte key.
func NewCipher(key []byte) (*Cipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Seal returns nonce || ciphertext.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// ErrSealed reports a value that cannot be decrypted with the current key,
// which normally means the key changed or the row was tampered with.
var ErrSealed = errors.New("cannot decrypt stored secret")

// Open reverses Seal.
func (c *Cipher) Open(sealed []byte) ([]byte, error) {
	n := c.aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrSealed
	}
	plaintext, err := c.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, ErrSealed
	}
	return plaintext, nil
}
