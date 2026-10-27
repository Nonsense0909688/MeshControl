# API reference

All endpoints are served by the controller (default `http://CONTROLLER:8000`).
Bodies and responses are JSON unless noted.

## Contents

- [Conventions](#conventions)
- [Authentication](#authentication)
- [Health](#health)
- [Agent endpoints](#agent-endpoints)
- [Nodes and models](#nodes-and-models)
- [Model pulls](#model-pulls)
- [Ollama proxy](#ollama-proxy)
- [Web UI routes](#web-ui-routes)
- [Data types](#data-types)
- [Error reference](#error-reference)

## Conventions

- Errors are `{"error": "<message>"}` with a matching HTTP status, sometimes with
  extra fields (documented per endpoint). Any unmatched route returns
  `404 {"error":"not found"}`.
- Timestamps (`last_seen`, `started_at`, `finished_at`, `modified_at`) are Unix
  epoch **seconds** as floating-point numbers.
- Model names are compared after normalisation: `llama3` ≡ `llama3:latest`.
- Examples assume `ADMIN_TOKEN` is set. If it is empty, drop the
  `Authorization` header.

## Authentication

| Group | Token | Header |
|---|---|---|
| Agent endpoints | `AGENT_TOKEN` | `Authorization: Bearer <token>` |
| Nodes, models, pulls, proxy | `ADMIN_TOKEN` (no check when `ADMIN_TOKEN` is empty) | `Authorization: Bearer <token>` |
| `/healthz`, web UI pages, `/static/*` | none | |

A missing or wrong token returns `401 {"error":"unauthorized"}`. Token
comparison is constant-time.

## Health

### `GET /healthz`

No auth.

```json
{"status": "ok", "nodes_online": 2}
```

`nodes_online` counts nodes whose heartbeat is fresh (including
`provider_unreachable` ones).

## Agent endpoints

Used by the agent; documented for custom agents and debugging. Body limit
256 KiB (larger → `400 request body too large`).

### `POST /api/agent/register`

```json
{"node": "gpu-box", "ollama_url": "http://10.0.0.21:11434"}
```

| Field | Rules |
|---|---|
| `node` | 1–64 chars `[A-Za-z0-9._-]`, first char alphanumeric |
| `ollama_url` | `http`/`https`; IP literal (or hostname if `ALLOW_HOSTNAMES=true`); host inside `ALLOWED_NODE_NETWORKS`; no credentials, path, query or fragment. A missing port defaults to 80/443. With `VERIFY_SOURCE_IP=true` the host must equal the caller's IP. |

Response: `201` for a new node, `200` when refreshing an existing one:

```json
{"status": "registered", "node": "gpu-box", "created": true}
```

Errors: `400` (validation), `401`.

### `POST /api/agent/heartbeat`

```json
{
  "node": "gpu-box",
  "cpu": 12.5,
  "ram": 48.1,
  "ollama_lan_reachable": true,
  "models": ["llama3.2:3b", "qwen2.5:7b"],
  "model_details": [
    {"name": "llama3.2:3b", "size": 2019393189, "modified_at": 1758000000.5,
     "digest": "a80c4f17acd5", "family": "llama", "parameter_size": "3.2B",
     "quantization_level": "Q4_K_M", "format": "gguf"}
  ]
}
```

| Field | Rules |
|---|---|
| `cpu`, `ram` | numbers 0–100 |
| `ollama_lan_reachable` | boolean (required) |
| `models` | list of ≤ 1000 non-empty strings (≤ 256 chars); duplicates dropped |
| `model_details` | optional. Invalid entries/fields are silently dropped. Entries for names not in `models` are ignored. |

Response `200 {"ok": true}`.

If the node is unknown (controller restarted or node purged):
`404 {"error": "node not registered", "reregister": true}`. A well-behaved agent
registers again.

## Nodes and models

### `GET /api/nodes`

```json
{
  "nodes": [
    {
      "node": "gpu-box",
      "ollama_url": "http://10.0.0.21:11434",
      "cpu": 12.5,
      "ram": 48.1,
      "ollama_lan_reachable": true,
      "models": ["llama3.2:3b"],
      "model_details": {"llama3.2:3b": {"size": 2019393189, "modified_at": 1758000000.5,
        "digest": "a80c4f17acd5", "family": "llama", "parameter_size": "3.2B",
        "quantization_level": "Q4_K_M", "format": "gguf"}},
      "last_seen": 1761500000.123,
      "online": true,
      "status": "online"
    }
  ]
}
```

Sorted by node name. `cpu` and `ram` are `null` until the first heartbeat after
registration. Stale nodes (older than `PURGE_AFTER`) are removed before the list
is built.

### `GET /api/nodes/{node}`

Returns one node object as above. `404 {"error":"node not found"}` if unknown.

### `GET /api/nodes/{node}/models`

```json
{"node": "gpu-box", "online": true, "models": ["llama3.2:3b", "qwen2.5:7b"]}
```

### `GET /api/models`

One row per (model, node) pair, sorted by model then node.

```json
{
  "models": [
    {"model": "llama3.2:3b", "node": "gpu-box", "available": true,
     "size": 2019393189, "modified_at": 1758000000.5, "digest": "a80c4f17acd5",
     "family": "llama", "parameter_size": "3.2B",
     "quantization_level": "Q4_K_M", "format": "gguf"}
  ]
}
```

`available` is `true` when that node is `online` (fresh heartbeat **and**
LAN-reachable). Detail fields are `null` when unknown.

## Model pulls

Pulls run as background jobs on the controller. Poll `GET /api/pulls` for
progress.

### `POST /api/nodes/{node}/pull`

```json
{"model": "qwen2.5:7b"}
```

Model name rules: letters, digits and `. _ : / @ + -`, up to 256 chars, starting
with a letter or digit; no `..` or `//`. A name is treated as starting with a
registry host when its first path segment contains `.` or `:`, or the name has
two or more `/`. Such names must use a registry from `PULL_REGISTRIES`
(for example `hf.co/org/repo`). Plain names (`llama3.2:3b`, `library/llama3`)
are always fine.

Responses:

| Status | Body |
|---|---|
| `202` | the new [PullJob](#pulljob) |
| `400` | invalid body or model name / disallowed registry |
| `404` | `node not found` |
| `503` | node offline: `{"error": "node 'x' is offline", "status": "offline"}` or `{"error": "provider unreachable on node 'x'", "status": "provider_unreachable"}` |
| `409` | the same model is already being pulled on that node |

### `GET /api/pulls`

```json
{"pulls": [ { "id": "9f2c1a7b3d40", "node": "gpu-box", "model": "qwen2.5:7b",
  "state": "running", "status": "pulling 3a4f…", "completed": 734003200,
  "total": 4683073184, "error": null, "started_at": 1761500100.2,
  "finished_at": null } ]}
```

Newest first. Up to 50 finished jobs are kept.

### `DELETE /api/pulls/{pull_id}`

| Status | Meaning |
|---|---|
| `200 {"ok": true}` | cancellation requested |
| `404` | `pull not found` |
| `409` | `pull already finished` |

## Ollama proxy

### `POST /api/ollama/api/{endpoint}`  (auto-routed)

`endpoint` ∈ `generate`, `chat`, `embeddings`, `embed`. Any other value returns
`404`. The body is forwarded unchanged to the chosen node's `/api/{endpoint}`,
so it is exactly Ollama's request format. It must be a JSON object with a
non-empty string `model`.

```bash
curl -N http://CONTROLLER:8000/api/ollama/api/chat \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"model":"llama3.2:3b","messages":[{"role":"user","content":"Hello!"}],"stream":true}'
```

The node is chosen as described in
[ARCHITECTURE.md](ARCHITECTURE.md#request-routing).

### `POST /api/nodes/{node}/ollama/api/{endpoint}`  (pinned)

Same, but always targets `{node}`. It does not check that the node hosts the
model (Ollama will answer). It requires only that the node is not `offline`.
If the node is `provider_unreachable`, expect a `502`.

### Proxy response

- Status code, `Content-Type` and body come from Ollama, streamed as they arrive.
- `X-MeshControl-Node: <node>` identifies the node that answered.
- `Cache-Control: no-cache`, `X-Accel-Buffering: no`.
- Client `Authorization` is **not** forwarded to Ollama.

Controller-generated errors:

| Status | Cause |
|---|---|
| `400` | body is not a JSON object, or `model` missing/empty |
| `404` | unknown endpoint; unknown node (pinned); no node hosts the model (auto) |
| `413` | body larger than 64 MiB |
| `502` | could not connect to any candidate, or error talking to the node |
| `503` | model hosted but no node `online`+reachable (auto); node `offline` (pinned) |
| `504` | node sent nothing for `READ_TIMEOUT` seconds before responding |

If a node fails *after* the response started, the connection is aborted rather
than ended cleanly, so clients see an incomplete stream. A request is never
retried on a second node once a connection was established.

### `GET /api/ollama/api/tags`

Cluster-wide model list in an Ollama-like shape. Not a faithful copy of Ollama's
`/api/tags`: each entry carries the nodes that have the model.

```json
{"models": [{"name": "llama3.2:3b", "model": "llama3.2:3b", "nodes": ["gpu-box", "pi-2"]}]}
```

Only nodes that are not `offline` contribute, including
`provider_unreachable` ones.

## Web UI routes

| Route | File in `WEB_DIR` |
|---|---|
| `GET /` | `dashboard.html` |
| `GET /nodes` | `nodes.html` |
| `GET /nodes/{node}` | `node.html` |
| `GET /models` | `models.html` |
| `GET /models/{model...}` | `model.html` |
| `GET /chat` | `chat.html` |
| `GET /static/{name}` | `static/<name>`, only for: `app.css`, `theme.js`, `common.js`, `pulls.js`, `dashboard.js`, `nodes.js`, `node.js`, `models.js`, `model.js`, `chat.js` |

HTML is served with a strict `Content-Security-Policy`, `X-Content-Type-Options:
nosniff` and `Cache-Control: no-cache`. The pages themselves are not
authenticated and hold no cluster data; the data comes from the API calls above.

## Data types

### Node

| Field | Type | Notes |
|---|---|---|
| `node` | string | Node ID |
| `ollama_url` | string | `scheme://host:port` normalised form |
| `cpu`, `ram` | number \| null | Percent, one decimal |
| `ollama_lan_reachable` | boolean | From the latest heartbeat |
| `models` | string[] | |
| `model_details` | object | model name → ModelDetail |
| `last_seen` | number | Epoch seconds |
| `online` | boolean | Heartbeat is fresh (true for `online` and `provider_unreachable`) |
| `status` | string | `online`, `provider_unreachable` or `offline` |

### ModelDetail

All fields nullable: `size` (bytes), `modified_at` (epoch seconds), `digest`,
`family`, `parameter_size`, `quantization_level`, `format`.

### PullJob

| Field | Type | Notes |
|---|---|---|
| `id` | string | 12 hex characters |
| `node`, `model` | string | |
| `state` | string | `running`, `success`, `error`, `cancelled` |
| `status` | string | Latest Ollama status text, e.g. `pulling manifest`; `starting` initially; `success` when done |
| `completed`, `total` | integer | Bytes, summed over layers seen so far. `total` can grow as new layers appear. |
| `error` | string \| null | Set when `state` is `error` |
| `started_at` | number | |
| `finished_at` | number \| null | |

## Error reference

| Status | Typical meaning |
|---|---|
| 400 | Validation failed; message says which field |
| 401 | Missing or wrong bearer token |
| 404 | Unknown route, node, pull or model |
| 409 | Duplicate pull / pull already finished |
| 413 | Body over 64 MiB |
| 500 | Unexpected controller error (details in the log only) |
| 502 / 503 / 504 | Upstream node problem; see [Proxy response](#proxy-response) |
