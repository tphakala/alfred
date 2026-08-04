package llm

import "testing"

func TestSchemaType_Values(t *testing.T) {
	types := []SchemaType{TypeObject, TypeString, TypeInteger, TypeNumber, TypeBoolean, TypeArray}
	for _, st := range types {
		if st == "" {
			t.Error("SchemaType constant is empty")
		}
	}
}

func TestToolMode_Values(t *testing.T) {
	modes := []ToolMode{ToolModeAuto, ToolModeAny, ToolModeNone}
	for _, m := range modes {
		if m == "" {
			t.Error("ToolMode constant is empty")
		}
	}
}
