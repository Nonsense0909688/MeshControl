package main

// Controller: HTTP API, web UI files, and the streaming Ollama reverse proxy.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Server struct {
	cfg         ControllerConfig
	reg         *Registry
	pulls       *PullManager
	client      *http.Client // proxy client: no overall timeout, streams can be long
	web         string
	sem         chan struct{} // bounded concurrent HTTP requests
	activeMu    sync.Mutex
	activeNodes map[string]int // in-flight proxy requests, including streams
	metrics     serverMetrics
}

type serverMetrics struct {
	httpRequests  atomic.Uint64
	httpActive    atomic.Int64
	httpRejected  atomic.Uint64
	proxyRequests atomic.Uint64
	proxyErrors   atomic.Uint64
}

func NewServer(cfg ControllerConfig, web string) *Server {
	clock := time.Now
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 10 * time.Minute
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 * 1024 * 1024
	}
	if cfg.MaxConcurrentRequests <= 0 {
		cfg.MaxConcurrentRequests = 256
	}
	tr := newSafeTransport(cfg)
	return &Server{
		cfg: cfg, web: web,
		reg:    NewRegistry(cfg, clock),
		pulls:  NewPullManager(cfg, clock),
		client: &http.Client{Transport: tr, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		sem:    make(chan struct{}, cfg.MaxConcurrentRequests), activeNodes: map[string]int{},
	}
}

// newSafeTransport revalidates every resolved IP at connection time. This
// prevents hostname changes between registration and proxying from escaping
// the controller's node-network allowlist (DNS rebinding). ResponseHeaderTimeout
// uses READ_TIMEOUT so cold model loads can take longer than a short connect timeout.
func newSafeTransport(cfg ControllerConfig) *http.Transport {
	dialer := &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil, // never inherit ambient HTTP_PROXY settings for trusted node traffic
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("invalid upstream address")
			}
			portN, err := net.LookupPort("tcp", port)
			if err != nil || portN < 1 || portN > 65535 {
				return nil, fmt.Errorf("invalid upstream port")
			}
			var ips []net.IP
			if ip := net.ParseIP(host); ip != nil {
				ips = []net.IP{ip}
			} else {
				resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil || len(resolved) == 0 {
					return nil, fmt.Errorf("upstream hostname did not resolve")
				}
				for _, addr := range resolved {
					ips = append(ips, addr.IP)
				}
			}
			for _, ip := range ips {
				if !ipAllowed(ip, cfg.AllowedNetworks) {
					return nil, fmt.Errorf("upstream address outside allowed networks")
				}
			}
			if len(cfg.AllowedNodePorts) > 0 && !cfg.AllowedNodePorts[portN] {
				return nil, fmt.Errorf("upstream port is not in ALLOWED_NODE_PORTS")
			}
			var lastErr error
			for _, ip := range ips {
				conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(portN)))
				if dialErr == nil {
					return conn, nil
				}
				lastErr = dialErr
			}
			return nil, lastErr
		},
		MaxIdleConns: 128, MaxIdleConnsPerHost: 16, MaxConnsPerHost: 64,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: cfg.ReadTimeout,
		ForceAttemptHTTP2:     true,
	}
}

// Active proxy counts are used by load-aware routing. A streamed response remains
// active until the upstream stream finishes or the client disconnects.
func (s *Server) beginNode(node string) {
	s.activeMu.Lock()
	s.activeNodes[node]++
	s.activeMu.Unlock()
}

func (s *Server) endNode(node string) {
	s.activeMu.Lock()
	if s.activeNodes[node] <= 1 {
		delete(s.activeNodes, node)
	} else {
		s.activeNodes[node]--
	}
	s.activeMu.Unlock()
}

func (s *Server) activeCount(node string) int {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	return s.activeNodes[node]
}

// orderCandidates prefers nodes with lower reported CPU/RAM utilization and
// fewer active requests. Registry round-robin rotation remains the tie-breaker.
func (s *Server) orderCandidates(cands []NodeView) []NodeView {
	out := append([]NodeView(nil), cands...)
	// Snapshot the score once: reading changing in-flight counters from a sort
	// comparator could make its ordering inconsistent under concurrent traffic.
	scores := make(map[string]float64, len(out))
	for _, n := range out {
		load := float64(s.activeCount(n.Node)) * 30
		if n.CPU != nil {
			load += *n.CPU * 0.6
		}
		if n.RAM != nil {
			load += *n.RAM * 0.4
		}
		scores[n.Node] = load
	}
	sort.SliceStable(out, func(i, j int) bool { return scores[out[i].Node] < scores[out[j].Node] })
	return out
}

