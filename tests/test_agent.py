import json
import threading
from datetime import datetime, timedelta, timezone

import requests

from agent import Agent, AgentConfig, parse_timestamp

LOCAL = "http://127.0.0.1:11434"
LAN = "http://192.168.1.5:11434"


class Resp:
    def __init__(self, status=200, payload=None):
        self.status_code = status
        self._payload = payload if payload is not None else {}
        self.text = json.dumps(self._payload)

    def json(self):
        return self._payload

    def raise_for_status(self):
        if self.status_code >= 400:
            raise requests.HTTPError(str(self.status_code))


class FakeSession:
    """Routes (METHOD, url) to a Resp, an Exception, or a list consumed in order."""

    def __init__(self, routes):
        self.routes = routes
        self.headers = {}
        self.calls = []
        self.on_call = None

    def _handle(self, method, url, **kw):
        self.calls.append((method, url, kw))
        if self.on_call:
            self.on_call(method, url, kw)
        r = self.routes.get((method, url), requests.ConnectionError("refused"))
        if isinstance(r, list):
            r = r.pop(0) if len(r) > 1 else r[0]
        if isinstance(r, Exception):
            raise r
        return r

    def get(self, url, **kw):
        return self._handle("GET", url, **kw)

    def post(self, url, **kw):
        return self._handle("POST", url, **kw)


def make_agent(routes, **overrides):
    cfg = AgentConfig(
        controller_url="http://ctl:8000",
        token="secret",
        node_id="n1",
        ollama_local_url=LOCAL,
        advertise_url=LAN,
        heartbeat_interval=0,
        **overrides,
    )
    return Agent(cfg, session=FakeSession(routes), stop=threading.Event())


def tags(*names):
    return Resp(200, {"models": [{"name": n} for n in names]})


def test_sets_bearer_header_without_logging_it():
    agent = make_agent({})
    assert agent.session.headers["Authorization"] == "Bearer secret"


def test_read_ollama_via_lan_means_reachable():
    agent = make_agent({("GET", f"{LAN}/api/tags"): tags("a:1", "b:2")})
    assert agent.read_ollama() == (True, ["a:1", "b:2"])


def test_read_ollama_local_only_means_not_lan_reachable():
    agent = make_agent({("GET", f"{LOCAL}/api/tags"): tags("a:1")})  # LAN address refuses
    assert agent.read_ollama() == (False, ["a:1"])


def test_read_ollama_down():
    assert make_agent({}).read_ollama() == (False, [])


def test_read_ollama_tolerates_garbage():
    bad = Resp(200, {"models": [None, {"nope": 1}, {"name": "ok:1"}]})
    assert make_agent({("GET", f"{LAN}/api/tags"): bad}).read_ollama() == (True, ["ok:1"])


def test_register_success_and_failure_modes():
    url = "http://ctl:8000/api/agent/register"
    assert make_agent({("POST", url): Resp(201)}).register() is True
    assert make_agent({("POST", url): Resp(401)}).register() is False
    assert make_agent({("POST", url): Resp(400, {"error": "bad"})}).register() is False
    assert make_agent({("POST", url): requests.ConnectionError("down")}).register() is False


def test_register_payload():
    agent = make_agent({("POST", "http://ctl:8000/api/agent/register"): Resp(201)})
    agent.register()
    _, _, kw = agent.session.calls[0]
    assert kw["json"] == {"node": "n1", "ollama_url": LAN}


def test_heartbeat_payload_is_compact_and_complete():
    hb = "http://ctl:8000/api/agent/heartbeat"
    agent = make_agent({("GET", f"{LAN}/api/tags"): tags("a:1"), ("POST", hb): Resp(200)})
    assert agent.heartbeat() == "ok"
    _, _, kw = agent.session.calls[-1]
    assert " " not in kw["data"]
    body = json.loads(kw["data"])
    assert set(body) == {"node", "cpu", "ram", "ollama_lan_reachable", "models", "model_details"}
    assert body["models"] == ["a:1"] and body["ollama_lan_reachable"] is True
    assert 0 <= body["cpu"] <= 100 and 0 <= body["ram"] <= 100


