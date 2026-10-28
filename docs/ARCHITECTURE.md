# Architecture

How MeshControl works inside. For setup see the [README](../README.md); for exact
endpoints see [API.md](API.md).

## Contents

- [Overview](#overview)
- [Components](#components)
- [Node lifecycle](#node-lifecycle)
- [Node states](#node-states)
- [Request routing](#request-routing)
- [The streaming proxy](#the-streaming-proxy)
- [Model pulls](#model-pulls)
- [Web UI serving](#web-ui-serving)
- [State and restarts](#state-and-restarts)
- [Concurrency model](#concurrency-model)
- [Design decisions](#design-decisions)

## Overview

MeshControl is one Go binary (`meshcontrol`) that runs in one of two modes,
chosen by the first argument or `MESHCONTROL_MODE`.

```
                    heartbeat (HTTP POST, every HEARTBEAT_INTERVAL)
   ┌───────────┐ ───────────────────────────────────────────► ┌────────────────┐
   │  agent    │                                              │  controller    │
   │ (per node)│ ◄─── 404 {"reregister":true} if unknown ──── │                │
   └─────┬─────┘                                              │  registry      │
         │ GET /api/tags (local + LAN address)                │  pull manager  │
         ▼                                                    │  proxy         │
   ┌───────────┐ ◄──── proxied /api/chat, /api/generate ───── │  web UI files  │
   │  Ollama   │ ◄──── POST /api/pull (background jobs) ───── └────────────────┘
   └───────────┘                                                      ▲
                                                           browser / curl / apps
```

Two traffic paths exist and they never mix:

- **Control path (agent → controller).** Small JSON heartbeats. Authenticated with
  `AGENT_TOKEN`.
- **Data path (controller → Ollama).** Inference and pull requests go from the
  controller straight to a node's Ollama port. The agent is not involved.

## Components

| Source file | Responsibility |
|---|---|
| `main.go` | Parses `[--env-file PATH] <controller\|agent>`, loads the `.env` file, sets up `log/slog`, dispatches to `runController()` or `runAgent()`. |
| `dotenv.go` | Dependency-free `.env` parser. Never overrides variables already present in the real environment. |
| `agent.go` | Agent config, Ollama inventory (`/api/tags`), LAN-address discovery, register/heartbeat loop with exponential back-off. |
| `controller.go` | Controller config, input validation, the in-memory node `Registry`, model-name normalisation and candidate discovery. |
| `server.go` | HTTP routes, bearer-token auth, static UI serving, the streaming reverse proxy. |
| `pulls.go` | `PullManager`: background `ollama pull` jobs with progress tracking and cancellation. |
| `util.go` | Env helpers, `idleTimer`, small formatting/safety helpers. |

## Node lifecycle

1. **Start.** The agent reads its config, works out the URL at which the
   controller can reach its Ollama (the *advertised URL*, see below) and primes
   the CPU counter.
2. **Register.** `POST /api/agent/register` with `{node, ollama_url}`. The
   controller validates both (see [SECURITY.md](SECURITY.md)) and creates the
   record (`201`) or refreshes it (`200`).
3. **Heartbeat loop.** Every `HEARTBEAT_INTERVAL` seconds the agent:
   1. asks Ollama for `/api/tags` via the advertised URL;
   2. if that fails, tries `OLLAMA_LOCAL_URL`;
   3. samples CPU and RAM with `gopsutil`;
   4. posts `{node, cpu, ram, ollama_lan_reachable, models, model_details}`.
4. **Re-register.** If the controller answers `404` (it restarted and forgot the
   node), the agent registers again immediately.
5. **Failure handling.** If registration fails, the agent retries with back-off
   (1 s, doubling, capped at 30 s). A failed heartbeat is logged and retried on
   the next tick.
6. **Shutdown.** `SIGINT`/`SIGTERM` stop the loop. There is no deregistration;
   the controller simply stops hearing from the node and marks it `offline`.

### Advertised URL and reachability detection

The advertised URL is what the controller will use to reach Ollama. If
`OLLAMA_ADVERTISE_URL` is unset, the agent opens a UDP "connection" towards the
controller's host (no packets are sent) so the OS picks the outgoing interface,
and uses that interface's IP with `OLLAMA_PORT`.

`ollama_lan_reachable` is derived from *how* the model list was obtained:

| Result | `ollama_lan_reachable` | Models reported |
|---|---|---|
| `/api/tags` via the advertised URL succeeds | `true` | from Ollama |
| Advertised fails, `OLLAMA_LOCAL_URL` succeeds | `false` | from Ollama |
| Both fail | `false` | empty list |

The second row is the classic "Ollama is bound to `127.0.0.1`" case: the agent
can see Ollama locally, but the LAN address does not answer.

## Node states

State is computed on read, not stored.

| State | Condition |
|---|---|
| `offline` | `now - last_seen > OFFLINE_AFTER` |
| `provider_unreachable` | heartbeat is fresh and `ollama_lan_reachable == false` |
| `online` | heartbeat is fresh and `ollama_lan_reachable == true` |

Nodes with no heartbeat for more than `PURGE_AFTER` are deleted from the
registry the next time anyone reads it.

## Request routing

For an auto-routed request (`POST /api/ollama/api/{endpoint}`):

1. The body is read (configurable limit, default 8 MiB and maximum 64 MiB) and must be a JSON object with a non-empty
   string `model`.
2. `normModel` normalises the name: a name whose last path segment has no `:` gets
   `:latest` appended, so `llama3` equals `llama3:latest`. Names such as
   `hf.co/org/repo` are normalised the same way.
3. **Hosting** nodes are those whose reported models match. **Eligible** nodes are
   the hosting ones that are `online` and LAN-reachable.
4. Candidates are scored by reported CPU/RAM utilization and in-flight proxy
   requests. The registry round-robin counter breaks ties between similarly loaded nodes.
5. Outcomes:
   - nobody hosts the model → `404`;
   - hosted but nobody eligible → `503`;
   - otherwise the request is forwarded to the first eligible node in the load-ordered candidate list.

The pinned route (`/api/nodes/{node}/ollama/api/{endpoint}`) skips model lookup
and sends the request to that one node, provided the node exists and is not
`offline`.

Routing is load-aware but intentionally lightweight: it combines agent-reported
CPU/RAM percentages with active proxy requests, including streaming responses. It
does not predict model load or token-generation speed, so measure real workloads
before tuning the weights.

## The streaming proxy

`forward()` and `relay()` in `server.go` implement the proxy.

- **Allowed endpoints.** Only `generate`, `chat`, `embeddings` and `embed` are
  proxied. The upstream path is built as `/api/<endpoint>` from that fixed list,
  never from client input.
- **Headers.** The upstream request carries only `Content-Type: application/json`.
  The client's `Authorization` header is never forwarded.
- **Fail-over rule.** The next candidate is tried **only** when dialling the
  node fails (`net.OpError` with `Op == "dial"`). Once a connection was
  established, the request is never replayed on another node, because inference
  may already have started and replays could duplicate work or side effects.
- **Timeouts.** `CONNECT_TIMEOUT` bounds the dial. `READ_TIMEOUT` is an *idle*
  timeout: an `idleTimer` is reset on every chunk, so long generations are fine
  as long as bytes keep flowing. There is no overall deadline.
- **Streaming.** The response is copied in 32 KiB chunks and flushed after each
  one, which preserves Ollama's NDJSON streaming. `X-Accel-Buffering: no` is set
  so nginx does not buffer it.
- **Response headers.** The upstream status code and `Content-Type` are passed
  through; `X-MeshControl-Node` names the node that served the request.
- **Mid-stream failure.** After headers are sent the status cannot change. If the
  upstream breaks mid-stream the handler aborts the connection
  (`http.ErrAbortHandler`) so the client sees a truncated stream rather than a
  clean end.
- **Client disconnects.** The upstream request is tied to the client's context,
  so closing the client connection cancels the upstream request.

Errors produced by the controller itself: `502` (could not connect to any
candidate / upstream error), `504` (idle timeout hit before response headers).

## Model pulls

`PullManager` runs pulls as goroutines so they continue after the browser
navigates away.

- `Start` rejects a duplicate (same node, same normalised model, still running).
- The controller builds the request body itself (`{"model": ..., "stream": true}`)
  and POSTs it to the node's `/api/pull`.
- Ollama's NDJSON progress frames are parsed. Per-layer `total`/`completed` are
  tracked by digest and summed into the job's `total` and `completed`; the latest
  `status` string is stored.
- A job ends as `success` (a `success` status frame was seen), `error`, or
  `cancelled`. `Cancel` cancels the request context.
- The same idle timer (`READ_TIMEOUT`) aborts pulls that stall.
- At most 50 *finished* jobs are retained; the oldest are dropped.
- After a pull succeeds, the model appears in the cluster on the node's next
  heartbeat (within `HEARTBEAT_INTERVAL`).

## Web UI serving

The controller serves HTML from `WEB_DIR`; nothing is embedded in the binary.
Each page route maps to one static HTML file that contains no cluster data. The
JavaScript fetches data from the JSON API, sending the admin token. Only an
explicit allow-list of file names is served under `/static/`. Pages carry a
strict `Content-Security-Policy`; see [SECURITY.md](SECURITY.md).

## State and restarts

All state is in memory:

| State | Lost on controller restart? | Recovery |
|---|---|---|
| Node registry | yes | Agents get `404`, re-register, and the next heartbeat refills models |
| Pull jobs | yes | In-flight pulls are aborted from the controller's side; Ollama itself may continue or resume on re-request |
| Round-robin tie-breaker counter | yes | Resets to zero |

Agents hold no persistent state either. There is no database and nothing to
back up apart from the `.env` files.

## Concurrency model

- `Registry` guards its map with one mutex; the round-robin counter is an atomic.
  Views handed out are copies, so callers never touch live records.
- `PullManager` guards its job map with a mutex; each pull has its own goroutine.
- The HTTP server uses Go's one-goroutine-per-connection model with a bounded
  request semaphore; proxy requests use a shared `http.Client` with bounded
  per-host connections. In-flight proxy requests feed the load-aware scheduler.
- Shutdown on `SIGINT`/`SIGTERM` waits up to 5 s for requests, then closes
  connections so long streams don't block exit.

## Design decisions

- **Agent never proxies.** Keeps inference latency and bandwidth off the control
  path and keeps the agent tiny.
- **Controller dials Ollama directly.** That is also why the agent must prove
  *LAN* reachability rather than just "Ollama is running".
- **Fail over only on dial failure.** Avoids duplicate generations.
- **Allow-listed endpoints, server-built pull bodies, validated URLs.** The
  controller never forwards arbitrary client-chosen paths or targets.
- **State on read.** `online`/`offline` is computed from `last_seen`, so there is
  no background reaper to get out of sync.
- **Dependencies.** Only `gopsutil` (agent metrics); `.env` parsing and everything
  else use the standard library.