// ---- JSON / error helpers -------------------------------------------------

var errTooLarge = errors.New("request entity too large")

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// apiErr writes {"error": msg, ...extra} where extra is key, value, key, value...
func apiErr(w http.ResponseWriter, msg string, code int, kv ...any) {
	m := map[string]any{"error": msg}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if ok {
			m[key] = kv[i+1]
		}
	}
	writeJSON(w, code, m)
}

type apiFunc func(http.ResponseWriter, *http.Request) error

// api turns a returned error into the right JSON response.
func api(fn apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := fn(w, r)
		if err == nil {
			return
		}
		var ve *validationError
		switch {
		case errors.As(err, &ve):
			apiErr(w, ve.msg, http.StatusBadRequest)
		case errors.Is(err, errTooLarge):
			apiErr(w, "request entity too large", http.StatusRequestEntityTooLarge)
		default:
			log.Error("internal error", "path", r.URL.Path, "error", err.Error())
			apiErr(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, verr("could not read request body")
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

func parseObject(b []byte) (map[string]any, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, verr("body must be a JSON object")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, verr("body must be a JSON object")
	}
	return m, nil
}

func readObject(r *http.Request, limit int64) (map[string]any, error) {
	b, err := readBody(r, limit)
	if err != nil {
		return nil, err
	}
	return parseObject(b)
}

// ---- auth -----------------------------------------------------------------

func bearer(r *http.Request) string {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if strings.EqualFold(scheme, "bearer") {
		return strings.TrimSpace(token)
	}
	return ""
}

func tokenOK(r *http.Request, expected string) bool {
	given := bearer(r)
	return expected != "" && given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(expected)) == 1
}

type agentIdentityContextKey struct{}

func authenticatedAgentID(r *http.Request) string {
	id, _ := r.Context().Value(agentIdentityContextKey{}).(string)
	return id
}

func (s *Server) agentOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.AgentTokens) == 0 {
			if !tokenOK(r, s.cfg.AgentToken) {
				log.Debug("agent authentication failed", "path", r.URL.Path, "remote_ip", requestPeerIP(r))
				apiErr(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		} else {
			given := bearer(r)
			identity := ""
			for node, expected := range s.cfg.AgentTokens {
				if given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(expected)) == 1 {
					identity = node
				}
			}
			if identity == "" {
				log.Debug("per-node agent authentication failed", "path", r.URL.Path, "remote_ip", requestPeerIP(r))
				apiErr(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), agentIdentityContextKey{}, identity))
		}
		h(w, r)
	}
}

