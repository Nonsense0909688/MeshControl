package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func doRequest(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServerHandlerConstructionAndNotFound(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	h := s.Handler() // duplicate ServeMux route registrations panic here if present
	if got := doRequest(h, http.MethodGet, "/does-not-exist", "", "").Code; got != 404 {
		t.Fatalf("unknown route status=%d", got)
	}
}

func TestAdminAPIFailsClosedWhenAdminTokenMissing(t *testing.T) {
	cfg := testConfig()
	cfg.AdminToken = ""
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	for _, path := range []string{"/api/nodes", "/api/models", "/api/ollama/api/tags"} {
		r := doRequest(s.Handler(), http.MethodGet, path, "", "")
		if r.Code != http.StatusUnauthorized {
			t.Errorf("%s: status=%d want 401", path, r.Code)
		}
	}
}

func TestAdminAuthenticationTable(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	for _, tt := range []struct {
		name, header string
		want         int
	}{
		{"missing", "", 401}, {"wrong", "wrong-token", 401},
		{"wrong scheme", "Basic abc", 401}, {"correct", s.cfg.AdminToken, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := doRequest(s.Handler(), "GET", "/api/nodes", "", tt.header)
			if r.Code != tt.want {
				t.Fatalf("status=%d want=%d body=%s", r.Code, tt.want, r.Body.String())
			}
		})
	}
}

func TestAgentAuthenticationRequired(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	payload := `{"node":"node-a","ollama_url":"http://127.0.0.1:11434"}`
	for _, tt := range []struct {
		name, token string
		want        int
	}{
		{"missing", "", 401}, {"invalid", "not-the-token", 401}, {"valid", s.cfg.AgentToken, 201},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := doRequest(s.Handler(), "POST", "/api/agent/register", payload, tt.token)
			if r.Code != tt.want {
				t.Fatalf("status=%d want=%d body=%s", r.Code, tt.want, r.Body.String())
			}
		})
	}
}

func TestAgentRegisterAndHeartbeatAPI(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	h := s.Handler()
	reg := doRequest(h, "POST", "/api/agent/register", `{"node":"node-a","ollama_url":"http://127.0.0.1:11434"}`, s.cfg.AgentToken)
	if reg.Code != 201 {
		t.Fatalf("register status=%d body=%s", reg.Code, reg.Body.String())
	}
	hb := doRequest(h, "POST", "/api/agent/heartbeat", `{"node":"node-a","cpu":12.5,"ram":55,"ollama_lan_reachable":true,"models":["llama3"],"model_details":[{"name":"llama3","size":42}]}`, s.cfg.AgentToken)
	if hb.Code != 200 {
		t.Fatalf("heartbeat status=%d body=%s", hb.Code, hb.Body.String())
	}
	v, ok := s.reg.Get("node-a")
	if !ok || len(v.Models) != 1 || v.Models[0] != "llama3" || v.CPU == nil || *v.CPU != 12.5 {
		t.Fatalf("bad registry view %+v", v)
	}
}

