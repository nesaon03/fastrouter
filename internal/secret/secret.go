// Package secret encrypts values at rest with AES-256-GCM using a key kept in
// a local key file (mode 0600). It also redacts secrets from log text.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"regexp"
)

// Box seals and opens secrets.
type Box struct{ aead cipher.AEAD }

// LoadOrCreate reads the 32-byte key at path, creating it with 0600 if absent.
func LoadOrCreate(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key file %s: want 32 bytes, got %d", path, len(key))
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext; the nonce is prepended to the result.
func (b *Box) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, nil), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}

var redactRe = regexp.MustCompile(`(?i)(bearer\s+|x-api-key:\s*|api[_-]?key["'=:\s]+|token["'=:\s]+|password["'=:\s]+)[^\s"',}]+|\b(sk|tp|ak|rk|pk|xai|nvapi|AIza)[-_A-Za-z0-9]{12,}`)

// Redact masks anything that looks like a credential in s.
func Redact(s string) string {
	return redactRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := redactRe.FindStringSubmatch(m)
		if sub[1] != "" {
			return sub[1] + "[REDACTED]"
		}
		return "[REDACTED]"
	})
}
