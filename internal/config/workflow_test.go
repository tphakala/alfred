package config_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/alfred/internal/config"
)

const triageWorkflowYAML = `
name: ticket-triage
description: Classify and route new tickets
trigger:
  type: polling
  entity: Ticket
  filter:
    status: New
  interval: 5m
model: gemini-3-flash-preview
approval: none
memory:
  bank: alfred-tickets
  recall_before: true
  retain_after: true
steps:
  - analyze:
      prompt: |
        Analyze this Autotask ticket
      output_schema:
        category:
          type: string
  - act:
      - set_field: { name: category, value: "{{ .category }}" }
`

func TestLoadWorkflow(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "workflow-*.yaml")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if _, err := f.WriteString(triageWorkflowYAML); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing temp file: %v", err)
	}

	wf, err := config.LoadWorkflow(f.Name())
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}

	if wf.Name != "ticket-triage" {
		t.Errorf("Name = %q, want %q", wf.Name, "ticket-triage")
	}
	if wf.Trigger.Entity != "Ticket" {
		t.Errorf("Trigger.Entity = %q, want %q", wf.Trigger.Entity, "Ticket")
	}
	wantInterval := 5 * time.Minute
	if wf.Trigger.Interval != wantInterval {
		t.Errorf("Trigger.Interval = %v, want %v", wf.Trigger.Interval, wantInterval)
	}
	if wf.Model != "gemini-3-flash-preview" {
		t.Errorf("Model = %q, want %q", wf.Model, "gemini-3-flash-preview")
	}
	if wf.Approval != "none" {
		t.Errorf("Approval = %q, want %q", wf.Approval, "none")
	}
	if wf.Memory.Bank != "alfred-tickets" {
		t.Errorf("Memory.Bank = %q, want %q", wf.Memory.Bank, "alfred-tickets")
	}
	if !wf.Memory.RecallBefore {
		t.Errorf("Memory.RecallBefore = false, want true")
	}
	if !wf.Memory.RetainAfter {
		t.Errorf("Memory.RetainAfter = false, want true")
	}
	if len(wf.Steps) != 2 {
		t.Errorf("len(Steps) = %d, want 2", len(wf.Steps))
	}
}

func TestLoadWorkflowsFromDir(t *testing.T) {
	dir := t.TempDir()

	// Write two .yaml files
	for _, name := range []string{"wf1.yaml", "wf2.yaml"} {
		content := "name: " + name[:len(name)-5] + "\ntrigger:\n  type: polling\n  interval: 1m\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	// Write one non-yaml file that should be ignored
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a workflow"), 0o644); err != nil {
		t.Fatalf("writing notes.txt: %v", err)
	}

	workflows, err := config.LoadWorkflowsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadWorkflowsFromDir() error = %v", err)
	}

	if len(workflows) != 2 {
		t.Errorf("len(workflows) = %d, want 2", len(workflows))
	}
}

func TestLoadWorkflowsFromDir_DuplicateName(t *testing.T) {
	dir := t.TempDir()

	// Two files declare the same workflow name. They would collide on dispatch
	// identities keyed on that name, so the loader must reject them and name
	// both files. "collide" is deliberately not a substring of the error
	// boilerplate ("duplicate workflow name ...") so the name assertion below
	// cannot pass vacuously.
	for _, name := range []string{"a.yaml", "b.yaml"} {
		content := "name: collide\ntrigger:\n  type: polling\n  interval: 1m\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	// A uniquely named file alongside the collision must not change the outcome
	// and must not be swept into the error. "solo"/"c.yaml" appear nowhere in
	// the expected message.
	unique := "name: solo\ntrigger:\n  type: polling\n  interval: 1m\n"
	if err := os.WriteFile(filepath.Join(dir, "c.yaml"), []byte(unique), 0o644); err != nil {
		t.Fatalf("writing c.yaml: %v", err)
	}

	workflows, err := config.LoadWorkflowsFromDir(dir)
	if err == nil {
		t.Fatalf("LoadWorkflowsFromDir() error = nil, want a duplicate-name error (loaded %d)", len(workflows))
	}
	if workflows != nil {
		t.Errorf("LoadWorkflowsFromDir() workflows = %d, want nil on duplicate-name rejection", len(workflows))
	}
	msg := err.Error()
	// The error must name the duplicated workflow (quoted, so "collide" is the
	// name field and not incidental boilerplate) and both offending files.
	for _, want := range []string{`"collide"`, "a.yaml", "b.yaml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must mention %q", msg, want)
		}
	}
	// The uniquely-named file must not appear: the guard rejects only the
	// collision, and a single-file name must never be flagged.
	for _, unwanted := range []string{"solo", "c.yaml"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("error %q must not mention the uniquely-named workflow %q", msg, unwanted)
		}
	}
	// Offending files are reported in a deterministic (sorted) order.
	if ai, bi := strings.Index(msg, "a.yaml"), strings.Index(msg, "b.yaml"); ai > bi {
		t.Errorf("error %q must list a.yaml before b.yaml (deterministic order)", msg)
	}
}

