package secrets

import (
	"crypto/hkdf"
	"crypto/pbkdf2"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// Key derivation constants.
const (
	// PBKDF2Iterations meets the OWASP 2023 recommendation of 600,000 for PBKDF2-HMAC-SHA256.
	PBKDF2Iterations = 600_000

	// SaltLength is the required byte length for PBKDF2 salts.
	SaltLength = 32

	// hkdfInfo is the application-specific context label for HKDF key derivation.
	hkdfInfo = "alfred-secrets-v1"

	// minKeyFileLength is the minimum accepted size (in bytes) for a key file.
	minKeyFileLength = 16

	// maxKeyFileLength caps key file reads to prevent OOM from misconfigured paths.
	maxKeyFileLength = 1 << 20 // 1 MiB

	// AlgorithmPBKDF2SHA256 identifies the passphrase-based key derivation algorithm.
	AlgorithmPBKDF2SHA256 = "pbkdf2-sha256"

	// AlgorithmHKDFKeyFile identifies the key-file-based key derivation algorithm.
	AlgorithmHKDFKeyFile = "hkdf-keyfile"
)

// ErrNoMasterKey is returned by ResolveMasterKey when no key source is configured.
var ErrNoMasterKey = errors.New("no master key available: set ALFRED_MASTER_KEY env var or provide a key file")

// MasterKeyResult holds the derived key and the algorithm used to derive it.
type MasterKeyResult struct {
	Key       []byte
	Algorithm string
}

// DeriveKeyFromPassphrase derives a 32-byte AES-256 key from a passphrase and salt
// using PBKDF2-HMAC-SHA256.
func DeriveKeyFromPassphrase(passphrase string, salt []byte) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("passphrase must not be empty")
	}
	if len(salt) == 0 {
		return nil, errors.New("salt must not be empty")
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, PBKDF2Iterations, requiredKeyLength)
	if err != nil {
		return nil, fmt.Errorf("pbkdf2 key derivation failed: %w", err)
	}
	return key, nil
}

// DeriveKeyFromKeyFile derives a 32-byte AES-256 key from raw key material
// using HKDF-SHA256 with a fixed application context label.
func DeriveKeyFromKeyFile(keyMaterial []byte) ([]byte, error) {
	if len(keyMaterial) < minKeyFileLength {
		return nil, fmt.Errorf("key file must be at least %d bytes, got %d", minKeyFileLength, len(keyMaterial))
	}
	key, err := hkdf.Key(sha256.New, keyMaterial, nil, hkdfInfo, requiredKeyLength)
	if err != nil {
		return nil, fmt.Errorf("hkdf key derivation failed: %w", err)
	}
	return key, nil
}

// ResolveMasterKey resolves the master encryption key from available sources:
//  1. ALFRED_MASTER_KEY environment variable — passphrase-based via PBKDF2-SHA256.
//  2. ALFRED_KEY_FILE environment variable — path to key file, derived via HKDF.
//  3. defaultKeyFilePath — used if file exists at that path.
//
// Returns ErrNoMasterKey if no source is available.
func ResolveMasterKey(salt []byte, defaultKeyFilePath string) (*MasterKeyResult, error) {
	if passphrase := os.Getenv("ALFRED_MASTER_KEY"); passphrase != "" {
		key, err := DeriveKeyFromPassphrase(passphrase, salt)
		if err != nil {
			return nil, fmt.Errorf("failed to derive key from passphrase: %w", err)
		}
		return &MasterKeyResult{Key: key, Algorithm: AlgorithmPBKDF2SHA256}, nil
	}

	if keyFilePath := os.Getenv("ALFRED_KEY_FILE"); keyFilePath != "" {
		return deriveKeyFromFile(keyFilePath)
	}

	if defaultKeyFilePath != "" {
		if _, err := os.Stat(defaultKeyFilePath); err == nil {
			return deriveKeyFromFile(defaultKeyFilePath)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to check default key file %s: %w", defaultKeyFilePath, err)
		}
	}

	return nil, ErrNoMasterKey
}

// deriveKeyFromFile reads a key file, enforces safe permissions, and derives an AES-256 key.
// Uses a single open file descriptor for both stat and read to avoid TOCTOU.
func deriveKeyFromFile(path string) (*MasterKeyResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open key file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat key file %s: %w", path, err)
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return nil, fmt.Errorf("key file %s is group/other-readable (mode %04o); must be 0400 or 0600", path, mode)
	}

	if info.Size() > maxKeyFileLength {
		return nil, fmt.Errorf("key file %s is too large (%d bytes, max %d)", path, info.Size(), maxKeyFileLength)
	}
	keyMaterial, err := io.ReadAll(io.LimitReader(f, maxKeyFileLength+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read key file %s: %w", path, err)
	}
	defer clear(keyMaterial)
	key, err := DeriveKeyFromKeyFile(keyMaterial)
	if err != nil {
		return nil, fmt.Errorf("failed to derive key from key file: %w", err)
	}
	return &MasterKeyResult{Key: key, Algorithm: AlgorithmHKDFKeyFile}, nil
}
