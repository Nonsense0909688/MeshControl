# Security

MeshControl is designed for a **trusted LAN**. It defaults to loopback-only HTTP, requires strong bearer tokens, and has no user accounts. LAN deployments should use native HTTPS or a TLS-terminating reverse proxy. This document describes what it protects against, what it does not, and how to harden a deployment.

## Contents

- [Threat model](#threat-model)
- [What the code enforces](#what-the-code-enforces)
- [Known limitations](#known-limitations)
- [Hardening checklist](#hardening-checklist)
- [Token handling](#token-handling)
- [Reporting a vulnerability](#reporting-a-vulnerability)

## Threat model

**Assets**

- Prompts and responses flowing through the proxy.
- The ability to run inference and download models on your hardware.
- The integrity of the node registry (who counts as a cluster member).

**Trust boundaries**

| Boundary | Trust |
|---|---|
| Controller ↔ operators/clients | Protected by the required `ADMIN_TOKEN`; API requests fail closed if it is missing |
| Controller ↔ agents | Protected by shared `AGENT_TOKEN` or unique per-node `AGENT_TOKENS` |
| Controller ↔ Ollama | **No authentication**; Ollama's API is unauthenticated, so the network path must be trusted |
| Agent ↔ Ollama | Local/LAN, unauthenticated |

**In scope:** a curious or compromised device on the same LAN; a compromised
node attempting to make the controller reach places it shouldn't (SSRF); a
malicious web page or script in the admin's browser; a malformed or hostile
request body.

**Out of scope:** an attacker who can read network traffic when TLS is not enabled (they can
read tokens and prompts); an attacker with code execution on the controller
or on an Ollama host; denial of service by a legitimate token holder.

## What the code enforces

### Authentication

- Agent endpoints require either the shared `AGENT_TOKEN` or a unique credential from `AGENT_TOKENS` (per-node mode binds each credential to its configured node ID). Node, model, pull and proxy endpoints
  always require `ADMIN_TOKEN`; an empty or invalid token cannot open the API.
  Every configured secret must be at least 32 characters and non-placeholder.
- Comparisons use `crypto/subtle.ConstantTimeCompare`. The HTTP listener defaults
  to `127.0.0.1`; set `CONTROLLER_HOST=0.0.0.0` intentionally for LAN access.
- `/healthz`, the HTML pages and `/static/*` are unauthenticated by design;
  pages contain no cluster data.

### SSRF protection on registration

A registering node tells the controller where its Ollama lives, and the
controller will connect there. `validateOllamaURL` therefore:

- accepts only `http`/`https`;
- rejects credentials, paths, queries, fragments;
- requires an IP literal unless `ALLOW_HOSTNAMES=true`;
- requires **every** resolved address to lie inside `ALLOWED_NODE_NETWORKS`
  (default: private LAN ranges only; loopback, link-local and cloud metadata
  are excluded);
- restricts the destination to `ALLOWED_NODE_PORTS` (default: 11434);
- normalises to `scheme://host:port`, so later requests can't smuggle a path;
- re-resolves and re-checks DNS answers at connection time to mitigate DNS
  rebinding, and disables ambient HTTP proxy settings for node traffic;
- binds an active node ID to its registration peer IP and verifies heartbeat
  source IP consistency; a node moving to a new IP must re-register after its prior
  heartbeat ages beyond `OFFLINE_AFTER`;
- optionally (`VERIFY_SOURCE_IP=true`) requires the URL host (or one of its
  resolved addresses when hostnames are explicitly enabled) to match the peer.

### Proxy hardening

- Only `generate`, `chat`, `embeddings`, `embed` are proxied, and the upstream
  path is built from that allow-list, never from client input.
- The client's `Authorization` header is never forwarded to Ollama.
- Requests are never replayed on another node after a connection was made.
- JSON request bodies default to 8 MiB (`MAX_BODY_MB`, max 64 MiB); agents are
  capped at 256 KiB. A 30-second server read timeout limits slow request bodies.
- HTTP concurrency is bounded (`MAX_CONCURRENT_REQUESTS`); active model pulls
  have global and per-node limits. Upstream pull progress frames are capped at
  1 MiB each.
- Upstream connections are limited to allowlisted networks and ports, with
  capped connection pools and no automatic redirect following.

### Pull hardening

- The body sent to Ollama is built by the controller (`model` + `stream`).
- Model names are validated by regex; names that look like they start with a
  registry host must use a registry in `PULL_REGISTRIES`.

### Input validation

- Node IDs, percentages, model lists (≤ 1000 entries, ≤ 256 chars each) and
  model details are validated and bounded. Unknown or malformed detail fields
  are dropped.

### Web UI

Pages are served with:

```
Content-Security-Policy: default-src 'none'; script-src 'self';
  style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:;
  base-uri 'none'; form-action 'self'; frame-ancestors 'none'
X-Content-Type-Options: nosniff
```

No inline scripts, no third-party hosts, no framing. Only an allow-list of
static file names is served.

### Logging

Logs record events (registrations, pulls, upstream failures) and node names,
not prompts or response bodies. Error logging uses the error *type* only so
tokens and URLs don't leak into logs.

## Known limitations

- **TLS is opt-in.** Native HTTPS is available with `TLS_CERT_FILE` and
  `TLS_KEY_FILE`, or terminate TLS in a reverse proxy. Non-loopback plain HTTP
  is rejected unless `ALLOW_INSECURE_HTTP=true` is explicitly set.
- **Single shared admin role.** Anyone with `ADMIN_TOKEN` can chat, pull models,
  and cancel pulls. There are no per-user tokens or role-based permissions;
  operational audit events are ordinary process logs, not a tamper-proof audit log.
- **Shared-token mode gives every agent the same authority.** A holder of the
  shared `AGENT_TOKEN` can register identities from an allowed network. Use
  `AGENT_TOKENS` for unique node-bound secrets; also restrict
  `ALLOWED_NODE_NETWORKS` and optionally enable `VERIFY_SOURCE_IP` where NAT is
  not involved.
- **Ollama is unauthenticated.** Anyone on the LAN who can reach a node's port
  can use it directly, bypassing the controller. Use host firewalls to limit
  the Ollama port to the controller (and the node itself).
- **No request-rate limiting per identity.** Concurrency and pull limits protect
  controller resources, but a valid admin can still submit sustained workloads.
  Put an authenticated rate limiter at a TLS reverse proxy if required.
- **CSP allows inline styles** (`style-src 'unsafe-inline'`).
- **In-memory only.** Registry and pull state are not persisted.
- **Admin token in the browser.** The operator enters `ADMIN_TOKEN` into the UI;
  do not use a shared browser profile on an untrusted machine.

## Hardening checklist

1. Generate strong secrets (`openssl rand -hex 32`) for the agent credential(s) and a distinct `ADMIN_TOKEN`. For stronger isolation, configure unique `AGENT_TOKENS` entries per node. Never keep placeholders.
2. `ADMIN_TOKEN` is mandatory; requests fail closed if it is missing or weak.
3. Use native HTTPS (`TLS_CERT_FILE`/`TLS_KEY_FILE`) or a TLS reverse proxy.
   Non-loopback plain HTTP is rejected unless `ALLOW_INSECURE_HTTP=true` is explicitly set.
4. Firewall each Ollama port so only the controller can reach it.
5. Narrow `ALLOWED_NODE_NETWORKS` to your real node subnet(s), e.g.
   `192.168.1.0/24`, and keep `ALLOWED_NODE_PORTS` limited to Ollama ports.
6. Keep `ALLOW_HOSTNAMES=false`. Enable `VERIFY_SOURCE_IP=true` where nodes
   register directly (no NAT or proxy that changes peer IPs).
7. Trim `PULL_REGISTRIES` to registries you trust.
8. Run the controller and agents as unprivileged users (systemd examples in
   [DEPLOYMENT.md](DEPLOYMENT.md)). `.env` files should be mode `600`.
9. Don't commit `.env`; add it to `.gitignore`.
10. Rotate tokens if a machine is lost or retired. In per-node mode, replace only that node's `AGENT_TOKENS` entry and its agent token; in shared mode, change `AGENT_TOKEN` on the controller and all agents.
11. Keep Go toolchain and `gopsutil` current when rebuilding.

## Token handling

- Tokens are read from the environment/`.env`; there is no hashed storage.
- Changing `ADMIN_TOKEN` takes effect on controller restart; clients simply use
  the new value.
- Changing `AGENT_TOKEN` requires restarting the controller and every agent.
  Agents with the old token log `controller rejected our token; check AGENT_TOKEN`
  and keep retrying with back-off.
- Pass tokens to `curl` through an environment variable (as in the examples) so
  they don't land in shell history.

## Reporting a vulnerability

Please report security issues privately to the repository owner (for example via
GitHub's *Security → Report a vulnerability* feature if enabled) rather than in
a public issue. Include the version/commit, configuration (without secrets) and
steps to reproduce.
