// Package aetherrelaycredential encrypts recoverable credentials before they are
// persisted in the shared state database.
package aetherrelaycredential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	EnvironmentKey = "AETHERRELAY_CREDENTIAL_KEY"
	envelopePrefix = "aetherrelay-credential:v1:"
	keyBytes       = 32
)

type Codec struct{ aead cipher.AEAD }

func FromEnvironment() (*Codec, error) {
	value := strings.TrimSpace(os.Getenv(EnvironmentKey))
	if value == "" {
		return nil, fmt.Errorf("%s is required for encrypted credential storage", EnvironmentKey)
	}
	return New(value)
}

// DeriveKeyFromEnvironment derives a stable, purpose-bound server key without
// exposing or reusing the credential-encryption key directly.
func DeriveKeyFromEnvironment(purpose string) ([keyBytes]byte, error) {
	value := strings.TrimSpace(os.Getenv(EnvironmentKey))
	if value == "" {
		return [keyBytes]byte{}, fmt.Errorf("%s is required for key derivation", EnvironmentKey)
	}
	return DeriveKey(value, purpose)
}

// DeriveKey uses HMAC-SHA256 as a domain-separated PRF. Different purposes
// receive independent keys even though they share the deployment root key.
func DeriveKey(encodedKey, purpose string) ([keyBytes]byte, error) {
	key, err := decodeKey(encodedKey)
	if err != nil {
		return [keyBytes]byte{}, err
	}
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		return [keyBytes]byte{}, errors.New("derived key purpose is required")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("aetherrelay-derived-key:v1\n" + purpose))
	var derived [keyBytes]byte
	copy(derived[:], mac.Sum(nil))
	return derived, nil
}

func New(encodedKey string) (*Codec, error) {
	key, err := decodeKey(encodedKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize credential AEAD: %w", err)
	}
	return &Codec{aead: aead}, nil
}

func decodeKey(encodedKey string) ([]byte, error) {
	encodedKey = strings.TrimSpace(encodedKey)
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encodedKey)
	}
	if err != nil || len(key) != keyBytes {
		return nil, fmt.Errorf("credential key must be a base64-encoded %d-byte value", keyBytes)
	}
	return key, nil
}

func (c *Codec) Seal(scope, id string, plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("credential codec is unavailable")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate credential nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, plaintext, additionalData(scope, id))
	value := envelopePrefix + base64.RawStdEncoding.EncodeToString(sealed)
	return []byte(value), nil
}

func (c *Codec) Open(scope, id string, envelope []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("credential codec is unavailable")
	}
	value := strings.TrimSpace(string(envelope))
	if !strings.HasPrefix(value, envelopePrefix) {
		return nil, errors.New("credential payload is not an encrypted v1 envelope")
	}
	sealed, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, envelopePrefix))
	if err != nil || len(sealed) < c.aead.NonceSize()+c.aead.Overhead() {
		return nil, errors.New("credential envelope is malformed")
	}
	nonce, ciphertext := sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, additionalData(scope, id))
	if err != nil {
		return nil, errors.New("credential envelope cannot be decrypted with the active key")
	}
	return plaintext, nil
}

func additionalData(scope, id string) []byte { return []byte(scope + "\x00" + id) }
