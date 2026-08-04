package llm

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadServiceAccountWarnsOnLoosePermissions(t *testing.T) {
	if runtime.GOOS == osWindows {
		t.Skip("POSIX permission semantics not applicable on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sa.json")
	payload := `{"client_email":"x@y.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nAA\n-----END PRIVATE KEY-----\n","token_uri":"https://oauth2.googleapis.com/token"}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Defeat the process umask so the test observes the exact mode
	// it asserts on regardless of the runner environment.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if _, _, err := loadServiceAccount(path, logger); err != nil {
		t.Fatalf("loadServiceAccount: %v", err)
	}

	if !strings.Contains(buf.String(), "service account key file permissions are too permissive") {
		t.Errorf("expected permission warning, got: %s", buf.String())
	}
}

func TestLoadServiceAccountQuietOn0600(t *testing.T) {
	if runtime.GOOS == osWindows {
		t.Skip("POSIX permission semantics not applicable on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sa.json")
	payload := `{"client_email":"x@y.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nAA\n-----END PRIVATE KEY-----\n","token_uri":"https://oauth2.googleapis.com/token"}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Defeat the process umask so the test observes the exact mode
	// it asserts on regardless of the runner environment.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if _, _, err := loadServiceAccount(path, logger); err != nil {
		t.Fatalf("loadServiceAccount: %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("expected no log output on 0600, got: %s", buf.String())
	}
}
