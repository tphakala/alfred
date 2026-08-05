# Supervised Orchestration: Building and Running Workflows

Operator and developer guide for Alfred's supervised orchestration engine (the
`queue_monitor` strategy: Queue Monitor -> Case Supervisor -> Sub-Agents). Pair
this with `external-agent-runner.md` (the single-CLI-session strategy) and
`ALFRED.md` (architecture). Written 2026-07-10 from the first live end-to-end
run of the `sandbox-issue-triage` proving ground; facts marked "verified" were
confirmed live on the deployment host that day.

The goal of this document: creating a new supervised workflow should be a recipe,
not a research project.

## When to use supervised (`queue_monitor`) vs `external_agent`

- **`queue_monitor` (this guide):** a durable dispatcher watches a queue of
  candidates and runs one autonomous LLM Case Supervisor per candidate. The
  supervisor reasons across rounds, calls declared command tools, optionally
  spawns sub-agents, and finalizes with `record_outcome`. Use it when work is a
  stream of items each needing multi-step, branching handling.
- **`external_agent`:** one external CLI agent session (claude/gemini/copilot)
  per run. Use it when a single agentic session with its own tools is the right
  unit and you do not need per-candidate supervision.

**Absolute engine rule:** nothing in `internal/` or `cmd/` may name a domain
concept (no GitHub, issue, ticket, weather). All domain lives in deployment
config: the YAML AgentConfig, prompt files, and host helper scripts (prefilter,
declared-command tools). The engine stays generic.

## The three durable levels

1. **Level 0 Queue Monitor** (`QueueMonitorWorkflow`): Schedule-driven,
   deterministic, no LLM. Each pass runs the deployment prefilter (a candidate
   emitter), dedups candidates against the `task_state` ledger, and dispatches
   one Case child per fresh candidate within the concurrency budget.
2. **Level 1 Case Supervisor** (`CaseWorkflow`): one autonomous LLM ReAct loop
   per candidate, on the native provider (Vertex/OpenRouter). Tools: the
   deployment's declared commands, plus reserved `spawn_agent` and
   `record_outcome`. Guarded by `max_rounds`, `cost_cap_usd`, `max_duration`,
   and a no-progress guard: a round with no tool calls is nudged back toward
   `record_outcome`, and after two consecutive such rounds the case finalizes
   `needs_attention` (reason `no_tool_calls`) rather than crash.
3. **Level 2 Sub-Agents:** `backend: native` (a schema-bound LLM loop, terminal
   tools `submit_result`/`report_failure`) or `backend: cli` (an
   `ExternalAgentWorkflow`), interchangeable from the supervisor's view.

Three stores, three jobs: Temporal owns in-flight orchestration and the Web UI
tree; the `task_state` ledger owns the durable cross-run handled-set, outcomes,
and `/api/v1` observability; Hindsight owns knowledge.

## The recipe

### 1. The interaction surface (keep it disposable)

For a test or proving ground, use a throwaway surface so nothing production is
touched:

- A disposable repo/tracker (e.g. a public GitHub sandbox repo).
- A **bot identity distinct from the user**: the user files the work items, the
  bot acts on them. That two-account split is what makes it a real interaction
  test. Store the bot token in a host file, never in config or Temporal history.
- Token scope: a classic PAT with `public_repo` can write to public repos only;
  a private repo needs full `repo`. (Verified: the `birdnet-go-bot` token is
  `public_repo`, which is why the sandbox repo is public.)

### 2. Host helper scripts (the deployment's domain glue)

Declared commands and the prefilter are small wrapper binaries on the worker's
`PATH` (on this host, `~/bin` is on the `alfred.service` PATH, verified). They
read the bot token from a host file and act as the bot, keeping credentials off
the wire (the MCP-secret hydration gap does not apply to helper binaries). This
mirrors the existing `forgejo-issues` / `github-issues` / `sentry-triage-prefilter`
pattern.

