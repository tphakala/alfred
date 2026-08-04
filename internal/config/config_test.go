package config_test

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/config"
	"github.com/tphakala/alfred/internal/secrets"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing temp file: %v", err)
	}
	return f.Name()
}

//nolint:gocyclo // test function with many assertions is inherently complex
func TestLoad_BasicParsing(t *testing.T) {
	yaml := `
autotask:
  api_url: "https://webservices.autotask.net"
  username: "alice@example.com"
  password: "secret"
  integration_code: "MYAPP"
temporal:
  host: "localhost:7233"
  namespace: "default"
  task_queue: "alfred"
vertex_ai:
  project: "my-gcp-project"
  location: "us-central1"
  credentials_file: "/path/to/creds.json"
hindsight:
  url: "http://localhost:8080"
  bank: "alfred"
logging:
  level: "info"
  format: "json"
`
	path := writeTemp(t, yaml)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Autotask.APIUrl != "https://webservices.autotask.net" {
		t.Errorf("Autotask.APIUrl = %q, want %q", cfg.Autotask.APIUrl, "https://webservices.autotask.net")
	}
	if cfg.Autotask.Username != "alice@example.com" {
		t.Errorf("Autotask.Username = %q, want %q", cfg.Autotask.Username, "alice@example.com")
	}
	if cfg.Autotask.Password != "secret" {
		t.Errorf("Autotask.Password = %q, want %q", cfg.Autotask.Password, "secret")
	}
	if cfg.Autotask.IntegrationCode != "MYAPP" {
		t.Errorf("Autotask.IntegrationCode = %q, want %q", cfg.Autotask.IntegrationCode, "MYAPP")
	}
	if cfg.Temporal.Host != "localhost:7233" {
		t.Errorf("Temporal.Host = %q, want %q", cfg.Temporal.Host, "localhost:7233")
	}
	if cfg.Temporal.Namespace != "default" {
		t.Errorf("Temporal.Namespace = %q, want %q", cfg.Temporal.Namespace, "default")
	}
	if cfg.Temporal.TaskQueue != "alfred" {
		t.Errorf("Temporal.TaskQueue = %q, want %q", cfg.Temporal.TaskQueue, "alfred")
	}
	if cfg.VertexAI.Project != "my-gcp-project" {
		t.Errorf("VertexAI.Project = %q, want %q", cfg.VertexAI.Project, "my-gcp-project")
	}
	if cfg.VertexAI.Location != "us-central1" {
		t.Errorf("VertexAI.Location = %q, want %q", cfg.VertexAI.Location, "us-central1")
	}
	if cfg.VertexAI.CredentialsFile != "/path/to/creds.json" {
		t.Errorf("VertexAI.CredentialsFile = %q, want %q", cfg.VertexAI.CredentialsFile, "/path/to/creds.json")
	}
	if cfg.Hindsight.URL != "http://localhost:8080" {
		t.Errorf("Hindsight.URL = %q, want %q", cfg.Hindsight.URL, "http://localhost:8080")
	}
	if cfg.Hindsight.Bank != "alfred" {
		t.Errorf("Hindsight.Bank = %q, want %q", cfg.Hindsight.Bank, "alfred")
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, "info")
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Logging.Format = %q, want %q", cfg.Logging.Format, "json")
	}
}

