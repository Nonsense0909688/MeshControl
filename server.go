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
	"strings"
	"syscall"
	"time"
)

type Server struct {
	cfg    ControllerConfig
	reg    *Registry
	pulls  *PullManager
	client *http.Client // proxy client: no overall timeout, streams can be long
	web    string
}

func NewServer(cfg ControllerConfig, web string) *Server {
	clock := time.Now
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: cfg.ConnectTimeout}).DialContext,
		MaxIdleConnsPerHost: 16,
	}
	return &Server{
		cfg: cfg, web: web,
		reg:    NewRegistry(cfg, clock),
		pulls:  NewPullManager(cfg, clock),
		client: &http.Client{Transport: tr},
	}
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
		m[kv[i].(string)] = kv[i+1]
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

func (s *Server) agentOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tokenOK(r, s.cfg.AgentToken) {
			apiErr(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// adminOnly is open when ADMIN_TOKEN is unset (trusted dev LAN only).
func (s *Server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken != "" && !tokenOK(r, s.cfg.AdminToken) {
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
	return mux
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

func (s *Server) checkSourceIP(r *http.Request, rawURL string) error {
	if !s.cfg.VerifySourceIP {
		return nil
	}
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Hostname()
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	if host != remote {
		return verr("ollama_url host does not match the agent's source address")
	}
	return nil
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
	u, err := validateOllamaURL(data["ollama_url"], s.cfg)
	if err != nil {
		return err
	}
	if err := s.checkSourceIP(r, u); err != nil {
		return err
	}
	created := s.reg.Register(node, u)
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
	if !s.reg.Heartbeat(node, cpu, ram, reachable, models, details) {
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
	job, started := s.pulls.Start(node, rec.OllamaURL, model)
	if !started {
		apiErr(w, fmt.Sprintf("'%s' is already being pulled on '%s'", model, node), 409)
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
	switch s.pulls.Cancel(r.PathValue("pull_id")) {
	case "missing":
		apiErr(w, "pull not found", 404)
	case "finished":
		apiErr(w, "pull already finished", 409)
	default:
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}

// ---- proxy ----------------------------------------------------------------

func (s *Server) handleProxyAuto(w http.ResponseWriter, r *http.Request) error {
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
		if n.Online {
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
	lastErr := "no upstream attempted"
	for _, rec := range cands {
		ctx, cancel := context.WithCancel(r.Context())
		idle := newIdle(s.cfg.ReadTimeout, cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.OllamaURL+"/api/"+endpoint, bytes.NewReader(body))
		if err != nil {
			idle.stop()
			cancel()
			apiErr(w, "error talking to node "+rec.Node, 502)
			return
		}
		req.Header.Set("Content-Type", "application/json") // never forward client auth
		resp, err := s.client.Do(req)
		if err != nil {
			timedOut := idle.fired.Load()
			idle.stop()
			cancel()
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
				apiErr(w, fmt.Sprintf("node %s timed out", rec.Node), 504)
				return
			}
			apiErr(w, "error talking to node "+rec.Node, 502)
			return
		}
		s.relay(w, r, resp, rec.Node, idle, cancel)
		return
	}
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

func runController() int {
	cfg, err := controllerConfigFromEnv()
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	if cfg.AdminToken == "" {
		log.Warn("ADMIN_TOKEN is not set: node/model/proxy endpoints are unauthenticated")
	}
	web := envOr("WEB_DIR", ".")
	if _, err := os.Stat(web); err != nil {
		log.Error("WEB_DIR not found", "dir", web)
		return 1
	}
	addr := net.JoinHostPort(envOr("CONTROLLER_HOST", "0.0.0.0"), envOr("CONTROLLER_PORT", "8000"))
	srv := &http.Server{Addr: addr, Handler: NewServer(cfg, web).Handler(), ReadHeaderTimeout: 10 * time.Second}

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

	log.Info("MeshControl controller listening", "addr", addr, "web", web)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server error", "error", err.Error())
		return 1
	}
	return 0
}
