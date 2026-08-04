package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/config"
	"github.com/tphakala/alfred/internal/fsutil"
	"github.com/tphakala/alfred/internal/server/apiv1gen"
	"github.com/tphakala/alfred/internal/store"
	wf "github.com/tphakala/alfred/internal/workflow"
	"gopkg.in/yaml.v3"
)

// configETag computes a strong ETag (RFC 9110) for the given file bytes.
func configETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// safeConfigName rejects names that would escape WorkflowsDir or produce a
// broken config file. Whitespace-only names are rejected too: they pass the
// base/dot checks but persist a " .yaml" whose whitespace-only internal name is
// rejected by LoadWorkflow on the next boot, failing the all-or-nothing load
// and bricking startup (the same class the PUT name-lockstep guard closes).
func safeConfigName(name string) bool {
	return strings.TrimSpace(name) != "" && filepath.Base(name) == name && name != "." && name != ".."
}

// handleListWorkflowConfigs handles GET /api/v1/workflow-configs.
func (s *Server) handleListWorkflowConfigs(w http.ResponseWriter, _ *http.Request) {
	items := make([]apiv1gen.WorkflowConfig, 0)

	if s.deps.WorkflowsDir == "" {
		w.Header().Set("Content-Type", "application/json")
		if err := writeJSON(w, apiv1gen.WorkflowConfigList{Items: items}); err != nil {
			s.logger.Error("failed to encode workflow config list", "error", err)
		}
		return
	}

	entries, err := os.ReadDir(s.deps.WorkflowsDir)
	if err != nil {
		s.logger.Error("failed to read workflows directory", "dir", s.deps.WorkflowsDir, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to read workflows directory")
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		//nolint:gosec // name is validated by safeConfigName, path is confined to WorkflowsDir
		data, readErr := os.ReadFile(filepath.Join(s.deps.WorkflowsDir, entry.Name()))
		if readErr != nil {
			s.logger.Error("failed to read workflow config file", "file", entry.Name(), "error", readErr)
			continue
		}
		items = append(items, apiv1gen.WorkflowConfig{
			Name: name,
			Etag: ptr(configETag(data)),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, apiv1gen.WorkflowConfigList{Items: items}); err != nil {
		s.logger.Error("failed to encode workflow config list", "error", err)
	}
}

// handleGetWorkflowConfig handles GET /api/v1/workflow-configs/{name}.
func (s *Server) handleGetWorkflowConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !safeConfigName(name) {
		writeProblem(w, http.StatusBadRequest, "invalid workflow name")
		return
	}

	//nolint:gosec // name is validated by safeConfigName, path is confined to WorkflowsDir
	path := filepath.Join(s.deps.WorkflowsDir, name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeProblem(w, http.StatusNotFound, "workflow config not found")
			return
		}
		s.logger.ErrorContext(r.Context(), "failed to read workflow config", "path", path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to read workflow config")
		return
	}

	etag := configETag(data)
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, apiv1gen.WorkflowConfig{
		Name:    name,
		Content: ptr(string(data)),
		Etag:    ptr(etag),
	}); err != nil {
		s.logger.ErrorContext(r.Context(), "failed to encode workflow config", "error", err)
	}
}

// handlePutWorkflowConfig handles PUT /api/v1/workflow-configs/{name}.
func (s *Server) handlePutWorkflowConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !safeConfigName(name) {
		writeProblem(w, http.StatusBadRequest, "invalid workflow name")
		return
	}

	//nolint:gosec // name is validated by safeConfigName, path is confined to WorkflowsDir
	path := filepath.Join(s.deps.WorkflowsDir, name+".yaml")
	ifMatch := r.Header.Get("If-Match")

	// Parse and validate the body BEFORE taking the write lock: none of this
	// touches shared state, so keeping it outside the lock avoids holding the
	// process-wide config mutex across a (possibly slow) network body read.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req apiv1gen.WorkflowConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Content == nil || *req.Content == "" {
		writeProblem(w, http.StatusBadRequest, "content is required")
		return
	}

	var cfg config.WorkflowConfig
	if err := yaml.Unmarshal([]byte(*req.Content), &cfg); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid workflow yaml")
		return
	}

	// Keep the filename and the YAML body's internal name in lockstep. The file
	// is written as <name>.yaml but the loader keys dispatch on the body's
	// name:; a mismatch lets a valid-looking write manufacture a config that the
	// LoadWorkflowsFromDir dup-name guard rejects on the next restart, bricking
	// boot. Reject the write instead so the API cannot create that state.
	if cfg.Name != name {
		writeProblem(w, http.StatusBadRequest, "workflow name in body must match the URL name")
		return
	}

	// Serialize the disk read-check-write against concurrent PUTs to the same
	// name: two racing PUTs with the same If-Match could otherwise both read the
	// stale etag, both pass the precondition, and clobber each other. Config
	// writes are rare admin operations, so a single process-wide lock is fine.
	s.configMu.Lock()
	defer s.configMu.Unlock()

	existing, statErr := os.ReadFile(path)
	fileExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		s.logger.ErrorContext(r.Context(), "failed to stat workflow config", "path", path, "error", statErr)
		writeProblem(w, http.StatusInternalServerError, "failed to read workflow config")
		return
	}

	if fileExists {
		current := configETag(existing)
		// A missing If-Match on an existing resource is also a 412: require the
		// client to send the etag. Treat ifMatch=="" as a mismatch.
		if ifMatch != "*" && ifMatch != current {
			writeProblem(w, http.StatusPreconditionFailed, "etag precondition failed")
			return
		}
	} else if ifMatch != "" {
		// For a new resource, any If-Match value (including "*") requires
		// existence, so it must be rejected.
		writeProblem(w, http.StatusPreconditionFailed, "etag precondition failed")
		return
	}

	if err := fsutil.WriteFileAtomic(path, []byte(*req.Content), configFilePermission); err != nil {
		s.logger.ErrorContext(r.Context(), "failed to write workflow config", "path", path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to write workflow config")
		return
	}

	newEtag := configETag([]byte(*req.Content))
	w.Header().Set("ETag", newEtag)
	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, apiv1gen.WorkflowConfig{
		Name:    name,
		Content: req.Content,
		Etag:    ptr(newEtag),
	}); err != nil {
		s.logger.ErrorContext(r.Context(), "failed to encode workflow config response", "error", err)
	}
}

