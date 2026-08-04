package server

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestOpenAPISpecLoadsAndValidates(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("load openapi.yaml: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate openapi.yaml: %v", err)
	}
	if doc.Info.Version == "" {
		t.Fatal("missing info.version")
	}
}
