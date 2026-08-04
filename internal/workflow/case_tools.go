package workflow

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/store"
)

// Parameter names for the two built-in Case Supervisor tools.
const (
	argRecordStatus  = "status"
	argRecordOutcome = "outcome"
	argSpawnKind     = "kind"
	argSpawnInput    = "input"
	argFailReason    = "reason"
)

// numBuiltinTools is record_outcome + spawn_agent, the two tools every
// Supervisor gets regardless of config.
const numBuiltinTools = 2

// recordOutcomeStatuses are the terminal statuses record_outcome accepts.
// Sourced from store.TerminalTaskStatuses (the same set the queue monitor's
// dedup check uses) so the tool schema and the ledger's terminal-status set
// cannot drift apart.
var recordOutcomeStatuses = store.TerminalTaskStatuses()

// supervisorToolDeclarations returns the function declarations presented to
// the Case Supervisor LLM: the two built-in orchestration tools
// (record_outcome, spawn_agent) followed by one declaration per
// deployment-declared command tool, sorted by name. The order is
// deterministic so the same Supervisor config always yields the same
// declaration list.
func supervisorToolDeclarations(sup agentcfg.Supervisor) []*llm.FunctionDeclaration { //nolint:gocritic // hugeParam: signature fixed by the Task 2a design (mirrors the config value the workflow already holds)
	decls := make([]*llm.FunctionDeclaration, 0, numBuiltinTools+len(sup.Tools))
	decls = append(decls, recordOutcomeDeclaration(), spawnAgentDeclaration(sup.SubAgents))
	return appendSortedDeclaredTools(decls, sup.Tools)
}

// appendSortedDeclaredTools appends one declaration per declared tool, sorted
// by name so the same config always yields the same declaration order.
func appendSortedDeclaredTools(decls []*llm.FunctionDeclaration, tools []agentcfg.DeclaredTool) []*llm.FunctionDeclaration {
	sorted := slices.Clone(tools)
	slices.SortFunc(sorted, func(a, b agentcfg.DeclaredTool) int { return strings.Compare(a.Name, b.Name) })
	for i := range sorted {
		decls = append(decls, declaredToolDeclaration(&sorted[i]))
	}
	return decls
}

// recordOutcomeDeclaration builds the declaration for the built-in tool that
// finalizes a case with a terminal status and structured outcome.
func recordOutcomeDeclaration() *llm.FunctionDeclaration {
	return &llm.FunctionDeclaration{
		Name:        agentcfg.ReservedToolRecordOutcome,
		Description: "Finalize the case with a terminal status and a structured outcome. Call this once you have reached a conclusion; no further rounds run after it.",
		Parameters: &llm.Schema{
			Type: llm.TypeObject,
			Properties: map[string]*llm.Schema{
				argRecordStatus: {
					Type:        llm.TypeString,
					Description: "Terminal status for the case.",
					Enum:        recordOutcomeStatuses,
				},
				argRecordOutcome: {
					Type:        llm.TypeObject,
					Description: "Free-form structured outcome recorded alongside the status.",
				},
			},
			Required: []string{argRecordStatus},
		},
	}
}

// spawnAgentDeclaration builds the declaration for the built-in tool that
// dispatches a sub-agent child workflow and awaits its result. The kind enum
// lists the sub-agent kinds this Supervisor config declares; an empty
// SubAgents map still produces the declaration, just with no allowed kinds,
// so the workflow can reject an unknown kind at call time with a clear error
// instead of hiding the tool entirely.
func spawnAgentDeclaration(subAgents map[string]agentcfg.AgentConfig) *llm.FunctionDeclaration {
	kinds := slices.Sorted(maps.Keys(subAgents))

	return &llm.FunctionDeclaration{
		Name:        agentcfg.ReservedToolSpawnAgent,
		Description: "Dispatch a sub-agent of the named kind and return its result once it completes.",
		Parameters: &llm.Schema{
			Type: llm.TypeObject,
			Properties: map[string]*llm.Schema{
				argSpawnKind: {
					Type:        llm.TypeString,
					Description: "Which configured sub-agent kind to dispatch.",
					Enum:        kinds,
				},
				argSpawnInput: {
					Type:        llm.TypeObject,
					Description: "Free-form input passed to the sub-agent.",
				},
			},
			Required: []string{argSpawnKind},
		},
	}
}

