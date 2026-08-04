# Alfred

Agentic AI platform for IT operations. Alfred autonomously triages tickets, runs scheduled AI agent tasks, and provides a human-in-the-loop chat interface for interactive work. Temporal workflows ARE the agent loops: durable execution, per-tool activity visibility, and surgical retries are first-class.

## What It Does

Alfred connects to your ticketing system (Autotask), polls for new tickets, and processes them through YAML-defined multi-step workflows. It also orchestrates external AI agent tasks (Claude Code, Gemini CLI, GitHub Copilot CLI) as Temporal workflows with live streaming, per-call approval, cost tracking, and mid-run guidance injection.

For cases that need human judgment, Alfred provides a real-time chat interface where operators can instruct, observe, and approve agent actions as they happen.

## Key Features

- **Autonomous ticket triage** - YAML-defined workflows with templated LLM prompts and structured output
- **External agent tasks** - Orchestrate Claude, Gemini, and Copilot CLI tools as durable Temporal workflows with YAML-defined AgentConfig
- **Fanout execution** - A scout phase produces items; child workflows process each item in parallel with configurable concurrency
- **Per-call approval** - Claude runs route every permission decision through Alfred's MCP bridge for human approval
- **Mid-run guidance** - Inject suggestions into running Claude agents via bidirectional streaming (Tier 1A)
- **Interactive chat** - Multi-turn conversations with per-round workflow-orchestrated activities, streaming responses, and dynamic tool dispatch
- **Human-in-the-loop approval** - Operators approve or reject agent actions before execution, with durable workflow-level awaits
- **Epoch-based context management** - Automatic conversation summarization when token budgets are exhausted
- **Long-term memory** - Recall and retain facts across sessions via Hindsight
- **Real-time streaming** - Server-Sent Events push tool calls, results, approvals, and guidance to API clients
- **Budget controls** - Per-phase and per-workflow cost caps with post-hoc enforcement
- **OnComplete hooks** - Post-workflow actions triggered on success, failure, or always
- **Encrypted secrets** - AES-256-GCM encrypted config with KDF-derived keys
- **Workflow configuration API** - Read and update workflow definitions via the /api/v1 API

## Architecture

```mermaid
graph TB
    subgraph External["External Services"]
        AT["Autotask<br/><i>Ticketing API</i>"]
        VAI["Google Vertex AI<br/><i>Gemini LLM</i>"]
        HS["Hindsight<br/><i>Memory Service</i>"]
    end

    subgraph Alfred["Alfred"]
        direction TB
        Poller["Poller<br/><i>Ticket Ingestion</i>"]
        Server["HTTP Server<br/><i>REST API + SSE</i>"]
        TW["Temporal Worker<br/><i>Workflow Execution</i>"]
        
        subgraph Workflows["Workflows"]
            TktWf["TicketWorkflow<br/><i>YAML-driven</i>"]
            ChatWf["ChatWorkflow<br/><i>Interactive agent</i>"]
            ExtWf["ExternalAgentWorkflow<br/><i>CLI orchestration</i>"]
        end

        subgraph Runners["Agent Runners"]
            ClaudeR["Claude<br/><i>stream-json</i>"]
            GeminiR["Gemini<br/><i>stream-json</i>"]
            CopilotR["Copilot<br/><i>JSONL</i>"]
        end

        subgraph Activities["Activities"]
            LLMAct["LLM Activities"]
            ATAct["Autotask Activities"]
            MemAct["Memory Activities"]
            ChatAct["Chat Activities"]
            ExtAct["ExternalAgent Activities"]
        end

        Agent["Agent<br/><i>Tool registry + helpers</i>"]
        CtxBuild["Context Builder<br/><i>Token-aware pruning</i>"]
        Store["Store<br/><i>PostgreSQL</i>"]
        AgentCfg["AgentConfig<br/><i>YAML loader + validator</i>"]
    end

    subgraph Infra["Infrastructure"]
        Temporal["Temporal Server"]
        PG["PostgreSQL"]
    end

    subgraph CLIs["Agentic CLIs"]
        CC["Claude Code"]
        GC["Gemini CLI"]
        GHC["Copilot CLI"]
    end

    Poller -->|poll tickets| AT
    Poller -->|dispatch| Temporal
    Temporal <-->|execute| TW
    TW --> TktWf
    TW --> ChatWf
    TW --> ExtWf
    ExtWf --> ExtAct
    ExtAct --> ClaudeR
    ExtAct --> GeminiR
    ExtAct --> CopilotR
    ClaudeR -->|subprocess| CC
    GeminiR -->|subprocess| GC
    CopilotR -->|subprocess| GHC
    ChatWf --> ChatAct
    ChatAct --> Agent
    ChatWf --> CtxBuild
    TktWf --> LLMAct
    TktWf --> ATAct
    TktWf --> MemAct
    LLMAct -->|stream| VAI
    ChatAct -->|stream| VAI
    ATAct --> AT
    MemAct --> HS
    ChatAct -->|recall/retain| HS
    Store --> PG
    ChatAct --> Store
    ExtAct --> Store
    AgentCfg -->|load YAMLs| ExtWf
    Server -->|workflow updates| Temporal

    style External fill:#f0f4ff,stroke:#4a6fa5
    style Alfred fill:#fff8f0,stroke:#c77d3a
    style Infra fill:#f0fff4,stroke:#3a8c5c
    style CLIs fill:#f5f5f5,stroke:#666
```

