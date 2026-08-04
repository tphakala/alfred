# CLAUDE.md

Project orientation for Claude Code working on Alfred. Auto-loaded on session start. Pair this with the README (user-facing overview) and the design specs under `docs/superpowers/specs/`.

## What Alfred Is

Alfred is a Go-based agentic AI platform for IT ticket operations. It autonomously triages Autotask tickets through YAML-defined workflows and provides a human-in-the-loop chat agent for interactive work. The defining architectural choice is that **Temporal workflows ARE the agent loops**, not just a job runner around them. Durable execution, query handlers, update handlers, and per-tool activity visibility are first-class.

**Stack:** Go 1.26, Temporal (workflow engine), Google Vertex AI (Gemini) or OpenRouter (multi-provider LLM abstraction), PostgreSQL 17, Hindsight (long-term memory), Autotask (ticketing). Headless: no embedded frontend. A separate Svelte 5 client consumes the versioned `/api/v1` REST surface (OpenAPI 3.1 contract at `api/openapi.yaml`).

**Owner:** Tomi P. Hakala (`tphakala`). Private repo: `github.com/tphakala/alfred`.

## Two Workflow Types, Deliberately Different

### TicketWorkflow (`internal/workflow/engine.go`)
Generic YAML-interpreted step DAG. Each YAML in `workflows/` defines `analyze` / `act` / `recall_similar` steps. Deterministic; LLM non-determinism is contained to a single `Analyze` activity per analyze step. A single workflow-level approval gate (24h timeout) sits before the step loop, NOT per-step. Currently only one workflow YAML exists: `workflows/triage.yaml`.

Action types implemented: `add_note`, `set_field`, `assign_queue` (which is `set_field` on `queueID`). Unknown action types log a warning and are skipped.

### ChatWorkflow (`internal/workflow/chat.go`)
Long-lived, code-driven ReAct loop. Up to 10 rounds per turn (`maxAgentRounds`). Three update handlers (`send_message`, `retry_message`, `approve_action`) and three query handlers (`state`, `summary`, `approvals`). SSE streaming to API clients. Auto-recall on the first turn of epoch 1 when enabled. Idle timeout 30 min closes the workflow between turns (different from epoch transition); a mid-turn expiry re-arms the timer instead of closing, and approval waits are separately bounded by `DefaultApprovalTimeout` (24h, auto-reject on expiry).

The future spec in `docs/superpowers/specs/2026-05-22-hybrid-agent-loop-design.md` unifies these under a workflow-strategy abstraction (`react`, `step_dag`, `fanout`, `monitor`) with `AgentConfig` as the parameterization. Not yet built.

### ExternalAgentWorkflow (`internal/workflow/external_agent.go`)
Generic Temporal workflow that spawns an external agentic CLI (`claude -p` today; `gemini -p` and `copilot -p` in later plans) as a subprocess, parses its native streaming output into normalized Events, and persists each Event to the `messages` table. Each event also broadcasts on the SSE channel for live UI monitoring.

AgentConfig YAML under `workflows/agents/*.yaml` defines the schedule, prompt composition, allowlist, prefilter, and runner. Loaded at startup and registered as Temporal Schedules. The prompt is assembled from a base file plus optional includes with Go template rendering for the header.

Steps: preflight (fail-closed) -> prefilter (JSON gate: proceed or no-work) -> phases (sequential, Claude subprocess via ExecAgentCLI activity on task queue `external-agent-host1`) -> cleanup (always, best-effort).

Current MVP scope (foundation plan): Claude runner only, single-phase, scheduled mode (no per-call approval, no fanout, no mid-run injection). See `docs/superpowers/specs/2026-05-22-external-agent-runner-design.md` for the full design including features deferred to later plans.

## Supervised Orchestration (in progress, since 2026-07-08)

Alfred is being extended into a generic, Temporal-native supervised multi-agent orchestrator. Design spec: `docs/superpowers/specs/2026-07-08-generic-supervised-orchestration-design.md` (kept in the working tree, uncommitted, like the other superpowers design docs). Roadmap and progress are tracked in Forgejo issue #60.

