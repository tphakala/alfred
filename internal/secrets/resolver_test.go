package secrets

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- Test structs ---

type testFlatConfig struct {
	APIKey   string `secret:""`
	Username string `secret:""`
	Debug    bool   // no secret tag
}

type testNestedConfig struct {
	Name string
	DB   testDBConfig
}

type testDBConfig struct {
	Password string `secret:""`
	Host     string
}

// --- IsSecretRef tests ---

func TestIsSecretRef(t *testing.T) {
	cases := []struct {
		input    string
		expected bool
	}{
		{"${secret:my-key}", true},
		{"${secret:api.key}", true},
		{"${secret:}", true}, // syntactically valid (empty name)
		{"plaintext", false},
		{"${notasecret:foo}", false},
		{"${secret:foo", false}, // missing closing brace
		{"secret:foo}", false},  // missing opening
		{"", false},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.expected, IsSecretRef(tc.input), "IsSecretRef(%q)", tc.input)
	}
}

// --- ResolveSecretRefs tests ---

func TestResolveSecretRefsBasic(t *testing.T) {
	cfg := &testFlatConfig{
		APIKey:   "${secret:my-api-key}",
		Username: "${secret:my-user}",
		Debug:    false,
	}
	secrets := map[string]string{
		"my-api-key": "super-secret-value",
		"my-user":    "alice",
	}
	warnings := ResolveSecretRefs(cfg, secrets)
	assert.Empty(t, warnings)
	assert.Equal(t, "super-secret-value", cfg.APIKey)
	assert.Equal(t, "alice", cfg.Username)
}

func TestResolveSecretRefsPlaintextPassthrough(t *testing.T) {
	cfg := &testFlatConfig{
		APIKey:   "already-plain-text",
		Username: "bob",
	}
	secrets := map[string]string{
		"some-key": "some-value",
	}
	warnings := ResolveSecretRefs(cfg, secrets)
	assert.Empty(t, warnings)
	// Values that are not secret references should be left unchanged.
	assert.Equal(t, "already-plain-text", cfg.APIKey)
	assert.Equal(t, "bob", cfg.Username)
}

func TestResolveSecretRefsMissing(t *testing.T) {
	cfg := &testFlatConfig{
		APIKey:   "${secret:missing-key}",
		Username: "static",
	}
	secrets := map[string]string{} // empty — nothing to resolve

	warnings := ResolveSecretRefs(cfg, secrets)
	assert.Len(t, warnings, 1, "one warning for unresolved secret")
	assert.Contains(t, warnings[0], "missing-key")
	assert.Empty(t, cfg.APIKey, "unresolved field should be set to empty string")
	assert.Equal(t, "static", cfg.Username, "non-reference field should be unchanged")
}

func TestResolveSecretRefsNested(t *testing.T) {
	cfg := &testNestedConfig{
		Name: "myservice",
		DB: testDBConfig{
			Password: "${secret:db-password}",
			Host:     "localhost",
		},
	}
	secrets := map[string]string{
		"db-password": "s3cr3t",
	}
	warnings := ResolveSecretRefs(cfg, secrets)
	assert.Empty(t, warnings)
	assert.Equal(t, "s3cr3t", cfg.DB.Password)
	assert.Equal(t, "localhost", cfg.DB.Host)
	assert.Equal(t, "myservice", cfg.Name)
}

func TestResolveSecretRefsNilInput(t *testing.T) {
	warnings := ResolveSecretRefs(nil, map[string]string{})
	assert.Nil(t, warnings)
}

func TestResolveSecretRefsNonPointerInput(t *testing.T) {
	cfg := testFlatConfig{
		APIKey: "${secret:key}",
	}
	// Pass by value (not pointer) — should be a no-op.
	warnings := ResolveSecretRefs(cfg, map[string]string{"key": "val"})
	assert.Nil(t, warnings)
	// cfg is unchanged because we passed a copy.
	assert.Equal(t, "${secret:key}", cfg.APIKey)
}