## Three Workflow Types

### TicketWorkflow (YAML-driven)

Polls Autotask for tickets matching configurable rules, then runs a step DAG: analyze (LLM), act (set fields, add notes, assign queues), and optionally recall/retain from Hindsight memory. A single workflow-level approval gate sits before the step loop.

### ChatWorkflow (interactive agent)

Long-lived ReAct loop with up to 10 rounds per turn. Each phase (ValidatePrompt, BuildContext, LLMStream, per-tool ExecTool, PersistRound) is its own Temporal activity for full visibility and surgical retries. Three update handlers (send_message, retry_message, approve_action) and three query handlers (state, summary, approvals). SSE streaming to API clients. Auto-recall on the first turn of epoch 1.

### ExternalAgentWorkflow (CLI orchestration)

Spawns an external agentic CLI (Claude Code, Gemini CLI, or Copilot CLI) as a subprocess, parses its native streaming output into normalized Events, and persists each event to the database with SSE broadcast. Supports:

- **Preflight/prefilter gates** - Fail-closed preflight checks; JSON-envelope prefilter to skip no-work runs
- **Multi-phase execution** - Sequential phases with different models and prompts
- **Fanout** - A scout phase produces structured output; child workflows process each item in parallel
- **Per-call approval** - Claude's `--permission-prompt-tool` routes permission decisions through Alfred's MCP bridge
- **Mid-run guidance** - Bidirectional streaming injects user suggestions between Claude turns (Tier 1A)
- **Budget enforcement** - Per-phase `max_budget_usd` and per-workflow `workflow_max_cost_usd` caps
- **OnComplete hooks** - Post-workflow shell commands filtered by outcome (success, failure, always)
- **State.Bank binding** - Passes Hindsight bank name as `HINDSIGHT_BANK` env var to the subprocess

## Data Flow

### Ticket Processing

```mermaid
sequenceDiagram
    participant AT as Autotask
    participant P as Poller
    participant T as Temporal
    participant TW as TicketWorkflow
    participant LLM as Vertex AI
    participant M as Hindsight

    P->>AT: Poll for new tickets (interval)
    AT-->>P: Matching tickets
    P->>T: Start TicketWorkflow

    activate TW
    opt memory.recall_before
        TW->>M: Recall similar tickets
        M-->>TW: Prior knowledge
    end

    loop Each workflow step
        TW->>LLM: Analyze with templated prompt
        LLM-->>TW: Structured output (category, priority, etc.)
        TW->>AT: Apply actions (set_field, add_note, assign_queue)
    end

    opt memory.retain_after
        TW->>M: Retain analysis for future recall
    end
    deactivate TW
```