// ptrIfNotEmpty returns a pointer to s when s is non-empty, and nil otherwise.
// It is used to omit empty optional string fields from JSON output.
func ptrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return ptr(s)
}

// agentTaskFromConfig converts an AgentConfig to its API representation.
// Pointer fields are only set when the corresponding source string is non-empty.
func agentTaskFromConfig(c *agentcfg.AgentConfig) apiv1gen.AgentTask {
	return apiv1gen.AgentTask{
		Name:     c.Name,
		Runner:   ptrIfNotEmpty(c.Runner),
		Schedule: ptrIfNotEmpty(c.Schedule.Cron),
		Strategy: ptrIfNotEmpty(c.Strategy),
	}
}

// findAgentConfig searches s.deps.AgentConfigs by name and returns a pointer
// to the matching element, or nil if none is found. It iterates by index to
// avoid taking the address of a range-copy loop variable.
func (s *Server) findAgentConfig(name string) *agentcfg.AgentConfig {
	for i := range s.deps.AgentConfigs {
		if s.deps.AgentConfigs[i].Name == name {
			return &s.deps.AgentConfigs[i]
		}
	}
	return nil
}

// handleListAgentTasksV1 handles GET /api/v1/agent-tasks.
func (s *Server) handleListAgentTasksV1(w http.ResponseWriter, _ *http.Request) {
	items := make([]apiv1gen.AgentTask, 0, len(s.deps.AgentConfigs))
	for i := range s.deps.AgentConfigs {
		items = append(items, agentTaskFromConfig(&s.deps.AgentConfigs[i]))
	}
	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, apiv1gen.AgentTaskList{Items: items}); err != nil {
		s.logger.Error("failed to encode agent task list", "error", err)
	}
}

// handleGetAgentTaskV1 handles GET /api/v1/agent-tasks/{name}.
func (s *Server) handleGetAgentTaskV1(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := s.findAgentConfig(name)
	if cfg == nil {
		writeProblem(w, http.StatusNotFound, "agent task not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, agentTaskFromConfig(cfg)); err != nil {
		s.logger.ErrorContext(r.Context(), "failed to encode agent task", "error", err)
	}
}

// handleRunAgentTaskV1 handles POST /api/v1/agent-tasks/{name}/run.
func (s *Server) handleRunAgentTaskV1(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := s.findAgentConfig(name)
	if cfg == nil {
		writeProblem(w, http.StatusNotFound, "agent task not found")
		return
	}

	if s.deps.TemporalClient == nil {
		writeProblem(w, http.StatusServiceUnavailable, "temporal client not available")
		return
	}

	if cfg.Strategy == agentcfg.StrategyQueueMonitor {
		s.handleTriggerQueueMonitorV1(w, r, cfg)
		return
	}

	sessionID := uuid.New()
	wfID := agentcfg.WorkflowID(cfg.Name, sessionID.String())
	taskQueue := s.deps.AgentTaskQueue
	if taskQueue == "" {
		taskQueue = s.deps.ChatTaskQueue
	}

	_, err := s.deps.TemporalClient.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:        wfID,
		TaskQueue: taskQueue,
	}, wf.ExternalAgentWorkflow, wf.ExternalAgentInput{
		SessionID: sessionID,
		Config:    agentcfg.StripMCPSecrets(*cfg),
		PromptDir: ".",
	})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "failed to start agent task workflow",
			"name", name, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to start agent task")
		return
	}

	actions := supportedActionsForKind(store.SessionKindAgentTask)
	run := apiv1gen.Run{
		Id:               sessionID,
		Agent:            ptr("agent-tasks/" + name),
		Kind:             store.SessionKindAgentTask,
		Strategy:         apiv1gen.RunStrategyFanout,
		Status:           store.SessionStatusActive,
		SupportedActions: &actions,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := writeJSON(w, run); err != nil {
		s.logger.ErrorContext(r.Context(), "failed to encode run response", "error", err)
	}
}

// handleTriggerQueueMonitorV1 triggers an out-of-cadence pass of a
// queue_monitor task's Schedule via the Schedule's native trigger-now API,
// rather than starting a bespoke workflow execution, so a manual trigger and
// a scheduled pass can never race each other. A monitor pass is not a single
// run (it may dispatch many cases), so this returns 202 Accepted instead of
// synthesizing a run resource.
func (s *Server) handleTriggerQueueMonitorV1(w http.ResponseWriter, r *http.Request, cfg *agentcfg.AgentConfig) {
	handle := s.deps.TemporalClient.ScheduleClient().GetHandle(r.Context(), agentcfg.ScheduleID(cfg.Name))
	if err := handle.Trigger(r.Context(), client.ScheduleTriggerOptions{}); err != nil {
		s.logger.ErrorContext(r.Context(), "failed to trigger queue monitor schedule",
			"name", cfg.Name, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to trigger queue monitor")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
