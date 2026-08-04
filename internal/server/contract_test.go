package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/server"
	"github.com/tphakala/alfred/internal/store"
)

// specPath is the OpenAPI document validated against, relative to this package.
const specPath = "../../api/openapi.yaml"

// loadContractRouter loads and validates the OpenAPI spec, then builds a
// gorillamux router from it. The spec's servers.url is /api/v1, so the router
// matches request URLs that include the /api/v1 prefix.
func loadContractRouter(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(specPath)
	if err != nil {
		t.Fatalf("load openapi spec: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate openapi spec: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	return doc, router
}

// validateResponse routes req against the spec and asserts that the captured
// response conforms to the operation's declared schema for that status. A
// non-nil validation error fails the test and is reported as schema drift.
func validateResponse(
	t *testing.T,
	router routers.Router,
	req *http.Request,
	rr *httptest.ResponseRecorder,
) {
	t.Helper()
	route, pathParams, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("find route for %s %s: %v", req.Method, req.URL.Path, err)
	}
	reqInput := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
	}
	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 rr.Code,
		Header:                 rr.Header(),
		// IncludeResponseStatus makes the validator fail when the status is not
		// declared for the operation. The default is false, which silently
		// passes undeclared statuses; enabling it strengthens the anti-drift
		// guard for future cases.
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	respInput.SetBodyBytes(rr.Body.Bytes())
	if err := openapi3filter.ValidateResponse(t.Context(), respInput); err != nil {
		t.Fatalf("response for %s %s (status %d) does not conform to spec: %v\nbody: %s",
			req.Method, req.URL.Path, rr.Code, err, rr.Body.String())
	}
}