**Operator/developer guide (committed):** `docs/supervised-orchestration-guide.md` is the recipe for building and running a supervised (`queue_monitor`) workflow: the interaction surface, host helper scripts (prefilter + declared-command tools), the AgentConfig schema and validation rules, the deploy layout on this host, the register-then-pause flow, the `#122` schedule-recreate gotcha, the four verification windows, model/provider notes, robustness design principles, and known limitations. Read it before creating a new supervised task instead of re-deriving any of that.

**Absolute rule for this work:** the engine stays fully generic. Nothing in `internal/` may know any domain concept (no GitHub, issue, ticket, bug, birdnet). All domain specifics live in deployment task config (YAML AgentConfigs, prompt files, external prefilter/validate commands, tool allowlists, Hindsight bank names). The birdnet-go support bots are one deployment's config and the proving ground; a customer-service agent is a future deployment of the same engine.

**Three durable Temporal levels:**
1. Queue Monitor (Level 0): a Schedule-driven, one-pass, no-LLM dispatcher. Runs the deployment prefilter (now a candidate emitter), dedups candidates against the `task_state` ledger, and dispatches one Case child workflow per fresh candidate (deterministic `CaseWorkflowID(task, key)`, reuse-after-close, abandon parent-close, fire-and-forget).
2. Case Supervisor (Level 1): the autonomous LLM main loop (native Vertex/OpenRouter), one per candidate (`CaseWorkflow`, `internal/workflow/case.go`). Its tools are `spawn_agent` (starts a sub-agent child workflow and awaits its result) and `record_outcome`, plus whatever deployment-declared command tools the task config adds, each executed by the generic `RunDeclaredCommand` activity with a per-tool retry policy (idempotent tools retry, non-idempotent ones get one attempt) and the mandatory `--` flag-terminator guard against argument injection. `request_approval` and `escalate` are HITL additions, not yet built (Phase 5). Shipped (Phase 3, #55).
3. Sub-Agents (Level 2): child workflows with `backend: native` (LLM loop) or `backend: cli` (ExternalAgentWorkflow), interchangeable from the supervisor's view. `backend: cli` is live today (a supervisor's `spawn_agent` call starts an `ExternalAgentWorkflow` child from the sub-agent's embedded AgentConfig). `backend: native` is live as of Phase 4 (#56): `spawn_agent` dispatches a `NativeSubAgentWorkflow` child (`internal/workflow/native_agent.go`), a second parameterization of the shared `runAgentLoop` (`internal/workflow/agent_loop.go`) extracted from the Case Supervisor. Its terminal tools are `submit_result` (bound to the sub-agent's declared `output.schema`, accepted only when required top-level fields are present with matching top-level types) and `report_failure` (the explicit failure hatch, mapping to a `failed` outcome with the reason surfaced to the supervisor). Both backends return the same `Outcome` envelope; statuses and payload are self-describing to the supervisor LLM, though the shape differs (a native child returns its result under `output.result`, a cli child returns per-phase outputs), so flipping a kind between backends needs no engine change and at most cosmetic prompt awareness of the output shape. `workflows/agents/native-demo-supervised.yaml` demonstrates one task mixing both.

Three stores, three jobs: Temporal owns in-flight orchestration + crash recovery + the Web UI tree; the `task_state` ledger owns the durable cross-run handled-set + outcomes + `/api/v1` observability; Hindsight owns knowledge. HITL (approval gates, mid-run steering, escalation) is config-driven and reuses the chat approval machinery.

