// Package crypto provides envelope encryption for secrets stored at rest.
//
// # Threat model
//
// This package protects against database-only disclosure (e.g., a copied DB
// file or DB backup). It does NOT protect against full host compromise or an
// attacker who can read both the key file and the database, or inspect
// process memory. The master key is loaded from a file or environment
// variable; if the attacker has filesystem or process access, encryption at
// rest does not help.
//
// # Envelope format
//
// Data is encrypted using AES-256-GCM with a random per-data encryption key
// (DEK), which is itself wrapped (encrypted) by the master key. The key_id
// field identifies which master key was used; a future keyring could support
// decryption with multiple keys for rotation. Currently only one master key
// is supported.
//
// Both the DEK wrapping and the data encryption use Additional Authenticated
// Data (AAD) to bind the ciphertext to its context (tenant ID, backend ID,
// field purpose). This prevents an attacker from swapping ciphertext or DEKs
// between rows while still producing valid decryptions.
//
// The AAD is constructed as: "v1\x00" + purpose + "\x00" + tenantID + "\x00" + rowID
// and is used for BOTH the DEK wrap and the data encryption.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// KeySize is the master key length in bytes (AES-256).
const KeySize = 32

// nonceSize is the GCM standard nonce length.
const nonceSize = 12

// envelopeVersion is the format version byte embedded in AAD.
const envelopeVersion = "v1"

// MasterKey holds a master key with an associated key_id for rotation.
type MasterKey struct {
	keyID string
	key   []byte
}

// Envelope holds encrypted data and the wrapped DEK needed to decrypt it.
// The caller stores these fields in DB columns alongside the protected row.
// All fields are required for decryption.
type Envelope struct {
	KeyID         string // master key version tag
	DEKCiphertext []byte // DEK encrypted by the master key (AES-256-GCM, includes auth tag)
	Ciphertext    []byte // payload encrypted by the DEK (AES-256-GCM, includes auth tag)
	DataNonce     []byte // GCM nonce for the payload encryption
	DEKNonce      []byte // GCM nonce for the DEK wrapping
}

// AAD holds the context bound to encrypted data via Additional Authenticated
// Data. All fields are combined into a single byte string used as AAD for
// both the DEK wrap and the data encryption. This prevents ciphertext or DEKs
// from being swapped between rows.
type AAD struct {
	Purpose  string // field identifier, e.g. "backend.api_key"
	TenantID string // tenant scope
	RowID    string // immutable row identity (e.g., backend ID)
}

func (a AAD) bytes() []byte {
	return []byte(envelopeVersion + "\x00" + a.Purpose + "\x00" + a.TenantID + "\x00" + a.RowID)
}

// NewMasterKey creates a MasterKey from a raw 32-byte key and a key_id.
func NewMasterKey(keyID string, key []byte) (*MasterKey, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: master key must be %d bytes, got %d", KeySize, len(key))
	}
	if keyID == "" {
		return nil, errors.New("crypto: key_id is empty")
	}
	k := make([]byte, KeySize)
	copy(k, key)
	return &MasterKey{keyID: keyID, key: k}, nil
}

// GenerateMasterKey generates a random 32-byte master key.
func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("crypto: generate master key: %w", err)
	}
	return key, nil
}

// EncodeKey returns the hex encoding of a raw key.
func EncodeKey(key []byte) string {
	return hex.EncodeToString(key)
}

// DecodeKey decodes a key from hex or base64 encoding. It tries hex first,
// then base64 (standard and URL-safe), and returns an error if the result
// is not exactly KeySize bytes.
func DecodeKey(s string) ([]byte, error) {
	// Try hex first (most common for key material).
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	// Try standard base64.
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	// Try URL-safe base64.
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, errors.New("crypto: key is not valid hex or base64")
}