### Interactive Chat Session

```mermaid
sequenceDiagram
    participant U as Client / Operator
    participant S as HTTP Server
    participant T as Temporal
    participant CW as ChatWorkflow
    participant LLM as Vertex AI
    participant DB as PostgreSQL
    participant M as Hindsight

    U->>S: POST /api/v1/runs
    S->>T: Execute ChatWorkflow
    S-->>U: 201 {run}

    U->>S: GET /api/v1/events (SSE, ticket-authed)
    U->>S: POST /api/v1/runs/{run}/messages

    S->>T: Workflow Update (send_message)
    T->>CW: Handle update

    activate CW
    CW->>CW: ValidatePrompt (fail-open)
    CW->>DB: BuildContext (load history)

    opt First turn
        CW->>M: AutoRecall (query memories)
        M-->>CW: Formatted memories
    end

    loop Per-round activities (max 10)
        CW->>LLM: LLMStream (streaming + SSE chunks)
        LLM-->>U: SSE: chunk events

        alt Tool calls returned
            loop Each tool call
                alt request_approval
                    CW->>DB: PersistRound (pre-approval)
                    CW-->>U: SSE: approval_request
                    U->>S: POST /api/v1/runs/{run}/approvals/{callId}/resolve
                    S->>T: Workflow Update (approve_action)
                    T->>CW: Resolve approval
                else Dynamic tool
                    CW->>CW: ExecTool activity (per tool)
                    CW-->>U: SSE: tool_call / tool_result
                end
            end
        end

        CW->>DB: PersistRound (idempotent)
        CW->>DB: BuildContext (rebuild)
    end
    CW-->>U: SSE: done
    deactivate CW

    opt Token budget exhausted (80%)
        CW->>LLM: Extract epoch summary
        CW->>M: Retain facts
        CW->>DB: Record epoch transition
        CW->>T: Continue-As-New (epoch + 1)
    end
```

### External Agent Task

```mermaid
sequenceDiagram
    participant U as Client / Operator
    participant S as HTTP Server
    participant T as Temporal
    participant EW as ExternalAgentWorkflow
    participant CLI as Claude / Gemini / Copilot
    participant DB as PostgreSQL

    U->>S: POST /api/v1/agent-tasks/{name}/run
    S->>T: Execute ExternalAgentWorkflow
    S-->>U: 200 {run}

    U->>S: GET /api/v1/runs/{run}/events (SSE, ticket-authed)

    activate EW
    EW->>EW: RunPreflight (fail-closed)
    EW->>EW: RunPrefilter (JSON gate)

    alt proceed = false
        EW-->>DB: Status: no_work
    else proceed = true
        loop Each phase
            EW->>EW: PreparePhaseInput (render prompt)
            EW->>CLI: ExecAgentCLI (subprocess)
            CLI-->>U: SSE: text_delta, tool_call, tool_result
            CLI-->>DB: Persist each Event as message row

            opt Fanout phase
                EW->>T: Start child workflows (max_parallel)
                T-->>EW: Aggregate child outcomes
            end

            EW->>EW: Post-hoc budget check
        end

        opt OnComplete hooks
            EW->>EW: RunStateHook (best-effort)
        end
    end

    EW->>EW: RunCleanup (always, disconnected context)
    deactivate EW
```

## AgentConfig (YAML)

External agent tasks are defined in `workflows/agents/*.yaml`. Each config specifies the runner, phases, tools, MCP servers, budget limits, and scheduling:

```yaml
name: sentry-triage
strategy: external_agent
runner: claude
description: Triage new Sentry issues

concurrency:
  max_concurrent: 1
  on_conflict: skip

prefilter:
  type: command
  command: ~/bin/sentry-triage-prefilter

phases:
  - name: main
    model: claude-opus-4-6
    runner_flags:
      max_turns: 40
      max_budget_usd: 3.00
    prompt:
      base: prompts/agents/sentry-triage.md
      includes:
        - prompts/agents/_voice.md
    tools:
      bash:
        - "github-issues comment*"
        - "sentry issue list*"
      builtin: [Read, Grep, Glob, WebFetch]
      mcp:
        servers:
          hindsight-memory:
            type: http
            url: http://localhost:8890/mcp/birdnet-go-support/
    steering:
      mode: scheduled
    output:
      capture: text

cleanup: []
```