- **Prefilter (candidate emitter):** `mytask-prefilter --candidates` prints
  `{"candidates":[{"key":"...","context":{...}}]}`. `key` is the stable dedup
  identity against the ledger; `context` is opaque to the engine and becomes the
  case's first turn verbatim. Exclude already-handled items (e.g. drop items
  carrying a `handled` label) so re-triggering is idempotent.
- **Declared-command tools:** one wrapper per tool. Each reads the token, acts
  as the bot, and treats everything after a literal `--` as the value.

**Design principle (load-bearing): make declared tools idempotent so a chatty
model cannot cause damage.** A weak model will re-call a tool across rounds. If
a "comment" tool APPENDS, you get duplicate comments; if it UPSERTS (edit the
bot's existing comment in place, or key on a stable id), repeated calls converge
to one result. Prefer edit-in-place / upsert semantics for any tool where "do it
once" is the intent, and set `idempotent: true` in the config so the engine also
retries it safely. Do not rely on the prompt to make the model call a tool
exactly once.

### 3. The AgentConfig YAML

Location: `workflows/agents/<name>.yaml`. Shape (see
`workflows/agents/sandbox-issue-triage.yaml` for a complete example):

```yaml
name: my-supervised-task
strategy: queue_monitor
description: One line.

# queue_monitor REQUIRES a non-empty cron (validation rejects empty): the manual
# POST /run path triggers the Schedule's trigger-now action, so a Schedule must
# exist. Keep the Schedule PAUSED and drive it manually for a test.
schedule:
  cron: "0 6 * * *"

concurrency:
  max_concurrent: 1
  on_conflict: skip

prefilter:
  type: command
  command: mytask-prefilter --candidates

monitor:
  max_parallel_cases: 4      # cap on in-flight cases; budget = this minus in_progress ledger rows
  max_new_cases_per_pass: 5  # cap on cases started per monitor pass
  max_case_attempts: 2       # a candidate over this many attempts is finalized needs_attention

supervisor:
  model: openai/gpt-oss-120b # the native provider model; must do reliable tool-calling
  prompt:
    base: prompts/agents/my-supervisor.md
  max_rounds: 6
  cost_cap_usd: 1.00         # meters supervisor + sub-agent spend, IF the model is in the pricing table
  max_duration: 15m
  tools:
    - name: comment_on_item
      description: Post/update the bot's comment on this item.
      idempotent: false      # false -> the engine runs it at most once per retry (MaxAttempts=1)
      command: ["mytask-comment", "{number}", "--", "{body}"]
      args_schema:
        type: object
        required: [number, body]
        properties:
          number: { type: integer, description: "..." }
          body:   { type: string,  description: "..." }
```

**Declared-tool validation rules (enforced by `agentcfg.Validate`), verified
against the source:**

- `command` must be non-empty; `command[0]` (the executable) must be a LITERAL,
  never a `{placeholder}` (or the LLM could choose the binary).
- The command must contain a `--` flag-terminator element. Values substituted
  before `--` that start with `-` are rejected at exec time; put free-text
  values (bodies, labels) after `--`.
- Placeholders must be whole elements (`"{body}"`), never embedded
  (`"x{body}"`).
- Tool names must not collide with the reserved `spawn_agent` / `record_outcome`.
- `queue_monitor` requires both `prefilter.command` and a non-empty
  `schedule.cron`.
- A supervisor with declared tools and NO sub-agents is valid (Level 0+1 only).
- `idempotent: true` -> the engine retries the tool (MaxAttempts=3); `false` ->
  MaxAttempts=1 (safe for side effects).

### 4. The supervisor prompt

`prompts/agents/<name>.md`. The prompt is resolved relative to the prompt dir;
on this host `~/alfred/prompts` symlinks to the repo `prompts/`, so a prompt
committed to the repo is picked up automatically (no deploy step). The case's
first turn is the candidate `context` verbatim. Tell the supervisor to do the
work with its tools and finish with `record_outcome` (terminal). Keep it short.

### 5. Deploy on this host (verified layout)

- `alfred.service` (user systemd), `WorkingDirectory=/home/thakala/alfred`,
  `ExecStart=/home/thakala/src/alfred/bin/alfred --config
  /home/thakala/alfred/config.yaml --workflows /home/thakala/alfred/workflows`.
- `~/alfred/prompts` -> repo `prompts/` (prompts auto-load).
- `~/alfred/workflows` -> `~/alfred/workflows-deploy`; agent configs are
  PER-FILE symlinks in `~/alfred/workflows-deploy/agents/` pointing back to the
  repo `workflows/agents/`. **To add a task, symlink its YAML into that dir.**
- Binary: `bin/alfred`, built with `task build` (which runs
  `GOEXPERIMENT=jsonv2 go build -o ./bin/alfred ./cmd/alfred`). Rebuild ONLY for
  a code change; a config/prompt change needs no rebuild.

Deploy steps:

1. Merge the YAML + prompt to `main`; confirm they are on the host checkout.
2. `ln -s <repo>/workflows/agents/<name>.yaml ~/alfred/workflows-deploy/agents/`.
3. `systemctl --user restart alfred.service`; check `/readyz` and that
   `NRestarts` did not climb.
4. **Immediately pause the new Schedule** and drive it manually:
   `podman exec alfred-temporal tctl --address 10.90.0.91:7233 --ns default
   schedule toggle --sid "agent_task:<name>" --pause --reason "..."`.

**CRITICAL GOTCHA (#122): a `queue_monitor` Schedule stores the full AgentConfig
as its workflow input, captured WHEN THE SCHEDULE IS CREATED.** Editing the YAML
and restarting does NOT update the running Schedule: startup hits "schedule with
this ID is already registered" and the update path silently no-ops. So a config
change (model, `max_parallel_cases`, tools, prompt-base path, cron) does NOT
reach the running task until you recreate the Schedule:

```
# Push a config change to a queue_monitor task:
podman exec alfred-temporal tctl --address 10.90.0.91:7233 --ns default \
  schedule delete --sid "agent_task:<name>"
systemctl --user restart alfred.service      # Alfred recreates it with the new config
# then re-pause it (a fresh Schedule is created unpaused)
```

This same no-op behavior is why a paused Schedule stays paused across restarts.
Never delete or unpause the two production schedules
(`sentry-triage-supervised`, `native-demo-supervised`) without explicit sign-off.

### 6. Trigger and verify (four independent windows)

- **Trigger:** `POST /api/v1/agent-tasks/<name>/run`. Auth: the `/api/v1` routes
  sit behind a bearer guard, but **when `server.api_key` is empty the guard is
  disabled and all requests pass** (verified: it is empty on this host, so
  loopback calls need no token).
- **Temporal Web UI** (port `:8234` per the Phase 0 handoff; confirm on the
  host): the `QueueMonitorWorkflow` pass and the `CaseWorkflow` children, with
  per-round `CaseLLMStream` / `RunDeclaredCommand` activities. CLI equivalent:
  `tctl ... workflow list [--open]`.
- **Ledger:** `GET /api/v1/agent-tasks/<name>/state` -> `{items:[{candidateKey,
  status, attempts, ...}]}`. Terminal statuses: `done`, `skipped`, `escalated`,
  `needs_attention`; `in_progress` means claimed-but-not-finalized.
- **The interaction surface** (the repo): comments/labels appear as the bot.
- **Messages table** (deep debugging): the per-round role sequence.
  `psql "postgres://alfred:...@localhost:5434/alfred?sslmode=disable" -c "SELECT
  m.sequence, m.role, left(m.content,60) FROM messages m JOIN sessions s ON
  m.session_id=s.id WHERE s.workflow_id='case-<name>-<key>' ORDER BY sequence;"`

### 7. Reset a test run

- Clear the ledger for a fresh full run: `POST
  /api/v1/agent-tasks/<name>/state/<key>/requeue` per key (204).
- Delete the bot's comments on the surface if you want pristine issues.
- A case that failed leaves an `in_progress` row that consumes the parallel
  budget until requeued; requeue those before re-triggering.

## Provider and model notes

- **OpenRouter `:free` endpoints are rate-limited upstream (HTTP 429) under any
  real load** and cannot sustain a tool loop. Use a paid endpoint (e.g.
  `openai/gpt-oss-120b` without `:free`). Verified live.
- **The model must do reliable tool-calling.** A weak model duplicates tool
  calls across rounds and emits empty/no-tool-call rounds (the loop now nudges
  those, then finalizes `no_tool_calls`, but a model that cannot tool-call
  reliably still burns the nudge budget without progress). Model quality is a
  real dependency for multi-step tasks.
- **Cost metering:** `cost_cap_usd` only meters models present in
  `llm.DefaultPriceTable` (Gemini models) or the config `pricing` overlay. An
  unpriced model (e.g. `gpt-oss-120b`) contributes zero to the meter (logged
  once); the loop is then bounded only by `max_rounds` / `max_duration`. Add a
  `pricing` entry to make the cap meaningful.

## Design principles for robust supervised workflows

Autonomous agents misbehave; the harness must make misbehavior harmless rather
than trust the model.

- **Idempotency at two layers.** The `task_state` ledger dedups WORK across runs
  (one case per `key`, reuse-after-close, `handled` label filter in the
  prefilter). Declared TOOLS should be idempotent within a case where "do it
  once" is intended (edit-in-place / upsert), so an over-eager model converges
  to one result instead of spamming.
- **Bounded everything:** `max_rounds`, `cost_cap_usd`, `max_duration`,
  `max_parallel_cases`, `max_case_attempts`, and an internal no-progress bound
  (a no-tool-call round is nudged at most twice before the loop gives up).
- **Terminal tools:** the supervisor ends with `record_outcome`; a native
  sub-agent ends with `submit_result` / `report_failure`. A loop that never
  reaches its terminal tool is a bug to bound, not a model to trust.

## Known limitations / current status (2026-07-10)

- Level 0+1 supervised orchestration works end-to-end live (proven by
  `sandbox-issue-triage`: dispatch, multi-round supervisor loop, declared-command
  execution, real bot comments/labels).
- **#26 (resolved):** the agent loop no longer crashes on a no-tool-call round.
  Previously, when the model returned a round with no tool calls, the loop
  persisted a model-side turn and re-invoked the LLM with a model-side-last
  context, failing the last-role guard (`ErrLastRoleNotUser`) and sticking the
  case `in_progress`. The loop now persists a synthetic user nudge steering the
  model back toward its terminal tool and, after two consecutive no-tool-call
  rounds, finalizes `no_tool_calls` (supervisor: `needs_attention`; native
  sub-agent: `failed`) instead of re-invoking on a model-side-last context.
  Affected any model.
- **Duplicate tool calls:** a weak model re-calls a non-idempotent tool each
  round (observed: 3-5 duplicate comments). Mitigate with idempotent tool design
  (section above), the no-tool-call handling (#26), a capable model, and prompt
  hardening. Do not rely on the model alone.
- **#128 (open):** latent mapper edge cases in the unused structured tool-call
  path (OpenRouter drops `Text` alongside `ToolResults`; empty-message asymmetry).
- **Milestone 2 not yet exercised:** sub-agent `spawn_agent` + PR-filing.

## Reference example

- Config: `workflows/agents/sandbox-issue-triage.yaml`
- Prompt: `prompts/agents/sandbox-issue-supervisor.md`
- Host helpers: `~/bin/sandbox-issue-prefilter`, `~/bin/sandbox-issue-comment`,
  `~/bin/sandbox-issue-label`, `~/bin/sandbox-weather`
- Surface: `https://github.com/tphakala/alfred-sandbox` (public, disposable)
