package crypto

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

var testAAD = AAD{Purpose: "test", TenantID: "tnt_1", RowID: "row_1"}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	mk, _ := NewMasterKey("v1", mustGenKey(t))

	plaintext := []byte("sk-abcdefghijklmnopqrstuvwxyz0123456789")
	env, err := mk.Encrypt(plaintext, testAAD)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	decrypted, err := mk.Decrypt(env, testAAD)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted = %q, want %q", decrypted, plaintext)
	}
}

func TestEncryptDifferentEachTime(t *testing.T) {
	mk, _ := NewMasterKey("v1", mustGenKey(t))
	plaintext := []byte("secret")

	env1, _ := mk.Encrypt(plaintext, testAAD)
	env2, _ := mk.Encrypt(plaintext, testAAD)
	if bytes.Equal(env1.Ciphertext, env2.Ciphertext) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext")
	}
	if bytes.Equal(env1.DataNonce, env2.DataNonce) {
		t.Fatal("two encryptions produced identical data nonces")
	}
}

func TestDecryptWrongKeyID(t *testing.T) {
	mk1, _ := NewMasterKey("v1", mustGenKey(t))
	env, _ := mk1.Encrypt([]byte("secret"), testAAD)

	mk2, _ := NewMasterKey("v2", mustGenKey(t))
	_, err := mk2.Decrypt(env, testAAD)
	if err == nil {
		t.Fatal("Decrypt with wrong key_id should fail")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	mk1, _ := NewMasterKey("v1", mustGenKey(t))
	env, _ := mk1.Encrypt([]byte("secret"), testAAD)

	// Same key_id, different key bytes.
	mk2, _ := NewMasterKey("v1", mustGenKey(t))
	_, err := mk2.Decrypt(env, testAAD)
	if err == nil {
		t.Fatal("Decrypt with wrong key should fail")
	}
}

func TestDecryptTamperedCiphertext(t *testing.T) {
	mk, _ := NewMasterKey("v1", mustGenKey(t))
	env, _ := mk.Encrypt([]byte("secret"), testAAD)

	env.Ciphertext[0] ^= 0xff
	_, err := mk.Decrypt(env, testAAD)
	if err == nil {
		t.Fatal("Decrypt with tampered ciphertext should fail")
	}
}

func TestDecryptTamperedDEK(t *testing.T) {
	mk, _ := NewMasterKey("v1", mustGenKey(t))
	env, _ := mk.Encrypt([]byte("secret"), testAAD)

	env.DEKCiphertext[0] ^= 0xff
	_, err := mk.Decrypt(env, testAAD)
	if err == nil {
		t.Fatal("Decrypt with tampered DEK should fail")
	}
}

func TestDecryptWrongAAD(t *testing.T) {
	mk, _ := NewMasterKey("v1", mustGenKey(t))
	env, _ := mk.Encrypt([]byte("secret"), testAAD)

	wrongAAD := AAD{Purpose: "test", TenantID: "tnt_2", RowID: "row_1"}
	_, err := mk.Decrypt(env, wrongAAD)
	if err == nil {
		t.Fatal("Decrypt with wrong AAD (different tenant) should fail")
	}

	wrongAAD2 := AAD{Purpose: "test", TenantID: "tnt_1", RowID: "row_2"}
	_, err = mk.Decrypt(env, wrongAAD2)
	if err == nil {
		t.Fatal("Decrypt with wrong AAD (different row) should fail")
	}

	wrongAAD3 := AAD{Purpose: "different", TenantID: "tnt_1", RowID: "row_1"}
	_, err = mk.Decrypt(env, wrongAAD3)
	if err == nil {
		t.Fatal("Decrypt with wrong AAD (different purpose) should fail")
	}
}

func TestNewMasterKeyValidation(t *testing.T) {
	if _, err := NewMasterKey("v1", make([]byte, 16)); err == nil {
		t.Fatal("NewMasterKey with 16-byte key should fail")
	}
	if _, err := NewMasterKey("", mustGenKey(t)); err == nil {
		t.Fatal("NewMasterKey with empty key_id should fail")
	}
}

func TestEncodeDecodeKey(t *testing.T) {
	key, _ := GenerateMasterKey()
	encoded := EncodeKey(key)
	decoded, err := DecodeKey(encoded)
	if err != nil {
		t.Fatalf("DecodeKey: %v", err)
	}
	if !bytes.Equal(decoded, key) {
		t.Fatal("decode(encode(key)) != key")
	}
}

func TestDecodeKeyBase64(t *testing.T) {
	key, _ := GenerateMasterKey()
	encoded := hex.EncodeToString(key)
	decoded, err := DecodeKey(encoded)
	if err != nil {
		t.Fatalf("DecodeKey hex: %v", err)
	}
	if !bytes.Equal(decoded, key) {
		t.Fatal("hex decode mismatch")
	}
}

func TestLoadMasterKeyFromFile(t *testing.T) {
	key, _ := GenerateMasterKey()
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := WriteMasterKeyFile(path, key); err != nil {
		t.Fatalf("WriteMasterKeyFile: %v", err)
	}

	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file perm = %o, want 0600", perm)
	}

	mk, err := LoadMasterKeyFromFile("v1", path)
	if err != nil {
		t.Fatalf("LoadMasterKeyFromFile: %v", err)
	}

	plaintext := []byte("test-secret")
	env, _ := mk.Encrypt(plaintext, testAAD)
	decrypted, _ := mk.Decrypt(env, testAAD)
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("round-trip failed: %q != %q", decrypted, plaintext)
	}
}