def test_heartbeat_result_codes():
    hb = "http://ctl:8000/api/agent/heartbeat"
    assert make_agent({("POST", hb): Resp(404)}).heartbeat() == "reregister"
    assert make_agent({("POST", hb): Resp(401)}).heartbeat() == "error"
    assert make_agent({("POST", hb): requests.Timeout()}).heartbeat() == "error"


def test_run_registers_then_beats_and_reregisters_after_controller_restart():
    reg = "http://ctl:8000/api/agent/register"
    hb = "http://ctl:8000/api/agent/heartbeat"
    agent = make_agent({
        ("POST", reg): Resp(201),
        ("POST", hb): [Resp(200), Resp(404), Resp(200)],  # 404 = controller forgot us
    })
    beats = []

    def on_call(method, url, kw):
        if url == hb:
            beats.append(1)
            if len(beats) >= 3:
                agent.stop.set()

    agent.session.on_call = on_call
    agent.run()
    urls = [u for _, u, _ in agent.session.calls if u in (reg, hb)]
    assert urls == [reg, hb, hb, reg, hb]


def test_run_retries_registration_with_backoff(monkeypatch):
    reg = "http://ctl:8000/api/agent/register"
    agent = make_agent({
        ("POST", reg): [requests.ConnectionError("down"), requests.ConnectionError("down"), Resp(201)],
        ("POST", "http://ctl:8000/api/agent/heartbeat"): Resp(200),
    })
    waits = []
    monkeypatch.setattr(agent.stop, "wait", lambda t=None: (waits.append(t), agent.stop.set() if len(waits) > 3 else None))
    agent.run()
    assert waits[:2] == [1.0, 2.0]  # exponential backoff while the controller is unreachable


OLLAMA_ENTRY = {
    "name": "llama3.2:3b",
    "model": "llama3.2:3b",
    "modified_at": "2025-03-01T10:20:30.123456789+05:30",
    "size": 2019393189,
    "digest": "a80c4f17acd55265feec403c7aef86be0c25983ab279d83f3bcd3abbcb5b8b72",
    "details": {"format": "gguf", "family": "llama", "parameter_size": "3.2B", "quantization_level": "Q4_K_M"},
}


def test_read_models_reports_size_parameters_and_download_time():
    agent = make_agent({("GET", f"{LAN}/api/tags"): Resp(200, {"models": [OLLAMA_ENTRY]})})
    reachable, models = agent.read_models()
    assert reachable is True
    expected = datetime(2025, 3, 1, 10, 20, 30, 123456, tzinfo=timezone(timedelta(hours=5, minutes=30))).timestamp()
    assert models == [{
        "name": "llama3.2:3b", "size": 2019393189, "modified_at": expected,
        "digest": OLLAMA_ENTRY["digest"], "family": "llama", "parameter_size": "3.2B",
        "quantization_level": "Q4_K_M", "format": "gguf",
    }]


def test_read_models_tolerates_missing_and_odd_fields():
    odd = {"name": "x:1", "size": "big", "modified_at": 12, "details": "none"}
    agent = make_agent({("GET", f"{LAN}/api/tags"): Resp(200, {"models": [odd]})})
    assert agent.read_models() == (True, [{"name": "x:1"}])


def test_heartbeat_sends_model_details():
    hb = "http://ctl:8000/api/agent/heartbeat"
    agent = make_agent({("GET", f"{LAN}/api/tags"): Resp(200, {"models": [OLLAMA_ENTRY]}), ("POST", hb): Resp(200)})
    agent.heartbeat()
    body = json.loads(agent.session.calls[-1][2]["data"])
    assert body["models"] == ["llama3.2:3b"]
    assert body["model_details"][0]["parameter_size"] == "3.2B"



def test_parse_timestamp_variants():
    utc = datetime(2025, 1, 2, 3, 4, 5, tzinfo=timezone.utc).timestamp()
    assert parse_timestamp("2025-01-02T03:04:05Z") == utc
    assert parse_timestamp("2025-01-02T03:04:05+00:00") == utc
    assert parse_timestamp("2025-01-02T03:04:05.5Z") == utc + 0.5
    for bad in (None, 5, "", "yesterday", "2025-13-45T99:99:99Z"):
        assert parse_timestamp(bad) is None
