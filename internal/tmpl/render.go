package tmpl

import (
	"bytes"
	"fmt"
	"text/template"
)

// Render renders a Go text/template with the given data.
// Missing keys produce "<no value>" (default Go template behavior).
func Render(tmplStr string, data map[string]any) (string, error) {
	return render(tmplStr, data, false)
}

// RenderStrict renders a Go text/template with the given data.
// Missing keys cause an error instead of producing "<no value>".
func RenderStrict(tmplStr string, data map[string]any) (string, error) {
	return render(tmplStr, data, true)
}

func render(tmplStr string, data map[string]any, strict bool) (string, error) {
	t := template.New("tmpl")
	if strict {
		t = t.Option("missingkey=error")
	}
	t, err := t.Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}
