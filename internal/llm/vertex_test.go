package llm

import (
	"errors"
	"io"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

func TestToGenaiFunctionDeclarations(t *testing.T) {
	decls := []*FunctionDeclaration{
		{
			Name:        "test_tool",
			Description: "A test tool",
			Parameters: &Schema{
				Type: TypeObject,
				Properties: map[string]*Schema{
					"query": {Type: TypeString, Description: "search query"},
				},
				Required: []string{"query"},
			},
		},
	}
	result := toGenaiFunctionDeclarations(decls)
	if len(result) != 1 {
		t.Fatalf("expected 1 declaration, got %d", len(result))
	}
	if result[0].Name != "test_tool" {
		t.Errorf("name = %q, want %q", result[0].Name, "test_tool")
	}
	if result[0].Parameters == nil {
		t.Fatal("parameters is nil")
	}
	if result[0].Parameters.Type != genai.TypeObject {
		t.Errorf("type = %v, want TypeObject", result[0].Parameters.Type)
	}
	if _, ok := result[0].Parameters.Properties["query"]; !ok {
		t.Error("missing 'query' property")
	}
}

func TestToGenaiFunctionDeclarations_Empty(t *testing.T) {
	if toGenaiFunctionDeclarations(nil) != nil {
		t.Error("expected nil for nil input")
	}
}

func TestToGenaiToolConfig(t *testing.T) {
	tests := []struct {
		mode ToolMode
		want genai.FunctionCallingConfigMode
	}{
		{ToolModeAuto, genai.FunctionCallingConfigModeAuto},
		{ToolModeAny, genai.FunctionCallingConfigModeAny},
		{ToolModeNone, genai.FunctionCallingConfigModeNone},
	}
	for _, tt := range tests {
		tc := toGenaiToolConfig(&ToolConfig{Mode: tt.mode})
		if tc.FunctionCallingConfig.Mode != tt.want {
			t.Errorf("mode %q: got %v, want %v", tt.mode, tc.FunctionCallingConfig.Mode, tt.want)
		}
	}
}

func TestToGenaiToolConfig_Nil(t *testing.T) {
	if toGenaiToolConfig(nil) != nil {
		t.Error("expected nil for nil input")
	}
}

func TestToGenaiType_CaseInsensitive(t *testing.T) {
	if toGenaiType("OBJECT") != genai.TypeObject {
		t.Error("uppercase OBJECT should map to TypeObject")
	}
	if toGenaiType("STRING") != genai.TypeString {
		t.Error("uppercase STRING should map to TypeString")
	}
	if toGenaiType("object") != genai.TypeObject {
		t.Error("lowercase object should map to TypeObject")
	}
}

func TestToGenaiSchema_Recursive(t *testing.T) {
	s := &Schema{
		Type: TypeObject,
		Properties: map[string]*Schema{
			"items": {
				Type:  TypeArray,
				Items: &Schema{Type: TypeString},
			},
		},
		Required: []string{"items"},
	}
	gs := toGenaiSchema(s)
	if gs.Type != genai.TypeObject {
		t.Errorf("root type = %v, want TypeObject", gs.Type)
	}
	items, ok := gs.Properties["items"]
	if !ok {
		t.Fatal("missing 'items' property")
	}
	if items.Type != genai.TypeArray {
		t.Errorf("items type = %v, want TypeArray", items.Type)
	}
	if items.Items == nil || items.Items.Type != genai.TypeString {
		t.Error("items.Items should be TypeString")
	}
}

func TestToGenaiSchema_Nil(t *testing.T) {
	if toGenaiSchema(nil) != nil {
		t.Error("expected nil for nil input")
	}
}

// fakeSeq builds an iter.Seq2 from pre-canned (response, error) pairs.
func fakeSeq(pairs []struct {
	resp *genai.GenerateContentResponse
	err  error
}) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		for _, p := range pairs {
			if !yield(p.resp, p.err) {
				return
			}
		}
	}
}

// textResp is a tiny helper producing a genai response with a single text part.
func textResp(text string, usage *genai.GenerateContentResponseUsageMetadata) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Parts: []*genai.Part{{Text: text}},
			},
		}},
		UsageMetadata: usage,
	}
}

func TestPullStreamAdapterHappyPath(t *testing.T) {
	usage := &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     4,
		CandidatesTokenCount: 6,
		TotalTokenCount:      10,
	}
	seq := fakeSeq([]struct {
		resp *genai.GenerateContentResponse
		err  error
	}{
		{resp: textResp("Hel", nil)},
		{resp: textResp("lo ", nil)},
		{resp: textResp("world", usage)},
	})

	it := newPullStreamAdapter(t.Context(), seq)
	defer func() { _ = it.Close() }()

	var texts []string
	var gotUsage *TokenUsage
	for {
		c, err := it.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
		if c.Tokens != nil {
			gotUsage = c.Tokens
		}
	}

	assert.Equal(t, []string{"Hel", "lo ", "world"}, texts)
	require.NotNil(t, gotUsage)
	assert.Equal(t, int32(10), gotUsage.TotalTokens)
}

func TestPullStreamAdapterPropagatesIteratorError(t *testing.T) {
	wantErr := errors.New("vertex exploded")
	seq := fakeSeq([]struct {
		resp *genai.GenerateContentResponse
		err  error
	}{
		{resp: textResp("partial", nil)},
		{resp: nil, err: wantErr},
	})

	it := newPullStreamAdapter(t.Context(), seq)
	defer func() { _ = it.Close() }()

	c, err := it.Next()
	require.NoError(t, err)
	assert.Equal(t, "partial", c.Text)

	_, err = it.Next()
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestPullStreamAdapterCloseStopsIteration(t *testing.T) {
	seq := fakeSeq([]struct {
		resp *genai.GenerateContentResponse
		err  error
	}{
		{resp: textResp("a", nil)},
		{resp: textResp("b", nil)},
		{resp: textResp("c", nil)},
	})

	it := newPullStreamAdapter(t.Context(), seq)

	_, err := it.Next()
	require.NoError(t, err)
	require.NoError(t, it.Close())

	_, err = it.Next()
	assert.ErrorIs(t, err, io.EOF)
}
