package secrets

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"

	"github.com/tphakala/alfred/internal/fsutil"
)

const (
	// fileVersion is the on-disk format version stored in the secrets file.
	fileVersion = 1

	// filePermission is the required file mode for the secrets file.
	filePermission = 0o600

	// dirPermission is the file mode for the secrets directory.
	dirPermission = 0o700
)

// namePattern validates secret names: alphanumeric, dot, underscore, hyphen.
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// kdfParams holds the key-derivation parameters persisted alongside secrets.
type kdfParams struct {
	Algorithm  string `json:"algorithm"`
	Iterations int    `json:"iterations,omitempty"`
	Salt       string `json:"salt,omitempty"` // base64-encoded
}

// secretsFile is the top-level on-disk JSON structure.
type secretsFile struct {
	Version int               `json:"version"`
	KDF     kdfParams         `json:"kdf"`
	Secrets map[string][]byte `json:"secrets"` // name -> encrypted ciphertext
}

// SecretsStore is an encrypted, file-backed key/value store for secrets.
type SecretsStore struct {
	path    string
	key     []byte
	kdf     kdfParams
	secrets map[string][]byte // name -> plaintext (in-memory)
}

// NewSecretsStore creates a new, empty SecretsStore with a random salt.
// salt must be SaltLength bytes; key must be requiredKeyLength bytes.
func NewSecretsStore(path string, key, salt []byte) (*SecretsStore, error) {
	return NewSecretsStoreWithSalt(path, key, salt, AlgorithmPBKDF2SHA256)
}

// NewSecretsStoreWithSalt creates a new, empty SecretsStore with the given
// algorithm and salt.
func NewSecretsStoreWithSalt(path string, key, salt []byte, algorithm string) (*SecretsStore, error) {
	if len(key) != requiredKeyLength {
		return nil, fmt.Errorf("key must be %d bytes, got %d", requiredKeyLength, len(key))
	}
	if len(salt) != SaltLength {
		return nil, fmt.Errorf("salt must be %d bytes, got %d", SaltLength, len(salt))
	}
	return &SecretsStore{
		path: path,
		key:  key,
		kdf: kdfParams{
			Algorithm: algorithm,
			Salt:      base64.StdEncoding.EncodeToString(salt),
		},
		secrets: make(map[string][]byte),
	}, nil
}

// LoadSecretsStore loads a secrets store from path, decrypting with key.
// If the file is missing but a .bak backup exists, the backup is used.
func LoadSecretsStore(path string, key []byte) (*SecretsStore, error) {
	if len(key) != requiredKeyLength {
		return nil, fmt.Errorf("key must be %d bytes, got %d", requiredKeyLength, len(key))
	}

	data, err := readSecretsFile(path)
	if err != nil {
		return nil, err
	}

	var sf secretsFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("failed to parse secrets file: %w", err)
	}
	if sf.Version > fileVersion {
		return nil, fmt.Errorf("unsupported secrets file version %d (max supported: %d)", sf.Version, fileVersion)
	}

	secrets := make(map[string][]byte, len(sf.Secrets))
	for name, ciphertext := range sf.Secrets {
		plaintext, err := Decrypt(key, ciphertext)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt secret %q: %w", name, err)
		}
		secrets[name] = plaintext
	}

	return &SecretsStore{
		path:    path,
		key:     key,
		kdf:     sf.KDF,
		secrets: secrets,
	}, nil
}

// readSecretsFile reads the secrets file, falling back to the .bak file if
// the primary file does not exist.
func readSecretsFile(path string) ([]byte, error) {
	data, err := readAndCheckPerms(path)
	if err == nil {
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read secrets file %q: %w", path, err)
	}

	// Primary missing — try backup.
	bakPath := path + ".bak"
	data, err = readAndCheckPerms(bakPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("secrets file not found: %q", path)
		}
		return nil, fmt.Errorf("failed to read backup secrets file %q: %w", bakPath, err)
	}
	return data, nil
}

