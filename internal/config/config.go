// Package config handles loading and parsing alfred configuration.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tphakala/alfred/internal/secrets"
	"gopkg.in/yaml.v3"
)

// secretsFileName is the conventional name for the encrypted secrets file.
const secretsFileName = "secrets.enc.json"

// LLM provider identifiers used in config.llm_provider.
const (
	LLMProviderVertex     = "vertex"
	LLMProviderOpenRouter = "openrouter"
)

// Config is the top-level configuration for alfred.
type Config struct {
	LLMProvider string           `yaml:"llm_provider"`
	Autotask    AutotaskConfig   `yaml:"autotask"`
	Temporal    TemporalConfig   `yaml:"temporal"`
	VertexAI    VertexAIConfig   `yaml:"vertex_ai"`
	OpenRouter  OpenRouterConfig `yaml:"openrouter"`
	Hindsight   HindsightConfig  `yaml:"hindsight"`
	Logging     LoggingConfig    `yaml:"logging"`
	Server      ServerConfig     `yaml:"server"`
	Chat        ChatConfig       `yaml:"chat"`
	Database    DatabaseConfig   `yaml:"database"`
	Claude      ClaudeConfig     `yaml:"claude"`
	Gemini      GeminiConfig     `yaml:"gemini"`
	Copilot     CopilotConfig    `yaml:"copilot"`
	// Pricing overrides or extends the built-in per-model token pricing used to
	// meter a native LLM loop's own token spend against its cost cap. Keyed by
	// model id; values are USD per one million tokens. Overlays the built-in
	// default table (see llm.DefaultPriceTable) at startup, so a deployment only
	// lists models the defaults miss or price wrong.
	Pricing map[string]ModelPricing `yaml:"pricing,omitempty"`
}

// ModelPricing is a per-model token price in USD per one million tokens for a
// model's input (prompt) and output (completion) tokens. It mirrors
// llm.ModelPrice but stays in the config package to keep config free of an llm
// import; main.go converts it into an llm.PriceTable overlaid on the defaults.
type ModelPricing struct {
	InputPerMillion  float64 `yaml:"input_per_million"`
	OutputPerMillion float64 `yaml:"output_per_million"`
}

// AutotaskConfig holds credentials and endpoint for the Autotask API.
type AutotaskConfig struct {
	APIUrl          string `yaml:"api_url"`
	Username        string `yaml:"username"         secret:"autotask.username"`
	Password        string `yaml:"password"         secret:"autotask.password"`
	IntegrationCode string `yaml:"integration_code" secret:"autotask.integration_code"`
}

// TemporalConfig holds connection settings for the Temporal server.
type TemporalConfig struct {
	Host      string `yaml:"host"`
	Namespace string `yaml:"namespace"`
	TaskQueue string `yaml:"task_queue"`
}

// VertexAIConfig holds Google Cloud Vertex AI settings.
type VertexAIConfig struct {
	Project         string `yaml:"project"`
	Location        string `yaml:"location"`
	Model           string `yaml:"model"`
	ChatModel       string `yaml:"chat_model"`
	CredentialsFile string `yaml:"credentials_file" secret:"vertex_ai.credentials_file"`
}

// OpenRouterConfig holds OpenRouter API settings.
type OpenRouterConfig struct {
	APIKey string `yaml:"api_key" secret:"openrouter.api_key"`
	Model  string `yaml:"model"`
}

// HindsightConfig holds connection settings for the Hindsight memory service.
type HindsightConfig struct {
	URL  string `yaml:"url"  secret:"hindsight.url"`
	Bank string `yaml:"bank"`
}

// LoggingConfig controls log output.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Address string     `yaml:"address"`
	APIKey  string     `yaml:"api_key" secret:"server.api_key"`
	CORS    CORSConfig `yaml:"cors"`
}

// CORSConfig holds cross-origin settings. AllowedOrigins empty means CORS disabled.
type CORSConfig struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// ChatConfig holds agent/chat behavior settings.
type ChatConfig struct {
	SystemPrompt                string `yaml:"system_prompt"`
	AutoRecallOnNewConversation *bool  `yaml:"auto_recall_on_new_conversation"`
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	URL      string `yaml:"url"`
	MaxConns int    `yaml:"max_conns"`
}

// ClaudeConfig holds settings for the Claude CLI runner.
type ClaudeConfig struct {
	BinaryPath string `yaml:"binary_path"`
}

