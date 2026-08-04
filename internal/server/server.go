package server

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/poller"
	"github.com/tphakala/alfred/internal/store"
	"go.temporal.io/sdk/client"
)

const (
	readHeaderTimeout = 5 * time.Second

	// configFilePermission is the file mode for workflow config files written by the API.
	configFilePermission = 0o600

	// maxRequestBodyBytes limits the size of incoming request bodies (1 MiB).
	maxRequestBodyBytes = 1 << 20
)

// sseReplacer strips CR/LF from SSE event type fields to prevent CRLF injection.
var sseReplacer = strings.NewReplacer("\n", "", "\r", "")

// setSSEHeaders sets the standard headers for an SSE response. Must be called
// before the first Write to ensure headers are sent.
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// ReadinessProbe is a single named dependency health check run by GET /readyz.
type ReadinessProbe struct {
	Name  string
	Check func(ctx context.Context) error
}

// Deps holds the dependencies injected into the HTTP server.
type Deps struct {
	Broker            *SSEBroker
	SessionBroker     *SessionEventBroker
	Poller            *poller.Poller
	TemporalClient    client.Client
	WorkflowsDir      string
	ChatTaskQueue     string // Temporal task queue for chat workflows. Defaults to the main worker queue.
	Store             *store.Store
	AutoRecallEnabled bool                   // Passed through to ChatWorkflowParams on session creation.
	AgentConfigs      []agentcfg.AgentConfig // Loaded from workflows/agents/.
	AgentTaskQueue    string                 // Temporal task queue for agent tasks.
	Permits           *PermitStore           // Per-run approval tokens and channels.
	ApprovalNotifier  ApprovalNotifier       // Sends approval state to Temporal history (best-effort).
	GuidanceStore     *GuidanceStore         // Per-run guidance channels for mid-run operator input.
	WorkflowUpdater   WorkflowUpdater        // Dispatches Temporal workflow updates from REST handlers.
	RunnerCaps        RunnerCaps             // Answers capability questions about a run's runner.
	CORSOrigins       []string               // Allowed CORS origins; empty disables CORS.
	ReadinessProbes   []ReadinessProbe       // Dependency health checks for GET /readyz; empty means always ready.
}

// Server is the Alfred HTTP server.
type Server struct {
	httpServer *http.Server
	deps       Deps
	apiKey     string
	logger     *slog.Logger
	tickets    *ticketStore
	// configMu serializes workflow-config file writes so the PUT handler's
	// If-Match read-check-write is one critical section (guards the ETag TOCTOU).
	configMu sync.Mutex
}

// New creates a new Server.
func New(address, apiKey string, deps *Deps, logger *slog.Logger) *Server {
	s := &Server{
		deps:    *deps,
		apiKey:  apiKey,
		logger:  logger,
		tickets: newTicketStore(sseTicketTTL),
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	return s
}

// Handler returns the server's HTTP handler for use in tests.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Start begins listening. It blocks until the server is shut down.
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	s.logger.Info("http server listening", "address", ln.Addr().String())
	return s.httpServer.Serve(ln)
}

// Shutdown gracefully stops the server and the SSE brokers.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.deps.Broker != nil {
		s.deps.Broker.Shutdown()
	}
	if s.deps.SessionBroker != nil {
		s.deps.SessionBroker.Shutdown()
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// MCP permission bridge (uses per-run bearer token, not API key)
	if s.deps.Permits != nil {
		mcpHandler := NewMCPPermHandler(s.deps.Permits, s.deps.ApprovalNotifier, s.logger)
		mux.HandleFunc("POST /mcp/perm/{run_id}", mcpHandler.ServeHTTP)
	}

	s.registerV1Routes(mux)

	// Headless root index: no embedded frontend.
	// Note: registering as "/" (no method qualifier) avoids a Go 1.22 ServeMux
	// conflict with "/api/" which is method-agnostic. Method filtering is done
	// inside the handler.
	mux.HandleFunc("/", s.handleRootIndex)
}

// AuthMiddleware checks the Authorization header for a valid Bearer token.
// If apiKey is empty, auth is disabled and all requests pass through.
func AuthMiddleware(apiKey string, next http.Handler) http.Handler {
	if apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" || token == auth || subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
			writeProblem(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	})
}
