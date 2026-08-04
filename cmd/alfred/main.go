// Command alfred is the main entry point for the alfred agentic AI ticket operations platform.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	autotask "github.com/tphakala/go-autotask"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	temporalactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/tphakala/alfred/internal/activity"
	"github.com/tphakala/alfred/internal/agent"
	"github.com/tphakala/alfred/internal/agent/runner"
	claudeRunner "github.com/tphakala/alfred/internal/agent/runner/claude"
	"github.com/tphakala/alfred/internal/agent/runner/copilot"
	gemini "github.com/tphakala/alfred/internal/agent/runner/gemini"
	"github.com/tphakala/alfred/internal/agent/tools"
	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/config"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/memory"
	"github.com/tphakala/alfred/internal/poller"
	"github.com/tphakala/alfred/internal/server"
	"github.com/tphakala/alfred/internal/store"
	alfredworkflow "github.com/tphakala/alfred/internal/workflow"
)

const (
	shutdownTimeout           = 10 * time.Second
	defaultStaleTurnThreshold = 10
)

var (
	cfgFile      string
	workflowsDir string
)

var rootCmd = &cobra.Command{
	Use:   "alfred",
	Short: "Alfred agentic AI ticket operations platform",
	RunE:  runServe,
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the Alfred server (default command)",
	RunE:  runServe,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "config.yaml", "path to configuration file")
	rootCmd.PersistentFlags().StringVar(&workflowsDir, "workflows", "workflows", "directory containing workflow definition YAML files")

	rootCmd.AddCommand(serveCmd)
}

func main() {
	// Set up a basic JSON logger early so config load errors are structured.
	earlyLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(earlyLogger)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// parseLogLevel maps a config string to an slog.Level.
func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// initLogger creates and sets the global logger from config.
func initLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLogLevel(cfg.Logging.Level)}
	var handler slog.Handler
	if cfg.Logging.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