func TestLoadWorkflow_EmptyNameRejected(t *testing.T) {
	// A workflow file with an empty or whitespace-only name produces a broken,
	// ambiguous dispatch identity ("alfred--Ticket-<id>"), so the loader must
	// reject it. A quoted whitespace-only value ("  ") survives a plain == ""
	// check but is just as broken, so the guard uses strings.TrimSpace.
	cases := map[string]string{
		"no name field":    "trigger:\n  type: polling\n  interval: 1m\n",
		"empty name value": "name: \"\"\ntrigger:\n  type: polling\n  interval: 1m\n",
		"whitespace name":  "name: \"   \"\ntrigger:\n  type: polling\n  interval: 1m\n",
	}
	for label, content := range cases {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unnamed.yaml")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("writing %s: %v", path, err)
			}

			wf, err := config.LoadWorkflow(path)
			if err == nil {
				t.Fatalf("LoadWorkflow() error = nil, want an empty-name error (got %+v)", wf)
			}
			// The error names the offending file (by base, so the assertion is
			// path-separator-safe) and identifies the empty-name cause.
			for _, want := range []string{filepath.Base(path), "empty name"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must mention %q", err.Error(), want)
				}
			}
		})
	}
}

func TestLoadWorkflowsFromDir_EmptyNameRejected(t *testing.T) {
	// The empty-name guard lives in LoadWorkflow; LoadWorkflowsFromDir must
	// surface it fail-closed (whole load fails, nil workflows), matching the
	// all-or-nothing contract the dup-name guard also holds.
	dir := t.TempDir()
	content := "trigger:\n  type: polling\n  interval: 1m\n"
	if err := os.WriteFile(filepath.Join(dir, "nameless.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing nameless.yaml: %v", err)
	}

	workflows, err := config.LoadWorkflowsFromDir(dir)
	if err == nil {
		t.Fatalf("LoadWorkflowsFromDir() error = nil, want an empty-name error (loaded %d)", len(workflows))
	}
	if workflows != nil {
		t.Errorf("LoadWorkflowsFromDir() workflows = %d, want nil on empty-name rejection", len(workflows))
	}
	if !strings.Contains(err.Error(), "empty name") {
		t.Errorf("error %q must mention the empty-name cause", err.Error())
	}
}

func TestLoadWorkflowsFromDir_YmlSkippedWithWarning(t *testing.T) {
	// The workflow-config API (list/GET/PUT) is .yaml-canonical, so the loader
	// intentionally does NOT read .yml (unlike the sibling agentcfg.LoadFromDir):
	// loading only the loader half would let a hand-placed .yml plus an API PUT
	// manufacture a .yaml/.yml same-name pair that bricks boot via the dup-name
	// guard (#107 resolution: stay .yaml-canonical). A dropped .yml is skipped
	// but logged at Warn so an operator who expected it to load is not left in
	// silence.
	dir := t.TempDir()
	content := "name: from-yml\ntrigger:\n  type: polling\n  interval: 1m\n"
	// wf.yml exercises the .yml branch; caps.YAML exercises the case-fold branch
	// (a differently-cased .yaml is NOT loaded by the case-sensitive guard but IS
	// warned), so both inner arms of the warn condition are covered.
	for _, name := range []string{"wf.yml", "caps.YAML"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	workflows, err := config.LoadWorkflowsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadWorkflowsFromDir() error = %v", err)
	}
	if len(workflows) != 0 {
		t.Errorf("len(workflows) = %d, want 0 (.yml and a cased .YAML must be skipped, not loaded)", len(workflows))
	}

	logged := buf.String()
	for _, want := range []string{"wf.yml", "caps.YAML", "non-canonical"} {
		if !strings.Contains(logged, want) {
			t.Errorf("warn output must mention %q; got: %q", want, logged)
		}
	}
}

func TestLoadWorkflowsFromDir_NonYAMLSkippedSilently(t *testing.T) {
	// A genuinely non-YAML file (e.g. a leftover temp or notes file) is skipped
	// WITHOUT a warning: the warning is reserved for YAML-looking files that used
	// the wrong extension, not for every unrelated file in the directory.
	dir := t.TempDir()
	// config.yml.tmp is YAML-looking by name but its actual extension is .tmp, so
	// a correct filepath.Ext-based scope stays silent; a naive Contains(".yml")
	// scope would wrongly warn.
	for _, name := range []string{"notes.txt", ".atomic-abc.tmp", "config.yml.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	workflows, err := config.LoadWorkflowsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadWorkflowsFromDir() error = %v", err)
	}
	if len(workflows) != 0 {
		t.Errorf("len(workflows) = %d, want 0", len(workflows))
	}
	if logged := buf.String(); logged != "" {
		t.Errorf("non-YAML files must be skipped silently, got warning(s): %q", logged)
	}
}
