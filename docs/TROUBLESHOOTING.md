# Troubleshooting

Symptoms, causes and fixes. Log messages quoted here are the ones the code
emits. Set `LOG_LEVEL=DEBUG` for more detail.

## Contents

- [Quick diagnostic checklist](#quick-diagnostic-checklist)
- [Node shows `provider_unreachable`](#node-shows-provider_unreachable)
- [Node shows `offline` or never appears](#node-shows-offline-or-never-appears)
- [Registration errors](#registration-errors)
- [Proxy and chat errors](#proxy-and-chat-errors)
- [Pull problems](#pull-problems)
- [UI problems](#ui-problems)
- [Start-up errors](#start-up-errors)
- [Getting help](#getting-help)

## Quick diagnostic checklist

```bash
# 1. Controller alive?
curl http://CONTROLLER:8000/healthz

# 2. What does the controller think of each node?
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://CONTROLLER:8000/api/nodes

# 3. From the controller machine, can it reach Ollama on the node?
curl http://NODE_IP:11434/api/tags

# 4. On the node, what does the agent say?
journalctl -u meshcontrol-agent -n 50
```

## Node shows `provider_unreachable`

The agent is alive, but Ollama could not be queried at the advertised URL.

| Cause | Fix |
|---|---|
| Ollama bound to `127.0.0.1` (default) | Set `OLLAMA_HOST=0.0.0.0` for the Ollama service and restart it ([DEPLOYMENT.md](DEPLOYMENT.md#ollama-must-listen-on-the-lan)) |
| Host firewall blocks 11434 | Allow the controller's IP on that port |
| Auto-detected advertised IP is wrong (several NICs, VPN, container) | Set `OLLAMA_ADVERTISE_URL=http://<reachable-ip>:11434` on the agent |
| Ollama uses a non-default port | Set `OLLAMA_PORT` (and `OLLAMA_LOCAL_URL` if needed) |
| Ollama is simply not running | Start it. The node will report no models. |

Test from the **node**: `curl http://<advertised-ip>:11434/api/tags` must work,
not just `127.0.0.1`. Then test from the **controller**.

## Node shows `offline` or never appears

| Symptom | Cause / fix |
|---|---|
| Agent log: `registration failed error=...` repeating | Agent can't reach `CONTROLLER_URL`. Check IP/port, firewall, that the controller is listening on a LAN address (`CONTROLLER_HOST`). Retries back off up to 30 s. |
| Agent log: `controller rejected our token; check AGENT_TOKEN` | The agent credential does not match the controller configuration. In shared mode compare `AGENT_TOKEN`; in per-node mode ensure the controller maps `NODE_ID` to the exact token held by that agent. Real environment variables override `.env`, so check both. |
| Agent log: `registration rejected status=400 ...` | Controller validation failed; the body text says why. See below. |
| Agent log: `heartbeat failed` intermittently | Network trouble, or controller overloaded. If frequent, raise `OFFLINE_AFTER`. |
| Agent log: `controller does not know us (restarted?); re-registering` | Normal after a controller restart or a purge. |
| Node flaps online/offline | `OFFLINE_AFTER` too close to `HEARTBEAT_INTERVAL`. Keep it ≥ 3× the interval (defaults: 20 s vs 5 s). |
| Agent behind an HTTP proxy | The agent honours `HTTP_PROXY`/`HTTPS_PROXY`. Add the controller to `NO_PROXY`. |
| Node vanished after a long outage | Nodes silent for more than `PURGE_AFTER` (24 h default) are forgotten; the agent re-registers when it reconnects. |

## Registration errors

These are `400` responses from `POST /api/agent/register`:

| Message | Meaning / fix |
|---|---|
| `node must be 1-64 chars of letters, digits, '.', '_' or '-', starting alphanumeric` | Fix `NODE_ID` (hostnames with spaces or other characters need an explicit ID) |
| `ollama_url host is outside the allowed networks` | The node's address isn't in `ALLOWED_NODE_NETWORKS`. Add its subnet (e.g. `100.64.0.0/10` for Tailscale; link-local `169.254.x.x` is blocked deliberately). |
| `ollama_url must use an IP address, not a hostname` | Use an IP in `OLLAMA_ADVERTISE_URL`, or set `ALLOW_HOSTNAMES=true` on the controller |
| `ollama_url host does not resolve` | DNS failure for a hostname advertise URL |
| `ollama_url host does not match the agent's source address` | `VERIFY_SOURCE_IP=true` and the advertised IP differs from the IP the controller sees. Happens with several NICs, NAT, proxies or containers. Fix the advertise URL or disable the check. |
| `ollama_url must not contain a path, query or fragment` | Advertise only `scheme://host:port` |
| `ollama_url must not contain credentials` | Remove `user:pass@` |

## Proxy and chat errors

| Response | Cause / fix |
|---|---|
| `404 no node hosts model 'X'` | No node reports that model. Check the name (`llama3` equals `llama3:latest`) and `GET /api/models`. Pull it onto a node. |
| `503 model 'X' exists but no hosting node is online` | Hosting nodes are `offline` or `provider_unreachable`. See above. |
| `400 request must include a 'model' string` | The body must be a JSON object with `model` |
| `400 body must be a JSON object` | Invalid JSON, or a JSON array/string; also check `Content-Type`/quoting in the shell |
| `404 not found` on `/api/ollama/...` | Endpoint not in the allow-list (`generate`, `chat`, `embeddings`, `embed`), or wrong path. Correct form: `/api/ollama/api/chat`. |
| `502 could not connect to node X` | TCP connect failed to every candidate (node down, firewall). With several candidates the next one is tried first. |
| `502 error talking to node X` | The connection was made but failed before a response |
| `504 node X timed out` | Nothing received for `READ_TIMEOUT` seconds. Loading a huge model on a slow disk can exceed 600 s; raise `READ_TIMEOUT`. |
| `401 unauthorized` | Missing/wrong `ADMIN_TOKEN`. Header format: `Authorization: Bearer <token>`. |
| `413 request entity too large` | Body over 64 MiB (usually large base64 images) |
| Stream cuts off mid-answer | The node failed after streaming began; the controller deliberately aborts instead of retrying elsewhere. Retry the request. |
| Stream arrives all at once | A reverse proxy is buffering. Disable buffering (see [DEPLOYMENT.md](DEPLOYMENT.md#tls-with-native-https-or-a-reverse-proxy)). |
| Which node answered? | Look at the `X-MeshControl-Node` response header (`curl -i`) |
| Requests don't spread across nodes | Only nodes that host the model **and** are `online` take part; load-aware routing spreads requests when multiple eligible nodes exist. Check `GET /api/models` for `available`. |

## Pull problems

| Response / state | Cause / fix |
|---|---|
| `400 model name is not valid` | Disallowed characters, `..` or `//` |
| `400 registry 'X' is not allowed` | Add it to `PULL_REGISTRIES`. Names with two or more `/` are treated as `registry/org/repo`. |
| `503 node 'X' is offline` / `provider unreachable on node 'X'` | Fix node state first |
| `409 'M' is already being pulled on 'N'` | Wait for the running job or cancel it (`DELETE /api/pulls/{id}`) |
| Job `error`: *lost connection to the node* | Network drop or `READ_TIMEOUT` stall during the download. Start the pull again; Ollama resumes partial layers. |
| Job `error`: *the node ended the download before it completed* | Ollama closed the stream without a `success` status |
| Job `error`: Ollama's own message (e.g. model not found) | Shown verbatim (up to 300 chars). Check the model name on the registry. |
| Pull finished but model not listed | The node reports models in its next heartbeat; wait one `HEARTBEAT_INTERVAL` |
| Pull jobs gone after restart | They are kept in memory only |

## UI problems

| Symptom | Fix |
|---|---|
| Blank page / 404 on `/` | `WEB_DIR` points at the wrong folder. It must contain `dashboard.html` etc. and `static/`. |
| Page loads but is unstyled/dead | A file in `static/` is missing. Only the ten allow-listed names are served; custom extra files return 404. |
| Custom inline `<script>` doesn't run | The Content-Security-Policy forbids inline scripts. Put code in an allow-listed file under `static/`. |
| UI keeps asking for a token / shows 401 | The `ADMIN_TOKEN` entered doesn't match the controller's |
| Data looks stale | Pages poll the API; check the node's last heartbeat in `GET /api/nodes`. The server sends `no-cache` for pages and scripts. |

## Start-up errors

| Message | Fix |
|---|---|
| `unknown mode "..."` + usage | Pass `controller` or `agent`, or set `MESHCONTROL_MODE` |
| `cannot load env file: ...: expected KEY=value` | Syntax error at that `file:line` in the `.env` |
| `cannot load env file: ...: unterminated quote` | Close the quote on that line |
| `cannot load env file: open ...: no such file` | You passed `--env-file`/`ENV_FILE` explicitly and the file is missing |
| `AGENT_TOKEN environment variable is required` | Set it |
| `CONTROLLER_URL environment variable is required` | Agent only |
| `invalid OFFLINE_AFTER` (etc.) | The value isn't a finite number |
| `invalid network "..."` | Fix `ALLOWED_NODE_NETWORKS` CIDR syntax |
| `invalid HEARTBEAT_INTERVAL` | Must be a number > 0 |
| `WEB_DIR not found dir=...` | Create/point to the web folder |
| `listen tcp ...: bind: address already in use` | Another process uses the port; change `CONTROLLER_PORT` |
| `ADMIN_TOKEN must contain at least 32 characters` | Generate a strong token with `openssl rand -hex 32`; the controller refuses weak/missing tokens. |

## Getting help

Open an issue with:

- the output of `GET /api/nodes` (remove URLs you consider sensitive),
- relevant controller and agent log lines (`LOG_LEVEL=DEBUG`),
- your configuration **with tokens removed**,
- OS, Ollama version and how Ollama is started.