// runServe contains the main application logic and returns an error on failure.
// All os.Exit calls are in main() to avoid exiting after deferred cleanup.
func runServe(_ *cobra.Command, _ []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config %q: %w", cfgFile, err)
	}

	logger := initLogger(cfg)

	slog.Info("alfred starting",
		"temporal_host", cfg.Temporal.Host,
		"temporal_namespace", cfg.Temporal.Namespace,
		"log_level", cfg.Logging.Level,
		"log_format", cfg.Logging.Format,
	)

	// Load workflow configurations from directory.
	workflows, err := config.LoadWorkflowsFromDir(workflowsDir)
	if err != nil {
		return fmt.Errorf("failed to load workflows from %q: %w", workflowsDir, err)
	}
	slog.Info("workflows loaded", "count", len(workflows))

	// Context with signal handling so all components shut down cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Create Autotask client (optional: skip if no credentials configured).
	var atClient *autotask.Client
	if cfg.Autotask.Username != "" {
		atClient, err = autotask.NewClient(ctx, autotask.AuthConfig{
			Username:        cfg.Autotask.Username,
			Secret:          cfg.Autotask.Password,
			IntegrationCode: cfg.Autotask.IntegrationCode,
		}, autotask.WithLogger(logger))
		if err != nil {
			return fmt.Errorf("failed to create autotask client: %w", err)
		}
		defer func() { _ = atClient.Close() }()
	} else {
		slog.Info("autotask client disabled (no credentials configured)")
	}

	// Create Temporal client.
	temporalClient, err := client.Dial(client.Options{
		HostPort:  cfg.Temporal.Host,
		Namespace: cfg.Temporal.Namespace,
		Logger:    newTemporalLogger(logger),
	})
	if err != nil {
		return fmt.Errorf("failed to create temporal client: %w", err)
	}
	defer temporalClient.Close()

	// Create LLM client based on configured provider.
	var llmClient llm.Client
	switch cfg.LLMProvider {
	case config.LLMProviderOpenRouter:
		if cfg.OpenRouter.APIKey == "" {
			slog.Info("openrouter client disabled (no api_key configured)")
		} else {
			llmClient, err = llm.NewOpenRouterClient(cfg.OpenRouter.APIKey, logger)
			if err != nil {
				return fmt.Errorf("failed to create openrouter client: %w", err)
			}
			defer func() { _ = llmClient.Close() }()
			slog.Info("llm provider: openrouter", "model", cfg.OpenRouter.Model)
		}
	default:
		if cfg.VertexAI.Project != "" {
			llmClient, err = llm.NewVertexClient(ctx,
				cfg.VertexAI.Project,
				cfg.VertexAI.Location,
				cfg.VertexAI.CredentialsFile,
				logger,
			)
			if err != nil {
				return fmt.Errorf("failed to create llm client: %w", err)
			}
			defer func() { _ = llmClient.Close() }()
			slog.Info("llm provider: vertex", "model", cfg.VertexAI.ChatModel)
		} else {
			slog.Info("vertex ai client disabled (no project configured)")
		}
	}

	// Resolve chat model from the selected provider's config.
	var chatModel string
	switch cfg.LLMProvider {
	case config.LLMProviderOpenRouter:
		chatModel = cfg.OpenRouter.Model
	default:
		chatModel = cfg.VertexAI.ChatModel
	}

	// Create memory client.
	memoryClient := memory.NewClient(cfg.Hindsight.URL, cfg.Hindsight.Bank)

	// Connect to PostgreSQL.
	dbStore, err := store.New(ctx, cfg.Database.URL, cfg.Database.MaxConns)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer dbStore.Close()

	if err := dbStore.Migrate(); err != nil {
		return fmt.Errorf("failed to run database migrations: %w", err)
	}
	slog.Info("database migrations complete")

	// Create activity structs.
	// Note: TicketWorkflow uses agent.Registry internally through
	// its activities; those are wired inside the activity package.
	var atActivities *activity.AutotaskActivities
	if atClient != nil {
		atActivities = activity.NewAutotaskActivities(atClient)
	}
	var llmActivities *activity.LLMActivities
	if llmClient != nil {
		llmActivities = activity.NewLLMActivities(llmClient)
	}
	memActivities := activity.NewMemoryActivities(memoryClient)

	// Create session event broker (used by chat activities to push SSE events).
	sessionBroker := server.NewSessionEventBroker(logger)

	// Build tool registry for chat workflows.
	chatRegistry := agent.NewRegistry(
		tools.NewRecallTool(memoryClient, logger),
		tools.NewApprovalTool(tools.NewPanicBroker(), logger),
	)

	// Create chat workflow activities (optional: requires LLM client).
	var chatActivities *alfredworkflow.ChatActivities
	if llmClient != nil {
		chatActivities = &alfredworkflow.ChatActivities{
			Store:             dbStore,
			CtxBuilder:        ctxbuild.NewTieredContextBuilder(defaultStaleTurnThreshold),
			SessionBroker:     &server.SessionBrokerAdapter{Broker: sessionBroker},
			Logger:            logger,
			LLM:               llmClient,
			ChatModel:         chatModel,
			Registry:          chatRegistry,
			Memory:            memoryClient,
			SystemPrompt:      cfg.Chat.SystemPrompt,
			AutoRecallEnabled: cfg.Chat.AutoRecallEnabled(),
		}
	}

	// Load the encrypted secrets store for Case Supervisor declared-command
	// tool env hydration (a DeclaredTool.Env value may be "${secret:NAME}").
	// A missing secrets file or an unavailable master key degrades to an
	// empty map rather than blocking startup: most declared tools need no
	// secret at all. For a tool that does need one, a missing entry is not a
	// soft per-call rejection the LLM can retry around: RunDeclaredCommand
	// returns a hard activity error and fails closed.
	caseSecrets := map[string]string{}
	if _, statErr := os.Stat(getSecretsPath()); statErr == nil {
		secretsStore, loadErr := loadExistingStore()
		if loadErr != nil {
			slog.Warn("case supervisor: secrets store unavailable, declared tools needing a secret will fail", "err", loadErr)
		} else {
			caseSecrets = secretsStore.AllSecrets()
		}
	}

	// Create Case Supervisor activities (optional: requires LLM client, same
	// gate as chat, since the supervisor loop is an LLM main loop too).
	var caseActivities *alfredworkflow.CaseActivities
	if llmClient != nil {
		// Effective pricing = built-in defaults overlaid with any config
		// entries, so a deployment only lists models the defaults miss or price
		// wrong. Meters a native LLM loop's own token spend against cost_cap_usd.
		pricing := llm.DefaultPriceTable()
		for model, p := range cfg.Pricing {
			pricing[model] = llm.ModelPrice{InputPerMillion: p.InputPerMillion, OutputPerMillion: p.OutputPerMillion}
		}
		caseActivities = &alfredworkflow.CaseActivities{
			Store:         dbStore,
			SessionBroker: &server.SessionBrokerAdapter{Broker: sessionBroker},
			Logger:        logger.With("component", "case_supervisor"),
			LLM:           llmClient,
			DefaultModel:  chatModel,
			Secrets:       caseSecrets,
			Pricing:       pricing,
		}
	}

	// Build the runner registry (Claude only for MVP).
	runners := runner.NewRegistry()
	runners.Register(claudeRunner.New(claudeRunner.Config{BinaryPath: cfg.Claude.BinaryPath}))

	if cfg.Gemini.Enabled {
		bin := cfg.Gemini.BinaryPath
		if bin == "" {
			bin = "gemini"
		}
		supported, probeErr := gemini.ProbeStreamJSON(bin)
		switch {
		case probeErr != nil:
			slog.Warn("gemini probe failed, runner not registered", "err", probeErr)
		case !supported:
			slog.Warn("gemini stream-json unsupported, runner not registered", "binary", bin)
		default:
			runners.Register(gemini.New(gemini.Config{BinaryPath: bin}))
			slog.Info("gemini runner registered", "binary", bin)
		}
	}

	if cfg.Copilot.Enabled {
		bin := cfg.Copilot.BinaryPath
		if bin == "" {
			bin = "copilot"
		}
		runners.Register(copilot.New(copilot.Config{BinaryPath: bin}))
		slog.Info("copilot runner registered", "binary", bin)
	}

	// Load AgentConfigs early so activities can hydrate MCP secrets.
	agentConfigsDir := filepath.Join("workflows", "agents")
	agentConfigs, loadErr := agentcfg.LoadFromDir(agentConfigsDir)
	if loadErr != nil {
		slog.Warn("agentcfg load", "err", loadErr)
	}

	// Fail fast on a misconfigured deployment: a queue_monitor task with a
	// supervisor block needs caseActivities (built above from llmClient) to
	// ever finish a case. Without this check, such a task dispatches cases
	// that die at their first CaseLLMStream activity instead of the process
	// refusing to start with a clear message.
	if llmClient == nil {
		for i := range agentConfigs {
			ac := &agentConfigs[i]
			sup := ac.Supervisor
			if ac.Strategy == agentcfg.StrategyQueueMonitor &&
				(len(sup.Tools) > 0 || len(sup.SubAgents) > 0 || sup.Model != "" || sup.Prompt.Base != "") {
				return fmt.Errorf("agent task %q is a queue_monitor with a supervisor block but no LLM provider is configured; the case supervisor requires an LLM", ac.Name)
			}
		}
	}

	// Create the PermitStore for per-run MCP approval tokens.
	permits := server.NewPermitStore()

	// Create the GuidanceStore for per-run mid-run guidance channels.
	guidance := server.NewGuidanceStore()

	// Build the EventSink for external agent runs.
	externalAgentSink := alfredworkflow.NewStoreEventSink(dbStore, &server.SessionBrokerAdapter{Broker: sessionBroker})

	externalAgentActivities := &alfredworkflow.ExternalAgentActivities{
		Logger:       logger.With("component", "external_agent"),
		Runners:      runners,
		EventSink:    externalAgentSink,
		Store:        dbStore,
		AgentConfigs: agentConfigs,
		Permits:      permits,
		PermAddress:  cfg.Server.Address,
		Guidance:     guidance,
	}

	// Activities for the queue monitor and the cases it dispatches: all
	// task_state ledger reads/writes go through here.
	monitorActivities := &alfredworkflow.MonitorActivities{
		Store: dbStore,
	}

	// Create and configure Temporal worker.
	w := worker.New(temporalClient, cfg.Temporal.TaskQueue, worker.Options{})
	w.RegisterWorkflow(alfredworkflow.ExternalAgentWorkflow)
	w.RegisterWorkflow(alfredworkflow.QueueMonitorWorkflow)
	w.RegisterWorkflow(alfredworkflow.CaseWorkflow)
	w.RegisterWorkflow(alfredworkflow.NativeSubAgentWorkflow)
	w.RegisterActivity(monitorActivities)
	if atActivities != nil {
		w.RegisterWorkflow(alfredworkflow.TicketWorkflow)
		w.RegisterActivity(atActivities)
	}
	if llmActivities != nil {
		w.RegisterActivity(llmActivities)
	}
	w.RegisterActivity(memActivities)
	if chatActivities != nil {
		w.RegisterWorkflow(alfredworkflow.ChatWorkflow)
		w.RegisterActivity(chatActivities)
		// DynamicToolActivity dispatches a tool call by whatever activity
		// name the workflow scheduled it under (the tool's own name), so it
		// must go through Temporal's real dynamic-activity API, not a named
		// registration with an empty Name (that derives the name from the
		// function itself and collides with the blanket RegisterActivity
		// scan above, since DynamicToolActivity is an exported ChatActivities
		// method too; see #116).
		w.RegisterDynamicActivity(
			chatActivities.DynamicToolActivity,
			temporalactivity.DynamicRegisterOptions{},
		)
	}
	if caseActivities != nil {
		w.RegisterActivity(caseActivities)
	}
	w.RegisterActivity(externalAgentActivities)

	// Dispatch function: starts a Temporal workflow for a matched ticket event.
	dispatch := func(event poller.DispatchEvent) error {
		workflowID := fmt.Sprintf("alfred-%s-Ticket-%d", event.WorkflowConfig.Name, event.TicketID)
		_, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID:                    workflowID,
			TaskQueue:             cfg.Temporal.TaskQueue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		}, alfredworkflow.TicketWorkflow, alfredworkflow.WorkflowInput{
			TicketID:       event.TicketID,
			TicketData:     event.TicketData,
			WorkflowConfig: event.WorkflowConfig,
		})
		return err
	}

	// Create poller (optional: requires Autotask client).
	var p *poller.Poller
	if atClient != nil {
		p = poller.New(atClient, workflows, dispatch)
	}

	// Start worker in a goroutine (non-blocking).
	if err := w.Start(); err != nil {
		return fmt.Errorf("failed to start temporal worker: %w", err)
	}
	defer w.Stop()
	slog.Info("temporal worker started", "task_queue", cfg.Temporal.TaskQueue)

	// Register Temporal Schedules from loaded AgentConfigs.
	for _, ac := range agentConfigs {
		if ac.Schedule.Cron == "" {
			continue
		}
		sid := agentcfg.ScheduleID(ac.Name)
		schedSpec := client.ScheduleSpec{
			CronExpressions: []string{ac.Schedule.Cron},
			Jitter:          ac.Schedule.RandomizedDelay,
		}
		schedAction := &client.ScheduleWorkflowAction{
			ID:        agentcfg.WorkflowID(ac.Name, "{{.ScheduledTime}}"),
			TaskQueue: cfg.Temporal.TaskQueue,
		}
		switch ac.Strategy {
		case agentcfg.StrategyQueueMonitor:
			schedAction.Workflow = alfredworkflow.QueueMonitorWorkflow
			schedAction.Args = []any{alfredworkflow.QueueMonitorInput{Config: agentcfg.StripMCPSecrets(ac), PromptDir: "."}}
		default:
			schedAction.Workflow = alfredworkflow.ExternalAgentWorkflow
			schedAction.Args = []any{alfredworkflow.ExternalAgentInput{Config: agentcfg.StripMCPSecrets(ac), PromptDir: "."}}
		}
		if ac.Budgets.WorkflowMaxWallClock > 0 {
			schedAction.WorkflowExecutionTimeout = ac.Budgets.WorkflowMaxWallClock
		}

		_, err := temporalClient.ScheduleClient().Create(ctx, client.ScheduleOptions{
			ID:      sid,
			Spec:    schedSpec,
			Action:  schedAction,
			Overlap: overlapPolicyFromConfig(ac.Concurrency.OnConflict),
		})
		if err != nil {
			if _, ok := errors.AsType[*serviceerror.AlreadyExists](err); ok {
				handle := temporalClient.ScheduleClient().GetHandle(ctx, sid)
				uErr := handle.Update(ctx, client.ScheduleUpdateOptions{
					DoUpdate: func(input client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
						input.Description.Schedule.Spec = &schedSpec
						input.Description.Schedule.Action = schedAction
						return &client.ScheduleUpdate{Schedule: &input.Description.Schedule}, nil
					},
				})
				if uErr != nil {
					slog.Error("schedule update failed", "name", ac.Name, "err", uErr)
					continue
				}
			} else {
				slog.Error("schedule create failed", "name", ac.Name, "err", err)
				continue
			}
		}
		slog.Info("schedule registered", "name", ac.Name, "cron", ac.Schedule.Cron)
	}

	// Create SSE broker for global events (workflow/poller).
	broker := server.NewSSEBroker(slog.Default())

	// Create and start HTTP server.
	srv := server.New(cfg.Server.Address, cfg.Server.APIKey, &server.Deps{
		Broker:            broker,
		SessionBroker:     sessionBroker,
		Poller:            p,
		TemporalClient:    temporalClient,
		WorkflowsDir:      workflowsDir,
		ChatTaskQueue:     cfg.Temporal.TaskQueue,
		Store:             dbStore,
		AutoRecallEnabled: cfg.Chat.AutoRecallEnabled(),
		AgentConfigs:      agentConfigs,
		AgentTaskQueue:    cfg.Temporal.TaskQueue,
		Permits:           permits,
		ApprovalNotifier:  newTemporalApprovalNotifier(temporalClient, dbStore),
		GuidanceStore:     guidance,
		WorkflowUpdater:   newTemporalWorkflowUpdater(temporalClient, dbStore),
		RunnerCaps:        newRunnerCapsAdapter(),
		CORSOrigins:       cfg.Server.CORS.AllowedOrigins,
		ReadinessProbes: []server.ReadinessProbe{
			{Name: "database", Check: dbStore.Ping},
			{Name: "temporal", Check: func(ctx context.Context) error {
				_, err := temporalClient.CheckHealth(ctx, &client.CheckHealthRequest{})
				return err
			}},
		},
	}, logger)

	if cfg.Server.Address != "" {
		go func() {
			if err := srv.Start(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("http server error", "error", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				slog.Error("http server shutdown error", "error", err)
			}
		}()
		slog.Info("http server started", "address", cfg.Server.Address)
	} else {
		slog.Info("http server disabled (no server.address configured)")
	}

	// Run poller (blocks) or wait for signal if no poller is configured.
	if p != nil {
		if err := p.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("poller exited with error: %w", err)
		}
	} else {
		slog.Info("poller disabled (no autotask credentials), waiting for signal")
		<-ctx.Done()
	}

	slog.Info("alfred shutting down")
	return nil
}

