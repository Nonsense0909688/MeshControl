# Security

MeshControl is designed for a **trusted LAN**. It authenticates with shared
bearer tokens over plain HTTP by default and has no user accounts. This
document describes what it protects against, what it does not, and how to
harden a deployment.

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
| Controller ↔ operators/clients | Protected by `ADMIN_TOKEN` (if set) |
| Controller ↔ agents | Protected by `AGENT_TOKEN` |
| Controller ↔ Ollama | **No authentication**; Ollama's API is unauthenticated, so the network path must be trusted |
| Agent ↔ Ollama | Local/LAN, unauthenticated |

**In scope:** a curious or compromised device on the same LAN; a compromised
node attempting to make the controller reach places it shouldn't (SSRF); a
malicious web page or script in the admin's browser; a malformed or hostile
request body.

**Out of scope:** an attacker who can read network traffic (without TLS they
can read tokens and prompts); an attacker with code execution on the controller
or on an Ollama host; denial of service by a legitimate token holder.

## What the code enforces

### Authentication

- Agent endpoints require `AGENT_TOKEN`. Node, model, pull and proxy endpoints
  require `ADMIN_TOKEN` **when it is set**. Comparisons use
  `crypto/subtle.ConstantTimeCompare`.
- With `ADMIN_TOKEN` empty those endpoints are open and a warning is logged at
  start-up.
- `/healthz`, the HTML pages and `/static/*` are unauthenticated by design;
  pages contain no cluster data.

### SSRF protection on registration

A registering node tells the controller where its Ollama lives, and the
controller will connect there. `validateOllamaURL` therefore:

- accepts only `http`/`https`;
- rejects credentials, paths, queries, fragments;
- requires an IP literal unless `ALLOW_HOSTNAMES=true`;
- requires **every** resolved address to lie inside `ALLOWED_NODE_NETWORKS`
  (default: loopback and private ranges only; link-local and cloud-metadata
  addresses are excluded);
- normalises to `scheme://host:port`, so later requests can't smuggle a path;
- optionally (`VERIFY_SOURCE_IP=true`) requires the URL host to equal the
  registering connection's source IP.

### Proxy hardening

- Only `generate`, `chat`, `embeddings`, `embed` are proxied, and the upstream
  path is built from that allow-list, never from client input.
- The client's `Authorization` header is never forwarded to Ollama.
- Requests are never replayed on another node after a connection was made.
- Request bodies are capped at 64 MiB (agents: 256 KiB).

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

- **No TLS.** The controller speaks plain HTTP. Tokens and prompts cross the LAN
  in clear text unless you terminate TLS in front (see
  [DEPLOYMENT.md](DEPLOYMENT.md#tls-with-a-reverse-proxy)).
- **Single shared admin role.** Anyone with `ADMIN_TOKEN` can chat, pull models
  and cancel pulls. There are no per-user tokens, roles or audit trail.
- **A shared `AGENT_TOKEN` lets anyone holding it register as a node** (from an
  allowed network). The controller then sends prompts to that node. Treat the
  token like a credential, restrict `ALLOWED_NODE_NETWORKS`, and consider
  `VERIFY_SOURCE_IP=true`. That option doesn't work behind NAT or a proxy that
  changes the source address.
- **Hostnames are resolved once, at registration.** With `ALLOW_HOSTNAMES=true`
  a DNS change afterwards (rebinding) is not re-checked. Leave it `false`
  unless you need it.
- **Ollama is unauthenticated.** Anyone on the LAN who can reach a node's port
  can use it directly, bypassing the controller. Use host firewalls to limit
  the Ollama port to the controller (and the node itself).
- **No rate limiting or quotas.** A token holder can saturate nodes or start
  many pulls.
- **CSP allows inline styles** (`style-src 'unsafe-inline'`).
- **In-memory only.** Registry and pull state are not persisted.
- **Admin token in the browser.** The UI asks for `ADMIN_TOKEN` and uses it from
  the page; how it is stored client-side is determined by the UI code in
  `static/common.js`, which was not part of the reviewed sources here.

## Hardening checklist

1. Generate two different strong tokens (`openssl rand -base64 32`) for
   `AGENT_TOKEN` and `ADMIN_TOKEN`. Never keep the `change-me-*` placeholders.
2. Always set `ADMIN_TOKEN` on anything other than a throwaway lab.
3. Put the controller behind a TLS reverse proxy; bind it to `127.0.0.1`
   (`CONTROLLER_HOST`) if the proxy is on the same host.
4. Firewall each Ollama port so only the controller can reach it.
5. Narrow `ALLOWED_NODE_NETWORKS` to your real node subnet(s), e.g.
   `192.168.1.0/24`.
6. Keep `ALLOW_HOSTNAMES=false`. Enable `VERIFY_SOURCE_IP=true` where nodes
   register directly (no NAT).
7. Trim `PULL_REGISTRIES` to registries you trust.
8. Run the controller and agents as unprivileged users (systemd examples in
   [DEPLOYMENT.md](DEPLOYMENT.md)). `.env` files should be mode `600`.
9. Don't commit `.env`; add it to `.gitignore`.
10. Rotate tokens if a machine is lost or retired: change `AGENT_TOKEN` on the
    controller and all agents, then restart them.
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