See `docs/external-agent-runner.md` for the full operator guide.

## Agent Runners

Alfred abstracts external AI CLIs behind a `Runner` interface. Each adapter translates CLI-native streaming output into normalized events.

| Runner | CLI | Stream Format | Mid-run Guidance | Per-call Approval | Structured Output |
|--------|-----|---------------|:----------------:|:-----------------:|:-----------------:|
| Claude | `claude` | stream-json | Yes | Yes (MCP bridge) | Yes |
| Gemini | `gemini` | stream-json | No | No | Yes |
| Copilot | `copilot` | JSONL | No | No | Yes |

Runners are registered conditionally at startup. Gemini probes for `--output-format stream-json` support; if the binary is too old, it logs a warning and skips registration. Copilot requires `gh auth token`. Claude scrubs `ANTHROPIC_API_KEY` from the subprocess environment to use subscription auth.

## Epoch Lifecycle

Alfred manages long conversations through an epoch system that prevents token budget exhaustion while preserving context:

```mermaid
stateDiagram-v2
    [*] --> Active: Create session

    state Active {
        [*] --> AgentLoop
        AgentLoop --> WaitingForInput: Response complete
        WaitingForInput --> AgentLoop: User message
        AgentLoop --> ApprovalPending: Tool needs approval
        ApprovalPending --> AgentLoop: Approved / Rejected
    }

    Active --> EpochTransition: Token budget >= 80%\nor duration >= 4h
    
    state EpochTransition {
        [*] --> ExtractSummary
        ExtractSummary --> RetainFacts: LLM summarizes
        RetainFacts --> PersistTransition: Store in Hindsight
        PersistTransition --> ContinueAsNew: Record in DB
    }

    EpochTransition --> Active: New epoch\n(same session, fresh budget)
    Active --> Completed: User closes\nor idle timeout (30m)
    Completed --> [*]
```

## Database Schema

```mermaid
erDiagram
    sessions ||--o{ messages : contains
    sessions ||--o{ epoch_transitions : "transitions from"
    sessions ||--o{ epoch_transitions : "transitions to"

    sessions {
        uuid id PK
        text workflow_id
        text run_id
        uuid parent_session FK
        text kind "chat | ticket | monitor | agent_task"
        text status "active | completed | continued"
        int epoch
        text epoch_summary
        timestamptz created_at
        timestamptz updated_at
    }

    messages {
        uuid id PK
        uuid session_id FK
        int sequence
        text role "user | model | tool_call | tool_result | context | approval_request | approval_result | error"
        text content
        int token_estimate
        jsonb metadata
        text idempotency_key "nullable, partial unique with session_id"
        timestamptz created_at
    }

    epoch_transitions {
        uuid id PK
        uuid from_session FK
        uuid to_session FK
        text trigger
        text extracted_facts
        text summary
        timestamptz created_at
    }
```

## API

All `/api/v1` routes require `Authorization: Bearer <api_key>` when `api_key` is configured. Errors are RFC 9457 `application/problem+json`. SSE routes authenticate via a short-lived ticket from `POST /api/v1/auth/sse-tickets` (EventSource cannot send headers). List endpoints are paginated with `pageSize` / `pageToken`. The full OpenAPI 3.1 contract lives at `api/openapi.yaml`.

### Ops (unauthenticated, unversioned)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Liveness check |
| `GET` | `/readyz` | Readiness check (DB + Temporal) |

