package config

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// WorkflowConfig represents a single workflow definition loaded from a YAML file.
type WorkflowConfig struct {
	Name        string         `yaml:"name"         json:"name"`
	Description string         `yaml:"description"  json:"description"`
	Trigger     TriggerConfig  `yaml:"trigger"      json:"trigger"`
	Model       string         `yaml:"model"        json:"model"`
	Approval    string         `yaml:"approval"     json:"approval"`
	Memory      MemoryConfig   `yaml:"memory"       json:"memory"`
	Steps       []WorkflowStep `yaml:"steps"        json:"steps"`
}

// TriggerConfig defines how and when the workflow is triggered.
type TriggerConfig struct {
	Type     string         `yaml:"type"     json:"type"`
	Entity   string         `yaml:"entity"   json:"entity"`
	Filter   map[string]any `yaml:"filter"   json:"filter"`
	Interval time.Duration  `yaml:"interval" json:"interval"`
}

// MemoryConfig controls integration with the Hindsight memory bank.
type MemoryConfig struct {
	Bank         string `yaml:"bank"          json:"bank"`
	RecallBefore bool   `yaml:"recall_before" json:"recall_before"`
	RetainAfter  bool   `yaml:"retain_after"  json:"retain_after"`
}

// WorkflowStep is a map from step type to step configuration.
type WorkflowStep map[string]any

// LoadWorkflow reads and parses a single workflow YAML file at path.
func LoadWorkflow(path string) (*WorkflowConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file %q: %w", path, err)
	}

	var wf WorkflowConfig
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("parsing workflow file %q: %w", path, err)
	}

	// Reject an empty or whitespace-only name: dispatch identities are keyed on
	// the workflow name (the poller dedup key and the Temporal WorkflowID
	// "alfred-<name>-Ticket-<id>"), so a nameless workflow yields the broken,
	// ambiguous form "alfred--Ticket-<id>". Fail closed at load. The sibling
	// agentcfg.Validate applies the same TrimSpace guard (aligned in #109).
	if strings.TrimSpace(wf.Name) == "" {
		return nil, fmt.Errorf("workflow file %q has an empty name", path)
	}

	return &wf, nil
}

// LoadWorkflowsFromDir reads all .yaml files from dir and returns the parsed
// workflows. Workflow configs are .yaml-canonical: the config API only ever
// creates, reads, and lists .yaml, so the loader deliberately does not read
// .yml (unlike the sibling agentcfg.LoadFromDir) to keep the loader and the API
// in agreement (see #107). A file that looks like a YAML config but uses a
// non-canonical extension (.yml, or any differently-cased .yaml or .yml) is
// skipped with a Warn so an operator who expected it to load is not left in
// silence; any other non-.yaml file is skipped silently. Loading is
// all-or-nothing: if any file fails to
// parse, has an empty name, or if two files declare the same workflow name, the
// whole load fails and no workflows are returned.
func LoadWorkflowsFromDir(dir string) ([]*WorkflowConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading workflow directory %q: %w", dir, err)
	}

	var workflows []*WorkflowConfig
	filesByName := make(map[string][]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if ext := filepath.Ext(entry.Name()); ext != ".yaml" {
			// Warn only for YAML-looking files with the wrong extension (.yml or
			// a case variant), so a misnamed config is not silently ignored;
			// unrelated files (temps, notes) are skipped without noise.
			if lower := strings.ToLower(ext); lower == ".yml" || lower == ".yaml" {
				slog.Warn("skipping workflow config with a non-canonical extension; rename it to .yaml",
					"file", entry.Name(), "dir", dir)
			}
			continue
		}
		wf, err := LoadWorkflow(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, wf)
		filesByName[wf.Name] = append(filesByName[wf.Name], entry.Name())
	}

	// Reject duplicate workflow names fail-closed. Two workflows sharing a name
	// produce colliding dispatch identities keyed on that name (the poller's
	// per-event dedup key "name-id" and the Temporal WorkflowID
	// "alfred-<name>-Ticket-<id>" under the reuse policy), where one event's
	// dispatch silently short-circuits another's. Neither file can be presumed
	// the intended one, so the whole load fails (matching this function's
	// existing all-or-nothing contract) and the error names every offending
	// file. Names are visited in sorted order, and os.ReadDir already sorts
	// entries, so the message is deterministic.
	var dupErrs []string
	for _, name := range slices.Sorted(maps.Keys(filesByName)) {
		if files := filesByName[name]; len(files) > 1 {
			dupErrs = append(dupErrs, fmt.Sprintf("duplicate workflow name %q declared by %s", name, strings.Join(files, ", ")))
		}
	}
	if len(dupErrs) > 0 {
		return nil, fmt.Errorf("workflow load errors: %s", strings.Join(dupErrs, "; "))
	}

	return workflows, nil
}
