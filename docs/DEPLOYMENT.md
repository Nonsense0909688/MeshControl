# Deployment

How to run MeshControl for real: systemd, Docker, TLS in front, firewalling and
upgrades.

> The unit files and Dockerfile below are **reference examples**. If your
> repository's `deploy/` directory contains versions, those are authoritative;
> adjust paths and users to taste.

## Contents

- [Topology](#topology)
- [Prepare the machines](#prepare-the-machines)
- [Install the binary and web files](#install-the-binary-and-web-files)
- [systemd](#systemd)
- [Docker](#docker)
- [TLS with a reverse proxy](#tls-with-a-reverse-proxy)
- [Firewall](#firewall)
- [Upgrades](#upgrades)
- [Backups and state](#backups-and-state)
- [Verifying a deployment](#verifying-a-deployment)

## Topology

- **One** controller. It needs to reach every node's Ollama port, and every
  agent needs to reach the controller's port (default 8000).
- **One agent per Ollama machine**, on the same host as Ollama. The controller
  machine can also be a node (run an agent there too).
- Controller and agents are the same binary with different settings.

There is no clustering of the controller itself; it is a single point of
coordination. If it is down, inference through the cluster address stops, but
Ollama on each node keeps working and agents re-register when it returns.

## Prepare the machines

### Ollama must listen on the LAN

By default Ollama binds `127.0.0.1`, which the controller cannot reach (the node
will show `provider_unreachable`). Set `OLLAMA_HOST=0.0.0.0` (or a specific LAN
address) for the Ollama service.

**Linux (systemd):**

```bash
sudo systemctl edit ollama
# add:
#   [Service]
#   Environment="OLLAMA_HOST=0.0.0.0"
sudo systemctl daemon-reload
sudo systemctl restart ollama
```

**macOS (Ollama app):** `launchctl setenv OLLAMA_HOST "0.0.0.0"`, then restart
the app.

**Windows:** set a user/system environment variable `OLLAMA_HOST=0.0.0.0`, then
restart Ollama.

Then restrict who can reach port 11434 (see [Firewall](#firewall)).

### Tokens

```bash
openssl rand -base64 32   # AGENT_TOKEN
openssl rand -base64 32   # ADMIN_TOKEN (different value)
```

## Install the binary and web files

Build (see the README), or cross-compile for the target and copy it over.

```bash
sudo useradd --system --home /opt/meshcontrol --shell /usr/sbin/nologin meshcontrol
sudo install -d -o meshcontrol -g meshcontrol /opt/meshcontrol
sudo install -m 755 meshcontrol /opt/meshcontrol/meshcontrol
# controller only: the UI files
sudo cp -r web /opt/meshcontrol/web
sudo chown -R meshcontrol:meshcontrol /opt/meshcontrol
```

Create `/opt/meshcontrol/.env` (mode `600`, owner `meshcontrol`). See
[CONFIGURATION.md](CONFIGURATION.md) for contents.

## systemd

### Controller

`/etc/systemd/system/meshcontrol-controller.service`

```ini
[Unit]
Description=MeshControl controller
After=network-online.target
Wants=network-online.target

[Service]
User=meshcontrol
Group=meshcontrol
WorkingDirectory=/opt/meshcontrol
ExecStart=/opt/meshcontrol/meshcontrol --env-file /opt/meshcontrol/.env controller
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

### Agent

`/etc/systemd/system/meshcontrol-agent.service`

```ini
[Unit]
Description=MeshControl agent
After=network-online.target ollama.service
Wants=network-online.target

[Service]
User=meshcontrol
Group=meshcontrol
WorkingDirectory=/opt/meshcontrol
ExecStart=/opt/meshcontrol/meshcontrol --env-file /opt/meshcontrol/.env agent
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Enable and watch:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now meshcontrol-controller    # or -agent
journalctl -u meshcontrol-controller -f
```

The agent retries registration with back-off, so start order between agents and
controller does not matter.

## Docker

The binary is statically linkable (`CGO_ENABLED=0`), so images can be tiny.

`Dockerfile`

```dockerfile
FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /meshcontrol .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /meshcontrol /meshcontrol
COPY web /web
ENV WEB_DIR=/web
EXPOSE 8000
ENTRYPOINT ["/meshcontrol"]
CMD ["controller"]
```

`docker-compose.yml` (controller)

```yaml
services:
  meshcontrol:
    build: .
    command: ["controller"]
    restart: unless-stopped
    env_file: .env
    ports:
      - "8000:8000"
```

Notes:

- The **agent should normally run directly on the host**, not in a container.
  It reports the machine's CPU/RAM and must find a LAN address the controller
  can reach. In a container, auto-detection returns the container's address.
  If you do containerise it, use host networking (`network_mode: host`) or set
  `OLLAMA_ADVERTISE_URL` explicitly, and set `OLLAMA_LOCAL_URL` to an address
  the container can reach.
- Inside a container, `CONTROLLER_HOST` should stay `0.0.0.0`; restrict exposure
  with the published port (`127.0.0.1:8000:8000`) if a proxy runs on the host.
- The `distroless` image has no shell; `.env` is injected via `env_file`, so no
  `.env` file is needed inside the image.

## TLS with a reverse proxy

The controller has no built-in TLS. Terminate TLS in a reverse proxy and bind
the controller to loopback (`CONTROLLER_HOST=127.0.0.1`).

Important proxy settings: **disable response buffering** (streaming chat), and
use a **long read timeout** (generation can pause while a model loads).

**nginx**

```nginx
server {
    listen 443 ssl http2;
    server_name mesh.example.lan;
    ssl_certificate     /etc/ssl/mesh.crt;
    ssl_certificate_key /etc/ssl/mesh.key;

    client_max_body_size 64m;

    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_buffering off;
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
    }
}
```

**Caddy**

```
mesh.example.lan {
    reverse_proxy 127.0.0.1:8000 {
        flush_interval -1
    }
}
```

Agents can then use `CONTROLLER_URL=https://mesh.example.lan`. If agents see a
certificate error, install your CA on those hosts. Agents honour the standard
`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment variables; add the controller
address to `NO_PROXY` if a corporate proxy is configured on the node.

Note: if the proxy runs on another host, `VERIFY_SOURCE_IP` still compares
against the address the *controller* sees. Agents connecting through a proxy
will fail that check. Leave it `false` in that layout.

## Firewall

| Machine | Allow inbound | From |
|---|---|---|
| Controller | TCP 8000 (or 443 via proxy) | clients/browsers and all agents |
| Each Ollama node | TCP 11434 | **the controller only** |
| Each Ollama node | nothing else needed for MeshControl | agents make outbound calls only |

Example with `ufw` on a node (controller at 10.0.0.5):

```bash
sudo ufw allow from 10.0.0.5 to any port 11434 proto tcp
```

## Upgrades

1. Build or download the new binary.
2. Replace `/opt/meshcontrol/meshcontrol` and (controller) the `web/` folder.
   Keep the binary and the `web/` files from the same release; page scripts and
   API shapes move together.
3. `sudo systemctl restart meshcontrol-controller`. Agents get `404` on their
   next heartbeat and re-register automatically; nodes reappear within one
   heartbeat interval. Running pulls are interrupted.
4. Restart agents (`systemctl restart meshcontrol-agent`) on each node. Order
   does not matter.
5. Check `GET /healthz` and the dashboard.

Rollback is the reverse: put the previous binary back and restart. There is no
data migration.

## Backups and state

Nothing needs backing up apart from the `.env` files (they hold your tokens).
Registry and pull state is in memory and rebuilt from heartbeats.

## Verifying a deployment

```bash
# controller is up
curl http://CONTROLLER:8000/healthz

# nodes registered and reachable (look for "status":"online")
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://CONTROLLER:8000/api/nodes

# end-to-end
curl -N http://CONTROLLER:8000/api/ollama/api/generate \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"model":"<a model you have>","prompt":"hi"}' -i | head
# the response headers should include X-MeshControl-Node
```

If something is off, see [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