// adminOnly fails closed. An empty ADMIN_TOKEN never silently turns the API open.
func (s *Server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tokenOK(r, s.cfg.AdminToken) {
			log.Debug("admin authentication failed", "path", r.URL.Path, "remote_ip", requestPeerIP(r))
			apiErr(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// ---- routes ---------------------------------------------------------------

const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

var assetTypes = map[string]string{
	"app.css": "text/css", "theme.js": "text/javascript", "common.js": "text/javascript",
	"pulls.js": "text/javascript", "dashboard.js": "text/javascript", "nodes.js": "text/javascript",
	"node.js": "text/javascript", "models.js": "text/javascript", "model.js": "text/javascript",
	"chat.js": "text/javascript",
}

func (s *Server) page(file string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", pageCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(s.web, file))
	}
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ct, ok := assetTypes[name]
	if !ok {
		apiErr(w, "not found", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, filepath.Join(s.web, "static", name))
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Web UI: one static HTML file per page; none contain cluster data.
	for _, p := range []struct{ pattern, file string }{
		{"GET /{$}", "dashboard.html"},
		{"GET /nodes", "nodes.html"},
		{"GET /nodes/{node}", "node.html"},
		{"GET /models", "models.html"},
		{"GET /models/{model...}", "model.html"},
		{"GET /chat", "chat.html"},
	} {
		mux.HandleFunc(p.pattern, s.page(p.file))
	}
	mux.HandleFunc("GET /static/{name}", s.asset)

	// Agent endpoints.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/agent/register", s.agentOnly(api(s.handleRegister)))
	mux.HandleFunc("POST /api/agent/heartbeat", s.agentOnly(api(s.handleHeartbeat)))

	// Node / model information.
	mux.HandleFunc("GET /api/metrics", s.adminOnly(s.handleMetrics))
	mux.HandleFunc("GET /api/nodes", s.adminOnly(s.handleListNodes))
	mux.HandleFunc("GET /api/nodes/{node}", s.adminOnly(s.handleGetNode))
	mux.HandleFunc("GET /api/nodes/{node}/models", s.adminOnly(s.handleNodeModels))
	mux.HandleFunc("GET /api/models", s.adminOnly(s.handleListModels))

	// Model pulls.
	mux.HandleFunc("POST /api/nodes/{node}/pull", s.adminOnly(api(s.handlePull)))
	mux.HandleFunc("GET /api/pulls", s.adminOnly(s.handleListPulls))
	mux.HandleFunc("DELETE /api/pulls/{pull_id}", s.adminOnly(s.handleCancelPull))

	// Proxy.
	mux.HandleFunc("POST /api/ollama/api/{endpoint}", s.adminOnly(api(s.handleProxyAuto)))
	mux.HandleFunc("POST /api/nodes/{node}/ollama/api/{endpoint}", s.adminOnly(api(s.handleProxyDirect)))
	mux.HandleFunc("GET /api/ollama/api/tags", s.adminOnly(s.handleProxyTags))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { apiErr(w, "not found", http.StatusNotFound) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.metrics.httpRequests.Add(1)
		select {
		case s.sem <- struct{}{}:
			s.metrics.httpActive.Add(1)
			defer func() { s.metrics.httpActive.Add(-1); <-s.sem }()
			mux.ServeHTTP(w, r)
		case <-r.Context().Done():
			return
		default:
			s.metrics.httpRejected.Add(1)
			w.Header().Set("Retry-After", "1")
			apiErr(w, "server is at capacity; retry shortly", http.StatusServiceUnavailable)
		}
	})
}

// ---- handlers -------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	online := 0
	for _, n := range s.reg.Snapshot() {
		if n.Online {
			online++
		}
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "nodes_online": online})
}

// handleMetrics exports low-cardinality Prometheus text metrics. The endpoint
// is admin-authenticated like the rest of the control-plane API.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	nodes := s.reg.Snapshot()
	online := 0
	for _, n := range nodes {
		if n.Online {
			online++
		}
	}
	s.activeMu.Lock()
	proxyActive := 0
	for _, count := range s.activeNodes {
		proxyActive += count
	}
	s.activeMu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP meshcontrol_http_requests_total Total requests received by the controller.\n# TYPE meshcontrol_http_requests_total counter\nmeshcontrol_http_requests_total %d\n", s.metrics.httpRequests.Load())
	_, _ = fmt.Fprintf(w, "# HELP meshcontrol_http_active_requests Requests currently admitted by the concurrency limiter.\n# TYPE meshcontrol_http_active_requests gauge\nmeshcontrol_http_active_requests %d\n", s.metrics.httpActive.Load())
	_, _ = fmt.Fprintf(w, "# HELP meshcontrol_http_capacity_rejections_total Requests rejected by the concurrency limiter.\n# TYPE meshcontrol_http_capacity_rejections_total counter\nmeshcontrol_http_capacity_rejections_total %d\n", s.metrics.httpRejected.Load())
	_, _ = fmt.Fprintf(w, "# HELP meshcontrol_proxy_requests_total Requests passed to an Ollama upstream.\n# TYPE meshcontrol_proxy_requests_total counter\nmeshcontrol_proxy_requests_total %d\n", s.metrics.proxyRequests.Load())
	_, _ = fmt.Fprintf(w, "# HELP meshcontrol_proxy_errors_total Proxy requests that failed before a response was relayed.\n# TYPE meshcontrol_proxy_errors_total counter\nmeshcontrol_proxy_errors_total %d\n", s.metrics.proxyErrors.Load())
	_, _ = fmt.Fprintf(w, "# HELP meshcontrol_proxy_active_requests Active upstream requests, including streams.\n# TYPE meshcontrol_proxy_active_requests gauge\nmeshcontrol_proxy_active_requests %d\n", proxyActive)
	_, _ = fmt.Fprintf(w, "# HELP meshcontrol_nodes_known Current registered nodes.\n# TYPE meshcontrol_nodes_known gauge\nmeshcontrol_nodes_known %d\n", len(nodes))
	_, _ = fmt.Fprintf(w, "# HELP meshcontrol_nodes_online Current nodes with fresh heartbeats.\n# TYPE meshcontrol_nodes_online gauge\nmeshcontrol_nodes_online %d\n", online)
}