// Encrypt encrypts plaintext using AES-256-GCM with envelope encryption:
//  1. Generate a random DEK (32 bytes).
//  2. Encrypt the plaintext with the DEK, using aad as AAD.
//  3. Encrypt (wrap) the DEK with the master key, using aad as AAD.
//  4. Return the Envelope containing all components.
//
// The aad is bound to both encryption layers so swapping ciphertext or DEKs
// between rows with different AAD will fail decryption.
func (mk *MasterKey) Encrypt(plaintext []byte, aad AAD) (*Envelope, error) {
	aadBytes := aad.bytes()

	// Generate DEK.
	dek := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("crypto: generate DEK: %w", err)
	}

	// Encrypt payload with DEK.
	payloadCipher, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	dataNonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, dataNonce); err != nil {
		return nil, fmt.Errorf("crypto: generate data nonce: %w", err)
	}
	ciphertext := payloadCipher.Seal(nil, dataNonce, plaintext, aadBytes)

	// Wrap DEK with master key.
	masterCipher, err := newGCM(mk.key)
	if err != nil {
		return nil, err
	}
	dekNonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, dekNonce); err != nil {
		return nil, fmt.Errorf("crypto: generate DEK nonce: %w", err)
	}
	dekCipher := masterCipher.Seal(nil, dekNonce, dek, aadBytes)

	return &Envelope{
		KeyID:         mk.keyID,
		DEKCiphertext: dekCipher,
		Ciphertext:    ciphertext,
		DataNonce:     dataNonce,
		DEKNonce:      dekNonce,
	}, nil
}

// Decrypt reverses Encrypt: unwraps the DEK with the master key, then
// decrypts the payload ciphertext. The aad must match the AAD used during
// encryption or decryption will fail (GCM auth tag mismatch).
func (mk *MasterKey) Decrypt(env *Envelope, aad AAD) ([]byte, error) {
	if env.KeyID != mk.keyID {
		return nil, fmt.Errorf("crypto: key_id mismatch (have %q, want %q)", env.KeyID, mk.keyID)
	}
	if len(env.DEKCiphertext) == 0 || len(env.Ciphertext) == 0 {
		return nil, errors.New("crypto: empty ciphertext in envelope")
	}
	if len(env.DEKNonce) != nonceSize || len(env.DataNonce) != nonceSize {
		return nil, fmt.Errorf("crypto: invalid nonce length (DEK %d, data %d; want %d)", len(env.DEKNonce), len(env.DataNonce), nonceSize)
	}

	aadBytes := aad.bytes()

	// Unwrap DEK.
	masterCipher, err := newGCM(mk.key)
	if err != nil {
		return nil, err
	}
	dek, err := masterCipher.Open(nil, env.DEKNonce, env.DEKCiphertext, aadBytes)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap DEK: %w", err)
	}

	// Decrypt payload.
	payloadCipher, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	plaintext, err := payloadCipher.Open(nil, env.DataNonce, env.Ciphertext, aadBytes)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt payload: %w", err)
	}
	return plaintext, nil
}

// LoadMasterKeyFromFile reads a key from a file, trims whitespace, decodes it,
// and returns a MasterKey with the given key_id. The file must have 0600
// permissions (owner-only readable) to prevent key exposure to other users.
func LoadMasterKeyFromFile(keyID, path string) (*MasterKey, error) {
	// Verify file permissions before reading.
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: stat key file %s: %w", path, err)
	}
	// Reject non-regular files (symlinks, pipes, etc.).
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("crypto: key file %s is not a regular file", path)
	}
	// Reject group/world-readable files.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("crypto: key file %s has permissions %o; must be 0600 (owner-only)", path, perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: read key file %s: %w", path, err)
	}
	// Trim whitespace and newlines.
	s := strings.TrimSpace(string(data))
	key, err := DecodeKey(s)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode key from %s: %w", path, err)
	}
	return NewMasterKey(keyID, key)
}

// WriteMasterKeyFile writes a raw key to a file with 0600 permissions.
// If the file already exists, it is NOT overwritten (uses O_EXCL) to prevent
// accidental clobbering of an existing key.
func WriteMasterKeyFile(path string, key []byte) error {
	encoded := EncodeKey(key) + "\n"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("crypto: create key file %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(encoded); err != nil {
		return fmt.Errorf("crypto: write key file %s: %w", path, err)
	}
	return nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: create GCM: %w", err)
	}
	return gcm, nil
}
