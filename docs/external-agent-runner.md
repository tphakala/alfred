# External Agent Runner: Operator Guide

## Adding a new agent task

1. Create `workflows/agents/<name>.yaml` with the AgentConfig schema
2. Create `prompts/agents/<name>.md` with the LLM prompt
3. Restart Alfred (configs are loaded at startup)
4. The task appears at `/tasks` in the web UI

## AgentConfig schema

See `workflows/agents/memory-seed.yaml` for a complete example. Required fields:

- `name`: unique identifier
- `strategy`: must be `external_agent`
- `runner`: `claude`, `gemini`, or `copilot`. Capabilities differ; see "Alternative runners" below.
- `phases`: at least one phase with `name`, `model`, `prompt.base`, `steering.mode`, `output.capture`

Optional fields: `schedule.cron`, `preflight`, `prefilter`, `budgets`, `cleanup`, `state.bank`.

## Triggering a run

From the UI: click "Run now" on the Tasks page.

From the API: `POST /api/agent-tasks/<name>/run` with bearer auth.

## Stopping a run

From the UI: click "Stop" on the run detail page.

From the API: `POST /api/agent-tasks/runs/<run_id>/cancel` with a JSON body `{"reason": "..."}`.

## Tier 1A guidance injection (Claude only)

While a run is in progress, type guidance into the "Suggest something to the agent" footer textbox on the run detail page and press Enter. The agent picks up the suggestion at the next turn boundary without restarting.

REST surface: `POST /api/agent-tasks/runs/<run_id>/guidance` with JSON body `{"text": "..."}` and bearer auth. Returns:

- `202 Accepted`: guidance queued for delivery
- `400 Bad Request`: missing or empty text
- `404 Not Found`: no run is registered for the given run_id (the activity hasn't started or already ended)
- `409 Conflict`: the runner is not Claude (Gemini and Copilot do not support mid-run injection; use "Stop with new prompt" instead)
- `503 Service Unavailable`: the in-process guidance store is full or shut down

SSE event emitted during the flow:

- `guidance_received`: the runner consumed the injected message between turns (corresponds to a Claude system/replay event)

The MVP does not emit a separate `guidance_pending` event for the in-between state where the REST handler has accepted the request but the runner has not yet picked it up; clients should infer pending state from the absence of a corresponding `guidance_received` event after a successful 202 response. A `guidance_dropped` signal for the runner-exited-before-consumption case is also deferred.

Under the hood, the Claude subprocess is launched with `--input-format stream-json --replay-user-messages`; an in-process channel feeds user-message NDJSON lines into the subprocess's stdin between assistant turns. The same channel is created per-run only when the runner advertises `Capabilities.BidirectionalStream`. Gemini and Copilot return `false` for that capability, which is why guidance returns 409 for them.

## Alternative runners

In addition to Claude, the worker can register two more CLI runners. Each is gated by a config flag and must have the corresponding binary on the worker host's PATH (or an explicit path).

### Gemini

```yaml
gemini:
  enabled: true
  binary_path: gemini   # optional; defaults to PATH lookup
```

The worker probes `gemini --help` at startup and refuses to register the runner if the binary does not support `--output-format stream-json` (stream-json landed in google-gemini/gemini-cli#10883). A warning is logged in either failure mode and the runner is skipped; the process keeps running.

Auth: the `GEMINI_API_KEY` and `GOOGLE_API_KEY` env vars are scrubbed before exec so the subprocess uses OAuth Code Assist (the same auth `gemini` uses interactively). Each run gets an isolated `GEMINI_CLI_HOME` directory.

Capabilities: structured output yes, per-call permission hook no, mid-run guidance no, sandbox modes (`docker`, `podman`) advertised.

### Copilot

```yaml
copilot:
  enabled: true
  binary_path: copilot   # optional; defaults to PATH lookup
```

No version probe; Copilot is registered unconditionally when enabled. The runner expects `gh auth token` to return a valid GitHub token; if `GH_TOKEN` is not in the inherited environment, the runner shells out lazily at run start. Each run gets an isolated `COPILOT_HOME` directory and `COPILOT_CLI=1` is set.

Capabilities: structured output yes, per-call permission hook no, mid-run guidance no.

### Why this asymmetry matters

The UI surfaces `Capabilities.BidirectionalStream` via the guidance textbox's enabled/disabled state, and `Capabilities.PerCallPermissionHook` via the per-call approval panel. For Gemini and Copilot runs, both controls are hidden or disabled, and the run can only be steered via "Stop with new prompt" (Tier 1B; the REST handler cancels the running workflow and starts a fresh one with an augmented prompt).

## Monitoring

Live SSE stream: `GET /api/agent-tasks/runs/<run_id>/events` (backfills existing messages, then streams live).

Run detail page: `/tasks/runs/<run_id>` in the web UI.

## Reading run history

```sql
SELECT * FROM sessions WHERE kind = 'agent_task' ORDER BY created_at DESC LIMIT 50;
```

Messages for a specific run:
```sql
SELECT * FROM messages WHERE session_id = '<run_id>' ORDER BY sequence;
```

## Run statuses

- `ok`: all phases completed successfully
- `no_work`: prefilter returned `{"proceed": false}`
- `preflight_failed`: a preflight step exited non-zero
- `prefilter_failed`: the prefilter command failed or returned invalid JSON
- `phase_failed`: the CLI subprocess exited non-zero or the activity timed out
- `cancelled`: user cancelled the run via the UI or API

## Prefilter JSON envelope

A prefilter command must write a single JSON document to stdout of shape:

```json
{
  "proceed": true,
  "data": { "any": "json-encodable data" }
}
```

When `proceed` is `false`, the workflow ends with status `no_work` and no LLM phase runs. When `proceed` is `true`, the `data` block is exposed to every phase's prompt template as the `.prefilter` variable.

The migrated AgentConfigs (`sentry-triage.yaml`, `issue-update.yaml`) reference operator-managed wrapper scripts (`~/bin/sentry-triage-prefilter`, `~/bin/issue-prefilter update`) that produce this envelope. A minimal Python wrapper looks like:

```python
#!/usr/bin/env python3
"""Wrap the legacy line-prefix prefilter as a JSON envelope."""
import json
import subprocess
import sys

result = subprocess.run(["~/bin/legacy-prefilter"], capture_output=True, text=True)
candidates = []
for line in result.stdout.splitlines():
    if line.startswith("CANDIDATES:"):
        candidates = [int(n) for n in line.split(":", 1)[1].split(",")]
        break

print(json.dumps({
    "proceed": bool(candidates),
    "data": {"candidates": candidates},
}))
sys.exit(0)
```

The wrapper translates the legacy format into the new envelope; the AgentConfig YAML's prompt template can then access `{{ range .prefilter.candidates }}#{{ . }} {{ end }}` in its header.

## Troubleshooting

Check messages rows for the run; error details are in the `metadata` JSONB column. The `role = 'error'` rows contain error text directly in the `content` column.