### Discovery and Auth

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/auth-info` | Whether an API key is required (unauthenticated) |
| `POST` | `/api/v1/auth/sse-tickets` | Mint a short-lived single-use SSE ticket (bearer auth) |

### Runs

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/runs` | List runs |
| `POST` | `/api/v1/runs` | Create an ad-hoc run |
| `GET` | `/api/v1/runs/{run}` | Get run |
| `DELETE` | `/api/v1/runs/{run}` | Close run |
| `POST` | `/api/v1/runs/{run}/cancel` | Cancel run |
| `POST` | `/api/v1/runs/{run}/terminate` | Terminate run |
| `POST` | `/api/v1/runs/{run}/signal` | Signal run |
| `GET` | `/api/v1/runs/{run}/messages` | List messages |
| `POST` | `/api/v1/runs/{run}/messages` | Send message (streamed turn) |
| `POST` | `/api/v1/runs/{run}/messages/retry` | Retry last turn |
| `GET` | `/api/v1/runs/{run}/events` | Run event stream (SSE, ticket-authed, resumable) |
| `GET` | `/api/v1/runs/{run}/approvals` | List pending approvals |
| `POST` | `/api/v1/runs/{run}/approvals/{callId}/resolve` | Resolve approval |
| `POST` | `/api/v1/runs/{run}/guidance` | Inject guidance (fanout) |
| `PUT` | `/api/v1/runs/{run}/policy` | Set approval policy |
| `PATCH` | `/api/v1/runs/{run}/policy` | Extend approval policy |
| `GET` | `/api/v1/runs/{run}/children` | List fanout children |

### Workflow Configs

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/workflow-configs` | List workflow configs |
| `GET` | `/api/v1/workflow-configs/{name}` | Get workflow config (with ETag) |
| `PUT` | `/api/v1/workflow-configs/{name}` | Update workflow config (If-Match ETag) |

### Agent Tasks

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/agent-tasks` | List agent task configs |
| `GET` | `/api/v1/agent-tasks/{name}` | Get agent task config |
| `POST` | `/api/v1/agent-tasks/{name}/run` | Trigger a run (returns the created run) |

### Singletons

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/poller` | Poller state |
| `GET` | `/api/v1/events` | Global SSE firehose (ticket-authed, resumable) |

## Tech Stack

| Layer | Technology |
|-------|-----------|
| **Language** | Go 1.26 |
| **Workflow engine** | Temporal |
| **LLM** | Google Vertex AI (Gemini) or OpenRouter (multi-provider) |
| **Database** | PostgreSQL 17 |
| **Memory** | Hindsight |
| **Ticketing** | Autotask |
| **External agents** | Claude Code, Gemini CLI, Copilot CLI |
| **API contract** | OpenAPI 3.1 (`api/openapi.yaml`) |
| **Frontend** | Headless API-only backend; the frontend is a separate Svelte 5 project that consumes the `/api/v1` OpenAPI contract |
| **Secrets** | AES-256-GCM with KDF |

## Configuration

Alfred is configured via a YAML file with encrypted secret references:

```yaml
autotask:
  api_url: "https://webservices.autotask.net/atservicesrest/v1.0"
  username: "${secret:autotask.username}"
  password: "${secret:autotask.password}"
  integration_code: "${secret:autotask.integration_code}"

temporal:
  host: "localhost:7233"
  namespace: "default"
  task_queue: "alfred"

vertex_ai:
  project: "my-gcp-project"
  location: "us-central1"
  model: "gemini-2.5-flash"
  chat_model: "gemini-2.5-flash"

hindsight:
  url: "${secret:hindsight.url}"
  bank: "alfred-chat"

server:
  address: ":8080"
  api_key: "${secret:server.api_key}"

database:
  url: "postgres://alfred:alfred@localhost:5433/alfred"
  max_conns: 10

chat:
  system_prompt: "You are an AI assistant..."
  auto_recall_on_new_conversation: true

# Optional: enable alternative runners
gemini:
  enabled: true
copilot:
  enabled: true
```

Secret references (`${secret:...}`) are resolved at startup from an AES-256-GCM encrypted `secrets.enc.json` file.

## Workflow Definitions

### Ticket workflows

Ticket workflows are defined in YAML and loaded from the `workflows/` directory:

```yaml
name: ticket-triage
description: Classify and route new tickets
trigger:
  type: polling
  entity: Ticket
  filter:
    status: New
  interval: 5m