func agentJSON(r *http.Request) (map[string]any, error) {
	if r.ContentLength > agentBodyLimit {
		return nil, verr("request body too large")
	}
	m, err := readObject(r, agentBodyLimit)
	if errors.Is(err, errTooLarge) {
		return nil, verr("request body too large")
	}
	return m, err
}

func requestPeerIP(r *http.Request) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	if ip := net.ParseIP(remote); ip != nil {
		return ip.String()
	}
	return ""
}

func (s *Server) checkSourceIP(r *http.Request, rawURL string) error {
	if !s.cfg.VerifySourceIP {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return verr("invalid Ollama URL")
	}
	peer := net.ParseIP(requestPeerIP(r))
	if peer == nil {
		return verr("cannot identify agent source address")
	}
	ip := net.ParseIP(u.Hostname())
	if ip != nil && ip.Equal(peer) {
		return nil
	}
	if ip == nil && s.cfg.AllowHostnames {
		addrs, err := net.DefaultResolver.LookupIPAddr(r.Context(), u.Hostname())
		if err == nil {
			for _, a := range addrs {
				if a.IP.Equal(peer) {
					return nil
				}
			}
		}
	}
	return verr("ollama_url host does not match the agent's source address")
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) error {
	data, err := agentJSON(r)
	if err != nil {
		return err
	}
	node, err := validateNodeID(data["node"])
	if err != nil {
		return err
	}
	if identity := authenticatedAgentID(r); identity != "" && identity != node {
		return verr("node ID does not match the authenticated agent credential")
	}
	u, err := validateOllamaURL(data["ollama_url"], s.cfg)
	if err != nil {
		return err
	}
	if err := s.checkSourceIP(r, u); err != nil {
		return err
	}
	created, err := s.reg.RegisterFrom(node, u, requestPeerIP(r))
	if err != nil {
		return err
	}
	state := "re-registered"
	code := 200
	if created {
		state, code = "registered", 201
	}
	log.Info("node "+state, "node", node, "url", u)
	writeJSON(w, code, map[string]any{"status": "registered", "node": node, "created": created})
	return nil
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) error {
	data, err := agentJSON(r)
	if err != nil {
		return err
	}
	node, err := validateNodeID(data["node"])
	if err != nil {
		return err
	}
	if identity := authenticatedAgentID(r); identity != "" && identity != node {
		return verr("node ID does not match the authenticated agent credential")
	}
	cpu, err := validatePercent(data["cpu"], "cpu")
	if err != nil {
		return err
	}
	ram, err := validatePercent(data["ram"], "ram")
	if err != nil {
		return err
	}
	reachable, ok := data["ollama_lan_reachable"].(bool)
	if !ok {
		return verr("ollama_lan_reachable must be a boolean")
	}
	models, err := validateModels(data["models"])
	if err != nil {
		return err
	}
	details, err := validateModelDetails(data["model_details"])
	if err != nil {
		return err
	}
	ok, hbErr := s.reg.HeartbeatFrom(node, requestPeerIP(r), cpu, ram, reachable, models, details)
	if hbErr != nil {
		return hbErr
	}
	if !ok {
		// Typical after a controller restart: tell the agent to register again.
		apiErr(w, "node not registered", 404, "reregister", true)
		return nil
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"nodes": s.reg.Snapshot()})
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.reg.Get(r.PathValue("node"))
	if !ok {
		apiErr(w, "node not found", 404)
		return
	}
	writeJSON(w, 200, rec)
}

func (s *Server) handleNodeModels(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")
	rec, ok := s.reg.Get(node)
	if !ok {
		apiErr(w, "node not found", 404)
		return
	}
	writeJSON(w, 200, map[string]any{"node": node, "online": rec.Online, "models": rec.Models})
}

