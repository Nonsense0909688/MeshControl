# MeshControl

Run several [Ollama](https://ollama.com) machines on your LAN as **one cluster**.

MeshControl gives you a single address to chat with and call. A request for a
model goes to a node that has it, requests are spread across nodes (round-robin),
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

> **Status:** this Go version is a port of the original Python
> `controller.py` / `agent.py`. API shapes and behaviour are kept the same.
> Run `go vet ./...` and `go build` once on your machine before deploying.

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

At minimum set `AGENT_TOKEN` (same value on the controller and every agent) and,
on the controller, `ADMIN_TOKEN`. Generate strong values with:

```bash
openssl rand -base64 32
```

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
#       CONTROLLER_URL=http://<controller-ip>:8000
#       AGENT_TOKEN=<same token as the controller>
./meshcontrol agent
```

**5. Make Ollama reachable from the LAN.** By default Ollama only listens on
`127.0.0.1`. The controller then shows the node as `provider_unreachable`.
Set `OLLAMA_HOST=0.0.0.0` for the Ollama service (see
[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)).

**6. Open** `http://<controller-ip>:8000/` for the dashboard and `/chat` to talk to the cluster.

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

The most important variables:

| Variable | Role | Default | Meaning |
|---|---|---|---|
| `AGENT_TOKEN` | both | *(required)* | Shared secret agents use to talk to the controller |
| `ADMIN_TOKEN` | controller | empty | Protects UI data, API and proxy. **Empty = open.** |
| `CONTROLLER_URL` | agent | *(required)* | e.g. `http://10.0.0.5:8000` |
| `WEB_DIR` | controller | `.` | Folder with the HTML pages and `static/` |
| `CONTROLLER_HOST` / `CONTROLLER_PORT` | controller | `0.0.0.0` / `8000` | Listen address |
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

If `ADMIN_TOKEN` is set, the UI asks for it the first time it calls the API.

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
3. Eligible nodes are rotated round-robin so consecutive requests spread out.
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
server.go        controller HTTP routes, web serving, streaming proxy
pulls.go         background model-pull jobs
util.go          shared helpers
.env.example     every setting with its default
deploy/          systemd units, Dockerfile, docker-compose
docs/            detailed documentation
```

## More documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): how it works inside
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md): every setting
- [docs/API.md](docs/API.md): every endpoint, request and response
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md): systemd, Docker, TLS reverse proxy, upgrades
- [docs/SECURITY.md](docs/SECURITY.md): threat model and hardening checklist
- [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md): symptoms and fixes
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md): code tour, testing, extending