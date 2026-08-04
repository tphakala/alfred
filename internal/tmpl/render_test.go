package tmpl_test

import (
	"testing"

	"github.com/tphakala/alfred/internal/tmpl"
)

func TestRender(t *testing.T) {
	got, err := tmpl.Render("Hello {{ .Name }}", map[string]any{"Name": "World"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Hello World" {
		t.Errorf("got %q, want %q", got, "Hello World")
	}
}

func TestRender_MissingKeyProducesPlaceholder(t *testing.T) {
	got, err := tmpl.Render("Hello {{ .Name }}", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Hello <no value>" {
		t.Errorf("got %q, want %q", got, "Hello <no value>")
	}
}

func TestRenderStrict(t *testing.T) {
	got, err := tmpl.RenderStrict("Hello {{ .Name }}", map[string]any{"Name": "World"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Hello World" {
		t.Errorf("got %q, want %q", got, "Hello World")
	}
}

func TestRenderStrict_MissingKeyReturnsError(t *testing.T) {
	_, err := tmpl.RenderStrict("Hello {{ .Name }}", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing key in strict mode, got nil")
	}
}

func TestRender_InvalidTemplate(t *testing.T) {
	_, err := tmpl.Render("{{ .Broken", map[string]any{})
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}
