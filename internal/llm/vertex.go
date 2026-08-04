package llm

import (
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

var _ Client = &VertexClient{}

// messagesToContents converts llm.Messages into Vertex genai.Content,
// preserving tool-call history as FunctionCall/FunctionResponse parts.
// mapRoleToVertex maps an llm.Role to a Vertex/genai content role. Vertex
// accepts only "user" and "model", so the flattened ctxbuild roles that reach
// here (tool_call, tool_result, context, approval_request, approval_result,
// error) must be normalized or the genai backend rejects the request with an
// invalid-role error. Model-side roles become "model"; every user-side role
// becomes "user", mirroring the split isModelSideRole uses for the guard.
func mapRoleToVertex(r Role) string {
	if isModelSideRole(r) {
		return string(RoleModel)
	}
	return string(RoleUser)
}

func messagesToContents(messages []Message) []*genai.Content {
	contents := make([]*genai.Content, 0, len(messages))
	for _, m := range messages {
		var parts []*genai.Part
		if m.Text != "" {
			parts = append(parts, &genai.Part{Text: m.Text})
		}
		for _, tc := range m.ToolCalls {
			parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
				Name: tc.Name,
				Args: tc.Args,
			}})
		}
		for _, tr := range m.ToolResults {
			parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
				Name:     tr.Name,
				Response: toolResponsePayload(tr),
			}})
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, &genai.Content{
			Role:  mapRoleToVertex(m.Role),
			Parts: parts,
		})
	}
	return contents
}

// toolResponsePayload builds the map for genai.FunctionResponse.Response.
func toolResponsePayload(tr ToolResultPart) map[string]any {
	if tr.Error != "" {
		return map[string]any{"error": tr.Error}
	}
	return map[string]any{"output": tr.Result}
}

// VertexClient is a Client backed by Google Vertex AI via the GenAI SDK.
type VertexClient struct {
	client *genai.Client
	logger *slog.Logger
}

// NewVertexClient creates a new VertexClient connected to the given GCP
// project and location. If credentialsFile is non-empty, the service account
// JSON at that path is loaded and passed to the genai client; otherwise the
// genai client falls back to Application Default Credentials (GOOGLE_APPLI-
// CATION_CREDENTIALS env var, gcloud login, etc). If logger is nil a discard
// logger is used.
func NewVertexClient(ctx context.Context, project, location, credentialsFile string, logger *slog.Logger) (*VertexClient, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	cfg := &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  project,
		Location: location,
	}
	if credentialsFile != "" {
		creds, err := authCredentialsFromFile(credentialsFile, logger)
		if err != nil {
			return nil, fmt.Errorf("llm: load vertex credentials: %w", err)
		}
		cfg.Credentials = creds
	}
	c, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("llm: create vertex ai client: %w", err)
	}
	return &VertexClient{client: c, logger: logger}, nil
}

// Generate sends a prompt to the Vertex AI model and returns the generated
// content together with token usage metadata.
func (v *VertexClient) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	cfg := &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
	}
	if req.Temperature != 0 {
		cfg.Temperature = &req.Temperature
	}
	if req.MaxTokens != 0 {
		cfg.MaxOutputTokens = req.MaxTokens
	}

	resp, err := v.client.Models.GenerateContent(ctx, req.Model, genai.Text(req.Prompt), cfg)
	if err != nil {
		return nil, fmt.Errorf("llm: generate content: %w", err)
	}
	if resp == nil {
		return &GenerateResponse{}, nil
	}

	// Extract text from all candidates.
	var sb strings.Builder
	for _, cand := range resp.Candidates {
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				sb.WriteString(part.Text)
			}
		}
	}

	// Extract token usage.
	var usage TokenUsage
	if m := resp.UsageMetadata; m != nil {
		usage = TokenUsage{
			PromptTokens:   m.PromptTokenCount,
			ResponseTokens: m.CandidatesTokenCount,
			TotalTokens:    m.TotalTokenCount,
		}
	}

	v.logger.InfoContext(ctx, "llm token usage",
		"model", req.Model,
		"prompt_tokens", usage.PromptTokens,
		"response_tokens", usage.ResponseTokens,
		"total_tokens", usage.TotalTokens,
	)

	return &GenerateResponse{
		Content: sb.String(),
		Tokens:  usage,
	}, nil
}

