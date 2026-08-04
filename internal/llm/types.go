package llm

// SchemaType represents the data type in a function parameter schema.
type SchemaType string

const (
	TypeObject  SchemaType = "object"
	TypeString  SchemaType = "string"
	TypeInteger SchemaType = "integer"
	TypeNumber  SchemaType = "number"
	TypeBoolean SchemaType = "boolean"
	TypeArray   SchemaType = "array"
)

// Schema describes the structure of a function parameter, modeled after
// JSON Schema. Intentionally minimal: covers what Alfred tools use today.
type Schema struct {
	Type        SchemaType         `json:"type"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
}

// FunctionDeclaration describes a tool that the LLM can invoke.
type FunctionDeclaration struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Parameters  *Schema `json:"parameters,omitempty"`
}

// ToolMode controls how the LLM uses available tools.
type ToolMode string

const (
	ToolModeAuto ToolMode = "auto"
	ToolModeAny  ToolMode = "any"
	ToolModeNone ToolMode = "none"
)

// ToolConfig controls function calling behavior.
type ToolConfig struct {
	Mode ToolMode `json:"mode"`
}