// temporalLogger adapts *slog.Logger to Temporal's log.Logger interface.
type temporalLogger struct {
	logger *slog.Logger
}

func newTemporalLogger(l *slog.Logger) *temporalLogger {
	return &temporalLogger{logger: l}
}

func (t *temporalLogger) Debug(msg string, keyvals ...any) { t.logger.Debug(msg, keyvals...) }
func (t *temporalLogger) Info(msg string, keyvals ...any)  { t.logger.Info(msg, keyvals...) }
func (t *temporalLogger) Warn(msg string, keyvals ...any)  { t.logger.Warn(msg, keyvals...) }
func (t *temporalLogger) Error(msg string, keyvals ...any) { t.logger.Error(msg, keyvals...) }

func overlapPolicyFromConfig(s string) enums.ScheduleOverlapPolicy {
	switch s {
	case "queue":
		return enums.SCHEDULE_OVERLAP_POLICY_BUFFER_ONE
	case "replace_oldest":
		return enums.SCHEDULE_OVERLAP_POLICY_TERMINATE_OTHER
	default:
		return enums.SCHEDULE_OVERLAP_POLICY_SKIP
	}
}

// sessionResolver maps a runID (session UUID string) to the Temporal workflow
// execution that owns it. Shared by temporalWorkflowUpdater and
// temporalApprovalNotifier to eliminate duplicate UUID-parse + DB-lookup code.
type sessionResolver struct {
	store *store.Store
}

