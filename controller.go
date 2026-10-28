package main

// Controller: configuration, validation and the in-memory node registry.

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	maxModels              = 1000
	maxModelNameLen        = 256
	agentBodyLimit         = 256 * 1024 // heartbeats/registrations are tiny
	defaultPullRegistries  = "registry.ollama.ai,hf.co,huggingface.co"
	defaultAllowedNetworks = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"
)

var (
	nodeIDRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	modelPullRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,255}$`)

	// Only these Ollama endpoints may be proxied; the upstream path is never taken
	// from the client, which prevents arbitrary-path requests to a node.
	proxyEndpoints = map[string]bool{"generate": true, "chat": true, "embeddings": true, "embed": true}
)

// ---- configuration --------------------------------------------------------

type ControllerConfig struct {
	AgentToken                string            // shared credential; mutually exclusive with AgentTokens
	AgentTokens               map[string]string // optional node ID -> unique credential
	AdminToken                string            // required for read/proxy endpoints
	OfflineAfter              time.Duration
	PurgeAfter                time.Duration
	AllowedNetworks           []*net.IPNet
	AllowedNodePorts          map[int]bool
	AllowHostnames            bool // accept DNS names in ollama_url
	VerifySourceIP            bool // require ollama_url host == agent source IP
	ConnectTimeout            time.Duration
	ReadTimeout               time.Duration
	MaxBodyBytes              int64
	MaxConcurrentRequests     int
	MaxConcurrentPulls        int
	MaxConcurrentPullsPerNode int
	PullRegistries            map[string]bool
}

func parseNetworks(spec string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			if ip := net.ParseIP(p); ip != nil {
				if ip.To4() != nil {
					p += "/32"
				} else {
					p += "/128"
				}
			}
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("invalid network %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func validateSecret(name, value string) error {
	lower := strings.ToLower(strings.TrimSpace(value))
	if len(value) < 32 {
		return fmt.Errorf("%s must contain at least 32 characters; generate one with: openssl rand -hex 32", name)
	}
	for _, placeholder := range []string{"change-me", "replace-with", "your-token", "example-token", "changeme", "placeholder"} {
		if strings.Contains(lower, placeholder) {
			return fmt.Errorf("%s still contains a placeholder; set a unique random secret", name)
		}
	}
	return nil
}

// parseAgentTokens parses AGENT_TOKENS as comma-separated node-id:secret pairs.
// Tokens must be unique so one credential cannot authenticate as multiple nodes.
func parseAgentTokens(raw string) (map[string]string, error) {
	out := map[string]string{}
	tokenOwners := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, token, ok := strings.Cut(entry, ":")
		id = strings.TrimSpace(id)
		token = strings.TrimSpace(token)
		if !ok {
			return nil, fmt.Errorf("AGENT_TOKENS entry %q must be node-id:secret", entry)
		}
		if _, err := validateNodeID(id); err != nil {
			return nil, fmt.Errorf("AGENT_TOKENS has invalid node ID %q", id)
		}
		if err := validateSecret("AGENT_TOKENS["+id+"]", token); err != nil {
			return nil, err
		}
		if _, exists := out[id]; exists {
			return nil, fmt.Errorf("AGENT_TOKENS repeats node ID %q", id)
		}
		if owner, exists := tokenOwners[token]; exists {
			return nil, fmt.Errorf("AGENT_TOKENS reuses the same secret for %q and %q", owner, id)
		}
		out[id] = token
		tokenOwners[token] = id
	}
	if len(out) == 0 {
		return nil, errors.New("AGENT_TOKENS must contain at least one node-id:secret entry")
	}
	return out, nil
}

func controllerConfigFromEnv() (ControllerConfig, error) {
	var c ControllerConfig
	sharedToken := strings.TrimSpace(os.Getenv("AGENT_TOKEN"))
	rawAgentTokens := strings.TrimSpace(os.Getenv("AGENT_TOKENS"))
	if rawAgentTokens != "" {
		if sharedToken != "" {
			return c, errors.New("set either AGENT_TOKEN or AGENT_TOKENS, not both")
		}
		var err error
		c.AgentTokens, err = parseAgentTokens(rawAgentTokens)
		if err != nil {
			return c, err
		}
	} else {
		c.AgentToken = sharedToken
		if err := validateSecret("AGENT_TOKEN", c.AgentToken); err != nil {
			return c, err
		}
	}
	c.AdminToken = strings.TrimSpace(os.Getenv("ADMIN_TOKEN"))
	if err := validateSecret("ADMIN_TOKEN", c.AdminToken); err != nil {
		return c, err
	}
	if c.AgentToken != "" && c.AgentToken == c.AdminToken {
		return c, errors.New("AGENT_TOKEN and ADMIN_TOKEN must be different secrets")
	}
	for node, token := range c.AgentTokens {
		if token == c.AdminToken {
			return c, fmt.Errorf("AGENT_TOKENS secret for %q must differ from ADMIN_TOKEN", node)
		}
	}
	for _, f := range []struct {
		key string
		def float64
		dst *time.Duration
	}{
		{"OFFLINE_AFTER", 20, &c.OfflineAfter},
		{"PURGE_AFTER", 86400, &c.PurgeAfter},
		{"CONNECT_TIMEOUT", 5, &c.ConnectTimeout},
		{"READ_TIMEOUT", 600, &c.ReadTimeout},
	} {
		v, err := envFloat(f.key, f.def)
		if err != nil {
			return c, err
		}
		if v <= 0 || v > 365*24*60*60 {
			return c, fmt.Errorf("%s must be greater than 0 and at most one year", f.key)
		}
		*f.dst = secs(v)
	}
	if c.OfflineAfter < 5*time.Second || c.OfflineAfter > 10*time.Minute {
		return c, errors.New("OFFLINE_AFTER must be between 5 and 600 seconds")
	}
	if c.PurgeAfter < c.OfflineAfter {
		return c, errors.New("PURGE_AFTER must be greater than or equal to OFFLINE_AFTER")
	}
	if c.ConnectTimeout < time.Second || c.ConnectTimeout > time.Minute {
		return c, errors.New("CONNECT_TIMEOUT must be between 1 and 60 seconds")
	}
	if c.ReadTimeout < time.Second || c.ReadTimeout > 24*time.Hour {
		return c, errors.New("READ_TIMEOUT must be between 1 second and 24 hours")
	}
	nets, err := parseNetworks(envOr("ALLOWED_NODE_NETWORKS", defaultAllowedNetworks))
	if err != nil {
		return c, err
	}
	c.AllowedNetworks = nets
	ports := map[int]bool{}
	for _, raw := range strings.Split(envOr("ALLOWED_NODE_PORTS", "11434"), ",") {
		if raw = strings.TrimSpace(raw); raw != "" {
			p, parseErr := strconv.Atoi(raw)
			if parseErr != nil || p < 1 || p > 65535 {
				return c, fmt.Errorf("invalid ALLOWED_NODE_PORTS entry %q", raw)
			}
			ports[p] = true
		}
	}
	if len(ports) == 0 {
		return c, errors.New("ALLOWED_NODE_PORTS must contain at least one port")
	}
	c.AllowedNodePorts = ports
	c.AllowHostnames, err = envBoolChecked("ALLOW_HOSTNAMES", false)
	if err != nil {
		return c, err
	}
	c.VerifySourceIP, err = envBoolChecked("VERIFY_SOURCE_IP", false)
	if err != nil {
		return c, err
	}
	bodyMB, err := envFloat("MAX_BODY_MB", 8)
	if err != nil || bodyMB < 1 || bodyMB > 64 {
		return c, errors.New("MAX_BODY_MB must be between 1 and 64")
	}
	c.MaxBodyBytes = int64(bodyMB * 1024 * 1024)
	if bodyMB != math.Trunc(bodyMB) {
		return c, errors.New("MAX_BODY_MB must be a whole number")
	}
	concurrent, err := envFloat("MAX_CONCURRENT_REQUESTS", 256)
	if err != nil || concurrent < 1 || concurrent > 10000 || concurrent != math.Trunc(concurrent) {
		return c, errors.New("MAX_CONCURRENT_REQUESTS must be a whole number between 1 and 10000")
	}
	c.MaxConcurrentRequests = int(concurrent)
	pulls, err := envFloat("MAX_CONCURRENT_PULLS", 4)
	if err != nil || pulls < 1 || pulls > 128 || pulls != math.Trunc(pulls) {
		return c, errors.New("MAX_CONCURRENT_PULLS must be a whole number between 1 and 128")
	}
	c.MaxConcurrentPulls = int(pulls)
	perNode, err := envFloat("MAX_CONCURRENT_PULLS_PER_NODE", 1)
	if err != nil || perNode < 1 || perNode > pulls || perNode != math.Trunc(perNode) {
		return c, errors.New("MAX_CONCURRENT_PULLS_PER_NODE must be a whole number between 1 and MAX_CONCURRENT_PULLS")
	}
	c.MaxConcurrentPullsPerNode = int(perNode)
	c.PullRegistries = map[string]bool{}
	for _, r := range strings.Split(envOr("PULL_REGISTRIES", defaultPullRegistries), ",") {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			c.PullRegistries[r] = true
		}
	}
	return c, nil
}

// ---- validation -----------------------------------------------------------

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

func verr(format string, a ...any) error { return &validationError{fmt.Sprintf(format, a...)} }

func validateNodeID(v any) (string, error) {
	s, ok := v.(string)
	if !ok || !nodeIDRe.MatchString(s) {
		return "", verr("node must be 1-64 chars of letters, digits, '.', '_' or '-', starting alphanumeric")
	}
	return s, nil
}

func ipAllowed(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) { // Contains already unwraps IPv4-mapped IPv6 addresses
			return true
		}
	}
	return false
}

func resolveHost(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, verr("ollama_url host does not resolve")
	}
	return ips, nil
}

// validateOllamaURL validates and normalises an agent-supplied Ollama URL and
// returns scheme://host:port. It rejects credentials, paths, queries, non-HTTP
// schemes, and hosts outside the allowed networks, so a registered node can
// never turn the proxy into an SSRF primitive.
func validateOllamaURL(v any, cfg ControllerConfig) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 255 {
		return "", verr("ollama_url must be a non-empty string")
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", verr("ollama_url is malformed")
	}
	port := 0
	if p := u.Port(); p != "" {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return "", verr("ollama_url is malformed")
		}
		port = n
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", verr("ollama_url must use http or https")
	}
	if u.User != nil {
		return "", verr("ollama_url must not contain credentials")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", verr("ollama_url must not contain a path, query or fragment")
	}
	host := u.Hostname()
	if host == "" {
		return "", verr("ollama_url must include a host")
	}
	if net.ParseIP(host) == nil && !cfg.AllowHostnames {
		return "", verr("ollama_url must use an IP address, not a hostname")
	}
	addrs, err := resolveHost(host)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if !ipAllowed(a, cfg.AllowedNetworks) {
			return "", verr("ollama_url host is outside the allowed networks")
		}
	}
	if port == 0 {
		port = 80
		if u.Scheme == "https" {
			port = 443
		}
	}
	if len(cfg.AllowedNodePorts) > 0 && !cfg.AllowedNodePorts[port] {
		return "", verr("ollama_url port %d is not in ALLOWED_NODE_PORTS", port)
	}
	shown := host
	if strings.Contains(host, ":") {
		shown = "[" + host + "]"
	}
	return fmt.Sprintf("%s://%s:%d", u.Scheme, shown, port), nil
}

func validatePercent(v any, name string) (float64, error) {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 100 {
		return 0, verr("%s must be a number between 0 and 100", name)
	}
	return f, nil
}

func validateModels(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok || len(list) > maxModels {
		return nil, verr("models must be a list of at most %d names", maxModels)
	}
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, item := range list {
		m, ok := item.(string)
		if !ok || strings.TrimSpace(m) == "" || utf8.RuneCountInString(m) > maxModelNameLen {
			return nil, verr("every model name must be a non-empty string (max 256 chars)")
		}
		m = strings.TrimSpace(m)
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out, nil
}

// ModelDetail holds per-model facts reported by agents (from Ollama's /api/tags).
// Missing facts serialise as null, matching the web UI's expectations.
type ModelDetail struct {
	Size              *int64   `json:"size"`
	ModifiedAt        *float64 `json:"modified_at"`
	Digest            *string  `json:"digest"`
	Family            *string  `json:"family"`
	ParameterSize     *string  `json:"parameter_size"`
	QuantizationLevel *string  `json:"quantization_level"`
	Format            *string  `json:"format"`
}

// validateModelDetails sanitises the optional per-model facts sent by agents.
// Bad individual entries or fields are dropped rather than rejected, so an
// unexpected Ollama version can never make a node look broken.
func validateModelDetails(v any) (map[string]ModelDetail, error) {
	out := map[string]ModelDetail{}
	if v == nil {
		return out, nil
	}
	list, ok := v.([]any)
	if !ok || len(list) > maxModels {
		return nil, verr("model_details must be a list of at most %d entries", maxModels)
	}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, ok := item["name"].(string)
		if !ok || strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > maxModelNameLen {
			continue
		}
		txt := func(k string) *string {
			s, ok := item[k].(string)
			s = strings.TrimSpace(s)
			if !ok || s == "" {
				return nil
			}
			s = truncRunes(s, 128)
			return &s
		}
		d := ModelDetail{
			Digest: txt("digest"), Family: txt("family"), ParameterSize: txt("parameter_size"),
			QuantizationLevel: txt("quantization_level"), Format: txt("format"),
		}
		if f, ok := item["size"].(float64); ok && f >= 0 && f < float64(math.MaxInt64) && f == math.Trunc(f) {
			n := int64(f)
			d.Size = &n
		}
		if f, ok := item["modified_at"].(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 {
			d.ModifiedAt = &f
		}
		out[strings.TrimSpace(name)] = d
	}
	return out, nil
}

// validatePullModel validates a model name an admin asks a node to pull. Plain
// names ("llama3.2:3b", "library/llama3") are always fine. A name that starts
// with a registry host ("hf.co/org/repo") must use an allowed registry, so the
// UI cannot be used to point nodes at arbitrary servers.
func validatePullModel(v any, cfg ControllerConfig) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", verr("model must be a string")
	}
	name := strings.TrimSpace(s)
	if !modelPullRe.MatchString(name) || strings.Contains(name, "..") || strings.Contains(name, "//") {
		return "", verr("model name is not valid")
	}
	if strings.Contains(name, "/") {
		first := strings.ToLower(strings.SplitN(name, "/", 2)[0])
		looksLikeHost := strings.Contains(first, ".") || strings.Contains(first, ":") || strings.Count(name, "/") >= 2
		if looksLikeHost && !cfg.PullRegistries[first] {
			return "", verr("registry '%s' is not allowed", first)
		}
	}
	return name, nil
}

// normModel: Ollama treats 'llama3' and 'llama3:latest' as the same model.
func normModel(name string) string {
	name = strings.TrimSpace(name)
	last := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		last = name[i+1:]
	}
	if strings.Contains(last, ":") {
		return name
	}
	return name + ":latest"
}

// ---- node registry (thread-safe, one record per node) ---------------------

// NodeView is the JSON shape of a node, as returned by the API.
type NodeView struct {
	Node               string                 `json:"node"`
	OllamaURL          string                 `json:"ollama_url"`
	CPU                *float64               `json:"cpu"`
	RAM                *float64               `json:"ram"`
	OllamaLANReachable bool                   `json:"ollama_lan_reachable"`
	Models             []string               `json:"models"`
	ModelDetails       map[string]ModelDetail `json:"model_details"`
	LastSeen           float64                `json:"last_seen"`
	Online             bool                   `json:"online"`
	// online               = heartbeat fresh and Ollama reachable
	// provider_unreachable = heartbeat fresh but Ollama cannot be reached from the LAN
	// offline              = no recent heartbeat
	Status string `json:"status"`
}

type nodeRec struct {
	node, url string
	cpu, ram  *float64
	reachable bool
	models    []string
	details   map[string]ModelDetail
	lastSeen  time.Time
	sourceIP  string // peer IP that authenticated this node identity
}

type Registry struct {
	cfg   ControllerConfig
	clock func() time.Time
	mu    sync.Mutex
	nodes map[string]*nodeRec
	rr    atomic.Uint64
}

func NewRegistry(cfg ControllerConfig, clock func() time.Time) *Registry {
	return &Registry{cfg: cfg, clock: clock, nodes: map[string]*nodeRec{}}
}

// Register creates or refreshes a node record. Returns true if newly created.
func (r *Registry) Register(node, ollamaURL string) bool {
	created, _ := r.RegisterFrom(node, ollamaURL, "")
	return created
}

// RegisterFrom binds a node identity to the authenticated peer address. A live
// node cannot be silently taken over from a different source address.
func (r *Registry) RegisterFrom(node, ollamaURL, sourceIP string) (bool, error) {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[node]
	if !ok {
		r.nodes[node] = &nodeRec{node: node, url: ollamaURL, models: []string{}, details: map[string]ModelDetail{}, lastSeen: now, sourceIP: sourceIP}
		return true, nil
	}
	if sourceIP != "" && rec.sourceIP != "" && rec.sourceIP != sourceIP && now.Sub(rec.lastSeen) <= r.cfg.OfflineAfter {
		return false, verr("node identity is already registered from another source address")
	}
	if sourceIP != "" && (rec.sourceIP == "" || now.Sub(rec.lastSeen) > r.cfg.OfflineAfter) {
		rec.sourceIP = sourceIP
	}
	rec.url = ollamaURL
	rec.lastSeen = now
	return false, nil
}

// Heartbeat updates an existing record. Returns false if the node is unknown.
func (r *Registry) Heartbeat(node string, cpu, ram float64, reachable bool, models []string, details map[string]ModelDetail) bool {
	ok, _ := r.HeartbeatFrom(node, "", cpu, ram, reachable, models, details)
	return ok
}

func (r *Registry) HeartbeatFrom(node, sourceIP string, cpu, ram float64, reachable bool, models []string, details map[string]ModelDetail) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[node]
	if !ok {
		return false, nil
	}
	if sourceIP != "" && rec.sourceIP != "" && rec.sourceIP != sourceIP {
		return false, verr("node heartbeat source address does not match its registration")
	}
	kept := map[string]ModelDetail{}
	for _, m := range models {
		if d, ok := details[m]; ok {
			kept[m] = d
		}
	}
	rec.cpu, rec.ram, rec.reachable = &cpu, &ram, reachable
	rec.models, rec.details = append([]string(nil), models...), kept
	rec.lastSeen = r.clock()
	return true, nil
}

func (r *Registry) view(rec *nodeRec, now time.Time) NodeView {
	models := append([]string{}, rec.models...)
	details := make(map[string]ModelDetail, len(rec.details))
	for k, v := range rec.details {
		details[k] = v
	}
	online := now.Sub(rec.lastSeen) <= r.cfg.OfflineAfter
	status := "offline"
	if online {
		status = "provider_unreachable"
		if rec.reachable {
			status = "online"
		}
	}
	return NodeView{
		Node: rec.node, OllamaURL: rec.url, CPU: rec.cpu, RAM: rec.ram,
		OllamaLANReachable: rec.reachable, Models: models, ModelDetails: details,
		LastSeen: epoch(rec.lastSeen), Online: online, Status: status,
	}
}

func (r *Registry) purgeLocked(now time.Time) {
	for n, rec := range r.nodes {
		if now.Sub(rec.lastSeen) > r.cfg.PurgeAfter {
			delete(r.nodes, n)
			log.Info("purged stale node", "node", n)
		}
	}
}

func (r *Registry) Snapshot() []NodeView {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeLocked(now)
	names := make([]string, 0, len(r.nodes))
	for n := range r.nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]NodeView, 0, len(names))
	for _, n := range names {
		out = append(out, r.view(r.nodes[n], now))
	}
	return out
}

func (r *Registry) Get(node string) (NodeView, bool) {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeLocked(now)
	rec, ok := r.nodes[node]
	if !ok {
		return NodeView{}, false
	}
	return r.view(rec, now), true
}

// CandidatesFor returns (hosting, eligible) nodes for a model. Eligible means
// online, Ollama LAN-reachable, and hosting the model. Eligible nodes are
// rotated round-robin so consecutive requests spread out; the remaining ones
// serve as connect-failure fallbacks.
func (r *Registry) CandidatesFor(model string) (hosting, eligible []NodeView) {
	want := normModel(model)
	for _, n := range r.Snapshot() {
		for _, m := range n.Models {
			if normModel(m) == want {
				hosting = append(hosting, n)
				break
			}
		}
	}
	for _, n := range hosting {
		if n.Online && n.OllamaLANReachable {
			eligible = append(eligible, n)
		}
	}
	if len(eligible) > 1 {
		k := int((r.rr.Add(1) - 1) % uint64(len(eligible)))
		rotated := make([]NodeView, 0, len(eligible))
		rotated = append(rotated, eligible[k:]...)
		eligible = append(rotated, eligible[:k]...)
	}
	return hosting, eligible
}