**Shipped to `main` so far:**
- Phase 1 (#53, PR #61): the generic `task_state` ledger (migration `002`, store DAL, `GET/POST /api/v1/agent-tasks/{task}/state[/{key}/requeue]`).
- Phase 2 (#54, PR #63): the candidate-emitter prefilter contract, the `queue_monitor` strategy + `Monitor` limits, `QueueMonitorWorkflow`, its ledger activities, and a stub `CaseWorkflow` (claim-first then finalize `done`) so the dispatch path is testable.
- Phase 3 (#55): the real Case Supervisor. `agentcfg.Supervisor`/`DeclaredTool` schema and validation, `CaseActivities` (`CaseLLMStream`, `RunDeclaredCommand`, `RenderCasePrompt`, registered on the worker alongside `CaseWorkflow`), the supervisor's round loop (guards on `max_rounds`/`cost_cap_usd`/`max_duration`, parallel `spawn_agent` dispatch, `record_outcome` finalization; `cost_cap_usd` meters both the supervisor's own LLM token spend (priced in USD via a per-model pricing table, `llm.DefaultPriceTable` overlaid by the config `pricing` map, added in #66) and spawned sub-agent cost, the only spend it cannot see being a model absent from the pricing table, which is logged once), and a proving-ground deployment example expressing the full three-level shape: `workflows/agents/sentry-triage-supervised.yaml` (a `queue_monitor` reshaping of `sentry-triage.yaml`, left untouched as the pre-migration reference) with its supervisor prompt (`prompts/agents/sentry-triage-supervisor.md`) and its `analysis` cli sub-agent prompt (`prompts/agents/sentry-triage-analysis.md`, adapted from `sentry-triage.md` to one candidate at a time).

A live end-to-end run of the proving-ground task awaits Phase 0 (#52, bring the Temporal stack up), which is a separate parallel track, and requires the deployment host to provide the candidate-emitter form of the prefilter (`~/bin/sentry-triage-prefilter --candidates`, documented in the YAML) and the `forgejo-issues` declared-command helper the supervisor's `comment_on_item`/`label_item` tools shell out to.

**Not yet built:** Phase 5 HITL (#57), Phase 6 post-condition validation (#58), Phase 7 full birdnet-go bot migration + agy retirement (#59).

## Recent History: Hybrid Agent Loop Refactor (2026-05-22)

All commits between `281e3ab` and `22c19af` on `main` (single day) refactored the chat agent loop from a monolithic `RunAgentLoop` activity into per-round workflow-orchestrated activities. Reasons (per the spec):

1. The old design made individual LLM calls and tool executions invisible in Temporal event history.
2. Heartbeat-based retry checkpointing was coarse: a failure could re-run prior rounds' work.
3. No per-tool latency visibility in Temporal UI.

New design: the workflow function itself is the agent loop. Each phase (`ValidatePrompt`, `BuildContext`, `LLMStream`, per-tool `ExecTool` via `DynamicToolActivity`, `PersistRound`, `BuildContext` rebuild) is its own Temporal activity. Trade-off: more events per turn (about 3 to 5 per round vs 1), in exchange for full visibility and surgical retries.

**Phase 4** (commit `22c19af`) removed the old `RunAgentLoop`, `agent.Run`, the `EventIterator`, and the `workflow.GetVersion` gate. **No legacy code path remains.**

## Key Non-Obvious Patterns

### Workflow-as-Orchestrator
The chat workflow contains long loops with many yield points (activity calls). `state.turnInProgress` guards against overlapping `send_message` / `retry_message` updates. `approve_action` is NOT gated, so it can be accepted while a turn awaits approval.

Query handlers read directly from `chatWorkflowState` and must not call activities (determinism). `state.status` / `state.currentRound` / `state.turnInProgress` are updated synchronously in the workflow goroutine so query reads stay coherent.

### Dynamic Tool Dispatch
A single `DynamicToolActivity` handler is registered via `worker.RegisterDynamicActivity(chatActivities.DynamicToolActivity, activity.DynamicRegisterOptions{})` in `cmd/alfred/main.go`, Temporal's real dynamic-activity API (its function signature takes `converter.EncodedValues`, decoded with `args.Get(&req)`; the previously-used `RegisterActivityWithOptions{Name: ""}` is NOT the dynamic-activity API and caused a worker-startup panic whenever an LLM provider was configured, fixed in #116). It reads the tool name from `activity.GetInfo(ctx).ActivityType.Name` and routes through `agent.Registry` via `agent.SafeExecute` (panic-recovering). Adding a new tool requires only implementing the `Tool` interface and adding it to `agent.NewRegistry(...)` in `main.go`. No worker re-registration per tool. The blanket `w.RegisterActivity(chatActivities)` call also registers `DynamicToolActivity` a second time under its own Go method name (an ordinary, reachable activity Temporal's struct scan cannot be told to skip); `chat.go`'s tool-dispatch guard rejects a tool call named `"DynamicToolActivity"` up front so this reserved name is never reachable from the LLM.

CallIDs assigned during `LLMStream` follow the format `t{round}-c{index}`.

### request_approval Interception (Critical Invariant)
From the LLM's perspective `request_approval` is a normal tool, but the workflow intercepts the name BEFORE dispatching as an activity. See the `// --- Approval interception ---` branch in `runAgentTurnV2`. Sequence:

1. Persist pre-approval round messages (if any model text or earlier tool results in this round).
2. Persist `approval_request` message with idempotency key `{session}-{turn}-{round}-approval-req`.
3. Register `state.pendingApprovals[callID]` BEFORE emitting SSE (race fix from peer review).
4. Emit SSE `approval_request` and `done` events.
5. `workflow.AwaitWithTimeout(ctx, DefaultApprovalTimeout, state.allApprovalsResolved)` suspends durably; on expiry (24h) still-pending approvals resolve as status `timeout` (persisted as `approved:false` with the reason).
6. After resolution (human or timeout), persist `approval_result` and rebuild context.
7. Use `continue roundLoop` (NOT `break`) to skip the normal end-of-round persist and start the next LLM round with the approval result in history.

`ApprovalTool` is registered with `tools.NewPanicBroker()` (`internal/agent/tools/approval.go`). If anyone ever DID dispatch it as an activity, it would panic loudly. **This panic is the enforcement.**

Remaining tool calls AFTER `request_approval` in the same round are intentionally dropped. The LLM re-plans next round.

### Two-Layer Idempotency
**Layer 1 (DB):** Migration `002_idempotency_key.up.sql` adds an `idempotency_key` column to `messages` with a partial unique index `WHERE idempotency_key IS NOT NULL`. `PersistRound` generates keys of the form `{sessionID}-{turnID}-{round}-{messageIndex}`; `Store.AppendMessagesIdempotent` uses `INSERT ... ON CONFLICT DO NOTHING`. Pre-approval and approval-req/res use distinct key suffixes to avoid collision.

**Layer 2 (retry policy):** `Tool.Idempotent() bool` lets the workflow pick `MaxAttempts=3` for safe tools and `MaxAttempts=1` for tools with side effects. Wired as of Phase 3 Task 2a (#55): `ChatActivities.ToolIdempotency` queries `registry.IsIdempotent` once per workflow execution (cached in `chatWorkflowState.toolIdempotent`, fetched alongside tool declarations, and persisting across turns until ContinueAsNew), and `runAgentTurnV2`'s per-tool `toolMaxAttempts` helper selects `MaximumAttempts=1` for any tool the registry does not report as idempotent (an unknown or unregistered tool defaults to non-idempotent, the safe default).

### Epoch Transitions
`internal/epoch/manager.go`. Triggers in priority order:
1. Tokens used > 80% of `TokenBudget` (default 100000).
2. Epoch duration > `MaxDuration` (default 4h).

Flow: Extract (LLM produces JSON `{resolved, pending, decisions, context}`) → `RetainToHindsight` (non-fatal: log warning, proceed) → `PersistEpochTransition` row → `UpdateSessionEpoch` → `ContinueAsNew` with the SAME `session_id` but incremented epoch number.

**Key invariant:** `tokensUsed` is in-memory per workflow run and resets to zero on `ContinueAsNew`, and the epoch check only runs at turn completion, so a fresh epoch cannot re-trip the threshold with zero turns (no infinite transition loop) even though the session ID is reused. Prompt-side tokens from prior-epoch history still count toward each round's usage, so a near-budget context can legitimately trip the 80% threshold after one turn per epoch. (An `epochStartSequence` field once claimed to provide sequence-based scoping; it was write-only and removed.)

Token counts use a chars/4 heuristic via `estimateTokens`; real `Usage.TotalTokens` from the LLM is used when chunks include it.

### TieredContextBuilder (`internal/ctxbuild/builder.go`)
Builds the LLM context with:
1. A single `context` role prefix message from `EpochSummary + Memories`.
2. Stale-tool-result pruning: any `tool_result` or `approval_result` older than `staleTurnThreshold` turns has its content replaced by `"[pruned: stale tool result]"` (5-token stub). "Turns ago" is `(maxSeq - sequence) / 2` since tool call/result pairs span 2 sequences.
3. Newest-first fill: walks backward via `slices.Backward`, stops when the remaining budget would be exceeded, then reverses to ascending sequence order before returning.

Default `staleTurnThreshold` is 10 (`defaultStaleTurnThreshold` in `main.go`). Pruning ONLY affects results, never `user` / `model` / `tool_call` / `approval_request` messages, so the LLM still sees that "a tool was called and returned something" even when the details are stubbed.

Role constants `RoleUser`, `RoleModel`, `RoleToolCall`, `RoleToolResult`, `RoleApprovalReq`, `RoleApprovalResult`, `RoleContext` in `ctxbuild` are the single source of truth and match the DB `CHECK` constraint on `messages.role`.

## Bootstrap and Config

`cmd/alfred/main.go` is the entry point. `config.Load` reads `config.yaml`, expands env vars while preserving `${secret:...}` references, then resolves those via `internal/secrets` (AES-256-GCM with KDF-derived keys from `secrets.enc.json` + `master.key`).

Defaults applied if absent:
- Database URL: `postgres://localhost:5432/alfred`
- DB max conns: 10
- `vertex_ai.chat_model`: falls back to `vertex_ai.model`
- `chat.auto_recall_on_new_conversation`: `true`
- `DATABASE_URL` env var overrides YAML value if set

Workflow YAMLs are loaded from `workflows/` at startup via `config.LoadWorkflowsFromDir` (skips non-`.yaml` files).

The Temporal worker registers `TicketWorkflow`, `ChatWorkflow`, `AutotaskActivities`, `LLMActivities`, `MemoryActivities`, `ChatActivities` (struct-based), plus the single dynamic activity for tool dispatch via `worker.RegisterDynamicActivity`.

HTTP server (`internal/server`) is headless: the old `/api/*` routes and the embedded SPA are gone. The server now serves `/api/v1` behind `AuthMiddleware` (bearer; RFC 9457 `application/problem+json` on 401), SSE routes via short-lived `?ticket=` tokens, the unauthenticated `GET /api/v1/auth-info` discovery endpoint, the ops endpoints `GET /healthz` and `GET /readyz`, the internal `POST /mcp/perm/{run_id}` callback, and a small JSON index at `/`. Auth uses `subtle.ConstantTimeCompare`.

## Database Schema

Migrations under `internal/store/migrations/`. Four tables:

- `sessions` (uuid PK, workflow_id, run_id, parent_session FK, kind enum `{chat, ticket, monitor, agent_task}`, status enum `{active, completed, continued}`, epoch, epoch_summary, timestamps)
- `messages` (uuid PK, session_id FK, sequence INT, role CHECK enum, content, token_estimate, jsonb metadata, **idempotency_key** with partial unique index, created_at; `UNIQUE(session_id, sequence)`)
- `epoch_transitions` (uuid PK, from_session, to_session, trigger, extracted_facts, summary, created_at)
- `task_state` (migration `002`, added 2026-07-08): the generic supervised-orchestration ledger, PK `(task, candidate_key)`, status CHECK enum `{in_progress, done, skipped, escalated, needs_attention}`, opaque `outcome` JSONB, `case_run_id`, `attempts`, timestamps, `(task, status)` index. Owns the durable cross-run handled-set and outcomes. See the Supervised Orchestration section.

Role CHECK enum: `'user', 'model', 'tool_call', 'tool_result', 'context', 'approval_request', 'approval_result', 'error'`.

## Hindsight Banks (Two Distinct Uses, Do Not Confuse)

1. **Application runtime memory.** Alfred itself reads/writes via `internal/memory.Client` (REST). Bank name is configured in `config.yaml` under `hindsight.bank`. Example config uses `alfred`; the triage YAML uses `alfred-tickets`; the README example uses `alfred-chat`. The `RecallTool` registered in `agent.Registry` exposes this bank to the chat agent.

2. **Developer memory for Claude Code sessions about Alfred.** Separate bank named `alfred-dev`, created 2026-05-22 via `PUT https://memory.koti/v1/default/banks/alfred-dev`. Its mission includes a scope rule: only Alfred-related memories, no cross-project content unless explicitly requested. MCP entry lives in `~/.claude.json` (NOT `~/.claude/.mcp.json`) as `hindsight-memory-alfred-dev` pointing at `http://localhost:8890/mcp/alfred-dev/`. Tools available as `mcp__hindsight-memory-alfred-dev__*`.

When working on Alfred in Claude Code: recall from `alfred-dev` for context and retain Alfred-specific learnings there. Cross-project content belongs in its own bank (`birdnet-go-dev`, `hermes-agent`, etc.). Pinned mental models in `alfred-dev`: `alfred-architecture-map` and `alfred-conventions-and-roadmap` (both auto-refresh on consolidation).

## Vision: Agentic Backbone

See `docs/superpowers/specs/2026-05-22-hybrid-agent-loop-design.md` section "Agent Extensibility: Alfred as Agentic Backbone". Planned but **not yet built**:

- `AgentConfig` parameterization with four workflow strategies: `react`, `step_dag`, `fanout`, `monitor`.
- YAML agent catalog with JSON Schema validation, dry-run mode, secret refs, tool allowlist, CI preflight.
- Webhook and cron triggers (Temporal Schedules) in addition to the existing Autotask poller.
- Platform safety: `MaxTokenBudget` and `MaxCostUSD` hard stops, `EscalationPolicy` (`notify` / `create_ticket` / `escalate_to_chat`), `UpdateACL` for steering RBAC, `ApprovalRule` with CEL expressions for context-aware approval.
- Steering channel with multi-point drain (after round, during approval wait, on resume, before tool exec). Best-effort guidance, not hard control.
- Pause/resume update handlers.

**Explicit stance:** Google ADK is NOT used for the core agent loop because that would hide tool execution inside one Temporal activity, defeating the per-tool visibility this design is built around. If ADK is ever integrated it would be constrained to stateless model planning inside individual activities.

**Roadmap ordering matters:** safety controls (step 3) must ship BEFORE external triggers (steps 6 to 7). Autonomous agents without budget limits are a production risk.

## Development Workflow

Build and test via `task` (`Taskfile.yml`):

```
task dev               # fmt + lint + test + build
task test              # unit tests
task test:race         # race detector
task test:integration  # requires Temporal (build tag: integration)
task lint              # golangci-lint
task docker:up         # full stack (Alfred + Temporal + PostgreSQL)
task run               # local run
```

Unit tests use Temporal's `testsuite.WorkflowTestSuite`. See `integration_test.go` for the full triage workflow pattern: register activity stubs, mock per-activity with `env.OnActivity(...)`, execute, assert.

Lint config: `.golangci.yaml` (project-local). Custom rules: `rules/` (uses `quasilyte/go-ruleguard/dsl`).

## Conventions (Strict)

These come from the user's global CLAUDE.md and apply when working on Alfred:

1. **NEVER use em dashes** (or en dashes). Anywhere: prose, code, comments, commit messages, PR descriptions, branch names, file names, README, YAML, error messages, log lines. Substitute with comma, semicolon, colon, period, parentheses, or plain hyphen. Em dashes are an AI tell and the rule overrides every skill. If you see one in a file you are editing, fix it.
2. **Verify before stating facts** about deployment, file paths, container behavior. Do not lean on training-data assumptions.
3. **Hindsight is the memory system.** Use `mcp__hindsight-memory-alfred-dev__*` for project memory. Do not write to the file-based `.claude/projects/*/memory/` system.
4. **Forgejo is internal-only.** Alfred is currently a private repo, but if it ever goes public: never reference Forgejo issues, URLs, or the internal tracker in GitHub-visible content (PRs, commits, code comments, branch names).
5. **First-person singular ("I"), never "we"** in any external communication (issue replies, PR descriptions, commit messages).
6. **Peer review with Gemini CLI:** `gemini -m gemini-3.1-pro-preview -r latest -p "REVIEW ONLY, do NOT modify any files. ..."`. Always include the "REVIEW ONLY" instruction; Gemini takes initiative too eagerly otherwise. Use `-r latest` for session continuity across iterations.
7. **No emojis in code or docs** unless explicitly requested.

## File Map

```
cmd/alfred/                  entry point (main.go, secrets.go)
internal/
  activity/                  Temporal activities (autotask, llm, memory)
  agent/                     Tool registry + helpers + Tool interface
    runner/                  Runner interface + Registry
      claude/                Claude CLI adapter (flags, parser, guidance stdin)
      gemini/                Gemini CLI adapter (flags, parser, probe)
      copilot/               Copilot CLI adapter (flags, parser, MCP)
      runnerutil/            shared subprocess lifecycle (RunPipeStreaming,
                             KillWithGrace, bounded reads, MCP config authoring)
    tools/                   recall, approval (with PanicBroker)
  agentcfg/                  AgentConfig YAML types, loader, validator, schedule helpers
  config/                    YAML config + secrets resolution + workflow loading
  ctxbuild/                  TieredContextBuilder (token-aware pruning)
  epoch/                     ComposableEpochManager (token/time triggers)
  llm/                       Vertex AI Gemini client (streaming + non-streaming)
  memory/                    Hindsight REST client
  poller/                    Autotask ticket poller
  secrets/                   AES-256-GCM secret management
  server/                    /api/v1 REST + SSE + auth + MCP permission bridge + v1_*.go handlers
  store/                     PostgreSQL DAL + migrations (incl. task_state.go, migration 002)
  tmpl/                      Go template rendering (strict mode)
  workflow/                  TicketWorkflow + ChatWorkflow + ExternalAgentWorkflow + chat activities;
                             QueueMonitorWorkflow (queue_monitor.go) + CaseWorkflow (case.go) +
                             NativeSubAgentWorkflow (native_agent.go) + shared runAgentLoop
                             (agent_loop.go) + their activities
workflows/                   YAML workflow definitions (currently: triage.yaml)
  agents/                    AgentConfig YAMLs for external agent tasks
prompts/agents/              LLM prompts for external agent tasks
docs/
  research/                  Design research (2026-05-21 session context mgmt)
  superpowers/
    plans/                   Implementation plans (chat-ui slices, hybrid loop)
    specs/                   Design specs (canonical reference is
                             2026-05-22-hybrid-agent-loop-design.md)
    notes/                   Polish handoffs and completion notes
deploy/                      Dockerfile + docker-compose
rules/                       go-ruleguard custom lint rules
integration_test.go          Build-tagged end-to-end workflow test
config.yaml.example          Sample config with ${secret:...} references
Taskfile.yml                 Build orchestrator
.golangci.yaml               Lint config
```

## Reading Order for a Fresh Session

1. This file (you are here).
2. `README.md` (user-facing overview, mermaid diagrams of architecture and data flow).
3. `docs/superpowers/specs/2026-05-22-hybrid-agent-loop-design.md` (canonical design reference for the current chat workflow plus the agentic-backbone vision).
4. `docs/superpowers/specs/2026-05-22-external-agent-runner-design.md` (design spec for the ExternalAgentWorkflow and agentic-backbone foundation).
5. `docs/external-agent-runner.md` (operator guide: adding tasks, triggering runs, monitoring).
6. `internal/workflow/chat.go` (the ChatWorkflow + `runAgentTurnV2` orchestrator).
7. `internal/workflow/chat_activities.go` (the per-round activities).
8. `internal/workflow/engine.go` (the TicketWorkflow YAML interpreter).
9. `cmd/alfred/main.go` (wiring: how all the pieces connect at startup).