func (r *sessionResolver) resolve(ctx context.Context, runID string) (*store.Session, error) {
	parsed, err := uuid.Parse(runID)
	if err != nil {
		return nil, fmt.Errorf("invalid run_id: %w", err)
	}
	sess, err := r.store.GetSession(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("lookup session: %w", err)
	}
	return sess, nil
}

// temporalWorkflowUpdater dispatches Temporal workflow updates from REST
// handlers.
type temporalWorkflowUpdater struct {
	sessionResolver
	client client.Client
}

func newTemporalWorkflowUpdater(c client.Client, s *store.Store) server.WorkflowUpdater {
	return &temporalWorkflowUpdater{sessionResolver: sessionResolver{store: s}, client: c}
}

// InjectGuidance issues the inject_guidance update against the workflow that
// owns this runID.
func (u *temporalWorkflowUpdater) InjectGuidance(ctx context.Context, runID string, args server.GuidanceUpdateArgs) error {
	sess, err := u.resolve(ctx, runID)
	if err != nil {
		return err
	}
	handle, err := u.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   sess.WorkflowID,
		UpdateName:   alfredworkflow.ExternalAgentInjectGuidanceUpdate,
		WaitForStage: client.WorkflowUpdateStageAccepted,
		Args: []any{alfredworkflow.GuidanceRequest{
			Text:                          args.Text,
			RunnerCapsBidirectionalStream: args.RunnerCapsBidirectionalStream,
		}},
	})
	if err != nil {
		return fmt.Errorf("update workflow: %w", err)
	}
	// Wait for the update to complete (handler return) so a typed error like
	// GuidanceUnsupportedError reaches the REST handler. The workflow returns
	// *GuidanceUnsupportedError which Temporal wraps as ApplicationError;
	// translate it to the server-level sentinel so handlePostGuidance can
	// surface 409 without importing the workflow package.
	if err := handle.Get(ctx, nil); err != nil {
		if alfredworkflow.IsGuidanceUnsupportedError(err) {
			return fmt.Errorf("%w: %v", server.ErrGuidanceUnsupported, err)
		}
		return err
	}
	return nil
}