func TestLoadMasterKeyFromFileRejectsPermissive(t *testing.T) {
	key, _ := GenerateMasterKey()
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	// Write with permissive permissions.
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadMasterKeyFromFile("v1", path)
	if err == nil {
		t.Fatal("LoadMasterKeyFromFile should reject 0644 permissions")
	}
}

func TestWriteMasterKeyFileRefusesOverwrite(t *testing.T) {
	key, _ := GenerateMasterKey()
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	if err := WriteMasterKeyFile(path, key); err != nil {
		t.Fatalf("WriteMasterKeyFile: %v", err)
	}
	// Second write should fail (O_EXCL).
	if err := WriteMasterKeyFile(path, key); err == nil {
		t.Fatal("WriteMasterKeyFile should refuse to overwrite existing file")
	}
}

func TestReEncryptWithNewKey(t *testing.T) {
	// Simulate rotation: decrypt with old key, re-encrypt with new key.
	mk1, _ := NewMasterKey("v1", mustGenKey(t))
	plaintext := []byte("sk-test-key-rotation")
	env, _ := mk1.Encrypt(plaintext, testAAD)

	decrypted, err := mk1.Decrypt(env, testAAD)
	if err != nil {
		t.Fatalf("Decrypt with old key: %v", err)
	}

	mk2, _ := NewMasterKey("v2", mustGenKey(t))
	env2, err := mk2.Encrypt(decrypted, testAAD)
	if err != nil {
		t.Fatalf("Re-encrypt with new key: %v", err)
	}

	decrypted2, err := mk2.Decrypt(env2, testAAD)
	if err != nil {
		t.Fatalf("Decrypt with new key: %v", err)
	}
	if !bytes.Equal(decrypted2, plaintext) {
		t.Fatalf("rotation changed plaintext")
	}

	// Old key can't decrypt new envelope (different key_id).
	_, err = mk1.Decrypt(env2, testAAD)
	if err == nil {
		t.Fatal("old key should not decrypt re-encrypted data")
	}
}

func TestDecryptMalformedEnvelopes(t *testing.T) {
	mk, _ := NewMasterKey("v1", mustGenKey(t))
	plaintext := []byte("test")
	env, _ := mk.Encrypt(plaintext, testAAD)

	// Truncated ciphertext.
	bad := *env
	bad.Ciphertext = bad.Ciphertext[:1]
	_, err := mk.Decrypt(&bad, testAAD)
	if err == nil {
		t.Fatal("truncated ciphertext should fail")
	}

	// Empty ciphertext.
	bad2 := *env
	bad2.Ciphertext = nil
	_, err = mk.Decrypt(&bad2, testAAD)
	if err == nil {
		t.Fatal("empty ciphertext should fail")
	}

	// Invalid nonce length.
	bad3 := *env
	bad3.DataNonce = []byte{1, 2, 3}
	_, err = mk.Decrypt(&bad3, testAAD)
	if err == nil {
		t.Fatal("invalid nonce length should fail")
	}

	// Unknown key_id.
	bad4 := *env
	bad4.KeyID = "unknown"
	_, err = mk.Decrypt(&bad4, testAAD)
	if err == nil {
		t.Fatal("unknown key_id should fail")
	}
}

func mustGenKey(t *testing.T) []byte {
	t.Helper()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	return key
}
