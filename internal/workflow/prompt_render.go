package workflow

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/tphakala/alfred/internal/agentcfg"
)

// safePath validates that a relative path does not escape the base directory.
func safePath(base, rel string) (string, error) {
	resolved := filepath.Join(base, rel)
	r, err := filepath.Rel(base, resolved)
	if err != nil || strings.HasPrefix(r, "..") {
		return "", fmt.Errorf("path %q escapes prompt directory", rel)
	}
	return resolved, nil
}

// RenderPrompt loads the base file plus any includes, renders the optional
// header with templateVars, and concatenates with newlines.
// Order: header (rendered) + includes (in YAML order) + base.
func RenderPrompt(promptDir string, p agentcfg.Prompt, templateVars map[string]any) (string, error) {
	var parts []string

	if p.Header != "" {
		rendered, err := renderTemplate(p.Header, templateVars)
		if err != nil {
			return "", fmt.Errorf("render header: %w", err)
		}
		parts = append(parts, rendered)
	}

	for _, inc := range p.Includes {
		incPath, err := safePath(promptDir, inc)
		if err != nil {
			return "", err
		}
		body, err := os.ReadFile(incPath)
		if err != nil {
			return "", fmt.Errorf("read include %s: %w", inc, err)
		}
		parts = append(parts, string(body))
	}

	basePath, err := safePath(promptDir, p.Base)
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(basePath)
	if err != nil {
		return "", fmt.Errorf("read base %s: %w", p.Base, err)
	}
	parts = append(parts, string(body))

	return strings.Join(parts, "\n"), nil
}

func renderTemplate(s string, vars map[string]any) (string, error) {
	t, err := template.New("prompt").Option("missingkey=error").Parse(s)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}
