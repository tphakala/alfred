package agentcfg

// This file holds the small, dependency-free JSON Schema reading primitives
// shared between agentcfg's load-time validation and the workflow package's
// LLM-facing tool-schema builder. Both layers must agree on what a declared
// tool's args_schema means: a "required" name the builder ignores is a name
// the load-time placeholder check must also treat as not-required, or a config
// that loads clean would still reject calls at exec time. Keeping the readers
// here (agentcfg has no llm dependency, and workflow imports agentcfg) removes
// the previously hand-kept mirror copies that could silently drift.

// SchemaProperties returns the "properties" map of a JSON Schema node parsed
// from YAML into map[string]any, or nil when raw is not such a map (absent or
// malformed "properties"). Only this level is read; nested property schemas
// stay as raw values for the caller to convert further.
func SchemaProperties(raw any) map[string]any {
	props, _ := raw.(map[string]any)
	return props
}

// SchemaStringList returns the string elements of a JSON Schema
// array-of-strings value (used by "required" and "enum"), skipping any
// non-string element, or nil when raw is not a []any.
func SchemaStringList(raw any) []string {
	items, _ := raw.([]any)
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
