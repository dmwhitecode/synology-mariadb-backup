// Package vault encrypts and decrypts stored secrets with AES-256-GCM.
//
// The data-encryption key lives in its own file, created on first run with
// mode 0600. It is never stored alongside the encrypted data and never
// leaves this package.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyBytes = 32 // AES-256
	prefix   = "v1:"
)

// Vault holds the AEAD cipher for the master key.
type Vault struct {
	aead cipher.AEAD
}

// Open loads the master key from path, generating a new one if the file does
// not exist. The key file is created with mode 0600.
func Open(path string) (*Vault, error) {
	key, err := loadKey(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err = createKey(path)
	}
	if err != nil {
		return nil, err
	}
	return New(key)
}

// New builds a Vault from a raw 32-byte key. Used by Open and by tests.
func New(key []byte) (*Vault, error) {
	if len(key) != keyBytes {
		return nil, fmt.Errorf("vault: key must be %d bytes, got %d", keyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// Encrypt returns a self-describing ciphertext string for plaintext.
func (v *Vault) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := v.aead.Seal(nil, nonce, []byte(plaintext), []byte(prefix))
	return prefix + base64.RawStdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Decrypt reverses Encrypt. It fails if the ciphertext was tampered with or
// was produced with a different key.
func (v *Vault) Decrypt(ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, prefix) {
		return "", errors.New("vault: unrecognised ciphertext format")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(ciphertext, prefix))
	if err != nil {
		return "", errors.New("vault: malformed ciphertext")
	}
	ns := v.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("vault: malformed ciphertext")
	}
	plain, err := v.aead.Open(nil, raw[:ns], raw[ns:], []byte(prefix))
	if err != nil {
		return "", errors.New("vault: decryption failed")
	}
	return string(plain), nil
}

func loadKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("vault: key file %s is not valid hex", path)
	}
	if len(key) != keyBytes {
		return nil, fmt.Errorf("vault: key file %s has wrong length", path)
	}
	return key, nil
}

func createKey(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// O_EXCL so two concurrent starts cannot both write a key.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vault: create key file: %w", err)
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}