// declaredToolDeclaration converts a deployment-declared command tool's
// config into a function declaration. The engine never knows what the
// command does; it only knows the argument schema the deployment declared.
func declaredToolDeclaration(t *agentcfg.DeclaredTool) *llm.FunctionDeclaration {
	return &llm.FunctionDeclaration{
		Name:        t.Name,
		Description: t.Description,
		Parameters:  argsSchemaToLLMSchema(t.ArgsSchema),
	}
}

// argsSchemaToLLMSchema converts a JSON Schema object (as parsed from YAML
// into map[string]any) into the llm.Schema shape used by StreamRequest.Tools.
// Only the subset of JSON Schema Alfred's tools already use is supported:
// type, description, properties, required, items, enum. A nil or empty
// schema map produces an empty object schema.
func argsSchemaToLLMSchema(schema map[string]any) *llm.Schema {
	if len(schema) == 0 {
		return &llm.Schema{Type: llm.TypeObject}
	}
	return convertSchemaNode(schema)
}

// convertSchemaNode converts one JSON Schema node, recursing into properties
// and items. Split into per-field helpers to keep this function's own
// complexity low; each helper owns one JSON Schema keyword.
func convertSchemaNode(node map[string]any) *llm.Schema {
	s := &llm.Schema{Type: llm.TypeObject}
	if t, ok := node["type"].(string); ok {
		s.Type = llm.SchemaType(t)
	}
	if desc, ok := node["description"].(string); ok {
		s.Description = desc
	}
	s.Properties = convertSchemaProperties(node["properties"])
	s.Required = agentcfg.SchemaStringList(node["required"])
	s.Enum = agentcfg.SchemaStringList(node["enum"])
	if items, ok := node["items"].(map[string]any); ok {
		s.Items = convertSchemaNode(items)
	}
	return s
}

// convertSchemaProperties converts a JSON Schema "properties" value (a map of
// name to nested schema node) into llm.Schema's Properties map. Returns nil
// if raw is not a map[string]any (absent or malformed "properties"). The raw
// map is read via agentcfg.SchemaProperties so this LLM-facing builder and
// agentcfg's load-time validation agree on which properties a tool declares.
func convertSchemaProperties(raw any) map[string]*llm.Schema {
	props := agentcfg.SchemaProperties(raw)
	if props == nil {
		return nil
	}
	out := make(map[string]*llm.Schema, len(props))
	for name, rawNode := range props {
		if propNode, ok := rawNode.(map[string]any); ok {
			out[name] = convertSchemaNode(propNode)
		}
	}
	return out
}

// nativeSubAgentToolDeclarations returns the function declarations presented
// to a native sub-agent's LLM: the two built-in terminal tools (submit_result
// bound to the declared output schema, report_failure as the explicit failure
// hatch) followed by one declaration per deployment-declared command tool,
// sorted by name. A native sub-agent has no spawn_agent tool: it does not
// spawn further agents.
func nativeSubAgentToolDeclarations(na agentcfg.NativeAgent) []*llm.FunctionDeclaration { //nolint:gocritic // hugeParam: mirrors the config value the workflow already holds
	const numNativeBuiltins = 2
	decls := make([]*llm.FunctionDeclaration, 0, numNativeBuiltins+len(na.Tools))
	decls = append(decls, submitResultDeclaration(na.Output.Schema), reportFailureDeclaration())
	return appendSortedDeclaredTools(decls, na.Tools)
}

