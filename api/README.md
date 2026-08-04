# Alfred API: frontend integration guide

This directory holds `openapi.yaml`, the single source of truth for Alfred's
headless `/api/v1` REST surface. The backend serves the same document at
`GET /api/v1/openapi.json` (unauthenticated). This guide is the narrative
companion: it shows how a browser client (the separate Svelte 5 app) talks to
the API. For deployment and CORS, see `../deploy/README.md`.

## Generate a typed client

`openapi.yaml` is the contract; generate TypeScript types from it rather than
hand-writing them:

```bash
npx openapi-typescript api/openapi.yaml -o src/lib/api/types.ts
```

Regenerate whenever `openapi.yaml` changes. You can also fetch the served copy
(`/api/v1/openapi.json`) at build time if the backend repo is not checked out
alongside the frontend.

## Base URL and auth

Everything is under `/api/v1`. Send the API key as a bearer token on every
request:

```ts
const headers = { Authorization: `Bearer ${apiKey}` };
```

Before showing a login prompt, ask the server whether a key is even required
(unauthenticated call):

```ts
const res = await fetch("/api/v1/auth-info");
const { required } = await res.json(); // { required: boolean }
// required === false means the backend was started without an API key.
```

## Errors

Every non-2xx response is RFC 9457 `application/problem+json` with at least
`title` and `status`:

```ts
if (!res.ok) {
  const problem = await res.json(); // { title, status, detail?, type?, instance? }
  throw new Error(`${problem.status} ${problem.title}`);
}
```

Status meanings: `401` missing/wrong bearer token or a spent/expired SSE ticket;
`404` unknown run, config, or task; `409` the action is not valid for that run
(see capabilities below); `412` an `If-Match` etag mismatch on a workflow-config
write.

## Pagination

List endpoints take `pageSize` (1 to 200, default 50) and an opaque `pageToken`,
and return `{ items, nextPageToken }`:

```ts
async function* allRuns() {
  let pageToken = "";
  do {
    const qs = new URLSearchParams({ pageSize: "50" });
    if (pageToken) qs.set("pageToken", pageToken);
    const res = await fetch(`/api/v1/runs?${qs}`, { headers });
    const page = await res.json();
    yield* page.items;
    pageToken = page.nextPageToken ?? "";
  } while (pageToken);
}
```

## Runs and capabilities

A run is one execution, addressed by its stable `sessions.id` UUID (stable
across epoch `ContinueAsNew`, so it is safe to keep in the UI for the life of a
conversation). `strategy` selects behavior (`react` chat, `step_dag` ticket,
`fanout`/`monitor` agent). `supportedActions` lists which sub-resources are
valid for that run:

```ts
if (run.supportedActions.includes("messages")) {
  // show the chat composer
}
```

Calling an action a run does not support returns `409`. The vocabulary is
`messages`, `messages/retry`, `events`, `approvals`, `cancel`, `terminate`,
`signal`, `guidance`, `policy`, `children`.

Start a chat run:

```ts
const res = await fetch("/api/v1/runs", {
  method: "POST",
  headers: { ...headers, "Content-Type": "application/json" },
  body: JSON.stringify({ strategy: "react" }),
});
const run = await res.json(); // Run
```

## Server-sent events

There are three `text/event-stream` surfaces. All frames are `event: <type>`
plus `data: <json>`; parse `data` as JSON. The event types and their `data`
fields are tabulated on the `EventStream` schema in `openapi.yaml` (`chunk`,
`tool_call`, `tool_result`, `approval_request`, `usage`, `done`, `error`,
`message`, and `agent_event`/`guidance_received` for external-agent runs).

### 1. The interactive turn (POST, inline, not resumable)

`POST /runs/{run}/messages` streams one turn back in the POST response. Because
it is a POST with a body, `EventSource` cannot be used; read the response body
yourself. These frames carry no `id:` and are not resumable, so a dropped
connection means re-sending the turn.

```ts
const res = await fetch(`/api/v1/runs/${run.id}/messages`, {
  method: "POST",
  headers: { ...headers, "Content-Type": "application/json" },
  body: JSON.stringify({ content: userText }),
});

const reader = res.body!.getReader();
const decoder = new TextDecoder();
let buf = "";
for (;;) {
  const { value, done } = await reader.read();
  if (done) break;
  buf += decoder.decode(value, { stream: true });
  // SSE frames are separated by a blank line.
  let i;
  while ((i = buf.indexOf("\n\n")) !== -1) {
    const frame = buf.slice(0, i);
    buf = buf.slice(i + 2);
    let event = "message";
    let data = "";
    for (const line of frame.split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) data += line.slice(5).trim();
    }
    handle(event, data ? JSON.parse(data) : null); // e.g. append chunk.text
  }
}
```

A turn typically streams `chunk`* (append `data.text` in order), optional
`tool_call`/`tool_result` pairs, an optional `usage`, then a terminal `done`.
If the agent needs approval you get `approval_request` then `done`; resolve it
(below) and send the next turn.

### 2. The resumable per-run stream (GET, EventSource)

`GET /runs/{run}/events` replays and tails a run. `EventSource` cannot set an
Authorization header, so mint a single-use ticket first and pass it as
`?ticket=`. The ticket is short-lived (about 30 seconds) and single-use, so mint
it right before connecting:

```ts
async function openRunStream(runId: string, onEvent: (type: string, data: any) => void) {
  const t = await fetch("/api/v1/auth/sse-tickets", { method: "POST", headers });
  const { ticket } = await t.json();
  const es = new EventSource(`/api/v1/runs/${runId}/events?ticket=${ticket}`);
  for (const type of ["chunk", "tool_call", "tool_result", "approval_request", "usage", "done", "error", "message"]) {
    es.addEventListener(type, (e) => onEvent(type, JSON.parse((e as MessageEvent).data)));
  }
  return es; // call es.close() when done
}
```

Resumable frames carry `id: <sequence>` (the persisted `messages.sequence`). On
reconnect the browser sends the last id back in `Last-Event-ID` automatically,
and the server replays events after it, so resume neither loses nor duplicates
events across epoch transitions. `EventSource` reconnects on its own; if you
read with `fetch` instead, set the `Last-Event-ID` header yourself.

### 3. The global firehose (GET, EventSource)

`GET /events` is the same shape across all runs (mint a ticket the same way).
Use it for a dashboard that watches everything.

## Approvals

When the agent pauses for approval you receive an `approval_request` event
(`callId`, `name`, `action`, `description`). Resolve it:

```ts
await fetch(`/api/v1/runs/${runId}/approvals/${callId}/resolve`, {
  method: "POST",
  headers: { ...headers, "Content-Type": "application/json" },
  body: JSON.stringify({ approved: true }),
});
```

You can also list outstanding approvals with `GET /runs/{run}/approvals`.

## Message history

`GET /runs/{run}/messages` returns the persisted transcript (paginated). Use it
to hydrate a conversation on load, then tail new activity over the per-run SSE
stream.

## Local development across origins

When the frontend dev server runs on its own origin (for example Vite at
`http://localhost:5173`), add that origin to `server.cors.allowed_origins` in
the backend `config.yaml` so the browser's preflight and cross-origin reads
succeed. See `../deploy/README.md` for the single-origin vs separate-origin
trade-offs.
