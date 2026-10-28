# MeshControl
[![Go checks](https://github.com/Nonsense0909688/MeshControl/actions/workflows/test.yml/badge.svg)](https://github.com/Nonsense0909688/MeshControl/actions/workflows/test.yml) 

Run several [Ollama](https://ollama.com) machines on your LAN as **one cluster**.

MeshControl gives you a single address to chat with and call. A request for a
model goes to a node that has it, requests are routed using reported CPU/RAM load and active requests (round-robin breaks ties),
and a built-in web UI shows every node, its CPU/RAM, its models, and lets you
pull new models onto any node.

It ships as **one binary with two roles**:

| Role | Where it runs | What it does |
|---|---|---|
| `controller` | one machine | Web UI, API, node registry, and a streaming reverse proxy for Ollama |
| `agent` | every machine running Ollama | Registers with the controller and sends a heartbeat (CPU, RAM, installed models) every few seconds. It never carries inference traffic. |

```
 browser / your apps                      Ollama machines
        │                          ┌────────────────────────────┐
        ▼                          │  node-a   agent ─┐ Ollama  │
 ┌──────────────┐  heartbeats      │           (5s)   │ :11434  │
 │  controller  │◄─────────────────┤                  ▼         │
 │  :8000       │                  └────────────────────────────┘
 │  UI + API    │  proxied /api/chat, /api/generate, ...
 │  + proxy     ├──────────────────────────────► node-a / node-b / ...
 └──────────────┘        (streaming, straight to Ollama)
```

> **Security defaults:** the controller binds to loopback and fails closed when required credentials are missing or weak. For LAN access, use native HTTPS or a TLS reverse proxy; do not expose plain HTTP to untrusted networks. Run the test suite and `go vet ./...` before deploying.

## Contents

- [Quick start](#quick-start)
- [Requirements](#requirements)
- [Building](#building)
- [Configuration](#configuration)
- [Using the cluster](#using-the-cluster)
- [Web UI files](#web-ui-files)
- [Project layout](#project-layout)
- [More documentation](#more-documentation)

## Quick start

**1. Build**

```bash
go mod tidy
go build -o meshcontrol .
```

**2. Configure.** Copy the example and edit it. Both roles read the same kind of file.

```bash
cp .env.example .env
```

In shared mode, set `AGENT_TOKEN` to the same random value on the controller and every agent, and set a different `ADMIN_TOKEN`. For per-node isolation, set `AGENT_TOKEN=` and configure `AGENT_TOKENS=node-a:<secret>,node-b:<secret>` on the controller; each agent then uses only its own mapped secret. Every secret must be at least 32 characters and must not be a placeholder. Generate each independently with:

```bash
openssl rand -hex 32
```

In `AGENT_TOKENS`, each `node-id:secret` entry is generated independently; the agent on that node sets its matching secret as `AGENT_TOKEN` and the matching `NODE_ID`.

For direct LAN access, set `CONTROLLER_HOST=0.0.0.0` and configure `TLS_CERT_FILE` plus `TLS_KEY_FILE` (native HTTPS). With a same-host reverse proxy, keep `CONTROLLER_HOST=127.0.0.1` and let the proxy handle HTTPS. Non-loopback plaintext HTTP is rejected unless you deliberately set `ALLOW_INSECURE_HTTP=true` for a trusted internal lab only. `WEB_DIR=./web` is the default. Change `ALLOWED_NODE_NETWORKS`
and `ALLOWED_NODE_PORTS` only to the private subnet(s) and Ollama port(s) you use.

**3. Start the controller** (on the central machine):

```bash
# .env: MESHCONTROL_MODE=controller, WEB_DIR=./web, tokens set
./meshcontrol
# or explicitly
./meshcontrol controller
```

**4. Start an agent** on each Ollama machine:

```bash
# .env: MESHCONTROL_MODE=agent
#       CONTROLLER_URL=https://<controller-host>:8000 (native TLS)
#       AGENT_TOKEN=<shared token, or this node's mapped secret>
#       NODE_ID=<matching node-id when using AGENT_TOKENS>
./meshcontrol agent
```

**5. Make Ollama reachable from the LAN.** By default Ollama only listens on
`127.0.0.1`. The controller then shows the node as `provider_unreachable`.
Set `OLLAMA_HOST=0.0.0.0` for the Ollama service (see
[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)).

**6. Open** `https://<controller-host>:8000/` for the dashboard and `/chat` to talk to the cluster when using native HTTPS. When using a reverse proxy, open its HTTPS URL instead. A loopback-only controller is not directly reachable from other machines.

## Requirements

- **Go 1.22+** to build (uses `net/http` method/wildcard routing and `log/slog`).
- **Ollama** on each agent machine.
- Linux, macOS or Windows. The only third-party dependency is
  [`gopsutil`](https://github.com/shirou/gopsutil) (agent CPU/RAM readings).
- The controller and agents must reach each other over the network
  (agent → controller, controller → every node's Ollama port).

## Building

```bash
go build -o meshcontrol .                         # your platform
GOOS=linux  GOARCH=arm64 go build -o meshcontrol-linux-arm64 .   # e.g. Raspberry Pi
GOOS=windows GOARCH=amd64 go build -o meshcontrol.exe .
make build        # same as the first line, see Makefile
make cross        # linux amd64/arm64, darwin arm64, windows amd64 into ./dist
```

`CGO_ENABLED=0` works and gives a fully static binary.

## Configuration

Settings come from environment variables, optionally loaded from a `.env` file.

- `./.env` is loaded automatically if present.
- `--env-file PATH` or `ENV_FILE=PATH` loads a different file (and a missing file is then an error).
- **Real environment variables always win over the file.**
- `MESHCONTROL_MODE=controller|agent` lets you start with no argument.
- `meshcontrol version` prints build metadata.
- `meshcontrol check-config controller|agent` validates configuration without starting.
- `meshcontrol doctor controller|agent` validates settings and probes the configured services.

The most important variables:

| Variable | Role | Default | Meaning |
|---|---|---|---|
| `AGENT_TOKEN` | controller + agent | *(required in shared mode)* | Shared secret; in per-node mode agents set their own mapped secret and the controller sets `AGENT_TOKENS` |
| `AGENT_TOKENS` | controller | unset | Optional unique `node-id:secret` pairs for per-node agent authentication |
| `ADMIN_TOKEN` | controller | *(required)* | Protects UI API, node/model endpoints, pulls and inference proxy; missing/weak secrets fail closed. |
| `CONTROLLER_URL` | agent | *(required)* | e.g. `http://10.0.0.5:8000` |
| `WEB_DIR` | controller | `./web` | Folder with the HTML pages and `static/` |
| `CONTROLLER_HOST` / `CONTROLLER_PORT` | controller | `127.0.0.1` / `8000` | Listen address/port; non-loopback listeners require TLS or `ALLOW_INSECURE_HTTP=true`. |
| `ALLOWED_NODE_PORTS` | controller | `11434` | Ollama ports allowed for registered nodes and outbound proxy/pull connections. |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | controller | unset | Enable native HTTPS with a valid cert/key pair. |
| `MAX_CONCURRENT_REQUESTS` | controller | `256` | Bounded active HTTP requests; overflow returns `503`. |
| `MAX_BODY_MB` | controller | `8` | Maximum JSON request body (1–64 MiB). |
| `MAX_CONCURRENT_PULLS` / `_PER_NODE` | controller | `4` / `1` | Global and per-node active pull limits. |
| `NODE_ID` | agent | hostname | Name shown in the UI |
| `OLLAMA_ADVERTISE_URL` | agent | auto | URL the controller uses to reach this node's Ollama |

The full list with ranges, defaults and examples is in
[docs/CONFIGURATION.md](docs/CONFIGURATION.md).

## Using the cluster

### Web UI

| Page | Purpose |
|---|---|
| `/` | Dashboard: cluster overview |
| `/nodes`, `/nodes/<node>` | Node list and detail: status, CPU, RAM, models |
| `/models`, `/models/<model>` | Models across the cluster and where they live; pull a model onto a node |
| `/chat` | Streaming chat, auto-routed or pinned to one node |

The UI asks for `ADMIN_TOKEN` the first time it calls the API. All API routes fail closed without a valid bearer token.

### API (Ollama's own API behind one address)

Auto-routed chat (the controller picks a node that hosts the model):

```bash
curl -N http://CONTROLLER:8000/api/ollama/api/chat \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"model":"llama3.2:3b","messages":[{"role":"user","content":"Hello!"}],"stream":true}'
```

Pinned to a node:

```bash
curl -N http://CONTROLLER:8000/api/nodes/gpu-box/ollama/api/generate \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"model":"llama3.2:3b","prompt":"Why is the sky blue?"}'
```

Pull a model onto a node (runs in the background):

```bash
curl -X POST http://CONTROLLER:8000/api/nodes/gpu-box/pull \
  -H "Authorization: Bearer $ADMIN_TOKEN" -d '{"model":"qwen2.5:7b"}'
curl http://CONTROLLER:8000/api/pulls -H "Authorization: Bearer $ADMIN_TOKEN"
```

Proxied endpoints: `generate`, `chat`, `embeddings`, `embed`. Everything is
documented in [docs/API.md](docs/API.md).

### How a request is routed

1. The model name is normalised (`llama3` and `llama3:latest` are the same model).
2. Nodes that are **online**, report Ollama as **LAN-reachable**, and host the model are eligible.
3. Eligible nodes are scored by reported CPU/RAM utilization and active in-flight requests; round-robin rotation breaks ties.
4. If connecting to a node fails outright, the next eligible node is tried. Once a
   request has reached a node it is **never replayed** elsewhere.

The response header `X-MeshControl-Node` tells you which node answered.

### Node states

| State | Meaning |
|---|---|
| `online` | Recent heartbeat and Ollama reachable over the LAN |
| `provider_unreachable` | Agent is alive, but Ollama can't be reached from the LAN (usually bound to `127.0.0.1`) |
| `offline` | No heartbeat for `OFFLINE_AFTER` seconds (default 20) |

## Web UI files

The controller serves the UI from `WEB_DIR`; it does not embed it. Expected layout:

```
web/
├── dashboard.html  nodes.html  node.html  models.html  model.html  chat.html
└── static/
    ├── app.css  theme.js  common.js  pulls.js
    └── dashboard.js  nodes.js  node.js  models.js  model.js  chat.js
```

Only the files in that list are served from `/static/`. Pages are sent with a strict
Content-Security-Policy (`script-src 'self'`, no inline scripts, no third-party hosts).

## Project layout

```
main.go          entry point: picks controller or agent, loads .env
dotenv.go        small .env parser
agent.go         agent: register, heartbeat, Ollama inventory
controller.go    controller config, validation, node registry
server.go         controller HTTP routes, web serving, streaming proxy
pulls.go          background model-pull jobs
*_test.go         unit, integration, security, concurrency and fuzz tests
Makefile          build, test, race, vet, benchmark and cross-build targets
.env.example      example configuration and secure defaults
.github/workflows/test.yml  CI test, race, vet and build workflow
docs/             configuration, API, security and deployment documentation
```

## Testing and CLI diagnostics

Run the unit, integration, concurrency, and fuzz-seed tests with:

```bash
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench . ./...
```

Fuzz the input-validation boundaries with `go test -fuzz FuzzValidateOllamaURL -fuzztime 30s` (or another `Fuzz...` target). `make test` and `make test-race` are convenience targets. In a configured environment, run `./meshcontrol check-config controller` before starting and `./meshcontrol doctor agent` on a node.

## More documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): how it works inside
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md): every setting
- [docs/API.md](docs/API.md): every endpoint, request and response
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md): systemd, Docker, TLS reverse proxy, upgrades
- [docs/SECURITY.md](docs/SECURITY.md): threat model and hardening checklist
- [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md): symptoms and fixes
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md): code tour, testing, extending