func TestAgentRegisterRejectsMalformedPayloads(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	cases := []struct{ name, body string }{
		{"invalid JSON", "{"}, {"array", `[]`}, {"bad node id", `{"node":"../etc","ollama_url":"http://127.0.0.1:11434"}`},
		{"bad URL", `{"node":"node-a","ollama_url":"http://user:pass@127.0.0.1:11434"}`},
		{"public URL", `{"node":"node-a","ollama_url":"http://8.8.8.8:11434"}`},
		{"path URL", `{"node":"node-a","ollama_url":"http://127.0.0.1:11434/api"}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := doRequest(s.Handler(), "POST", "/api/agent/register", tt.body, s.cfg.AgentToken)
			if r.Code < 400 || r.Code >= 500 {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
		})
	}
}

func TestAgentHeartbeatValidationAndReregister(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	base := `{"node":"node-a","cpu":0,"ram":0,"ollama_lan_reachable":true,"models":[],"model_details":[]}`
	for _, tt := range []struct {
		name, body string
		want       int
	}{
		{"unknown node", base, 404},
		{"CPU over 100", strings.Replace(base, `"cpu":0`, `"cpu":101`, 1), 400},
		{"negative RAM", strings.Replace(base, `"ram":0`, `"ram":-1`, 1), 400},
		{"missing boolean", `{"node":"node-a","cpu":0,"ram":0,"models":[]}`, 400},
		{"models wrong type", strings.Replace(base, `"models":[]`, `"models":"llama3"`, 1), 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := doRequest(s.Handler(), "POST", "/api/agent/heartbeat", tt.body, s.cfg.AgentToken)
			if r.Code != tt.want {
				t.Fatalf("status=%d want=%d body=%s", r.Code, tt.want, r.Body.String())
			}
		})
	}
}

func TestOversizedRequestReturns413(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBodyBytes = 64
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	body, _ := json.Marshal(map[string]string{"model": strings.Repeat("x", 200)})
	r := doRequest(s.Handler(), "POST", "/api/ollama/api/chat", string(body), cfg.AdminToken)
	if r.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestUnknownProxyEndpointReturns404(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	r := doRequest(s.Handler(), "POST", "/api/ollama/api/exec", `{}`, s.cfg.AdminToken)
	if r.Code != 404 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestConcurrencyLimitReturns503(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConcurrentRequests = 1
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	s.sem <- struct{}{} // simulate a request holding the only slot
	r := doRequest(s.Handler(), "GET", "/healthz", "", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", r.Code)
	}
	if r.Header().Get("Retry-After") != "1" {
		t.Fatalf("missing Retry-After: %+v", r.Header())
	}
}

func TestTagsOnlyListsUsableNodes(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	s.reg.Register("unreachable", "http://127.0.0.1:11434")
	s.reg.Heartbeat("unreachable", 1, 1, false, []string{"should-hide"}, nil)
	s.reg.Register("reachable", "http://127.0.0.1:11434")
	s.reg.Heartbeat("reachable", 1, 1, true, []string{"should-show"}, nil)
	r := doRequest(s.Handler(), "GET", "/api/ollama/api/tags", "", s.cfg.AdminToken)
	if r.Code != 200 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), "should-hide") || !strings.Contains(r.Body.String(), "should-show") {
		t.Fatalf("unexpected tags response: %s", r.Body.String())
	}
}

func TestProxyAutoStreamsAndDoesNotForwardAuth(t *testing.T) {
	var sawAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("upstream path=%s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if !bytes.Contains(b, []byte(`"model":"llama3"`)) {
			t.Errorf("wrong body %s", b)
		}
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"message\":{\"content\":\"hi\"}}\n"))
	}))
	defer upstream.Close()
	cfg := testConfig()
	port, _ := strconv.Atoi(strings.Split(upstream.URL[strings.LastIndex(upstream.URL, ":")+1:], "/")[0])
	cfg.AllowedNodePorts[port] = true
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	s.reg.Register("node-a", upstream.URL)
	s.reg.Heartbeat("node-a", 1, 1, true, []string{"llama3"}, nil)
	req := httptest.NewRequest("POST", "/api/ollama/api/chat", strings.NewReader(`{"model":"llama3","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("stream body missing: %s", rec.Body.String())
	}
	if got := rec.Header().Get("X-MeshControl-Node"); got != "node-a" {
		t.Fatalf("node header=%q", got)
	}
	if sawAuth != "" {
		t.Fatalf("upstream received controller auth header %q", sawAuth)
	}
}

func TestDirectProxyRejectsProviderUnreachable(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	s.reg.Register("node-a", "http://127.0.0.1:11434")
	s.reg.Heartbeat("node-a", 0, 0, false, []string{"llama3"}, nil)
	r := doRequest(s.Handler(), "POST", "/api/nodes/node-a/ollama/api/chat", `{"model":"llama3"}`, s.cfg.AdminToken)
	if r.Code != 503 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestRequestBodyHelpers(t *testing.T) {
	valid := httptest.NewRequest("POST", "/", strings.NewReader(`{"hello":"world"}`))
	obj, err := readObject(valid, 64)
	if err != nil || obj["hello"] != "world" {
		t.Fatalf("obj=%v err=%v", obj, err)
	}
	for _, body := range []string{"[1,2]", `"string"`, "null", "{", ""} {
		if _, err := parseObject([]byte(body)); err == nil {
			t.Errorf("parseObject accepted %q", body)
		}
	}
	tooLong := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 20)))
	if _, err := readBody(tooLong, 10); err != errTooLarge {
		t.Fatalf("err=%v want errTooLarge", err)
	}
}