// Close releases resources held by the VertexClient. The underlying GenAI
// client does not expose a Close method, so this is a no-op.
func (v *VertexClient) Close() error {
	return nil
}

// Stream opens a streaming chat completion against Vertex AI and returns a
// StreamIterator that yields StreamChunks as tokens arrive.
//
// Cancellation: the returned StreamIterator is single-goroutine (see the
// StreamIterator doc comment). The caller should NOT call Close from a
// different goroutine than Next. To cancel an in-flight stream, cancel the
// context passed here, which propagates to the underlying genai HTTP call
// and causes Next to return with an error (and auto-close the iterator).
func (v *VertexClient) Stream(ctx context.Context, req StreamRequest) (StreamIterator, error) { //nolint:gocritic // hugeParam: req must match Client interface signature.
	if err := validateStreamRequest(req.Messages); err != nil {
		return nil, err
	}

	contents := messagesToContents(req.Messages)

	cfg := &genai.GenerateContentConfig{}
	if req.Temperature != 0 {
		cfg.Temperature = &req.Temperature
	}
	if req.MaxTokens != 0 {
		cfg.MaxOutputTokens = req.MaxTokens
	}
	if req.SystemInstruction != "" {
		cfg.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: req.SystemInstruction}},
		}
	}
	if len(req.Tools) > 0 {
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: toGenaiFunctionDeclarations(req.Tools)}}
		cfg.ToolConfig = toGenaiToolConfig(req.ToolConfig)
	}

	v.logger.InfoContext(ctx, "llm stream open",
		"model", req.Model,
		"messages", len(req.Messages),
	)

	// GenerateContentStream returns a lazy iter.Seq2; the actual HTTP
	// request fires on the first Pull2 next() call, not here. The log
	// above is an "open" (request prepared) rather than "first token
	// received" event.
	seq := v.client.Models.GenerateContentStream(ctx, req.Model, contents, cfg)

	return newPullStreamAdapter(ctx, seq), nil
}

// pullStreamAdapter converts an iter.Seq2 of genai streaming responses into
// a StreamIterator. It uses iter.Pull2 internally so it does not need to
// manage a goroutine: Pull2's own cancellation primitive (the stop func)
// tears down the push iterator cleanly.
//
// Concurrency: see the StreamIterator doc comment. Next and Close must be
// called from the same goroutine. iter.Pull2 documents that calling next
// or stop from multiple goroutines is an error, and this adapter inherits
// that constraint.
//
// The adapter is intentionally dumb about context: cancellation is the
// caller's responsibility via the context they already passed to
// GenerateContentStream. When that context is cancelled, Vertex's HTTP
// call returns an error, the iter.Seq2 yields it, and Next returns it
// (auto-closing the adapter in the process).
type pullStreamAdapter struct {
	next   func() (*genai.GenerateContentResponse, error, bool)
	stop   func()
	closed bool
}

// newPullStreamAdapter wraps the given iter.Seq2. The ctx argument is
// accepted for symmetry with callers that already hold the streaming
// context, but note that iter.Pull2 does not observe it: cancellation is
// via Close or via the context that was already passed to the underlying
// GenerateContentStream call.
func newPullStreamAdapter(_ context.Context, seq iter.Seq2[*genai.GenerateContentResponse, error]) *pullStreamAdapter {
	next, stop := iter.Pull2(seq)
	return &pullStreamAdapter{next: next, stop: stop}
}