// GeminiConfig holds settings for the Gemini CLI runner.
type GeminiConfig struct {
	Enabled    bool   `yaml:"enabled"`
	BinaryPath string `yaml:"binary_path"`
}

// CopilotConfig holds settings for the GitHub Copilot CLI runner.
type CopilotConfig struct {
	Enabled    bool   `yaml:"enabled"`
	BinaryPath string `yaml:"binary_path"`
}

// AutoRecallEnabled returns whether auto-recall is enabled (default: true).
func (c ChatConfig) AutoRecallEnabled() bool {
	if c.AutoRecallOnNewConversation == nil {
		return true
	}
	return *c.AutoRecallOnNewConversation
}

// expandEnvSkipSecrets is like os.ExpandEnv but leaves ${secret:...} references
// untouched so they can be resolved later by the secrets package.
func expandEnvSkipSecrets(s string) string {
	return os.Expand(s, func(key string) string {
		// Preserve secret references so they survive to the secrets resolution step.
		if strings.HasPrefix(key, "secret:") {
			return "${" + key + "}"
		}
		return os.Getenv(key)
	})
}

// Load reads the YAML configuration file at path, expands environment variables
// in the raw content, and returns the parsed Config. If a secrets.enc.json file
// exists alongside the config, encrypted secret references in the config are
// resolved using the master key.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	expanded := expandEnvSkipSecrets(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	// Apply database defaults.
	if cfg.Database.URL == "" {
		cfg.Database.URL = "postgres://localhost:5432/alfred"
	}
	if cfg.Database.MaxConns == 0 {
		cfg.Database.MaxConns = 10
	}

	// Default ChatModel to Model if not set.
	if cfg.VertexAI.ChatModel == "" {
		cfg.VertexAI.ChatModel = cfg.VertexAI.Model
	}

	// Default LLM provider to "vertex" for backwards compatibility.
	if cfg.LLMProvider == "" {
		cfg.LLMProvider = LLMProviderVertex
	}
	switch cfg.LLMProvider {
	case LLMProviderVertex, LLMProviderOpenRouter:
	default:
		return nil, fmt.Errorf("unknown llm_provider %q: must be %q or %q", cfg.LLMProvider, LLMProviderVertex, LLMProviderOpenRouter)
	}

	// Reject non-positive pricing entries. The overlay in main.go replaces a
	// whole model entry, so a partial override (only one side set), a mistyped
	// entry decoding to {0,0}, or a negative rate would silently make token
	// spend free or negative: the cost cap then under-counts and can be
	// overspent, and a zeroed entry even suppresses the "no pricing" warning.
	// Fail closed at load so the mistake is loud, not silently unsafe.
	for model, p := range cfg.Pricing {
		if p.InputPerMillion <= 0 || p.OutputPerMillion <= 0 {
			return nil, fmt.Errorf("pricing[%q]: input_per_million and output_per_million must both be > 0 (got input=%v output=%v)",
				model, p.InputPerMillion, p.OutputPerMillion)
		}
	}

	// DATABASE_URL env var overrides YAML value and default.
	if envURL := os.Getenv("DATABASE_URL"); envURL != "" {
		cfg.Database.URL = envURL
	}

	configDir := filepath.Dir(path)
	secretsPath := filepath.Join(configDir, secretsFileName)

	if _, err := os.Stat(secretsPath); err == nil {
		if err := resolveSecrets(&cfg, secretsPath, configDir); err != nil {
			return nil, fmt.Errorf("resolving secrets: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking secrets file %q: %w", secretsPath, err)
	}

	return &cfg, nil
}

// resolveSecrets loads the secrets store and resolves any secret references in cfg.
func resolveSecrets(cfg *Config, secretsPath, configDir string) error {
	salt, err := secrets.ReadSaltFromFile(secretsPath)
	if err != nil {
		return fmt.Errorf("reading salt from secrets file: %w", err)
	}
	if salt == nil {
		return fmt.Errorf("secrets file %q contains empty KDF salt", secretsPath)
	}

	defaultKeyFile := filepath.Join(configDir, "master.key")
	result, err := secrets.ResolveMasterKey(salt, defaultKeyFile)
	if err != nil {
		return fmt.Errorf("resolving master key: %w", err)
	}

	store, err := secrets.LoadSecretsStore(secretsPath, result.Key)
	if err != nil {
		return fmt.Errorf("loading secrets store: %w", err)
	}

	warnings := secrets.ResolveSecretRefs(cfg, store.AllSecrets())
	for _, w := range warnings {
		slog.Warn("secrets resolution warning", "detail", w)
	}

	return nil
}