type modelEntry struct {
	Model     string `json:"model"`
	Node      string `json:"node"`
	Available bool   `json:"available"`
	ModelDetail
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	entries := []modelEntry{}
	for _, n := range s.reg.Snapshot() {
		for _, m := range n.Models {
			entries = append(entries, modelEntry{
				Model: m, Node: n.Node, Available: n.Online && n.OllamaLANReachable,
				ModelDetail: n.ModelDetails[m],
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Model != entries[j].Model {
			return entries[i].Model < entries[j].Model
		}
		return entries[i].Node < entries[j].Node
	})
	writeJSON(w, 200, map[string]any{"models": entries})
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) error {
	data, err := readObject(r, s.cfg.MaxBodyBytes)
	if err != nil {
		return err
	}
	model, err := validatePullModel(data["model"], s.cfg)
	if err != nil {
		return err
	}
	node := r.PathValue("node")
	rec, ok := s.reg.Get(node)
	if !ok {
		apiErr(w, "node not found", 404)
		return nil
	}
	if !rec.Online {
		apiErr(w, fmt.Sprintf("node '%s' is offline", node), 503, "status", "offline")
		return nil
	}
	if !rec.OllamaLANReachable {
		apiErr(w, fmt.Sprintf("provider unreachable on node '%s'", node), 503, "status", "provider_unreachable")
		return nil
	}
	job, started, reason := s.pulls.StartDetailed(node, rec.OllamaURL, model)
	if !started {
		switch reason {
		case "global_limit", "node_limit":
			w.Header().Set("Retry-After", "5")
			apiErr(w, "model-pull capacity reached; retry shortly", http.StatusTooManyRequests, "reason", reason)
		case "random_failure":
			apiErr(w, "could not generate a pull job identifier", http.StatusInternalServerError)
		default:
			apiErr(w, fmt.Sprintf("'%s' is already being pulled on '%s'", model, node), http.StatusConflict)
		}
		return nil
	}
	log.Info("pull started", "model", model, "node", node)
	writeJSON(w, 202, job)
	return nil
}

func (s *Server) handleListPulls(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"pulls": s.pulls.Snapshot()})
}

func (s *Server) handleCancelPull(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("pull_id")
	switch s.pulls.Cancel(id) {
	case "missing":
		apiErr(w, "pull not found", 404)
	case "finished":
		apiErr(w, "pull already finished", 409)
	default:
		log.Info("pull cancellation requested", "pull_id", id)
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}

// ---- proxy ----------------------------------------------------------------

func (s *Server) handleProxyAuto(w http.ResponseWriter, r *http.Request) error {

	if r.Header.Get("Content-Type") != "application/json" {
		return fmt.Errorf("unsupported Content-Type: expected application/json")
	}

	endpoint := r.PathValue("endpoint")
	if !proxyEndpoints[endpoint] {
		apiErr(w, "not found", 404)
		return nil
	}
	body, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		return err
	}

	obj, err := parseObject(body)
	if err != nil {
		return err
	}
	model, _ := obj["model"].(string)
	if strings.TrimSpace(model) == "" {
		return verr("request must include a 'model' string")
	}
	hosting, eligible := s.reg.CandidatesFor(model)
	if len(hosting) == 0 {
		apiErr(w, fmt.Sprintf("no node hosts model '%s'", model), 404)
		return nil
	}
	if len(eligible) == 0 {
		apiErr(w, fmt.Sprintf("model '%s' exists but no hosting node is online", model), 503)
		return nil
	}
	s.forward(w, r, eligible, endpoint, body)
	return nil
}

func (s *Server) handleProxyDirect(w http.ResponseWriter, r *http.Request) error {
	endpoint, node := r.PathValue("endpoint"), r.PathValue("node")
	if !proxyEndpoints[endpoint] {
		apiErr(w, "not found", 404)
		return nil
	}
	rec, ok := s.reg.Get(node)
	if !ok {
		apiErr(w, "node not found", 404)
		return nil
	}
	if !rec.Online {
		apiErr(w, fmt.Sprintf("node '%s' is offline", node), 503)
		return nil
	}
	if !rec.OllamaLANReachable {
		apiErr(w, fmt.Sprintf("provider unreachable on node '%s'", node), 503)
		return nil
	}
	body, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		return err
	}
	s.forward(w, r, []NodeView{rec}, endpoint, body)
	return nil
}