// readAndCheckPerms reads a file and enforces 0600 permissions.
// Uses a single open file descriptor for both the stat and the read to avoid
// a TOCTOU race between the permission check and the actual read.
func readAndCheckPerms(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm != filePermission {
		return nil, fmt.Errorf("secrets file %q has unsafe permissions %04o; must be 0600", path, perm)
	}
	return io.ReadAll(f)
}

// Get returns the plaintext value of the named secret and whether it was found.
func (s *SecretsStore) Get(name string) (string, bool) {
	v, ok := s.secrets[name]
	return string(v), ok
}

// Set stores a secret. The name must match ^[a-zA-Z0-9._-]+$.
func (s *SecretsStore) Set(name, value string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid secret name %q: must match %s", name, namePattern)
	}
	s.secrets[name] = []byte(value)
	return nil
}

// Delete removes a secret by name. Returns false if the name did not exist.
func (s *SecretsStore) Delete(name string) bool {
	_, ok := s.secrets[name]
	if ok {
		delete(s.secrets, name)
	}
	return ok
}

// Names returns a sorted list of all secret names.
func (s *SecretsStore) Names() []string {
	return slices.Sorted(maps.Keys(s.secrets))
}

// Len returns the number of secrets in the store.
func (s *SecretsStore) Len() int {
	return len(s.secrets)
}

// AllSecrets returns a shallow copy of the secrets map (name -> plaintext).
// Mutations to the returned map do not affect the store.
func (s *SecretsStore) AllSecrets() map[string]string {
	out := make(map[string]string, len(s.secrets))
	for k, v := range s.secrets {
		out[k] = string(v)
	}
	return out
}

// Save atomically writes the encrypted secrets to disk with flock protection.
// Steps: lock, encrypt, back up the previous file, then write the payload
// atomically via fsutil.WriteFileAtomic (temp, fsync, rename, dir fsync), and
// unlock on return.
func (s *SecretsStore) Save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, dirPermission); err != nil {
		return fmt.Errorf("failed to create secrets directory: %w", err)
	}

	// Acquire an exclusive flock on a lock file.
	lockPath := s.path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, filePermission)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("failed to acquire file lock: %w", err)
	}

	// Encrypt all secrets.
	encrypted := make(map[string][]byte, len(s.secrets))
	for name, plaintext := range s.secrets {
		ct, err := Encrypt(s.key, plaintext)
		if err != nil {
			return fmt.Errorf("failed to encrypt secret %q: %w", name, err)
		}
		encrypted[name] = ct
	}

	kdf := s.kdf
	if kdf.Algorithm == AlgorithmPBKDF2SHA256 {
		kdf.Iterations = PBKDF2Iterations
	}
	sf := secretsFile{
		Version: fileVersion,
		KDF:     kdf,
		Secrets: encrypted,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal secrets: %w", err)
	}

	// Backup existing file if present (copy, not rename, so the primary
	// stays intact until the atomic rename at the end replaces it).
	if _, err := os.Stat(s.path); err == nil {
		src, readErr := os.ReadFile(s.path)
		if readErr != nil {
			return fmt.Errorf("failed to read file for backup: %w", readErr)
		}
		if writeErr := os.WriteFile(s.path+".bak", src, filePermission); writeErr != nil {
			return fmt.Errorf("failed to write backup: %w", writeErr)
		}
	}

	// Write the encrypted payload atomically (temp file, fsync, rename, dir
	// fsync); the flock above serializes concurrent Save calls.
	if err := fsutil.WriteFileAtomic(s.path, data, filePermission); err != nil {
		return fmt.Errorf("failed to write secrets file: %w", err)
	}
	return nil
}

// ReadSaltFromFile reads the KDF salt from an existing secrets file header
// without fully loading or decrypting the store.
func ReadSaltFromFile(path string) ([]byte, error) {
	data, err := readAndCheckPerms(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file: %w", err)
	}
	var header struct {
		KDF struct {
			Salt string `json:"salt"`
		} `json:"kdf"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("failed to parse secrets file header: %w", err)
	}
	if header.KDF.Salt == "" {
		return nil, nil
	}
	salt, err := base64.StdEncoding.DecodeString(header.KDF.Salt)
	if err != nil {
		return nil, fmt.Errorf("failed to decode salt: %w", err)
	}
	return salt, nil
}
