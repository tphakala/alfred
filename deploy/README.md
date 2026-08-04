# Alfred deployment

Alfred runs as a headless backend; there is no built-in dashboard. A separate
Svelte 5 client (a different repo, not yet built) will consume `/api/v1`. In
the interim there is no web UI: the API, the health endpoints, and the OpenAPI
contract are the deliverables.

## Serving modes

### Single-origin behind a reverse proxy (recommended for production)

Run a reverse proxy in front of Alfred. The proxy serves the static client
build from disk and forwards `/api/*`, `/healthz`, and `/readyz` to the Alfred
backend. Because the browser app and the API share one origin, the browser
never issues a cross-origin request and no CORS configuration is needed. Keep
`server.cors.allowed_origins: []` (the default) in `config.yaml`.

`deploy/Caddyfile.example` shows a minimal Caddy config for this layout. Adapt
the upstream address to wherever `server.address` points (default `:8080`) and
replace `/srv/alfred-ui` with the path to the built client assets once they
exist.

### Separate-origin (local Svelte dev server)

When the Svelte client runs on its own origin, for example the Vite dev server
at `http://localhost:5173`, the browser treats requests to the Alfred API as
cross-origin and issues preflight OPTIONS requests. To allow this, add the dev
server origin to `config.yaml`:

```yaml
server:
  cors:
    allowed_origins:
      - "http://localhost:5173"
```

The CORS middleware allows `Authorization`, `Content-Type`, and
`Last-Event-ID` request headers. SSE streams use short-lived `?ticket=` tokens
minted at `POST /api/v1/auth/sse-tickets` because the browser `EventSource`
API cannot send an `Authorization` header. In the single-origin mode those
tickets still work, but you do not need CORS headers to use them.

## Health and readiness

Two probe endpoints are available for load balancers and orchestrators:

- `GET /healthz` - liveness: returns 200 while the process is running.
- `GET /readyz` - readiness: returns 200 when both the database and Temporal
  are reachable; returns 503 with `application/problem+json` when a dependency
  is down.

Configure your load balancer to gate traffic on `/readyz` and your process
supervisor or Kubernetes `livenessProbe` on `/healthz`.

## OpenAPI contract

`GET /api/v1/openapi.json` serves the OpenAPI 3.1 spec and requires no
authentication. Client projects can fetch it at runtime or generate types
statically from `api/openapi.yaml` in this repository.

## Docker Compose (reference)

`deploy/docker-compose.yml` and `deploy/Dockerfile` are a reference
container setup, not the only supported deployment shape (see "Native binary
+ separate Temporal stack" below for an alternative that runs Alfred as a
plain systemd-managed binary). A few things to note before running it:

- The alfred service does not publish a host port in the sample compose file.
  The container listens on `:8080` internally. If you add a host port binding
  for alfred, note that `temporal-ui` already publishes `8080:8080`; you must
  remap one of the two to avoid a clash.
- The stack expects `config.yaml`, `workflows/`, and GCP credentials at the
  paths shown in the volume mounts. Copy `config.yaml.example` and adjust
  before starting.
- `alfred-db` (postgres on host port 5433) is Alfred's own database.
  `temporal-postgres` (no published port) backs Temporal.

## Native binary + separate Temporal stack (alternative)

Alfred does not have to run in a container. It can run as a plain Go binary
under a process supervisor (systemd, in this example), pointed at a Temporal
+ Postgres stack managed separately. This is not mutually exclusive with the
Docker Compose reference above; pick whichever fits the host. Reference
files: `deploy/alfred.service.example`,
`deploy/alfred-temporal-stack.service.example`,
`deploy/alfred-temporal-compose.yml.example`,
`deploy/alfred-temporal-init.sql.example`.

Shape:

- **Alfred itself**: a `systemd --user` service (`alfred.service.example`)
  running `bin/alfred --config <path> --workflows <path>` (the default
  `serve` command; see `alfred --help` for the `secrets` subcommand), built
  via `task build` (or `task build:prod` for an optimized binary).
  `--config` points at a `config.yaml` that is NOT necessarily co-located
  with the source checkout (e.g. `~/alfred/config.yaml` while the binary
  lives in `~/src/alfred/bin/`). `server.address` is a normal Go listen
  address; the idiomatic bare-port form (`":8086"`, bind all interfaces) is
  the documented default and is fully supported. `secrets.enc.json` +
  `master.key` (see `alfred secrets init`) live alongside `config.yaml`.
- **Temporal + its own Postgres**: a small, independent Podman/Docker Compose
  stack (`alfred-temporal-compose.yml.example`: a dedicated Postgres hosting
  both Temporal's own schema and Alfred's application database via
  `alfred-temporal-init.sql.example`, `temporalio/auto-setup`, and
  `temporalio/ui`), started via `podman compose up -d` / `docker compose up
  -d`. Alfred's application data and Temporal's workflow-history data are
  different concerns even though this example stack happens to host both in
  one Postgres container for simplicity; split them if that stops being
  appropriate.
- **Reboot safety**: since the Temporal stack isn't itself a single
  long-running foreground process, `alfred-temporal-stack.service.example`
  wraps `compose up -d` / `down` in a `Type=oneshot`,
  `RemainAfterExit=yes` unit, and `alfred.service.example` sets
  `Requires=`/`After=` on it, so starting Alfred alone (the normal
  `default.target` boot path) cascades to bring the Temporal stack up
  first. Verified end to end on this deployment shape: tearing down both
  containers and units, then `systemctl --user start alfred.service` alone,
  correctly brought up the full stack with zero restarts and `/readyz`
  green.
- **`server.address` is a listen address, not a dial address.** Anything
  that connects TO Alfred from a same-host subprocess it spawns (the
  `alfred-perm` MCP permission-bridge callback used by interactive-steering
  external-agent phases) needs a genuinely dialable address, not the raw
  bind-wildcard listen string; Alfred normalizes this internally (see
  `dialAddress` in `internal/workflow/external_agent_activities.go`, fixed
  in #118 after this exact class of bug silently broke the permission
  bridge with the idiomatic bare-port config). No operator action needed,
  documented here because it burned an entire smoke test the first time
  this deployment shape actually ran live.

## Internal callback endpoint

`POST /mcp/perm/{run_id}` is a loopback callback from the agent subprocess to
the backend. It is not part of the public API and must not be exposed through
a reverse proxy.