// submitResultDeclaration builds the declaration for the built-in tool a
// native sub-agent calls to return its final result. Its parameters ARE the
// declared output schema, so the provider's function-calling enforces the
// output shape structurally; submitResultTypeMismatches and
// missingRequiredFields backstop it at acceptance time.
func submitResultDeclaration(schema any) *llm.FunctionDeclaration {
	return &llm.FunctionDeclaration{
		Name:        agentcfg.ReservedToolSubmitResult,
		Description: "Submit the final result for this task. Arguments must satisfy the declared output schema. Call exactly once, as your last action; no further rounds run after it.",
		Parameters:  outputSchemaToLLMSchema(schema),
	}
}

// reportFailureDeclaration builds the declaration for the built-in tool a
// native sub-agent calls to give up explicitly. Without it the only terminal
// is schema-bound submit_result, which would push a stuck agent toward
// fabricating output or burning rounds until a guard trips.
func reportFailureDeclaration() *llm.FunctionDeclaration {
	return &llm.FunctionDeclaration{
		Name:        agentcfg.ReservedToolReportFailure,
		Description: "Report that the task cannot be completed. Call this instead of submit_result when you hit an unrecoverable problem, with a concrete reason. No further rounds run after it.",
		Parameters: &llm.Schema{
			Type: llm.TypeObject,
			Properties: map[string]*llm.Schema{
				argFailReason: {
					Type:        llm.TypeString,
					Description: "Why the task cannot be completed.",
				},
			},
			Required: []string{argFailReason},
		},
	}
}

// outputSchemaToLLMSchema converts a declared output schema (YAML-parsed into
// map[string]any, or nil) into the llm.Schema shape. A nil or non-object
// schema yields an empty object schema so submit_result still has valid
// parameters.
func outputSchemaToLLMSchema(schema any) *llm.Schema {
	node, ok := schema.(map[string]any)
	if !ok {
		return &llm.Schema{Type: llm.TypeObject}
	}
	return argsSchemaToLLMSchema(node)
}

// missingRequiredFields returns the required top-level fields named in the
// output schema that are absent (or present but nil) in args. Returns nil
// when the schema is not an object or declares no required list. Full nested
// JSON Schema validation and deployment validate commands are Phase 6.
func missingRequiredFields(schema any, args map[string]any) []string {
	node, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	var gaps []string
	for _, f := range agentcfg.SchemaStringList(node["required"]) {
		if v, present := args[f]; !present || v == nil {
			gaps = append(gaps, f)
		}
	}
	return gaps
}

// submitResultTypeMismatches checks each top-level schema property with a
// "type" keyword against the dynamic type of the matching arg, for args that
// are present and non-nil (absence is missingRequiredFields' job). Properties
// are visited in sorted order so the resulting message is deterministic in
// workflow code. string/object/array/boolean are enforced; number and integer
// are deliberately lenient because JSON decoding blurs int/float across
// providers.
func submitResultTypeMismatches(schema any, args map[string]any) []string {
	node, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	props := agentcfg.SchemaProperties(node["properties"])
	if props == nil {
		return nil
	}
	var mismatches []string
	for _, name := range slices.Sorted(maps.Keys(props)) {
		propNode, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		wantType, ok := propNode["type"].(string)
		if !ok {
			continue
		}
		val, present := args[name]
		if !present || val == nil {
			continue
		}
		if !matchesSchemaType(wantType, val) {
			mismatches = append(mismatches, fmt.Sprintf("%s: expected %s", name, wantType))
		}
	}
	return mismatches
}

// matchesSchemaType reports whether val's dynamic type satisfies a JSON
// Schema top-level type keyword. Numeric types (number, integer) accept any
// numeric dynamic representation, since JSON decoding blurs int and float
// across providers, but reject strings, bools, objects, and arrays. Unknown
// keywords return true (lenient).
func matchesSchemaType(wantType string, val any) bool {
	switch wantType {
	case "string":
		_, ok := val.(string)
		return ok
	case "object":
		_, ok := val.(map[string]any)
		return ok
	case "array":
		_, ok := val.([]any)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "number", "integer":
		switch val.(type) {
		case float64, float32, int, int32, int64, json.Number:
			return true
		default:
			return false
		}
	default:
		return true
	}
}
