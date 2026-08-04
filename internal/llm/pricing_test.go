package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// testModelFlash is the shipped native model, used across several pricing tests.
const testModelFlash = "gemini-2.0-flash"

func TestPriceTable_CostUSD_ExactMatch(t *testing.T) {
	t.Parallel()
	tbl := PriceTable{"m": {InputPerMillion: 0.10, OutputPerMillion: 0.40}}

	// 2M input @ 0.10/M + 1M output @ 0.40/M = 0.20 + 0.40 = 0.60.
	usd, known := tbl.CostUSD("m", 2_000_000, 1_000_000)
	assert.True(t, known)
	assert.InDelta(t, 0.60, usd, 1e-9)
}

func TestPriceTable_CostUSD_PrefixMatch(t *testing.T) {
	t.Parallel()
	tbl := PriceTable{testModelFlash: {InputPerMillion: 0.10, OutputPerMillion: 0.40}}

	// A versioned/suffixed id resolves to its base entry by prefix.
	usd, known := tbl.CostUSD("gemini-2.0-flash-001", 1_000_000, 0)
	assert.True(t, known)
	assert.InDelta(t, 0.10, usd, 1e-9)
}

func TestPriceTable_CostUSD_LongestPrefixWins(t *testing.T) {
	t.Parallel()
	tbl := PriceTable{
		"gemini":       {InputPerMillion: 99, OutputPerMillion: 99},
		testModelFlash: {InputPerMillion: 0.10, OutputPerMillion: 0.40},
	}
	usd, known := tbl.CostUSD("gemini-2.0-flash-lite", 1_000_000, 0)
	assert.True(t, known)
	assert.InDelta(t, 0.10, usd, 1e-9, "the longer, more specific key must win over the short one")
}

func TestPriceTable_CostUSD_UnknownModel(t *testing.T) {
	t.Parallel()
	tbl := PriceTable{testModelFlash: {InputPerMillion: 0.10, OutputPerMillion: 0.40}}

	usd, known := tbl.CostUSD("some-unlisted-model", 5_000_000, 5_000_000)
	assert.False(t, known, "an unknown model must report known=false, not silently price at zero")
	assert.Zero(t, usd)
}

func TestPriceTable_CostUSD_NilTableIsSafe(t *testing.T) {
	t.Parallel()
	var tbl PriceTable
	usd, known := tbl.CostUSD("anything", 1_000_000, 1_000_000)
	assert.False(t, known)
	assert.Zero(t, usd)
}

func TestPriceTable_CostUSD_ZeroTokens(t *testing.T) {
	t.Parallel()
	tbl := PriceTable{"m": {InputPerMillion: 0.10, OutputPerMillion: 0.40}}
	usd, known := tbl.CostUSD("m", 0, 0)
	assert.True(t, known)
	assert.Zero(t, usd)
}

func TestDefaultPriceTable_PricesShippedNativeModel(t *testing.T) {
	t.Parallel()
	// The shipped supervised configs run their native loops on gemini-2.0-flash,
	// so the built-in table must price it (otherwise cost_cap is inert out of
	// the box for those deployments).
	usd, known := DefaultPriceTable().CostUSD(testModelFlash, 1_000_000, 1_000_000)
	assert.True(t, known, "the built-in table must price the shipped native model")
	assert.Positive(t, usd)
}
