# MeshControl

Lightweight, self-hosted control plane for a LAN cluster of Ollama instances:
node health, cluster-wide model inventory, and a streaming-capable reverse proxy
that routes requests to a node hosting the requested model.

This is **Phase 1** (reliable core) of the roadmap. State is in memory.

```
 client ──► controller :8000 ──► node A ollama :11434
                 ▲   │ ──────► node B ollama :11434
        heartbeat│   
   agent A   agent B   (each agent only reports; it never carries inference traffic)
```

## Quick start

```bash
pip install -r requirements.txt
export AGENT_TOKEN=$(python -c "import secrets; print(secrets.token_urlsafe(32))")
export ADMIN_TOKEN=$(python -c "import secrets; print(secrets.token_urlsafe(32))")

# central machine
python controller.py

# every Ollama machine (copy AGENT_TOKEN across)
export CONTROLLER_URL=http://<controller-ip>:8000
python agent.py
```

Ollama must listen on the LAN, e.g. `OLLAMA_HOST=0.0.0.0:11434`. If it only binds
to localhost the agent reports `ollama_lan_reachable: false` and the node is skipped
by automatic routing.

```bash
H="Authorization: Bearer $ADMIN_TOKEN"
curl -H "$H" localhost:8000/api/nodes
curl -H "$H" localhost:8000/api/models
curl -H "$H" localhost:8000/api/ollama/api/tags
curl -H "$H" localhost:8000/api/ollama/api/generate \
     -d '{"model":"qwen3.5:0.8b","prompt":"Hello"}'                 # auto-routed, streamed
curl -H "$H" localhost:8000/api/nodes/nandiastra/ollama/api/chat \
     -d '{"model":"qwen3.5:0.8b","messages":[{"role":"user","content":"Hi"}]}'
```

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/api/agent/register` | agent | Create/refresh a node record |
| POST | `/api/agent/heartbeat` | agent | Update metrics + models. `404 {"reregister":true}` if unknown |
| GET | `/api/nodes`, `/api/nodes/<n>`, `/api/nodes/<n>/models` | admin | Node info (with computed `online`) |
| GET | `/api/models` | admin | Every (model, node) pair + availability, size, parameters, quantization, family, digest, `modified_at` |
| GET | `/api/ollama/api/tags` | admin | Aggregated tags from online nodes (cluster view, not native Ollama) |
| POST | `/api/ollama/api/{generate,chat,embeddings,embed}` | admin | Auto-routed by `model` |
| POST | `/api/nodes/<n>/pull` | admin | Start pulling `{"model": "..."}` on a node (202, job record) |
| GET / DELETE | `/api/pulls`, `/api/pulls/<id>` | admin | Pull progress / cancel |
| POST | `/api/nodes/<n>/ollama/api/{generate,chat,embeddings,embed}` | admin | Direct to a node |
| GET | `/`, `/nodes`, `/nodes/<n>`, `/models`, `/models/<m>`, `/chat`, `/static/*` | none | Web UI (static, no data) |
| GET | `/healthz` | none | Liveness |

"admin" = `ADMIN_TOKEN` bearer (open if unset). The agent token never grants admin access.

## Web UI

Open `http://<controller-ip>:8000/`. Each page is its own static HTML file with its own script:

| URL | File | Page |
|---|---|---|
| `/` | `dashboard.html` | Overview: stats, cluster map, activity, CPU/RAM chart |
| `/nodes` | `nodes.html` | All nodes, filter All / Online / Provider unreachable / Offline |
| `/nodes/<n>` | `node.html` | One node: status banner, metrics, installed models (size, parameters, downloaded) |
| `/models` | `models.html` | Cluster-wide model inventory (family, parameters, quantization, size, downloaded, nodes) and **Pull a model** |
| `/models/<m>` | `model.html` | One model: every copy with size/date/digest, pull to another node, `curl` example |
| `/chat` | `chat.html` | Streaming chat with markdown, code blocks, reasoning blocks; auto-routed or pinned (`?model=...&node=...`) |

Shared code: `static/common.js` (shell, API, polling), `static/pulls.js`, `static/app.css`, `static/theme.js`.
No build step, no dependencies, no inline scripts (CSP `script-src 'self'`). Pages contain no cluster data and all
node-supplied text is inserted as plain text. History charts, the activity feed and the chat transcript are kept per browser tab.

### Node status

| Status | Colour | Meaning |
|---|---|---|
| Online | green | Heartbeat is fresh and Ollama is reachable from the LAN |
| **Provider unreachable** | **orange** | The agent is heartbeating, but Ollama cannot be reached (not running, bound to 127.0.0.1, or firewalled). Skipped by routing; pulls are refused |
| Offline | red | No heartbeat within `OFFLINE_AFTER` |

The API exposes this as `status` (`online` / `provider_unreachable` / `offline`) on every node.

### Pulling models

Use **Pull a model** on `/models` (or "Pull to a node" on a model page). The controller tells the chosen node's Ollama to
pull and tracks progress as a background job, so a download keeps running if you close the page; a sidebar indicator shows
active downloads. Cancel from the UI, or `DELETE /api/pulls/<id>`. The model appears in the inventory after the node's next heartbeat.

Only the model name is taken from the client; the upstream request is built by the controller. Names that start with a
registry host must use one listed in `PULL_REGISTRIES` (default `registry.ollama.ai,hf.co,huggingface.co`).
Pulling downloads many GB onto a node, so set `ADMIN_TOKEN`.

## Behaviour worth knowing

- **Online** = heartbeat within `OFFLINE_AFTER` (20 s). Computed on read; one record per node ID.
- **Routing** (auto): model must be hosted by an *online* node reporting Ollama reachable.
  `llama3` and `llama3:latest` match. Eligible nodes are rotated round-robin.
  Unknown model → 404; known but no eligible node → 503.
- **Failover**: only when the TCP connection to a node provably failed (request never sent).
  Once any response byte is relayed, or a read times out, there is **no retry**, so inference is never replayed.
  A mid-stream upstream failure cuts the connection so clients see an incomplete stream.
- **Streaming**: upstream chunks are relayed as they arrive (NDJSON preserved).
- **Controller restart**: agents get a 404 on their next heartbeat and re-register automatically.
  Agents retry registration with exponential backoff (max 30 s).
- Stale nodes (silent > `PURGE_AFTER`) are dropped.

## Security

- `AGENT_TOKEN` is required (no default, never in code); compared in constant time; never logged.
- Registered `ollama_url` is the only upstream the proxy can reach. It must be a bare
  `http(s)://IP:port` inside `ALLOWED_NODE_NETWORKS` (private ranges by default; link-local/cloud
  metadata excluded). Credentials, paths, queries and hostnames are rejected. Only
  `generate|chat|embeddings|embed` paths are proxied, and client `Authorization` headers are not forwarded.
- With `ALLOW_HOSTNAMES=true`, DNS is resolved only at registration (DNS-rebinding caveat); prefer IPs.
- Agent-reported metrics/reachability are advisory. Failover covers a node that lies about reachability.
- A shared token means any holder can register as any node ID. Per-node credentials are Phase 4.
- Set `ADMIN_TOKEN`, and firewall ports 8000 and 11434 to the LAN / controller only.
- For anything past development, put it behind a production WSGI server and TLS:
  `waitress-serve --threads=32 --call controller:create_app` (needs `AGENT_TOKEN` in env).

## Tests

```bash
pip install -r requirements-dev.txt
pytest
```

The suite includes a real fake-Ollama HTTP server, so chunked streaming, failover and
header handling are tested over actual sockets.

## Known limitations (by design for Phase 1)

In-memory state (lost on restart), no metrics history, no load-based scheduling,
no model sync between nodes, single shared token.

## Next

Phase 2: SQLite persistence + metrics history. Phase 3: least-loaded routing, in-flight request
tracking, node priorities. Phase 4: per-node credentials.