// Next reads the next response from the underlying iterator and converts it
// into a StreamChunk. Terminal usage metadata is surfaced as a StreamChunk
// with a non-nil Tokens field and (usually) an empty Text. io.EOF is
// returned when the underlying iterator completes cleanly. On any other
// error, the iterator auto-closes so subsequent Next calls return io.EOF
// deterministically.
//
// Vertex's Gemini streaming API yields exactly one candidate per response
// in practice, but the extraction loop below walks all candidates and all
// parts defensively so the adapter does not have to change if that ever
// changes.
func (a *pullStreamAdapter) Next() (StreamChunk, error) {
	if a.closed {
		return StreamChunk{}, io.EOF
	}
	resp, err, ok := a.next()
	if !ok {
		a.closed = true
		a.stop()
		return StreamChunk{}, io.EOF
	}
	if err != nil {
		a.closed = true
		a.stop()
		return StreamChunk{}, err
	}

	if resp == nil {
		return StreamChunk{}, nil
	}

	var chunk StreamChunk

	var sb strings.Builder
	var calls []ToolCallPart
	for _, cand := range resp.Candidates {
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			switch {
			case part.Text != "":
				sb.WriteString(part.Text)
			case part.FunctionCall != nil:
				calls = append(calls, ToolCallPart{
					Name: part.FunctionCall.Name,
					Args: part.FunctionCall.Args,
				})
			}
		}
	}
	chunk.Text = sb.String()
	chunk.ToolCalls = calls

	if m := resp.UsageMetadata; m != nil {
		chunk.Tokens = &TokenUsage{
			PromptTokens:   m.PromptTokenCount,
			ResponseTokens: m.CandidatesTokenCount,
			TotalTokens:    m.TotalTokenCount,
		}
	}

	return chunk, nil
}

// Close stops the underlying iter.Pull2 and marks the adapter closed.
// Safe to call multiple times.
func (a *pullStreamAdapter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	a.stop()
	return nil
}

// toGenaiFunctionDeclarations converts provider-neutral declarations to genai SDK types.
func toGenaiFunctionDeclarations(decls []*FunctionDeclaration) []*genai.FunctionDeclaration {
	if len(decls) == 0 {
		return nil
	}
	out := make([]*genai.FunctionDeclaration, len(decls))
	for i, d := range decls {
		out[i] = &genai.FunctionDeclaration{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  toGenaiSchema(d.Parameters),
		}
	}
	return out
}

// toGenaiSchema converts a provider-neutral Schema to a genai.Schema.
func toGenaiSchema(s *Schema) *genai.Schema {
	if s == nil {
		return nil
	}
	gs := &genai.Schema{
		Type:        toGenaiType(s.Type),
		Description: s.Description,
		Required:    s.Required,
		Items:       toGenaiSchema(s.Items),
		Enum:        s.Enum,
	}
	if len(s.Properties) > 0 {
		gs.Properties = make(map[string]*genai.Schema, len(s.Properties))
		for k, v := range s.Properties {
			gs.Properties[k] = toGenaiSchema(v)
		}
	}
	return gs
}

var schemaTypeMap = map[SchemaType]genai.Type{
	TypeObject:  genai.TypeObject,
	TypeString:  genai.TypeString,
	TypeInteger: genai.TypeInteger,
	TypeNumber:  genai.TypeNumber,
	TypeBoolean: genai.TypeBoolean,
	TypeArray:   genai.TypeArray,
}

// toGenaiType maps SchemaType to genai.Type. Case-insensitive: genai serializes
// as uppercase ("OBJECT") but llm.SchemaType uses lowercase ("object"). In-flight
// Temporal workflows may replay with old uppercase values deserialized into
// SchemaType, so we normalize before lookup.
func toGenaiType(t SchemaType) genai.Type {
	if gt, ok := schemaTypeMap[SchemaType(strings.ToLower(string(t)))]; ok {
		return gt
	}
	return genai.TypeUnspecified
}

// toGenaiToolConfig converts a provider-neutral ToolConfig to a genai.ToolConfig.
func toGenaiToolConfig(tc *ToolConfig) *genai.ToolConfig {
	if tc == nil {
		return nil
	}
	var mode genai.FunctionCallingConfigMode
	switch tc.Mode {
	case ToolModeAuto:
		mode = genai.FunctionCallingConfigModeAuto
	case ToolModeAny:
		mode = genai.FunctionCallingConfigModeAny
	case ToolModeNone:
		mode = genai.FunctionCallingConfigModeNone
	default:
		mode = genai.FunctionCallingConfigModeAuto
	}
	return &genai.ToolConfig{
		FunctionCallingConfig: &genai.FunctionCallingConfig{
			Mode: mode,
		},
	}
}
