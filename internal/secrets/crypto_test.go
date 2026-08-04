package secrets

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generateTestKey creates a random 32-byte AES-256 key for testing.
func generateTestKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, requiredKeyLength)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

// --- Crypto tests ---

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := generateTestKey(t)
	plaintext := []byte("hello, secrets world!")

	ciphertext, err := Encrypt(key, plaintext)
	require.NoError(t, err)
	require.NotEmpty(t, ciphertext)

	decrypted, err := Decrypt(key, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestEncryptProducesDifferentCiphertexts(t *testing.T) {
	key := generateTestKey(t)
	plaintext := []byte("same plaintext")

	ct1, err := Encrypt(key, plaintext)
	require.NoError(t, err)

	ct2, err := Encrypt(key, plaintext)
	require.NoError(t, err)

	assert.NotEqual(t, ct1, ct2, "encryption of same plaintext should produce different ciphertexts (random nonce)")
}

func TestDecryptWrongKey(t *testing.T) {
	key := generateTestKey(t)
	wrongKey := generateTestKey(t)
	plaintext := []byte("secret data")

	ciphertext, err := Encrypt(key, plaintext)
	require.NoError(t, err)

	_, err = Decrypt(wrongKey, ciphertext)
	assert.Error(t, err)
}

func TestDecryptCorruptedCiphertext(t *testing.T) {
	key := generateTestKey(t)
	plaintext := []byte("secret data")

	ciphertext, err := Encrypt(key, plaintext)
	require.NoError(t, err)

	// Corrupt a byte in the middle of the ciphertext.
	corrupted := make([]byte, len(ciphertext))
	copy(corrupted, ciphertext)
	corrupted[len(corrupted)/2] ^= 0xFF

	_, err = Decrypt(key, corrupted)
	assert.Error(t, err)
}

func TestEncryptInvalidKeyLength(t *testing.T) {
	plaintext := []byte("test")
	cases := []int{16, 64, 0, 31}
	for _, keyLen := range cases {
		key := make([]byte, keyLen)
		_, err := Encrypt(key, plaintext)
		assert.Errorf(t, err, "expected error for key length %d", keyLen)
	}
}

func TestEncryptEmptyPlaintext(t *testing.T) {
	key := generateTestKey(t)
	plaintext := []byte{}

	ciphertext, err := Encrypt(key, plaintext)
	require.NoError(t, err)
	require.NotEmpty(t, ciphertext)

	decrypted, err := Decrypt(key, ciphertext)
	require.NoError(t, err)
	// gcm.Open returns nil for empty plaintext; nil and []byte{} are equivalent here.
	assert.Empty(t, decrypted)
}

// --- KDF tests ---

func TestDeriveKeyFromPassphrase(t *testing.T) {
	salt := make([]byte, SaltLength)
	_, _ = rand.Read(salt)

	key1, err := DeriveKeyFromPassphrase("my-passphrase", salt)
	require.NoError(t, err)
	assert.Len(t, key1, 32)

	// Same passphrase + salt = same key (deterministic).
	key2, err := DeriveKeyFromPassphrase("my-passphrase", salt)
	require.NoError(t, err)
	assert.Equal(t, key1, key2)

	// Different passphrase = different key.
	key3, err := DeriveKeyFromPassphrase("other-passphrase", salt)
	require.NoError(t, err)
	assert.NotEqual(t, key1, key3)
}

func TestDeriveKeyFromPassphraseEmpty(t *testing.T) {
	salt := make([]byte, 32)
	_, _ = rand.Read(salt)
	_, err := DeriveKeyFromPassphrase("", salt)
	require.Error(t, err)
}

func TestDeriveKeyFromKeyFile(t *testing.T) {
	keyMaterial := make([]byte, 64)
	_, _ = rand.Read(keyMaterial)

	key1, err := DeriveKeyFromKeyFile(keyMaterial)
	require.NoError(t, err)
	assert.Len(t, key1, 32)

	// Same input = same key.
	key2, err := DeriveKeyFromKeyFile(keyMaterial)
	require.NoError(t, err)
	assert.Equal(t, key1, key2)
}

func TestDeriveKeyFromKeyFileTooShort(t *testing.T) {
	_, err := DeriveKeyFromKeyFile([]byte("short"))
	require.Error(t, err)
}

func TestResolveMasterKeyFromEnvPassphrase(t *testing.T) {
	t.Setenv("ALFRED_MASTER_KEY", "test-passphrase-123")
	t.Setenv("ALFRED_KEY_FILE", "")

	salt := make([]byte, 32)
	_, _ = rand.Read(salt)

	result, err := ResolveMasterKey(salt, "")
	require.NoError(t, err)
	assert.Len(t, result.Key, 32)
	assert.Equal(t, AlgorithmPBKDF2SHA256, result.Algorithm)
}

func TestResolveMasterKeyFromKeyFile(t *testing.T) {
	t.Setenv("ALFRED_MASTER_KEY", "")

	keyFile := filepath.Join(t.TempDir(), "master.key")
	keyData := make([]byte, 32)
	_, _ = rand.Read(keyData)
	require.NoError(t, os.WriteFile(keyFile, keyData, 0o400))

	t.Setenv("ALFRED_KEY_FILE", keyFile)

	result, err := ResolveMasterKey(nil, "")
	require.NoError(t, err)
	assert.Len(t, result.Key, 32)
	assert.Equal(t, AlgorithmHKDFKeyFile, result.Algorithm)
}

func TestResolveMasterKeyFilePermsTooOpen(t *testing.T) {
	t.Setenv("ALFRED_MASTER_KEY", "")

	keyFile := filepath.Join(t.TempDir(), "master.key")
	keyData := make([]byte, 32)
	_, _ = rand.Read(keyData)
	require.NoError(t, os.WriteFile(keyFile, keyData, 0o644))

	t.Setenv("ALFRED_KEY_FILE", keyFile)

	_, err := ResolveMasterKey(nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group/other-readable")
}

func TestResolveMasterKeyNoneAvailable(t *testing.T) {
	t.Setenv("ALFRED_MASTER_KEY", "")
	t.Setenv("ALFRED_KEY_FILE", "")

	_, err := ResolveMasterKey(nil, "/nonexistent/path/master.key")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoMasterKey)
}
