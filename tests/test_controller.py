import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from controller import (
    DETAIL_KEYS, Config, ValidationError, create_app, norm_model, validate_ollama_url, validate_pull_model,
)

AGENT_TOKEN = "agent-secret"
ADMIN_TOKEN = "admin-secret"
AGENT = {"Authorization": f"Bearer {AGENT_TOKEN}"}
ADMIN = {"Authorization": f"Bearer {ADMIN_TOKEN}"}


# --------------------------------------------------------------------------- #
# Fixtures
# --------------------------------------------------------------------------- #
class Clock:
    def __init__(self):
        self.now = 1_700_000_000.0

    def __call__(self):
        return self.now


class FakeOllama:
    """A tiny real HTTP server that mimics Ollama's chunked NDJSON streaming."""

    def __init__(self, name, models):
        self.name = name
        self.models = models
        self.requests = []
        self.pull_frames = None              # frames streamed for /api/pull; "WAIT" blocks on pull_gate
        self.pull_started = threading.Event()
        self.pull_gate = threading.Event()
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def do_GET(self):
                body = json.dumps({"models": [{"name": m} for m in outer.models]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def _chunk(self, payload):
                chunk = (json.dumps(payload) + "\n").encode()
                self.wfile.write(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n")
                self.wfile.flush()

            def _pull(self):
                frames = outer.pull_frames or [
                    {"status": "pulling manifest"},
                    {"status": "pulling abc", "digest": "sha256:abc", "total": 100, "completed": 40},
                    {"status": "pulling abc", "digest": "sha256:abc", "total": 100, "completed": 100},
                    {"status": "success"},
                ]
                self.send_response(200)
                self.send_header("Content-Type", "application/x-ndjson")
                self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                try:
                    for frame in frames:
                        if frame == "WAIT":
                            outer.pull_started.set()
                            outer.pull_gate.wait(10)
                            continue
                        self._chunk(frame)
                    self.wfile.write(b"0\r\n\r\n")
                except OSError:
                    pass  # client cancelled

            def do_POST(self):
                raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                data = json.loads(raw)
                outer.requests.append({"path": self.path, "body": data, "auth": self.headers.get("Authorization")})
                if self.path == "/api/pull":
                    return self._pull()
                if data.get("stream", True):
                    self.send_response(200)
                    self.send_header("Content-Type", "application/x-ndjson")
                    self.send_header("Transfer-Encoding", "chunked")
                    self.end_headers()
                    for i, tok in enumerate(["Hel", "lo", ""]):
                        line = json.dumps({"response": tok, "done": i == 2, "served_by": outer.name}) + "\n"
                        chunk = line.encode()
                        self.wfile.write(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n")
                        self.wfile.flush()
                    self.wfile.write(b"0\r\n\r\n")
                else:
                    body = json.dumps({"response": "Hello", "done": True, "served_by": outer.name}).encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server.server_address[1]}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def make_ollama():
    servers = []

    def factory(name, models):
        s = FakeOllama(name, models)
        servers.append(s)
        return s

    yield factory
    for s in servers:
        s.close()


@pytest.fixture
def clock():
    return Clock()


@pytest.fixture
def app(clock):
    cfg = Config(agent_token=AGENT_TOKEN, admin_token=ADMIN_TOKEN, connect_timeout=2, read_timeout=10)
    app = create_app(cfg, clock=clock)
    app.testing = True
    return app


@pytest.fixture
def client(app):
    return app.test_client()


def join(client, node, url, models, cpu=10.0, ram=20.0, reachable=True):
    r = client.post("/api/agent/register", json={"node": node, "ollama_url": url}, headers=AGENT)
    assert r.status_code in (200, 201), r.get_json()
    r = client.post(
        "/api/agent/heartbeat",
        json={"node": node, "cpu": cpu, "ram": ram, "ollama_lan_reachable": reachable, "models": models},
        headers=AGENT,
    )
    assert r.status_code == 200, r.get_json()


def ndjson(resp):
    return [json.loads(line) for line in resp.data.decode().splitlines() if line.strip()]


# --------------------------------------------------------------------------- #
# Authentication
# --------------------------------------------------------------------------- #
@pytest.mark.parametrize("path", ["/api/agent/register", "/api/agent/heartbeat"])
def test_agent_endpoints_reject_missing_and_wrong_token(client, path):
    assert client.post(path, json={}).status_code == 401
    assert client.post(path, json={}, headers={"Authorization": "Bearer nope"}).status_code == 401
    assert client.post(path, json={}, headers={"Authorization": AGENT_TOKEN}).status_code == 401


def test_admin_endpoints_require_admin_token(client):
    assert client.get("/api/nodes").status_code == 401
    assert client.get("/api/nodes", headers=AGENT).status_code == 401  # agent token != admin
    assert client.get("/api/nodes", headers=ADMIN).status_code == 200
    assert client.post("/api/ollama/api/generate", json={"model": "x"}).status_code == 401
    assert client.get("/api/ollama/api/tags").status_code == 401


def test_admin_endpoints_open_when_no_admin_token(clock):
    app = create_app(Config(agent_token=AGENT_TOKEN), clock=clock)
    assert app.test_client().get("/api/nodes").status_code == 200


def test_healthz_is_public(client):
    assert client.get("/healthz").get_json()["status"] == "ok"


# --------------------------------------------------------------------------- #
# Registration and validation
# --------------------------------------------------------------------------- #
def test_register_creates_then_refreshes(client):
    body = {"node": "n1", "ollama_url": "http://192.168.29.242:11434"}
    assert client.post("/api/agent/register", json=body, headers=AGENT).status_code == 201
    assert client.post("/api/agent/register", json=body, headers=AGENT).status_code == 200
    assert len(client.get("/api/nodes", headers=ADMIN).get_json()["nodes"]) == 1


@pytest.mark.parametrize("node", [None, "", 5, "bad node", "../etc", "a" * 65, "-lead"])
def test_register_rejects_bad_node_ids(client, node):
    r = client.post("/api/agent/register", json={"node": node, "ollama_url": "http://127.0.0.1:11434"}, headers=AGENT)
    assert r.status_code == 400


@pytest.mark.parametrize(
    "url",
    [
        "http://8.8.8.8:11434",                # public IP
        "http://169.254.169.254",              # cloud metadata
        "ftp://127.0.0.1:11434",               # wrong scheme
        "http://user:pw@127.0.0.1:11434",      # credentials
        "http://127.0.0.1:11434/admin",        # path
        "http://127.0.0.1:11434/?x=1",         # query
        "http://example.com:11434",            # hostname (disabled by default)
        "http://[::ffff:8.8.8.8]:11434",       # IPv4-mapped public IP
        "http://127.0.0.1:99999",              # bad port
        "",
        None,
        12,
    ],
)
def test_validate_ollama_url_rejects(url):
    with pytest.raises(ValidationError):
        validate_ollama_url(url, Config(agent_token="t"))


def test_validate_ollama_url_normalises():
    cfg = Config(agent_token="t")
    assert validate_ollama_url("http://192.168.29.242:11434/", cfg) == "http://192.168.29.242:11434"
    assert validate_ollama_url("http://[::1]:11434", cfg) == "http://[::1]:11434"


def test_register_rejects_non_json(client):
    r = client.post("/api/agent/register", data="nope", headers=AGENT)
    assert r.status_code == 400


# --------------------------------------------------------------------------- #
# Heartbeats
# --------------------------------------------------------------------------- #
def test_heartbeat_for_unknown_node_requests_reregistration(client):
    r = client.post(
        "/api/agent/heartbeat",
        json={"node": "ghost", "cpu": 1, "ram": 1, "ollama_lan_reachable": True, "models": []},
        headers=AGENT,
    )
    assert r.status_code == 404
    assert r.get_json()["reregister"] is True


@pytest.mark.parametrize(
    "patch",
    [
        {"cpu": -1},
        {"cpu": 101},
        {"cpu": "12"},
        {"cpu": True},
        {"cpu": float("nan")},
        {"ram": None},
        {"ollama_lan_reachable": "yes"},
        {"models": "qwen"},
        {"models": [1, 2]},
        {"models": [""]},
        {"models": ["x" * 300]},
        {"models": ["m"] * 1001},
    ],
)
def test_heartbeat_rejects_malformed(client, patch):
    client.post("/api/agent/register", json={"node": "n1", "ollama_url": "http://127.0.0.1:11434"}, headers=AGENT)
    body = {"node": "n1", "cpu": 5, "ram": 5, "ollama_lan_reachable": True, "models": ["a:1"], **patch}
    # NaN is not valid JSON for most clients, send raw to be sure the server copes.
    r = client.post("/api/agent/heartbeat", data=json.dumps(body), content_type="application/json", headers=AGENT)
    assert r.status_code == 400


def test_heartbeats_update_single_record(client):
    join(client, "n1", "http://127.0.0.1:11434", ["a:1"], cpu=10)
    join(client, "n1", "http://127.0.0.1:11434", ["a:1", "b:2"], cpu=55.5)
    join(client, "n1", "http://127.0.0.1:11434", ["b:2"], cpu=70)
    nodes = client.get("/api/nodes", headers=ADMIN).get_json()["nodes"]
    assert len(nodes) == 1
    assert nodes[0]["cpu"] == 70 and nodes[0]["models"] == ["b:2"]


def test_duplicate_model_names_are_deduplicated(client):
    join(client, "n1", "http://127.0.0.1:11434", ["a:1", "a:1"])
    assert client.get("/api/nodes/n1/models", headers=ADMIN).get_json()["models"] == ["a:1"]


# --------------------------------------------------------------------------- #
# Online / offline / stale handling
# --------------------------------------------------------------------------- #
def test_online_status_follows_heartbeat_age(client, clock):
    join(client, "n1", "http://127.0.0.1:11434", ["a:1"])
    get = lambda: client.get("/api/nodes/n1", headers=ADMIN).get_json()
    assert get()["online"] is True
    clock.now += 20
    assert get()["online"] is True       # exactly at the limit
    clock.now += 0.5
    assert get()["online"] is False
    join(client, "n1", "http://127.0.0.1:11434", ["a:1"])
    assert get()["online"] is True       # recovers on next heartbeat


def test_stale_nodes_are_purged(client, clock):
    join(client, "n1", "http://127.0.0.1:11434", ["a:1"])
    clock.now += 86401
    assert client.get("/api/nodes/n1", headers=ADMIN).status_code == 404
    assert client.get("/api/nodes", headers=ADMIN).get_json()["nodes"] == []


def test_controller_restart_clears_state_and_agent_is_told_to_reregister(clock):
    cfg = Config(agent_token=AGENT_TOKEN)
    a1 = create_app(cfg, clock=clock).test_client()
    join(a1, "n1", "http://127.0.0.1:11434", ["a:1"])
    a2 = create_app(cfg, clock=clock).test_client()  # "restart"
    r = a2.post(
        "/api/agent/heartbeat",
        json={"node": "n1", "cpu": 1, "ram": 1, "ollama_lan_reachable": True, "models": []},
        headers=AGENT,
    )
    assert r.status_code == 404 and r.get_json()["reregister"] is True


# --------------------------------------------------------------------------- #
# Node and model queries
# --------------------------------------------------------------------------- #
def test_get_unknown_node_404(client):
    assert client.get("/api/nodes/nope", headers=ADMIN).status_code == 404
    assert client.get("/api/nodes/nope/models", headers=ADMIN).status_code == 404


def test_node_record_shape(client):
    join(client, "nandiastra", "http://192.168.29.242:11434", ["qwen3.5:0.8b"], cpu=12.5, ram=48.2)
    rec = client.get("/api/nodes/nandiastra", headers=ADMIN).get_json()
    assert rec == {
        "node": "nandiastra",
        "ollama_url": "http://192.168.29.242:11434",
        "cpu": 12.5,
        "ram": 48.2,
        "ollama_lan_reachable": True,
        "models": ["qwen3.5:0.8b"],
        "model_details": {},
        "last_seen": 1_700_000_000.0,
        "online": True,
        "status": "online",
    }


def test_models_with_same_name_stay_associated_with_their_nodes(client, clock):
    join(client, "n1", "http://127.0.0.1:1", ["shared:1", "only1:1"])
    join(client, "n2", "http://127.0.0.1:2", ["shared:1"])
    clock.now += 30
    join(client, "n2", "http://127.0.0.1:2", ["shared:1"])  # n1 is now offline
    models = client.get("/api/models", headers=ADMIN).get_json()["models"]
    none = dict.fromkeys(DETAIL_KEYS)  # agents that send no details leave these empty
    assert models == [
        {"model": "only1:1", "node": "n1", "available": False, **none},
        {"model": "shared:1", "node": "n1", "available": False, **none},
        {"model": "shared:1", "node": "n2", "available": True, **none},
    ]


def test_aggregate_tags_only_includes_online_nodes(client, clock):
    join(client, "n1", "http://127.0.0.1:1", ["shared:1", "old:1"])
    clock.now += 30
    join(client, "n2", "http://127.0.0.1:2", ["shared:1", "new:1"])
    tags = client.get("/api/ollama/api/tags", headers=ADMIN).get_json()["models"]
    assert [(t["name"], t["nodes"]) for t in tags] == [("new:1", ["n2"]), ("shared:1", ["n2"])]


def test_norm_model():
    assert norm_model("llama3") == "llama3:latest"
    assert norm_model("llama3:8b") == "llama3:8b"
    assert norm_model("localhost:5000/lib/m") == "localhost:5000/lib/m:latest"


# --------------------------------------------------------------------------- #
# Proxy: automatic routing
# --------------------------------------------------------------------------- #
def test_auto_route_non_streaming(client, make_ollama):
    o = make_ollama("alpha", ["tiny:1b"])
    join(client, "alpha", o.url, ["tiny:1b"])
    r = client.post("/api/ollama/api/generate", json={"model": "tiny:1b", "prompt": "hi", "stream": False}, headers=ADMIN)
    assert r.status_code == 200
    assert r.get_json()["served_by"] == "alpha"
    assert r.headers["X-MeshControl-Node"] == "alpha"
    # The client's admin token must never leak upstream.
    assert o.requests[0]["auth"] is None
    assert o.requests[0]["path"] == "/api/generate"


def test_auto_route_streams_ndjson(client, make_ollama):
    o = make_ollama("alpha", ["tiny:1b"])
    join(client, "alpha", o.url, ["tiny:1b"])
    r = client.post("/api/ollama/api/chat", json={"model": "tiny:1b", "messages": []}, headers=ADMIN)
    assert r.status_code == 200
    assert r.headers["Content-Type"].startswith("application/x-ndjson")
    lines = ndjson(r)
    assert [l["response"] for l in lines] == ["Hel", "lo", ""]
    assert lines[-1]["done"] is True


def test_latest_tag_is_implicit(client, make_ollama):
    o = make_ollama("alpha", ["llama3:latest"])
    join(client, "alpha", o.url, ["llama3:latest"])
    r = client.post("/api/ollama/api/generate", json={"model": "llama3", "stream": False}, headers=ADMIN)
    assert r.status_code == 200


def test_auto_route_picks_only_node_with_model(client, make_ollama):
    a, b = make_ollama("alpha", ["x:1"]), make_ollama("beta", ["y:1"])
    join(client, "alpha", a.url, ["x:1"])
    join(client, "beta", b.url, ["y:1"])
    r = client.post("/api/ollama/api/generate", json={"model": "y:1", "stream": False}, headers=ADMIN)
    assert r.get_json()["served_by"] == "beta"


def test_round_robin_across_nodes_hosting_same_model(client, make_ollama):
    a, b = make_ollama("alpha", ["m:1"]), make_ollama("beta", ["m:1"])
    join(client, "alpha", a.url, ["m:1"])
    join(client, "beta", b.url, ["m:1"])
    served = {
        client.post("/api/ollama/api/generate", json={"model": "m:1", "stream": False}, headers=ADMIN).get_json()["served_by"]
        for _ in range(4)
    }
    assert served == {"alpha", "beta"}


def test_unknown_model_404_and_offline_model_503(client, make_ollama, clock):
    o = make_ollama("alpha", ["m:1"])
    join(client, "alpha", o.url, ["m:1"])
    r = client.post("/api/ollama/api/generate", json={"model": "nope:1"}, headers=ADMIN)
    assert r.status_code == 404
    clock.now += 60
    r = client.post("/api/ollama/api/generate", json={"model": "m:1"}, headers=ADMIN)
    assert r.status_code == 503
    assert o.requests == []  # offline node must not be contacted


def test_node_with_unreachable_ollama_is_not_selected(client, make_ollama):
    o = make_ollama("alpha", ["m:1"])
    join(client, "alpha", o.url, ["m:1"], reachable=False)
    r = client.post("/api/ollama/api/generate", json={"model": "m:1"}, headers=ADMIN)
    assert r.status_code == 503


def test_connect_failure_falls_back_to_next_node(client, make_ollama):
    good = make_ollama("good", ["m:1"])
    join(client, "dead", "http://127.0.0.1:1", ["m:1"])   # nothing listens on port 1
    join(client, "good", good.url, ["m:1"])
    for _ in range(4):  # run enough times to exercise both rotation orders
        r = client.post("/api/ollama/api/generate", json={"model": "m:1", "stream": False}, headers=ADMIN)
        assert r.status_code == 200 and r.get_json()["served_by"] == "good"


def test_all_nodes_down_returns_502(client):
    join(client, "dead", "http://127.0.0.1:1", ["m:1"])
    r = client.post("/api/ollama/api/generate", json={"model": "m:1"}, headers=ADMIN)
    assert r.status_code == 502


@pytest.mark.parametrize("body", [b"not json", b"[]", b'{"prompt": "no model"}', b'{"model": ""}', b'{"model": 5}'])
def test_proxy_rejects_bad_bodies(client, body):
    r = client.post("/api/ollama/api/generate", data=body, content_type="application/json", headers=ADMIN)
    assert r.status_code == 400


def test_proxy_only_allows_known_endpoints(client, make_ollama):
    o = make_ollama("alpha", ["m:1"])
    join(client, "alpha", o.url, ["m:1"])
    for ep in ("pull", "delete", "..", "show"):
        r = client.post(f"/api/ollama/api/{ep}", json={"model": "m:1"}, headers=ADMIN)
        assert r.status_code == 404
    assert o.requests == []


# --------------------------------------------------------------------------- #
# Proxy: direct routing
# --------------------------------------------------------------------------- #
def test_direct_routing_targets_chosen_node(client, make_ollama):
    a, b = make_ollama("alpha", ["m:1"]), make_ollama("beta", ["m:1"])
    join(client, "alpha", a.url, ["m:1"])
    join(client, "beta", b.url, ["m:1"])
    for _ in range(3):
        r = client.post("/api/nodes/beta/ollama/api/generate", json={"model": "m:1", "stream": False}, headers=ADMIN)
        assert r.get_json()["served_by"] == "beta"
    assert a.requests == []


def test_direct_routing_streams(client, make_ollama):
    o = make_ollama("alpha", ["m:1"])
    join(client, "alpha", o.url, ["m:1"])
    r = client.post("/api/nodes/alpha/ollama/api/chat", json={"model": "m:1", "messages": []}, headers=ADMIN)
    assert [l["response"] for l in ndjson(r)] == ["Hel", "lo", ""]


def test_direct_routing_errors(client, make_ollama, clock):
    o = make_ollama("alpha", ["m:1"])
    join(client, "alpha", o.url, ["m:1"])
    assert client.post("/api/nodes/ghost/ollama/api/generate", json={}, headers=ADMIN).status_code == 404
    clock.now += 60
    assert client.post("/api/nodes/alpha/ollama/api/generate", json={}, headers=ADMIN).status_code == 503


def test_error_responses_are_json(client):
    r = client.get("/nope", headers=ADMIN)
    assert r.status_code == 404 and "error" in r.get_json()


@pytest.mark.parametrize("path", ["/", "/nodes", "/nodes/nandiastra", "/models", "/models/qwen3.5%3A0.8b", "/models/lib%2Fm%3A1", "/chat"])
def test_ui_pages_served_without_auth_and_locked_down(client, path):
    r = client.get(path)
    assert r.status_code == 200 and r.mimetype == "text/html"
    assert b"MeshControl" in r.data
    csp = r.headers["Content-Security-Policy"]
    assert "default-src 'none'" in csp and "script-src 'self'" in csp and "connect-src 'self'" in csp
    assert "'unsafe-inline'" not in csp.split("style-src")[0]  # no inline scripts
    assert r.headers["X-Content-Type-Options"] == "nosniff"


@pytest.mark.parametrize("name,mime", [
    ("app.css", "text/css"), ("theme.js", "text/javascript"), ("common.js", "text/javascript"), ("pulls.js", "text/javascript"),
    ("dashboard.js", "text/javascript"), ("nodes.js", "text/javascript"), ("node.js", "text/javascript"),
    ("models.js", "text/javascript"), ("model.js", "text/javascript"), ("chat.js", "text/javascript"),
])
def test_ui_assets(client, name, mime):
    r = client.get(f"/static/{name}")
    assert r.status_code == 200 and r.mimetype == mime
    assert r.headers["X-Content-Type-Options"] == "nosniff"


@pytest.mark.parametrize("name", ["controller.py", "agent.py", "..%2Fcontroller.py", "app.js", "app.js.map", ".env"])
def test_only_whitelisted_assets_are_served(client, name):
    assert client.get(f"/static/{name}").status_code == 404


def test_ui_does_not_expose_cluster_data_and_api_still_needs_auth(client):
    join(client, "secretnode", "http://127.0.0.1:11434", ["m:1"])
    assert b"secretnode" not in client.get("/nodes/secretnode").data
    assert client.get("/api/nodes/secretnode").status_code == 401


PAGES = {
    "/": b'id="stats"', "/nodes": b'id="chips"', "/nodes/n1": b'id="alert"',
    "/models": b'id="pullform"', "/models/m%3A1": b'id="copies"', "/chat": b'id="send"',
}


@pytest.mark.parametrize("path,marker", list(PAGES.items()))
def test_each_page_is_its_own_html_file_without_inline_scripts(client, path, marker):
    import re
    html = client.get(path).data
    assert marker in html
    assert not re.search(rb"<script(?![^>]*\bsrc=)", html)
    assert b"/static/common.js" in html and b"/static/theme.js" in html
    assert b"/static/app.css" in html


def test_shell_script_provides_theme_toggle(client):
    js = client.get("/static/common.js").data
    assert b'"theme"' in js and b"mc_theme" in js


# --------------------------------------------------------------------------- #
# Node status: online / provider unreachable / offline
# --------------------------------------------------------------------------- #
def test_node_status_has_three_states(client, clock):
    join(client, "up", "http://127.0.0.1:1", ["a:1"])
    join(client, "noprov", "http://127.0.0.1:2", [], reachable=False)
    status = lambda n: client.get(f"/api/nodes/{n}", headers=ADMIN).get_json()["status"]
    assert status("up") == "online"
    assert status("noprov") == "provider_unreachable"   # heartbeating, but Ollama is not reachable
    clock.now += 60
    assert status("up") == "offline" and status("noprov") == "offline"


def test_provider_unreachable_node_is_online_but_not_routable(client, make_ollama):
    o = make_ollama("alpha", ["m:1"])
    join(client, "alpha", o.url, ["m:1"], reachable=False, cpu=33)
    rec = client.get("/api/nodes/alpha", headers=ADMIN).get_json()
    assert rec["online"] is True and rec["status"] == "provider_unreachable" and rec["cpu"] == 33
    assert client.get("/api/models", headers=ADMIN).get_json()["models"][0]["available"] is False


# --------------------------------------------------------------------------- #
# Model details (size, parameters, download date)
# --------------------------------------------------------------------------- #
DETAIL = {
    "name": "llama3.2:3b", "size": 2019393189, "modified_at": 1_760_000_000.5, "digest": "sha256:a80c4f17acd5",
    "family": "llama", "parameter_size": "3.2B", "quantization_level": "Q4_K_M", "format": "gguf",
}


def heartbeat(client, node, **body):
    payload = {"node": node, "cpu": 1, "ram": 1, "ollama_lan_reachable": True, "models": [DETAIL["name"]], **body}
    return client.post("/api/agent/heartbeat", data=json.dumps(payload), content_type="application/json", headers=AGENT)


def test_model_details_are_stored_and_exposed(client):
    join(client, "n1", "http://127.0.0.1:1", [])
    assert heartbeat(client, "n1", model_details=[DETAIL]).status_code == 200
    entry = client.get("/api/models", headers=ADMIN).get_json()["models"][0]
    assert entry == {"model": "llama3.2:3b", "node": "n1", "available": True,
                     **{k: v for k, v in DETAIL.items() if k != "name"}}
    rec = client.get("/api/nodes/n1", headers=ADMIN).get_json()
    assert rec["model_details"]["llama3.2:3b"]["parameter_size"] == "3.2B"


def test_model_details_are_replaced_with_each_heartbeat(client):
    join(client, "n1", "http://127.0.0.1:1", [])
    heartbeat(client, "n1", model_details=[DETAIL])
    heartbeat(client, "n1", models=["other:1"], model_details=[{**DETAIL, "name": "other:1", "size": 5}])
    models = client.get("/api/models", headers=ADMIN).get_json()["models"]
    assert [(m["model"], m["size"]) for m in models] == [("other:1", 5)]


def test_details_for_models_not_in_the_list_are_ignored(client):
    join(client, "n1", "http://127.0.0.1:1", [])
    heartbeat(client, "n1", model_details=[DETAIL, {**DETAIL, "name": "ghost:1"}])
    assert list(client.get("/api/nodes/n1", headers=ADMIN).get_json()["model_details"]) == ["llama3.2:3b"]


def test_model_details_are_sanitised_not_trusted(client):
    join(client, "n1", "http://127.0.0.1:1", [])
    junk = [None, 5, {"nope": 1}, {"name": ""}, {"name": "x" * 300},
            {**DETAIL, "size": -5, "modified_at": "yesterday", "family": 7, "digest": "d" * 500, "extra": "<script>"}]
    assert heartbeat(client, "n1", model_details=junk).status_code == 200
    info = client.get("/api/nodes/n1", headers=ADMIN).get_json()["model_details"]["llama3.2:3b"]
    assert info["size"] is None and info["modified_at"] is None and info["family"] is None
    assert len(info["digest"]) == 128 and "extra" not in info


@pytest.mark.parametrize("bad", ["x", {"a": 1}, [DETAIL] * 1001])
def test_model_details_wrong_shape_is_rejected(client, bad):
    join(client, "n1", "http://127.0.0.1:1", [])
    assert heartbeat(client, "n1", model_details=bad).status_code == 400


# --------------------------------------------------------------------------- #
# Pulling models
# --------------------------------------------------------------------------- #
def wait_for(pred, timeout=5.0):
    end = time.time() + timeout
    while time.time() < end:
        value = pred()
        if value:
            return value
        time.sleep(0.02)
    raise AssertionError("condition not met in time")


def pull(client, node, model, **kw):
    return client.post(f"/api/nodes/{node}/pull", json={"model": model}, headers=ADMIN, **kw)


def job(client, jid):
    return next(j for j in client.get("/api/pulls", headers=ADMIN).get_json()["pulls"] if j["id"] == jid)


def test_pull_requires_admin(client):
    assert client.post("/api/nodes/n1/pull", json={"model": "m:1"}).status_code == 401
    assert client.post("/api/nodes/n1/pull", json={"model": "m:1"}, headers=AGENT).status_code == 401
    assert client.get("/api/pulls").status_code == 401
    assert client.delete("/api/pulls/x").status_code == 401


def test_pull_runs_in_background_and_reports_progress(client, make_ollama):
    o = make_ollama("alpha", [])
    join(client, "alpha", o.url, [])
    r = pull(client, "alpha", "llama3.2:3b")
    assert r.status_code == 202
    body = r.get_json()
    assert body["state"] == "running" and body["node"] == "alpha" and body["model"] == "llama3.2:3b"
    done = wait_for(lambda: (j := job(client, body["id"]))["state"] != "running" and j)
    assert done["state"] == "success" and done["status"] == "success"
    assert done["completed"] == 100 and done["total"] == 100 and done["error"] is None
    # The upstream request is built by the controller and never carries client credentials or extra flags.
    assert o.requests[0]["path"] == "/api/pull"
    assert o.requests[0]["body"] == {"model": "llama3.2:3b", "stream": True}
    assert o.requests[0]["auth"] is None


def test_pull_progress_sums_layers(client, make_ollama):
    o = make_ollama("alpha", [])
    o.pull_frames = [
        {"status": "pulling a", "digest": "sha256:a", "total": 100, "completed": 100},
        {"status": "pulling b", "digest": "sha256:b", "total": 300, "completed": 30},
        "WAIT",
        {"status": "success"},
    ]
    join(client, "alpha", o.url, [])
    jid = pull(client, "alpha", "m:1").get_json()["id"]
    assert o.pull_started.wait(5)
    j = wait_for(lambda: (x := job(client, jid))["completed"] == 130 and x)
    assert (j["completed"], j["total"], j["state"]) == (130, 400, "running")
    o.pull_gate.set()
    wait_for(lambda: job(client, jid)["state"] == "success")


def test_pull_rejects_offline_and_provider_unreachable_nodes(client, make_ollama, clock):
    o = make_ollama("alpha", [])
    join(client, "alpha", o.url, [], reachable=False)
    r = pull(client, "alpha", "m:1")
    assert r.status_code == 503 and r.get_json()["status"] == "provider_unreachable"
    assert "provider unreachable" in r.get_json()["error"]
    clock.now += 60
    r = pull(client, "alpha", "m:1")
    assert r.status_code == 503 and r.get_json()["status"] == "offline"
    assert o.requests == []


def test_pull_unknown_node_404(client):
    assert pull(client, "ghost", "m:1").status_code == 404


@pytest.mark.parametrize("model", [None, 5, "", "a b", "../x", "m\n:1", "x" * 300, "evil.example/ns/m", "localhost:5000/m", "a/b/c"])
def test_pull_rejects_bad_model_names(client, make_ollama, model):
    o = make_ollama("alpha", [])
    join(client, "alpha", o.url, [])
    assert pull(client, "alpha", model).status_code == 400
    assert o.requests == []


@pytest.mark.parametrize("model", ["llama3.2:3b", "qwen3.5:0.8b", "library/llama3", "hf.co/org/repo:Q4_K_M", "registry.ollama.ai/library/m:1"])
def test_validate_pull_model_accepts(model):
    assert validate_pull_model(model, Config(agent_token="t")) == model


def test_pull_body_must_be_json_object(client, make_ollama):
    o = make_ollama("alpha", [])
    join(client, "alpha", o.url, [])
    r = client.post("/api/nodes/alpha/pull", data=b"nope", content_type="application/json", headers=ADMIN)
    assert r.status_code == 400


def test_duplicate_pull_is_a_conflict(client, make_ollama):
    o = make_ollama("alpha", [])
    o.pull_frames = [{"status": "pulling manifest"}, "WAIT", {"status": "success"}]
    join(client, "alpha", o.url, [])
    jid = pull(client, "alpha", "m").get_json()["id"]
    assert o.pull_started.wait(5)
    assert pull(client, "alpha", "m:latest").status_code == 409       # same model: ":latest" is implicit
    o.pull_gate.set()
    wait_for(lambda: job(client, jid)["state"] == "success")
    assert pull(client, "alpha", "m").status_code == 202              # allowed again once finished


def test_pull_can_be_cancelled(client, make_ollama):
    o = make_ollama("alpha", [])
    o.pull_frames = [{"status": "pulling a", "digest": "sha256:a", "total": 100, "completed": 10}, "WAIT", {"status": "success"}]
    join(client, "alpha", o.url, [])
    jid = pull(client, "alpha", "m:1").get_json()["id"]
    assert o.pull_started.wait(5)
    assert client.delete(f"/api/pulls/{jid}", headers=ADMIN).status_code == 200
    assert wait_for(lambda: job(client, jid)["state"] == "cancelled")
    assert client.delete(f"/api/pulls/{jid}", headers=ADMIN).status_code == 409   # already finished
    o.pull_gate.set()


def test_cancel_unknown_pull_404(client):
    assert client.delete("/api/pulls/nope", headers=ADMIN).status_code == 404


def test_pull_error_frame_marks_job_failed(client, make_ollama):
    o = make_ollama("alpha", [])
    o.pull_frames = [{"status": "pulling manifest"}, {"error": "pull model manifest: file does not exist"}]
    join(client, "alpha", o.url, [])
    jid = pull(client, "alpha", "nope:1").get_json()["id"]
    j = wait_for(lambda: (x := job(client, jid))["state"] != "running" and x)
    assert j["state"] == "error" and "file does not exist" in j["error"]


def test_pull_that_ends_without_success_is_an_error(client, make_ollama):
    o = make_ollama("alpha", [])
    o.pull_frames = [{"status": "pulling manifest"}]
    join(client, "alpha", o.url, [])
    jid = pull(client, "alpha", "m:1").get_json()["id"]
    j = wait_for(lambda: (x := job(client, jid))["state"] != "running" and x)
    assert j["state"] == "error" and "before it completed" in j["error"]


def test_pull_to_dead_node_is_an_error(client):
    join(client, "dead", "http://127.0.0.1:1", [])
    jid = pull(client, "dead", "m:1").get_json()["id"]
    j = wait_for(lambda: (x := job(client, jid))["state"] != "running" and x)
    assert j["state"] == "error" and "127.0.0.1" not in j["error"]


def test_pull_endpoint_is_still_not_proxied_through_the_generic_route(client, make_ollama):
    o = make_ollama("alpha", ["m:1"])
    join(client, "alpha", o.url, ["m:1"])
    assert client.post("/api/ollama/api/pull", json={"model": "m:1"}, headers=ADMIN).status_code == 404
    assert client.post("/api/nodes/alpha/ollama/api/pull", json={"model": "m:1"}, headers=ADMIN).status_code == 404
    assert o.requests == []