func (s *Server) handleProxyTags(w http.ResponseWriter, r *http.Request) {
	byModel := map[string][]string{}
	for _, n := range s.reg.Snapshot() {
		if n.Online && n.OllamaLANReachable {
			for _, m := range n.Models {
				byModel[m] = append(byModel[m], n.Node)
			}
		}
	}
	names := make([]string, 0, len(byModel))
	for m := range byModel {
		names = append(names, m)
	}
	sort.Strings(names)
	models := make([]map[string]any, 0, len(names))
	for _, m := range names {
		nodes := byModel[m]
		sort.Strings(nodes)
		models = append(models, map[string]any{"name": m, "model": m, "nodes": nodes})
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

// forward tries each candidate in order. It only moves to the next node when
// the request provably never left (a dial failure); anything later could mean
// inference already started, and non-idempotent requests must not be replayed.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, cands []NodeView, endpoint string, body []byte) {
	s.metrics.proxyRequests.Add(1)
	lastErr := "no upstream attempted"
	cands = s.orderCandidates(cands)
	for _, rec := range cands {
		s.beginNode(rec.Node)
		ctx, cancel := context.WithCancel(r.Context())
		idle := newIdle(s.cfg.ReadTimeout, cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.OllamaURL+"/api/"+endpoint, bytes.NewReader(body))
		if err != nil {
			idle.stop()
			cancel()
			s.endNode(rec.Node)
			s.metrics.proxyErrors.Add(1)
			apiErr(w, "error talking to node "+rec.Node, 502)
			return
		}
		req.Header.Set("Content-Type", "application/json") // never forward client auth
		resp, err := s.client.Do(req)
		if err != nil {
			timedOut := idle.fired.Load()
			idle.stop()
			cancel()
			s.endNode(rec.Node)
			if r.Context().Err() != nil {
				return // client went away
			}
			var oe *net.OpError
			if errors.As(err, &oe) && oe.Op == "dial" {
				lastErr = "could not connect to node " + rec.Node
				log.Warn(lastErr)
				continue
			}
			if timedOut {
				s.metrics.proxyErrors.Add(1)
				apiErr(w, fmt.Sprintf("node %s timed out", rec.Node), 504)
				return
			}
			s.metrics.proxyErrors.Add(1)
			apiErr(w, "error talking to node "+rec.Node, 502)
			return
		}
		func() {
			defer s.endNode(rec.Node)
			s.relay(w, r, resp, rec.Node, idle, cancel)
		}()
		return
	}
	s.metrics.proxyErrors.Add(1)
	apiErr(w, lastErr, 502)
}

// relay streams the upstream response to the client chunk by chunk, preserving
// NDJSON streaming.
func (s *Server) relay(w http.ResponseWriter, r *http.Request, resp *http.Response, node string, idle *idleTimer, cancel context.CancelFunc) {
	defer cancel()
	defer idle.stop()
	defer resp.Body.Close()
	h := w.Header()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	h.Set("Content-Type", ct)
	h.Set("X-MeshControl-Node", node)
	h.Set("X-Accel-Buffering", "no")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			idle.touch()
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err == io.EOF || r.Context().Err() != nil {
				return
			}
			// Headers are already sent, so the status can't change and we must not
			// retry elsewhere. Abort the connection so the client sees an incomplete
			// stream instead of a clean ending.
			log.Warn("upstream failed mid-stream", "node", node)
			panic(http.ErrAbortHandler)
		}
	}
}

// ---- entry point ----------------------------------------------------------

func controllerListenAddress() (string, error) {
	host := strings.TrimSpace(envOr("CONTROLLER_HOST", "127.0.0.1"))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if host == "" || strings.ContainsAny(host, "/\\[] \t\r\n") || (strings.Contains(host, ":") && net.ParseIP(host) == nil) {
		return "", fmt.Errorf("CONTROLLER_HOST is invalid")
	}
	port, err := strconv.Atoi(envOr("CONTROLLER_PORT", "8000"))
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("CONTROLLER_PORT must be between 1 and 65535")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func runController() int {
	cfg, err := controllerConfigFromEnv()
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	web := envOr("WEB_DIR", "./web")
	info, err := os.Stat(web)
	if err != nil || !info.IsDir() {
		log.Error("WEB_DIR is missing or not a directory", "dir", web)
		return 1
	}
	addr, err := controllerListenAddress()
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	certFile, keyFile, err := controllerTLSFromEnv()
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	srv := &http.Server{
		Addr: addr, Handler: NewServer(cfg, web).Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if srv.Shutdown(sctx) != nil {
			_ = srv.Close() // long-running streams: don't wait forever
		}
	}()

	transport := "http"
	if certFile != "" {
		transport = "https"
	}
	log.Info("MeshControl controller listening", "addr", addr, "transport", transport, "web", web)
	var serveErr error
	if certFile != "" {
		serveErr = srv.ListenAndServeTLS(certFile, keyFile)
	} else {
		serveErr = srv.ListenAndServe()
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Error("server error", "error", serveErr.Error())
		return 1
	}
	return 0
}
