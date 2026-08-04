package secrets

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore creates a SecretsStore with a random key and salt in a temp dir.
func newTestStore(t *testing.T) *SecretsStore {
	t.Helper()
	key := generateTestKey(t)
	salt := make([]byte, SaltLength)
	_, err := rand.Read(salt)
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")

	store, err := NewSecretsStore(path, key, salt)
	require.NoError(t, err)
	return store
}

func TestSecretsStoreSetGetDelete(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.Set("api-key", "super-secret"))

	val, ok := store.Get("api-key")
	assert.True(t, ok)
	assert.Equal(t, "super-secret", val)

	deleted := store.Delete("api-key")
	assert.True(t, deleted)

	_, ok = store.Get("api-key")
	assert.False(t, ok)

	// Deleting non-existent key returns false.
	assert.False(t, store.Delete("api-key"))
}

func TestSecretsStoreLoadFromDisk(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.Set("db-pass", "hunter2"))
	require.NoError(t, store.Set("token", "abc123"))
	require.NoError(t, store.Save())

	// Reload from disk using the same key.
	loaded, err := LoadSecretsStore(store.path, store.key)
	require.NoError(t, err)

	val, ok := loaded.Get("db-pass")
	assert.True(t, ok)
	assert.Equal(t, "hunter2", val)

	val, ok = loaded.Get("token")
	assert.True(t, ok)
	assert.Equal(t, "abc123", val)
}

func TestSecretsStoreWrongKeyFails(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.Set("secret", "value"))
	require.NoError(t, store.Save())

	wrongKey := generateTestKey(t)
	_, err := LoadSecretsStore(store.path, wrongKey)
	assert.Error(t, err, "loading with wrong key should fail")
}

func TestSecretsStoreNames(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.Set("zebra", "z"))
	require.NoError(t, store.Set("alpha", "a"))
	require.NoError(t, store.Set("middle", "m"))

	names := store.Names()
	assert.Equal(t, []string{"alpha", "middle", "zebra"}, names, "names must be sorted")
}

func TestSecretsStoreInvalidName(t *testing.T) {
	store := newTestStore(t)

	cases := []string{"", "has space", "has/slash", "has@at", "has#hash", "has!bang"}
	for _, name := range cases {
		err := store.Set(name, "value")
		assert.Errorf(t, err, "expected error for invalid name %q", name)
	}
}

func TestSecretsStoreBakFallback(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.Set("key", "value"))
	require.NoError(t, store.Save())

	// Save a second time to create a .bak alongside the primary.
	require.NoError(t, store.Set("key2", "value2"))
	require.NoError(t, store.Save())

	// Manually remove the primary file; .bak should remain.
	require.NoError(t, os.Remove(store.path))

	loaded, err := LoadSecretsStore(store.path, store.key)
	require.NoError(t, err, "should fall back to .bak file")

	// The .bak was the state after the first Save (only "key").
	val, ok := loaded.Get("key")
	assert.True(t, ok)
	assert.Equal(t, "value", val)
}

func TestSecretsStoreVersionMismatch(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.Set("x", "y"))
	require.NoError(t, store.Save())

	// Craft invalid JSON version directly.
	badJSON := `{"version":99,"kdf":{"algorithm":"pbkdf2-sha256","salt":"AA=="},"secrets":{}}`
	require.NoError(t, os.WriteFile(store.path, []byte(badJSON), 0o600))

	_, err := LoadSecretsStore(store.path, store.key)
	require.Error(t, err, "version mismatch should return error")
	assert.Contains(t, err.Error(), "unsupported secrets file version")
}

func TestSecretsStoreAllSecrets(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.Set("k1", "v1"))
	require.NoError(t, store.Set("k2", "v2"))

	all := store.AllSecrets()
	assert.Equal(t, map[string]string{"k1": "v1", "k2": "v2"}, all)

	// Mutation of the returned map must not affect the store.
	all["k1"] = "mutated"
	all["k3"] = "injected"

	v, ok := store.Get("k1")
	assert.True(t, ok)
	assert.Equal(t, "v1", v, "store should not be affected by mutation of AllSecrets return value")

	_, ok = store.Get("k3")
	assert.False(t, ok, "injected key must not appear in store")
}