func TestLoad_EnvVarExpansion(t *testing.T) {
	t.Setenv("TEST_AT_USER", "bob@example.com")
	t.Setenv("TEST_AT_PASS", "p@ssw0rd!")
	t.Setenv("TEST_AT_CODE", "ENVTEST")

	yaml := `
autotask:
  api_url: "https://webservices.autotask.net"
  username: "${TEST_AT_USER}"
  password: "$TEST_AT_PASS"
  integration_code: "${TEST_AT_CODE}"
temporal:
  host: "localhost:7233"
  namespace: "default"
  task_queue: "alfred"
vertex_ai:
  project: "proj"
  location: "us-central1"
  credentials_file: "/creds.json"
hindsight:
  url: "http://localhost:8080"
  bank: "alfred"
logging:
  level: "info"
  format: "json"
`
	path := writeTemp(t, yaml)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Autotask.Username != "bob@example.com" {
		t.Errorf("Autotask.Username = %q, want %q", cfg.Autotask.Username, "bob@example.com")
	}
	if cfg.Autotask.Password != "p@ssw0rd!" {
		t.Errorf("Autotask.Password = %q, want %q", cfg.Autotask.Password, "p@ssw0rd!")
	}
	if cfg.Autotask.IntegrationCode != "ENVTEST" {
		t.Errorf("Autotask.IntegrationCode = %q, want %q", cfg.Autotask.IntegrationCode, "ENVTEST")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.yaml")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load() expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTemp(t, "autotask: [\ninvalid yaml{{{{")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid YAML, got nil")
	}
}

func TestLoad_RejectsNonPositivePricing(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		// A partial override sets only one side; the omitted side decodes to 0,
		// which would make those tokens free (the cap under-counts).
		{"partial override leaves input zero", `
pricing:
  "some/model":
    output_per_million: 5.0
`},
		{"negative rate", `
pricing:
  "some/model":
    input_per_million: 1.0
    output_per_million: -2.0
`},
		// A mistyped entry decodes to {0,0} and would silence the "no pricing"
		// warning while leaving the cap inert for that model.
		{"both zero", `
pricing:
  "some/model":
    input_per_million: 0
    output_per_million: 0
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.yaml)
			if _, err := config.Load(path); err == nil {
				t.Fatal("Load() expected error for non-positive pricing, got nil")
			}
		})
	}
}

func TestLoad_AcceptsValidPricing(t *testing.T) {
	path := writeTemp(t, `
pricing:
  "anthropic/claude-sonnet-4":
    input_per_million: 3.0
    output_per_million: 15.0
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	p, ok := cfg.Pricing["anthropic/claude-sonnet-4"]
	if !ok {
		t.Fatal("pricing entry not loaded")
	}
	if p.InputPerMillion != 3.0 || p.OutputPerMillion != 15.0 {
		t.Errorf("pricing = %+v, want {input:3.0, output:15.0}", p)
	}
}

func TestLoad_ServerConfig(t *testing.T) {
	yamlContent := `
autotask:
  api_url: "https://webservices.autotask.net"
  username: "alice@example.com"
  password: "secret"
  integration_code: "MYAPP"
temporal:
  host: "localhost:7233"
  namespace: "default"
  task_queue: "alfred"
vertex_ai:
  project: "my-gcp-project"
  location: "us-central1"
  credentials_file: "/path/to/creds.json"
hindsight:
  url: "http://localhost:8080"
  bank: "alfred"
logging:
  level: "info"
  format: "json"
server:
  address: ":9090"
  api_key: "test-secret-key"
`
	path := writeTemp(t, yamlContent)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Address != ":9090" {
		t.Errorf("Server.Address = %q, want %q", cfg.Server.Address, ":9090")
	}
	if cfg.Server.APIKey != "test-secret-key" {
		t.Errorf("Server.APIKey = %q, want %q", cfg.Server.APIKey, "test-secret-key")
	}
}

func TestLoad_ServerConfigDefaults(t *testing.T) {
	yamlContent := `
autotask:
  api_url: "https://webservices.autotask.net"
  username: "alice@example.com"
  password: "secret"
  integration_code: "MYAPP"
temporal:
  host: "localhost:7233"
  namespace: "default"
  task_queue: "alfred"
vertex_ai:
  project: "my-gcp-project"
  location: "us-central1"
  credentials_file: "/path/to/creds.json"
hindsight:
  url: "http://localhost:8080"
  bank: "alfred"
logging:
  level: "info"
  format: "json"
`
	path := writeTemp(t, yamlContent)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Address != "" {
		t.Errorf("Server.Address = %q, want empty", cfg.Server.Address)
	}
	if cfg.Server.APIKey != "" {
		t.Errorf("Server.APIKey = %q, want empty", cfg.Server.APIKey)
	}
}

// TestLoad_WithSecretsStore creates a temp directory with a config.yaml that
// uses ${secret:name} references and a secrets.enc.json file. It verifies that
// Load resolves the references using the master key from ALFRED_MASTER_KEY.
func TestLoad_WithSecretsStore(t *testing.T) {
	const passphrase = "test-master-key-passphrase"
	const apiKeySecret = "s3cr3t-api-k3y"
	const usernameSecret = "alice@example.com"
	const passwordSecret = "hunter2"

	dir := t.TempDir()

	// Generate a random salt and derive the encryption key.
	salt := make([]byte, secrets.SaltLength)
	_, err := rand.Read(salt)
	require.NoError(t, err)

	key, err := secrets.DeriveKeyFromPassphrase(passphrase, salt)
	require.NoError(t, err)

	// Build and save the secrets store.
	secretsPath := filepath.Join(dir, "secrets.enc.json")
	store, err := secrets.NewSecretsStore(secretsPath, key, salt)
	require.NoError(t, err)
	require.NoError(t, store.Set("server.api_key", apiKeySecret))
	require.NoError(t, store.Set("autotask.username", usernameSecret))
	require.NoError(t, store.Set("autotask.password", passwordSecret))
	require.NoError(t, store.Save())

	// Write config.yaml with secret references.
	yamlContent := `
autotask:
  api_url: "https://webservices.autotask.net"
  username: "${secret:autotask.username}"
  password: "${secret:autotask.password}"
  integration_code: "MYAPP"
temporal:
  host: "localhost:7233"
  namespace: "default"
  task_queue: "alfred"
vertex_ai:
  project: "my-gcp-project"
  location: "us-central1"
  credentials_file: "/path/to/creds.json"
hindsight:
  url: "http://localhost:8080"
  bank: "alfred"
logging:
  level: "info"
  format: "json"
server:
  address: ":9090"
  api_key: "${secret:server.api_key}"
`
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(yamlContent), 0o600))

	// Set the master key env var so ResolveMasterKey can derive the key.
	t.Setenv("ALFRED_MASTER_KEY", passphrase)

	cfg, err := config.Load(configPath)
	require.NoError(t, err)

	assert.Equal(t, usernameSecret, cfg.Autotask.Username)
	assert.Equal(t, passwordSecret, cfg.Autotask.Password)
	assert.Equal(t, apiKeySecret, cfg.Server.APIKey)

	// Fields not using secret refs should remain as-is.
	assert.Equal(t, "https://webservices.autotask.net", cfg.Autotask.APIUrl)
	assert.Equal(t, "MYAPP", cfg.Autotask.IntegrationCode)
	assert.Equal(t, ":9090", cfg.Server.Address)
}

func TestChatConfig_SystemPrompt(t *testing.T) {
	yamlContent := `
chat:
  system_prompt: "You are Alfred, an IT operations assistant."
  auto_recall_on_new_conversation: true
`
	path := writeTemp(t, yamlContent)

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "You are Alfred, an IT operations assistant.", cfg.Chat.SystemPrompt)
	assert.True(t, cfg.Chat.AutoRecallEnabled())
}

func TestLoad_DatabaseDefaults(t *testing.T) {
	yamlContent := `
server:
  address: ":8080"
`
	path := writeTemp(t, yamlContent)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Database.URL != "postgres://localhost:5432/alfred" {
		t.Errorf("Database.URL = %q, want default", cfg.Database.URL)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("Database.MaxConns = %d, want 10", cfg.Database.MaxConns)
	}
}

func TestLoad_DatabaseFromYAML(t *testing.T) {
	yamlContent := `
database:
  url: "postgres://db:5432/prod"
  max_conns: 25
`
	path := writeTemp(t, yamlContent)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Database.URL != "postgres://db:5432/prod" {
		t.Errorf("Database.URL = %q", cfg.Database.URL)
	}
	if cfg.Database.MaxConns != 25 {
		t.Errorf("Database.MaxConns = %d, want 25", cfg.Database.MaxConns)
	}
}

func TestLoad_DatabaseURLEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env-host:5432/envdb")
	yamlContent := `
database:
  url: "postgres://yaml-host:5432/yamldb"
`
	path := writeTemp(t, yamlContent)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Database.URL != "postgres://env-host:5432/envdb" {
		t.Errorf("Database.URL = %q, want env override", cfg.Database.URL)
	}
}

// TestLoad_WithoutSecretsStoreBackwardCompat verifies that Load works normally
// when no secrets.enc.json file exists alongside the config (backward compat).
func TestLoad_WithoutSecretsStoreBackwardCompat(t *testing.T) {
	yamlContent := `
autotask:
  api_url: "https://webservices.autotask.net"
  username: "alice@example.com"
  password: "plaintext-password"
  integration_code: "MYAPP"
temporal:
  host: "localhost:7233"
  namespace: "default"
  task_queue: "alfred"
vertex_ai:
  project: "my-gcp-project"
  location: "us-central1"
  credentials_file: "/path/to/creds.json"
hindsight:
  url: "http://localhost:8080"
  bank: "alfred"
logging:
  level: "info"
  format: "json"
server:
  address: ":9090"
  api_key: "plain-api-key"
`
	// writeTemp creates the file in a unique TempDir — no secrets.enc.json present.
	path := writeTemp(t, yamlContent)

	cfg, err := config.Load(path)
	require.NoError(t, err)

	assert.Equal(t, "alice@example.com", cfg.Autotask.Username)
	assert.Equal(t, "plaintext-password", cfg.Autotask.Password)
	assert.Equal(t, "plain-api-key", cfg.Server.APIKey)
	assert.Equal(t, "MYAPP", cfg.Autotask.IntegrationCode)
}
