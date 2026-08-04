package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"cloud.google.com/go/auth"
)

// ErrNoCredentialsPath is returned by authCredentialsFromFile when path is empty.
var ErrNoCredentialsPath = errors.New("no credentials file path provided")

const (
	// gcpCloudPlatformScope is the OAuth scope required for Vertex AI access.
	gcpCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

	// osWindows is the GOOS value for Windows, used to skip POSIX-only checks.
	osWindows = "windows"
)

// serviceAccountInfo is the subset of a GCP service account JSON key that
// this package consumes to build an OAuth token provider.
type serviceAccountInfo struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// loadServiceAccount reads and validates a service account JSON file,
// returning the parsed metadata and the raw bytes for downstream consumers
// that want to embed the key material (e.g. auth.CredentialsOptions.JSON).
// If logger is non-nil and the key file has world- or group-readable bits
// set (POSIX only), a warning is emitted — this is informational, not fatal.
func loadServiceAccount(path string, logger *slog.Logger) (*serviceAccountInfo, []byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat service account file: %w", err)
	}
	if runtime.GOOS != osWindows && logger != nil {
		mode := info.Mode().Perm()
		if mode&0o077 != 0 {
			logger.Warn("service account key file permissions are too permissive",
				"path", path,
				"mode", fmt.Sprintf("%04o", mode),
				"recommended", "0600",
			)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read service account file: %w", err)
	}
	var sa serviceAccountInfo
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, nil, fmt.Errorf("parse service account JSON: %w", err)
	}
	if sa.ClientEmail == "" {
		return nil, nil, fmt.Errorf("service account missing client_email")
	}
	if sa.PrivateKey == "" {
		return nil, nil, fmt.Errorf("service account missing private_key")
	}
	if sa.TokenURI == "" {
		return nil, nil, fmt.Errorf("service account missing token_uri")
	}
	return &sa, data, nil
}

// authCredentialsFromFile loads Google auth credentials for the GenAI SDK
// from the service account JSON at the given path. The returned value is
// suitable for passing as genai.ClientConfig.Credentials. logger may be nil.
//
// Returns nil if path is empty — callers can interpret that as "fall back
// to Application Default Credentials" by not setting Credentials on the
// genai client config.
func authCredentialsFromFile(path string, logger *slog.Logger) (*auth.Credentials, error) {
	if path == "" {
		return nil, ErrNoCredentialsPath
	}
	sa, data, err := loadServiceAccount(path, logger)
	if err != nil {
		return nil, err
	}
	tp, err := auth.New2LOTokenProvider(&auth.Options2LO{
		Email:      sa.ClientEmail,
		PrivateKey: []byte(sa.PrivateKey),
		TokenURL:   sa.TokenURI,
		Scopes:     []string{gcpCloudPlatformScope},
	})
	if err != nil {
		return nil, fmt.Errorf("create token provider: %w", err)
	}
	return auth.NewCredentials(&auth.CredentialsOptions{
		TokenProvider: tp,
		JSON:          data,
	}), nil
}
