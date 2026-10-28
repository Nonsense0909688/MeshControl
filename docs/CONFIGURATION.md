# Configuration

MeshControl is configured with environment variables, optionally loaded from a
`.env` file. A ready-to-edit template ships as `.env.example`.

## Contents

- [How settings are loaded](#how-settings-are-loaded)
- [.env file syntax](#env-file-syntax)
- [Shared settings](#shared-settings)
- [Controller settings](#controller-settings)
- [Agent settings](#agent-settings)
- [Value formats](#value-formats)
- [Fixed limits](#fixed-limits)
- [Examples](#examples)

## How settings are loaded

Order of precedence (highest first):

1. **Real environment variables.**
2. The `.env` file.
3. Built-in defaults.

Which file is read:

| How | Behaviour |
|---|---|
| nothing specified | `./.env` is loaded if it exists; a missing file is silently ignored |
| `--env-file PATH` / `--env-file=PATH` | that file is loaded; a missing file is an **error** |
| `ENV_FILE=PATH` | same as `--env-file` |

The role is chosen by the first non-flag argument (`controller` or `agent`), or,
if there is none, by `MESHCONTROL_MODE`. With neither, usage is printed and the
process exits with status 2.

Empty values count as unset for most variables (values are trimmed; an empty
string falls back to the default).

## .env file syntax

- `KEY=value` and `export KEY=value`.
- Full-line comments with `#`, and inline comments when `#` follows whitespace
  (`KEY=value # comment`).
- `"double quoted"` values support the escapes `\n`, `\r`, `\t`, `\"`, `\\`.
  Quote values that contain `#`: `KEY="a # b"`.
- `'single quoted'` values are taken literally.
- A UTF-8 byte-order mark on the first line is ignored.
- Keys must match `[A-Za-z_][A-Za-z0-9_]*`. A bad line aborts startup with a
  `file:line` message, as does an unterminated quote.

## Shared settings

| Variable | Default | Meaning |
|---|---|---|
| `MESHCONTROL_MODE` | none | `controller` or `agent`; used when no argument is given |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN` or `ERROR` (Go `slog` levels). Logs go to stderr. |
| `AGENT_TOKEN` | **required in shared mode** | Shared secret identical on controller and every agent. Generate with `openssl rand -hex 32`. Mutually exclusive with `AGENT_TOKENS`. |
| `AGENT_TOKENS` | unset | Optional controller-only per-node mode: comma-separated `node-id:secret` pairs. When set, leave controller `AGENT_TOKEN` blank; each agent sets `AGENT_TOKEN` to its own mapped secret. IDs and secrets must be unique; each secret is 32+ characters. |

## Controller settings

| Variable | Default | Meaning |
|---|---|---|
| `ADMIN_TOKEN` | **required** | Bearer token for the UI API, node/model endpoints, pulls and proxy. Must be at least 32 characters and not a placeholder; requests fail closed if absent. Use a different value from the shared `AGENT_TOKEN` or every mapped `AGENT_TOKENS` secret. |
| `CONTROLLER_HOST` | `127.0.0.1` | Listen address; set `0.0.0.0` to accept LAN connections. |
| `CONTROLLER_PORT` | `8000` | Listen port |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | unset | Optional native HTTPS certificate/key pair; both must be set and valid |
| `ALLOW_INSECURE_HTTP` | `false` | Explicit opt-in for non-loopback plain HTTP; strongly prefer TLS or a reverse proxy |
| `WEB_DIR` | `./web` | Folder containing the `*.html` pages and `static/`. Start-up fails if the directory doesn't exist. |
| `OFFLINE_AFTER` | `20` | Seconds without a heartbeat before a node is `offline`. Keep it comfortably above `HEARTBEAT_INTERVAL` (≥ 3×). |
| `PURGE_AFTER` | `86400` | Seconds without a heartbeat before a node is forgotten entirely |
| `ALLOWED_NODE_NETWORKS` | `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7` | Comma-separated CIDRs (or bare IPs) that node URLs may point into. Loopback, link-local and cloud-metadata ranges are excluded by default. |
| `ALLOWED_NODE_PORTS` | `11434` | Comma-separated Ollama ports accepted during registration and rechecked for every upstream connection. Add custom Ollama ports explicitly. |
| `ALLOW_HOSTNAMES` | `false` | Accept DNS names in `ollama_url`. When `false`, only IP literals are accepted. |
| `VERIFY_SOURCE_IP` | `false` | Require the host in `ollama_url` to equal the IP the registration came from |
| `CONNECT_TIMEOUT` | `5` | Seconds allowed to open a TCP connection to a node (proxy and pulls) |
| `READ_TIMEOUT` | `600` | Seconds of *silence* tolerated from a node while proxying or pulling. Not a total deadline. |
| `PULL_REGISTRIES` | `registry.ollama.ai,hf.co,huggingface.co` | Registries a pull request may name. Case-insensitive, comma-separated. |
| `MAX_BODY_MB` | `8` | Maximum API request body in MiB (whole number from 1 to 64). |
| `MAX_CONCURRENT_REQUESTS` | `256` | Maximum requests processed concurrently; excess requests receive `503`. |
| `MAX_CONCURRENT_PULLS` | `4` | Maximum active model pulls across the controller. |
| `MAX_CONCURRENT_PULLS_PER_NODE` | `1` | Maximum active pulls on one node (cannot exceed global limit). |

## Agent settings

| Variable | Default | Meaning |
|---|---|---|
| `CONTROLLER_URL` | **required** | Base URL of the controller, e.g. `http://10.0.0.5:8000`. A trailing `/` is removed. `https://` works with native TLS or a TLS reverse proxy. |
| `NODE_ID` | machine hostname | Name shown in the UI and used in `/api/nodes/<node>`. 1–64 characters: letters, digits, `.`, `_`, `-`; must start with a letter or digit. |
| `OLLAMA_LOCAL_URL` | `http://127.0.0.1:11434` | Where the agent itself reaches Ollama (fallback probe) |
| `OLLAMA_ADVERTISE_URL` | auto | URL the **controller** should use to reach this node's Ollama, e.g. `http://192.168.1.20:11434`. Auto-detected from the route to the controller if unset. |
| `OLLAMA_PORT` | `11434` | Port used when building the auto-detected advertised URL |
| `HEARTBEAT_INTERVAL` | `5` | Seconds between heartbeats (1–3600; decimals allowed) |

> Set `OLLAMA_ADVERTISE_URL` explicitly if the machine has several network
> interfaces, runs in a container, or sits behind NAT. The auto-detected address
> may not be the one the controller can reach.

## Value formats

| Type | Accepted |
|---|---|
| Seconds | Most timeout settings accept decimals; each variable also has a documented range. Non-numeric, `NaN` or `Inf` values fail validation. |
| Booleans | `1`, `true`, `yes`, `on` (case-insensitive) are true. Any other *set* value is false. Unset uses the default. |
| Networks | `10.0.0.0/8`, `192.168.1.0/24`, `fd00::/8`, or a bare IP (treated as `/32` or `/128`). Invalid entries fail start-up. |
| Lists | Comma-separated; whitespace around items is trimmed |

## Fixed limits

These are compiled in and not configurable:

| Limit | Value |
|---|---|
| Max proxy / pull request body | 64 MiB |
| Max agent register/heartbeat body | 256 KiB |
| Models per heartbeat | 1000 |
| Model name length | 256 characters |
| Pull jobs kept after finishing | 50 |
| Agent registration back-off | 1 s doubling up to 30 s |
| Agent HTTP timeouts | 3 s connect, 5 s response header, 10 s overall |

## Examples

### Controller `.env`

```ini
MESHCONTROL_MODE=controller
AGENT_TOKEN=<openssl rand -base64 32>
ADMIN_TOKEN=<a different openssl rand -hex 32>
WEB_DIR=/opt/meshcontrol/web
CONTROLLER_HOST=0.0.0.0
CONTROLLER_PORT=8000
```

### Agent `.env`

```ini
MESHCONTROL_MODE=agent
CONTROLLER_URL=http://10.0.0.5:8000
AGENT_TOKEN=<same value as the controller>
NODE_ID=gpu-box
OLLAMA_ADVERTISE_URL=http://10.0.0.21:11434
```

### Allow an extra network (e.g. Tailscale)

The default allow-list does not include `100.64.0.0/10`. Add it, keeping the
defaults you still need:

```ini
ALLOWED_NODE_NETWORKS=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10
ALLOWED_NODE_PORTS=11434
```

### Quoted values

```ini
ADMIN_TOKEN="abc#def"      # '#' inside quotes is not a comment
```