model: gemini-3-flash-preview
approval: none
memory:
  bank: alfred-tickets
  recall_before: true
  retain_after: true
steps:
  - analyze:
      prompt: |
        Analyze this Autotask ticket and determine:
        - Category (network, server, application, security, other)
        - Priority (critical, high, medium, low)
        Ticket title: {{ .Title }}
        Ticket description: {{ .Description }}
      output_schema:
        category:
          type: string
          enum: [network, server, application, security, other]
        priority:
          type: string
          enum: [critical, high, medium, low]
  - act:
      - set_field: { name: category, value: "{{ .category }}" }
      - set_field: { name: priority, value: "{{ .priority }}" }
      - add_note: "Auto-triaged by Alfred: {{ .reasoning }}"
```

### Agent task configs

Agent tasks are defined in `workflows/agents/*.yaml` with prompts in `prompts/agents/`. See [External Agent Runner: Operator Guide](docs/external-agent-runner.md) for the full schema and examples.

Current agent tasks:
- `memory-seed` - Seeds Hindsight bank with curated facts from merged PRs
- `sentry-triage` - Triages Sentry issues with Claude Opus
- `issue-update` - Updates Forgejo issues with structured analysis
- `approval-demo` - Demonstrates per-call MCP approval flow
- `fanout-demo` - Demonstrates scout/child fanout pattern

## Development

### Prerequisites

- Go 1.26+
- PostgreSQL 17
- Temporal server
- Google Cloud credentials (for Vertex AI)
- [Task](https://taskfile.dev/) runner

### Quick Start

```bash
# Start infrastructure (Temporal + PostgreSQL)
task temporal:start
task db:up

# Build backend
task build

# Run
task run
```

### Common Tasks

```bash
task dev            # fmt + lint + test + build
task test           # Unit tests
task test:race      # Race condition detection
task test:integration  # Integration tests (requires Temporal)
task docker:up      # Full Docker stack
task lint           # golangci-lint
```

### Docker

```bash
task docker:build   # Build container image
task docker:up      # Start full stack (Alfred + Temporal + PostgreSQL)
task docker:down    # Stop stack
```

## Project Structure

```
alfred/
├── cmd/alfred/           # Application entry point
├── internal/
│   ├── activity/         # Temporal activities (LLM, Autotask, Memory)
│   ├── agent/            # Tool registry, helpers, SafeExecute
│   │   ├── runner/       # Runner interface + Registry
│   │   │   ├── claude/   # Claude Code adapter (stream-json, bidirectional)
│   │   │   ├── gemini/   # Gemini CLI adapter (stream-json, probe)
│   │   │   ├── copilot/  # Copilot CLI adapter (JSONL, gh auth)
│   │   │   └── runnerutil/ # Shared subprocess helpers
│   │   └── tools/        # recall, approval (with PanicBroker)
│   ├── agentcfg/         # AgentConfig YAML types, loader, validator, schedules
│   ├── config/           # YAML config + secrets resolution + workflow loading
│   ├── ctxbuild/         # TieredContextBuilder (token-aware pruning)
│   ├── epoch/            # ComposableEpochManager (token/time triggers)
│   ├── llm/              # Vertex AI Gemini client (streaming)
│   ├── memory/           # Hindsight REST client
│   ├── poller/           # Autotask ticket poller
│   ├── secrets/          # AES-256-GCM secret management
│   ├── server/           # REST + SSE + auth + MCP permission bridge + guidance
│   ├── store/            # PostgreSQL store + migrations
│   ├── tmpl/             # Go template rendering (strict mode)
│   └── workflow/         # TicketWorkflow + ChatWorkflow + ExternalAgentWorkflow
├── workflows/            # YAML workflow definitions
│   └── agents/           # AgentConfig YAMLs for external agent tasks
├── prompts/agents/       # LLM prompts for external agent tasks
├── docs/                 # Operator guides and design specs
├── deploy/               # Dockerfile + docker-compose
├── Taskfile.yml          # Build tasks
└── go.mod
```

## License

Private.
