package api

import (
	"encoding/json"
	"testing"
)

func TestSpecJSON(t *testing.T) {
	data, err := SpecJSON()
	if err != nil {
		t.Fatalf("SpecJSON() error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("SpecJSON() returned empty bytes")
	}

	data2, err2 := SpecJSON()
	if err2 != nil {
		t.Fatalf("second SpecJSON() error: %v", err2)
	}
	if len(data) == 0 || len(data2) == 0 || &data[0] != &data2[0] {
		t.Error("SpecJSON() did not return the memoized backing array on second call")
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal SpecJSON output: %v", err)
	}

	if got, ok := doc["openapi"]; !ok {
		t.Fatal("missing 'openapi' field in spec JSON")
	} else if got != "3.1.0" {
		t.Fatalf("expected openapi == '3.1.0', got %q", got)
	}

	if _, ok := doc["paths"]; !ok {
		t.Fatal("missing 'paths' field in spec JSON")
	}
}