// temporalApprovalNotifier sends approval state changes to the Temporal
// workflow history.
type temporalApprovalNotifier struct {
	sessionResolver
	client client.Client
}

func newTemporalApprovalNotifier(c client.Client, s *store.Store) server.ApprovalNotifier {
	return &temporalApprovalNotifier{sessionResolver: sessionResolver{store: s}, client: c}
}

func (n *temporalApprovalNotifier) NotifyApprovalPending(ctx context.Context, runID, callID, toolName string, input json.RawMessage) error {
	sess, err := n.resolve(ctx, runID)
	if err != nil {
		return err
	}
	return n.client.SignalWorkflow(ctx, sess.WorkflowID, sess.RunID, alfredworkflow.ExternalAgentApprovalPendingSignal, alfredworkflow.PermissionRequest{
		RunID:    runID,
		CallID:   callID,
		ToolName: toolName,
		Input:    input,
	})
}

func (n *temporalApprovalNotifier) NotifyApprovalResolved(ctx context.Context, runID, callID, decision, reason string) error {
	sess, err := n.resolve(ctx, runID)
	if err != nil {
		return err
	}
	handle, err := n.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   sess.WorkflowID,
		UpdateName:   alfredworkflow.ExternalAgentResolveApprovalUpdate,
		WaitForStage: client.WorkflowUpdateStageAccepted,
		Args:         []any{alfredworkflow.PermissionResolution{CallID: callID, Decision: decision, Reason: reason}},
	})
	if err != nil {
		return fmt.Errorf("update workflow: %w", err)
	}
	return handle.Get(ctx, nil)
}

// runnerCapsAdapter implements server.RunnerCaps. For MVP, BidirectionalStream
// always returns true: the activity registers a GuidanceStore channel ONLY for
// runners whose Capabilities.BidirectionalStream is true (see ExecAgentCLI),
// and the REST handler's 404 check on GuidanceStore.Receiver therefore acts as
// the effective capability gate. The interface itself is preserved so a future
// multi-process deployment can swap in a real check (e.g. by resolving the
// session's AgentConfig and re-querying the runner registry).
type runnerCapsAdapter struct{}

func newRunnerCapsAdapter() server.RunnerCaps {
	return &runnerCapsAdapter{}
}

func (*runnerCapsAdapter) BidirectionalStream(_, _ string) bool {
	return true
}