func TestPageAndAssetSecurityHeaders(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(testConfig(), filepath.Join(cwd, "web"))
	defer s.client.CloseIdleConnections()
	page := doRequest(s.Handler(), "GET", "/", "", "")
	if page.Code != 200 {
		t.Fatalf("page status=%d", page.Code)
	}
	if page.Header().Get("Content-Security-Policy") == "" || page.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing security headers: %+v", page.Header())
	}
	asset := doRequest(s.Handler(), "GET", "/static/common.js", "", "")
	if asset.Code != 200 || asset.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("asset status=%d headers=%+v", asset.Code, asset.Header())
	}
	if got := doRequest(s.Handler(), "GET", "/static/../../server.go", "", "").Code; got == 200 {
		t.Fatalf("asset traversal unexpectedly succeeded")
	}
}

func TestSafeTransportBlocksNonAllowlistedIPBeforeDial(t *testing.T) {
	cfg := testConfig()
	tr := newSafeTransport(cfg)
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := tr.DialContext(ctx, "tcp", "8.8.8.8:53")
	if err == nil || !strings.Contains(err.Error(), "outside allowed networks") {
		t.Fatalf("expected allowlist block, err=%v", err)
	}
}

func TestParseObjectAcceptsNestedObjectsAndArrays(t *testing.T) {
	got, err := parseObject([]byte(`{"nested":{"items":[1,true,null]},"n":1.5}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["nested"].(map[string]any); !ok {
		t.Fatalf("nested type %T", got["nested"])
	}
}

func TestHealthEndpointIsLightweightAndValidJSON(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	r := doRequest(s.Handler(), "GET", "/healthz", "", "")
	if r.Code != 200 {
		t.Fatalf("status=%d", r.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("health body=%v", body)
	}
}

func TestRequestCapacityCanRecover(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConcurrentRequests = 1
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	first := doRequest(s.Handler(), "GET", "/healthz", "", "")
	if first.Code != 200 {
		t.Fatalf("first status=%d", first.Code)
	}
	second := doRequest(s.Handler(), "GET", "/healthz", "", "")
	if second.Code != 200 {
		t.Fatalf("capacity did not recover: status=%d", second.Code)
	}
}

func TestConcurrentHTTPHealthStress(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConcurrentRequests = 32
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	const workers, requestsEach = 80, 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, overloaded := 0, 0
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < requestsEach; i++ {
				resp, err := client.Get(httpServer.URL + "/healthz")
				if err != nil {
					errs <- err
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				mu.Lock()
				switch resp.StatusCode {
				case http.StatusOK:
					accepted++
				case http.StatusServiceUnavailable:
					overloaded++
				default:
					errs <- fmt.Errorf("unexpected status %d", resp.StatusCode)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if accepted+overloaded != workers*requestsEach {
		t.Fatalf("handled=%d want=%d", accepted+overloaded, workers*requestsEach)
	}
	if accepted == 0 {
		t.Fatal("no requests were accepted under load")
	}
}

func TestProxyFallsBackOnlyWhenConnectionCannotBeOpened(t *testing.T) {
	// Reserve then close a port to obtain a reliable refused-connection candidate.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"ok":true}`) }))
	defer live.Close()
	livePort, _ := strconv.Atoi(strings.Split(live.URL[strings.LastIndex(live.URL, ":")+1:], "/")[0])
	cfg := testConfig()
	cfg.AllowedNodePorts[deadPort] = true
	cfg.AllowedNodePorts[livePort] = true
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	s.reg.Register("a-dead", fmt.Sprintf("http://127.0.0.1:%d", deadPort))
	s.reg.Heartbeat("a-dead", 0, 0, true, []string{"llama3"}, nil)
	s.reg.Register("b-live", live.URL)
	s.reg.Heartbeat("b-live", 0, 0, true, []string{"llama3"}, nil)
	r := doRequest(s.Handler(), "POST", "/api/ollama/api/chat", `{"model":"llama3"}`, cfg.AdminToken)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"ok":true`) {
		t.Fatalf("fallback failed status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestProxyDoesNotReplayAfterUpstreamHTTPResponse(t *testing.T) {
	var secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "intentional upstream error", http.StatusBadGateway)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer second.Close()
	firstPort, _ := strconv.Atoi(strings.Split(first.URL[strings.LastIndex(first.URL, ":")+1:], "/")[0])
	secondPort, _ := strconv.Atoi(strings.Split(second.URL[strings.LastIndex(second.URL, ":")+1:], "/")[0])
	cfg := testConfig()
	cfg.AllowedNodePorts[firstPort] = true
	cfg.AllowedNodePorts[secondPort] = true
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	s.reg.Register("a-first", first.URL)
	s.reg.Heartbeat("a-first", 0, 0, true, []string{"llama3"}, nil)
	s.reg.Register("b-second", second.URL)
	s.reg.Heartbeat("b-second", 0, 0, true, []string{"llama3"}, nil)
	r := doRequest(s.Handler(), "POST", "/api/ollama/api/chat", `{"model":"llama3"}`, cfg.AdminToken)
	if r.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("request was replayed after an upstream response: calls=%d", secondCalls.Load())
	}
}

func TestSafeTransportRejectsDisallowedPort(t *testing.T) {
	tr := newSafeTransport(testConfig())
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := tr.DialContext(ctx, "tcp", "10.0.0.1:8080")
	if err == nil || !strings.Contains(err.Error(), "ALLOWED_NODE_PORTS") {
		t.Fatalf("expected port allowlist error, got %v", err)
	}
}

func TestLoadAwareRoutingPrefersLessBusyNode(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	lowCPU, lowRAM := 5.0, 20.0
	highCPU, highRAM := 85.0, 90.0
	cands := []NodeView{
		{Node: "busy", CPU: &highCPU, RAM: &highRAM},
		{Node: "free", CPU: &lowCPU, RAM: &lowRAM},
	}
	got := s.orderCandidates(cands)
	if len(got) != 2 || got[0].Node != "free" {
		t.Fatalf("load order = %#v; expected free node first", got)
	}
	// Active streaming requests are a substantial penalty even when the
	// heartbeat metrics are otherwise identical.
	zero, ram := 10.0, 10.0
	s.beginNode("second")
	s.beginNode("second")
	got = s.orderCandidates([]NodeView{{Node: "second", CPU: &zero, RAM: &ram}, {Node: "first", CPU: &zero, RAM: &ram}})
	if got[0].Node != "first" {
		t.Fatalf("in-flight request load was ignored: %#v", got)
	}
	s.endNode("second")
	s.endNode("second")
	if s.activeCount("second") != 0 {
		t.Fatal("active request count did not return to zero")
	}
}

func TestProxyDoesNotFollowUpstreamRedirects(t *testing.T) {
	var redirected atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = w.Write([]byte(`{"redirected":true}`))
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/api/chat", http.StatusFound)
	}))
	defer first.Close()
	portOf := func(raw string) int {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			t.Fatal(err)
		}
		return port
	}
	cfg := testConfig()
	cfg.AllowedNodePorts[portOf(first.URL)] = true
	cfg.AllowedNodePorts[portOf(second.URL)] = true
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	s.reg.Register("node-a", first.URL)
	s.reg.Heartbeat("node-a", 1, 1, true, []string{"llama3"}, nil)
	r := doRequest(s.Handler(), "POST", "/api/nodes/node-a/ollama/api/chat", `{"model":"llama3"}`, cfg.AdminToken)
	if r.Code != http.StatusFound {
		t.Fatalf("redirect response status=%d body=%s", r.Code, r.Body.String())
	}
	if redirected.Load() != 0 {
		t.Fatalf("upstream redirect was followed; second server called %d times", redirected.Load())
	}
}

func TestConcurrentActiveRequestCountersRemainConsistent(t *testing.T) {
	s := NewServer(testConfig(), "web")
	const workers, loops = 32, 200
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < loops; j++ {
				s.beginNode("node")
				_ = s.orderCandidates([]NodeView{{Node: "node"}, {Node: "other"}})
				s.endNode("node")
			}
		}()
	}
	wg.Wait()
	if got := s.activeCount("node"); got != 0 {
		t.Fatalf("active count leaked after concurrent operations: %d", got)
	}
}

func TestPerNodeCredentialsCannotImpersonateOtherNode(t *testing.T) {
	cfg := testConfig()
	cfg.AgentToken = ""
	cfg.AgentTokens = map[string]string{
		"node-a": "a6D1f7H2j9K4m8N3p5Q0r6S1t7V2w9X4",
		"node-b": "b7E2g8J3k0L5n9P4q6R1s7T2v8W3x0Y5",
	}
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	h := s.Handler()
	wrongIdentity := doRequest(h, "POST", "/api/agent/register", `{"node":"node-b","ollama_url":"http://10.0.0.1:11434"}`, cfg.AgentTokens["node-a"])
	if wrongIdentity.Code != http.StatusBadRequest || !strings.Contains(wrongIdentity.Body.String(), "does not match") {
		t.Fatalf("node-a credential impersonated node-b: status=%d body=%s", wrongIdentity.Code, wrongIdentity.Body.String())
	}
	badToken := doRequest(h, "POST", "/api/agent/register", `{"node":"node-a","ollama_url":"http://10.0.0.1:11434"}`, "not-a-valid-token")
	if badToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong credential status=%d body=%s", badToken.Code, badToken.Body.String())
	}
	valid := doRequest(h, "POST", "/api/agent/register", `{"node":"node-a","ollama_url":"http://10.0.0.1:11434"}`, cfg.AgentTokens["node-a"])
	if valid.Code != http.StatusCreated {
		t.Fatalf("valid node token status=%d body=%s", valid.Code, valid.Body.String())
	}
	wrongHeartbeat := doRequest(h, "POST", "/api/agent/heartbeat", `{"node":"node-b","cpu":1,"ram":1,"ollama_lan_reachable":true,"models":[]}`, cfg.AgentTokens["node-a"])
	if wrongHeartbeat.Code != http.StatusBadRequest {
		t.Fatalf("node-a credential heartbeated node-b: status=%d body=%s", wrongHeartbeat.Code, wrongHeartbeat.Body.String())
	}
}

func TestPrometheusMetricsRequireAdminAndExportCounters(t *testing.T) {
	s := NewServer(testConfig(), "web")
	defer s.client.CloseIdleConnections()
	h := s.Handler()
	unauth := doRequest(h, "GET", "/api/metrics", "", "")
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth metrics status=%d", unauth.Code)
	}
	got := doRequest(h, "GET", "/api/metrics", "", s.cfg.AdminToken)
	if got.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics content-type=%q", got.Header().Get("Content-Type"))
	}
	for _, expected := range []string{"meshcontrol_http_requests_total 2", "meshcontrol_http_active_requests 1", "meshcontrol_nodes_known 0", "meshcontrol_nodes_online 0", "meshcontrol_proxy_active_requests 0"} {
		if !strings.Contains(got.Body.String(), expected) {
			t.Fatalf("metrics missing %q: %s", expected, got.Body.String())
		}
	}
}
