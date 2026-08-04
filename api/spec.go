// Package api embeds the OpenAPI 3.1 contract for the Alfred HTTP API and
// exposes it as JSON. api/openapi.yaml is the single source of truth.
package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var specYAML []byte

var specJSON = sync.OnceValues(func() ([]byte, error) {
	var doc any
	if err := yaml.Unmarshal(specYAML, &doc); err != nil {
		return nil, fmt.Errorf("parse embedded openapi.yaml: %w", err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode openapi spec as json: %w", err)
	}
	return out, nil
})

// SpecJSON returns the OpenAPI 3.1 contract (api/openapi.yaml) encoded as JSON.
// The conversion runs once on first call; subsequent calls return the cached result.
func SpecJSON() ([]byte, error) { return specJSON() }
