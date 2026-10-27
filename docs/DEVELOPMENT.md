# Development

A tour of the code, how to build and test it, and how to extend it.

## Contents

- [Prerequisites](#prerequisites)
- [Build, run, check](#build-run-check)
- [Running a local cluster](#running-a-local-cluster)
- [Code tour](#code-tour)
- [Key types and functions](#key-types-and-functions)
- [Testing](#testing)
- [Extending MeshControl](#extending-meshcontrol)
- [Conventions](#conventions)
- [Releasing](#releasing)

## Prerequisites

- Go 1.22+ (the router relies on method + wildcard patterns, and `log/slog`).
- Ollama, for end-to-end testing (optional for unit work).
- `make` (optional), `curl`, and `jq` are handy.

## Build, run, check

```bash
go mod tidy
go vet ./...
go build -o meshcontrol .

CGO_ENABLED=0 go build -o meshcontrol .   # static binary
```

Run from the repository root so that `.env` and `./web` resolve.

## Running a local cluster

Everything can run on one machine, with several "nodes" being just several
agents with different `NODE_ID`s. For real nodes you need separate Ollama
instances; for UI development, a fake Ollama is enough.

```bash
# terminal 1: controller
cat > .env <<'ENV'
AGENT_TOKEN=dev-agent
ADMIN_TOKEN=dev-admin
WEB_DIR=./web
ENV
./meshcontrol controller

# terminal 2: agent (talks to your local Ollama)
AGENT_TOKEN=dev-agent CONTROLLER_URL=http://127.0.0.1:8000 NODE_ID=local-1 \
  ./meshcontrol agent
```

On a single machine the auto-detected advertised address will be the loopback
or LAN IP; if the controller reports `provider_unreachable`, set
`OLLAMA_ADVERTISE_URL=http://127.0.0.1:11434` (loopback is in the default
`ALLOWED_NODE_NETWORKS`).

To simulate nodes without Ollama, run a tiny HTTP server that answers
`GET /api/tags` with `{"models":[{"name":"test:latest"}]}` and start an agent
pointing `OLLAMA_ADVERTISE_URL` and `OLLAMA_LOCAL_URL` at it.

## Code tour

| File | Read it for |
|---|---|
| `main.go` | Argument/`.env` handling; the global `log` (`slog`) |
| `dotenv.go` | `loadDotEnv`, `parseEnvValue`: pure functions that are easy to unit-test |
| `util.go` | `envOr`/`envBool`/`envFloat`, `idleTimer`, `errName` |
| `agent.go` | `AgentConfig`, `Agent.Run` loop, `readModels`, `discoverLANIP`, `summarizeModel` |
| `controller.go` | `ControllerConfig`, validators, `Registry`, `normModel` |
| `server.go` | `Handler()` route table, auth wrappers, `forward`/`relay` proxy |
| `pulls.go` | `PullManager`, `PullJob`, NDJSON progress parsing |

Request flow for a proxied chat: `Handler()` route →
`adminOnly(api(handleProxyAuto))` → `Registry.CandidatesFor` → `forward` →
`relay`.

## Key types and functions

| Name | Where | Purpose |
|---|---|---|
| `ControllerConfig` / `controllerConfigFromEnv` | `controller.go` | All controller settings and their parsing/validation |
| `AgentConfig` / `agentConfigFromEnv` | `agent.go` | Agent settings |
| `Registry` | `controller.go` | Mutex-guarded node map; takes an injectable `clock` |
| `NodeView`, `ModelDetail` | `controller.go` | JSON shapes returned by the API |
| `validateOllamaURL` | `controller.go` | SSRF gate for agent-supplied URLs |
| `normModel` | `controller.go` | `llama3` → `llama3:latest` normalisation |
| `CandidatesFor` | `controller.go` | Hosting/eligible lists + round-robin rotation |
| `Server.Handler` | `server.go` | Route table (Go 1.22 `ServeMux` patterns) |
| `api(fn)` | `server.go` | Maps returned errors to JSON responses (`validationError` → 400, `errTooLarge` → 413, else 500) |
| `forward`, `relay` | `server.go` | Fail-over on dial errors; streamed copy to client |
| `PullManager` | `pulls.go` | Background pull jobs, progress and cancel |
| `idleTimer` | `util.go` | "Silence between chunks" timeout |

## Testing

The sources contain no `_test.go` files yet. The design makes testing
straightforward:

- **Inject the clock.** `NewRegistry(cfg, clock)` and `NewPullManager(cfg,
  clock)` take a `func() time.Time`, so state transitions (`online` →
  `offline` → purged) can be tested without sleeping.
- **Validators are pure.** `validateNodeID`, `validateOllamaURL` (with IP
  literals), `validateModels`, `validateModelDetails`, `validatePullModel`,
  `parseNetworks`, `normModel`, `parseEnvValue` all take plain inputs.
- **Handlers via `httptest`.** `NewServer(cfg, webDir).Handler()` returns an
  `http.Handler`; drive it with `httptest.NewRecorder`. For the proxy, point a
  registered node's `ollama_url` at an `httptest.Server` bound to `127.0.0.1`
  (inside the default allowed networks).
- **Fake Ollama** for pull tests: an `httptest.Server` that writes NDJSON lines
  (`{"status":"pulling x","digest":"d","total":100,"completed":50}` …
  `{"status":"success"}`) and flushes between them.

Suggested first cases:

1. `normModel`: `llama3` → `llama3:latest`; `hf.co/org/repo` → `hf.co/org/repo:latest`;
   `host:5000/foo` → `host:5000/foo:latest`.
2. `validateOllamaURL`: rejects `http://169.254.169.254:80`, `http://u:p@10.0.0.1`,
   `http://10.0.0.1/x`, `ftp://10.0.0.1`; accepts `http://10.0.0.1:11434`.
3. Registry status transitions with a fake clock.
4. Round-robin: three requests across two eligible nodes alternate.
5. Proxy: first candidate refuses connections → second serves and
   `X-MeshControl-Node` shows it; a mid-stream failure is **not** retried.
6. Pull: duplicate start returns `409`; cancel ends in `cancelled`.

Run with the race detector, since the registry and pull manager are
concurrent:

```bash
go test -race ./...
```

## Extending MeshControl

### Proxy another Ollama endpoint

Add the name to `proxyEndpoints` in `controller.go`. Only add endpoints whose
request body carries a `model` field (auto-routing needs it) and that are safe
to expose; avoid anything that mutates a node (`pull`, `delete`, `create`,
`copy`, `push`).

### Report a new fact from agents

1. Add the field to `heartbeatPayload` and fill it in `Agent.heartbeat`
   (`agent.go`).
2. Validate it in `handleHeartbeat` (`server.go`) with a validator in
   `controller.go`; **bound sizes and reject bad types**.
3. Store it in `nodeRec` and pass it through `Registry.Heartbeat`.
4. Expose it in `NodeView` / `Registry.view`.
5. Update [API.md](API.md) and the UI scripts.

Keep new fields optional on the controller side so older agents continue to
work during rolling upgrades.

### Add a UI page

1. Create `web/<name>.html` and `web/static/<name>.js` (no inline scripts; the
   CSP forbids them).
2. Add the route to the table in `Handler()` (`server.go`).
3. Add the script (and any CSS) to `assetTypes` in `server.go`; only listed
   names are served.
4. Fetch data from the JSON API; don't put cluster data in the HTML.

### Add a setting

1. Parse it in `controllerConfigFromEnv` or `agentConfigFromEnv` (use
   `envOr`/`envBool`/`envFloat` and return an error for invalid values).
2. Add it to `usage()` in `main.go` and to `.env.example`.
3. Document it in [CONFIGURATION.md](CONFIGURATION.md).

### Smarter routing

`Registry.CandidatesFor` is the single place that orders nodes. To weight by
load, sort/rotate `eligible` using `CPU`/`RAM` (note they're `*float64` and
may be `nil`). The fail-over rule in `forward` should stay as is.

## Conventions

- **Standard library first.** The only third-party import is `gopsutil`.
- **Never log secrets or URLs from errors.** Use `errName(err)`, as elsewhere.
- **Validate everything agents send.** Agents are semi-trusted.
- **Never take upstream paths or bodies for pulls from clients.**
- **Return errors from handlers** wrapped with `api(...)`; use `verr(...)` for
  client mistakes (400).
- **Keep state in memory and computed on read**, unless a persistence layer is
  added deliberately.
- Run `gofmt` and `go vet ./...` before committing.

## Releasing

```bash
make cross   # linux amd64/arm64, darwin arm64, windows amd64 -> ./dist
```

Release the binaries together with the `web/` folder (the controller needs it),
`.env.example`, and the `docs/`. Note the version in the release notes, and call
out any change to heartbeat/register payloads so users upgrade agents and
controller together.
