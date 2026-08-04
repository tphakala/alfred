package llm

import "strings"

// ModelPrice is the USD price per one million tokens for a model's input
// (prompt) and output (completion) tokens.
type ModelPrice struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

// PriceTable maps a model identifier to its per-million-token USD pricing. It
// prices native-provider LLM calls (Vertex, OpenRouter) so the supervised
// orchestration loops can meter their own token spend against a cost cap.
//
// Prices change and vary by provider, so a deployment overrides or extends the
// built-in DefaultPriceTable via config (the top-level `pricing` map). An
// unknown model is reported as such (CostUSD returns known=false) rather than
// silently priced at zero, so the caller can surface that its cost cap cannot
// see that model's spend.
type PriceTable map[string]ModelPrice

const tokensPerMillion = 1_000_000.0

// CostUSD returns the USD cost of a single call given its input and output
// token counts, and whether model was found in the table. Lookup is exact
// first, then by the longest table key that is a prefix of model, so a
// versioned id like "gemini-2.0-flash-001" resolves to a "gemini-2.0-flash"
// entry. An unknown model (and a nil table) returns (0, false).
func (t PriceTable) CostUSD(model string, inputTokens, outputTokens int) (usd float64, known bool) {
	price, ok := t.lookup(model)
	if !ok {
		return 0, false
	}
	usd = float64(inputTokens)/tokensPerMillion*price.InputPerMillion +
		float64(outputTokens)/tokensPerMillion*price.OutputPerMillion
	return usd, true
}

// lookup resolves model to a price: an exact key match wins, else the longest
// table key that is a prefix of model. Returns ok=false when nothing matches.
func (t PriceTable) lookup(model string) (ModelPrice, bool) {
	if p, ok := t[model]; ok {
		return p, true
	}
	var (
		bestKey   string
		bestPrice ModelPrice
	)
	for k, p := range t {
		if len(k) > len(bestKey) && strings.HasPrefix(model, k) {
			bestKey, bestPrice = k, p
		}
	}
	if bestKey == "" {
		return ModelPrice{}, false
	}
	return bestPrice, true
}

// DefaultPriceTable returns built-in per-million-token USD pricing for the
// native-provider models Alfred's supervised loops commonly use. These are a
// necessarily-dated snapshot of public list prices; a deployment keeps them
// current or adds models (e.g. OpenRouter-hosted models) via the config
// `pricing` map, which overlays this table. Models absent here and from config
// are unpriced: their spend does not count toward a cost cap, and CaseLLMStream
// logs a warning so the operator can add pricing.
func DefaultPriceTable() PriceTable {
	//nolint:mnd // per-model list prices are inherently literal numbers, not magic constants
	return PriceTable{
		// Google Gemini (Vertex AI), USD per 1M tokens.
		"gemini-2.5-pro":   {InputPerMillion: 1.25, OutputPerMillion: 10.00},
		"gemini-2.5-flash": {InputPerMillion: 0.30, OutputPerMillion: 2.50},
		"gemini-2.0-flash": {InputPerMillion: 0.10, OutputPerMillion: 0.40},
		"gemini-1.5-pro":   {InputPerMillion: 1.25, OutputPerMillion: 5.00},
		"gemini-1.5-flash": {InputPerMillion: 0.075, OutputPerMillion: 0.30},
	}
}