// contractRequest builds an authenticated request for the contract router. The
// host is set to example.com (httptest default) and the path carries the full
// /api/v1 prefix so the gorillamux router resolves the operation.
func contractRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	if body == "" {
		req := httptest.NewRequestWithContext(t.Context(), method, "http://example.com"+path, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		return req
	}
	req := httptest.NewRequestWithContext(t.Context(), method, "http://example.com"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", contentTypeJSON)
	return req
}

// TestContractConformance validates representative handler responses against
// the OpenAPI contract without a database or Temporal. Each case reaches a
// status declared in the spec for that operation.
func TestContractConformance(t *testing.T) {
	_, router := loadContractRouter(t)

	t.Run("createSSETicket_201", func(t *testing.T) {
		srv := newTestServer(t, &server.Deps{})
		req := contractRequest(t, http.MethodPost, "/api/v1/auth/sse-tickets", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("createRun_400", func(t *testing.T) {
		srv := newTestServer(t, &server.Deps{})
		req := contractRequest(t, http.MethodPost, "/api/v1/runs", `{"strategy":"fanout"}`)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("getAuthInfo_200", func(t *testing.T) {
		srv := newTestServer(t, &server.Deps{})
		// auth-info needs no bearer, but sending one is harmless.
		req := contractRequest(t, http.MethodGet, "/api/v1/auth-info", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("listAgentTasks_200", func(t *testing.T) {
		cfgs := []agentcfg.AgentConfig{{
			Name:     "t",
			Strategy: "fanout",
			Runner:   "claude",
			Schedule: agentcfg.Schedule{Cron: "0 * * * *"},
		}}
		srv := newTestServer(t, &server.Deps{AgentConfigs: cfgs})
		req := contractRequest(t, http.MethodGet, "/api/v1/agent-tasks", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("listWorkflowConfigs_200", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte(sampleWorkflowYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		srv := newTestServer(t, &server.Deps{WorkflowsDir: dir})
		req := contractRequest(t, http.MethodGet, "/api/v1/workflow-configs", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("getAgentTask_404", func(t *testing.T) {
		cfgs := []agentcfg.AgentConfig{{
			Name:     "t",
			Strategy: "fanout",
			Runner:   "claude",
			Schedule: agentcfg.Schedule{Cron: "0 * * * *"},
		}}
		srv := newTestServer(t, &server.Deps{AgentConfigs: cfgs})
		req := contractRequest(t, http.MethodGet, "/api/v1/agent-tasks/nope", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})
}

// newContractStore spins up a throwaway Postgres via testcontainers and returns
// a migrated store. It mirrors newTestStore in internal/store/store_test.go.
func newContractStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := t.Context()

	pgC, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("alfred_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	// t.Context() is already canceled by the time cleanups run, so a
	// Terminate call using it would fail silently and leak the container.
	// context.WithoutCancel keeps the deadline-free parent values without
	// inheriting the cancellation.
	t.Cleanup(func() { _ = pgC.Terminate(context.WithoutCancel(t.Context())) }) //nolint:errcheck // container teardown error is non-actionable in tests

	connStr, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	s, err := store.New(ctx, connStr, 5)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(s.Close)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

// TestContractConformanceRuns validates the runs collection responses against
// the OpenAPI contract using a real store. It is Docker/Podman-gated with the
// same testing.Short() guard used by internal/store/store_test.go.
func TestContractConformanceRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	_, router := loadContractRouter(t)
	realStore := newContractStore(t)
	srv := newTestServer(t, &server.Deps{Store: realStore})

	t.Run("listRuns_200_empty", func(t *testing.T) {
		req := contractRequest(t, http.MethodGet, "/api/v1/runs", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("getRun_404", func(t *testing.T) {
		req := contractRequest(t, http.MethodGet, "/api/v1/runs/"+uuid.NewString(), "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("listTaskState_200_empty", func(t *testing.T) {
		req := contractRequest(t, http.MethodGet, "/api/v1/agent-tasks/contract-empty-task/state", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("listTaskState_200_withData", func(t *testing.T) {
		task := "contract-task-" + uuid.NewString()
		if _, err := realStore.UpsertClaim(t.Context(), task, "candidate-1", "run-1"); err != nil {
			t.Fatalf("seed claim: %v", err)
		}

		req := contractRequest(t, http.MethodGet, "/api/v1/agent-tasks/"+task+"/state", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("listTaskState_200_withNonObjectOutcome", func(t *testing.T) {
		// outcome is opaque JSON the engine never interprets, and must round-trip
		// for any valid JSON value, not just objects. Seed a non-object outcome
		// (a JSON array) to verify it both survives the DAL and validates against
		// the OpenAPI response schema.
		task := "contract-outcome-" + uuid.NewString()
		if _, err := realStore.UpsertClaim(t.Context(), task, "candidate-1", "run-1"); err != nil {
			t.Fatalf("seed claim: %v", err)
		}
		if err := realStore.FinalizeOutcome(t.Context(), task, "candidate-1", store.TaskStatusDone,
			json.RawMessage(`[1,2,3]`)); err != nil {
			t.Fatalf("finalize outcome: %v", err)
		}

		req := contractRequest(t, http.MethodGet, "/api/v1/agent-tasks/"+task+"/state", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
		if !strings.Contains(rr.Body.String(), `"outcome":[1,2,3]`) {
			t.Errorf("body does not contain the raw outcome array untouched: %s", rr.Body.String())
		}
	})

	t.Run("requeueTaskState_404", func(t *testing.T) {
		req := contractRequest(t, http.MethodPost, "/api/v1/agent-tasks/contract-empty-task/state/no-such-key/requeue", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})

	t.Run("requeueTaskState_204", func(t *testing.T) {
		task := "contract-requeue-" + uuid.NewString()
		if _, err := realStore.UpsertClaim(t.Context(), task, "candidate-1", "run-1"); err != nil {
			t.Fatalf("seed claim: %v", err)
		}

		req := contractRequest(t, http.MethodPost, "/api/v1/agent-tasks/"+task+"/state/candidate-1/requeue", "")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusNoContent, rr.Body.String())
		}
		validateResponse(t, router, req, rr)
	})
}
