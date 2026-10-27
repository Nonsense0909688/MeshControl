// MeshControl node agent.
//
// Runs next to an Ollama server. Registers with the controller, then sends a
// small heartbeat every few seconds with CPU/RAM usage and the installed models.
// The agent never forwards inference traffic.
//
// Run:  CONTROLLER_URL=http://10.0.0.5:8000 AGENT_TOKEN=... ./agent
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

const maxBackoff = 30 * time.Second

// ---- config ---------------------------------------------------------------

type AgentConfig struct {
	ControllerURL     string
	Token             string
	NodeID            string
	OllamaLocalURL    string
	AdvertiseURL      string // URL the controller should use; auto if empty
	OllamaPort        int
	HeartbeatInterval time.Duration
}

func agentConfigFromEnv() (AgentConfig, error) {
	token := strings.TrimSpace(os.Getenv("AGENT_TOKEN"))
	controller := strings.TrimRight(strings.TrimSpace(os.Getenv("CONTROLLER_URL")), "/")
	if token == "" {
		return AgentConfig{}, errors.New("AGENT_TOKEN environment variable is required")
	}
	if controller == "" {
		return AgentConfig{}, errors.New("CONTROLLER_URL environment variable is required")
	}
	nodeID := envOr("NODE_ID", "")
	if nodeID == "" {
		h, err := os.Hostname()
		if err != nil {
			return AgentConfig{}, fmt.Errorf("cannot determine hostname, set NODE_ID: %w", err)
		}
		nodeID = h
	}
	port, err := strconv.Atoi(envOr("OLLAMA_PORT", "11434"))
	if err != nil {
		return AgentConfig{}, fmt.Errorf("invalid OLLAMA_PORT: %w", err)
	}
	hb, err := strconv.ParseFloat(envOr("HEARTBEAT_INTERVAL", "5"), 64)
	if err != nil || hb <= 0 {
		return AgentConfig{}, errors.New("invalid HEARTBEAT_INTERVAL")
	}
	return AgentConfig{
		ControllerURL:     controller,
		Token:             token,
		NodeID:            nodeID,
		OllamaLocalURL:    strings.TrimRight(envOr("OLLAMA_LOCAL_URL", "http://127.0.0.1:11434"), "/"),
		AdvertiseURL:      strings.TrimRight(envOr("OLLAMA_ADVERTISE_URL", ""), "/"),
		OllamaPort:        port,
		HeartbeatInterval: time.Duration(hb * float64(time.Second)),
	}, nil
}

// ---- model summaries ------------------------------------------------------

type ModelInfo struct {
	Name              string   `json:"name"`
	Size              *int64   `json:"size,omitempty"`
	ModifiedAt        *float64 `json:"modified_at,omitempty"`
	Digest            string   `json:"digest,omitempty"`
	Family            string   `json:"family,omitempty"`
	ParameterSize     string   `json:"parameter_size,omitempty"`
	QuantizationLevel string   `json:"quantization_level,omitempty"`
	Format            string   `json:"format,omitempty"`
}

var tsLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} // second one: no zone = UTC

// parseTimestamp converts an Ollama ISO-8601 timestamp (nanosecond precision)
// to epoch seconds. Returns nil if the value is not a valid timestamp.
func parseTimestamp(v any) *float64 {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	s = strings.TrimSpace(s)
	for _, layout := range tsLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			f := float64(t.Unix()) + float64(t.Nanosecond())/1e9
			return &f
		}
	}
	return nil
}

// summarizeModel reduces one /api/tags entry to the fields the controller displays.
func summarizeModel(entry map[string]any) ModelInfo {
	details, _ := entry["details"].(map[string]any)
	out := ModelInfo{
		Name:              str(entry, "name"),
		ModifiedAt:        parseTimestamp(entry["modified_at"]),
		Digest:            str(entry, "digest"),
		Family:            str(details, "family"),
		ParameterSize:     str(details, "parameter_size"),
		QuantizationLevel: str(details, "quantization_level"),
		Format:            str(details, "format"),
	}
	if f, ok := entry["size"].(float64); ok && f >= 0 && f == math.Trunc(f) {
		n := int64(f)
		out.Size = &n
	}
	return out
}

// discoverLANIP finds the local IP used to reach targetHost. A UDP "connect"
// sends no packets; it only makes the OS pick a route.
func discoverLANIP(targetHost string) string {
	conn, err := net.Dial("udp4", net.JoinHostPort(targetHost, "9"))
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return "127.0.0.1"
}

// ---- agent ----------------------------------------------------------------

type Agent struct {
	cfg        AgentConfig
	client     *http.Client
	advertised string
}

func NewAgent(cfg AgentConfig) *Agent {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	a := &Agent{cfg: cfg, client: &http.Client{Transport: transport, Timeout: 10 * time.Second}}
	if cfg.AdvertiseURL != "" {
		a.advertised = cfg.AdvertiseURL
	} else {
		host := "127.0.0.1"
		if u, err := url.Parse(cfg.ControllerURL); err == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
		a.advertised = fmt.Sprintf("http://%s:%d", discoverLANIP(host), cfg.OllamaPort)
	}
	cpu.Percent(0, false) // prime so the first reading is meaningful
	return a
}

// ---- Ollama ---------------------------------------------------------------

func (a *Agent) modelsAt(ctx context.Context, base string) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req) // no auth header: the token is for the controller only
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Models []any `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	models := []ModelInfo{}
	for _, raw := range body.Models {
		if m, ok := raw.(map[string]any); ok && str(m, "name") != "" {
			models = append(models, summarizeModel(m))
		}
	}
	return models, nil
}

// readModels returns (lanReachable, models). It queries Ollama through its
// advertised LAN address first; that both lists the models and proves other
// machines can reach it. If that fails but the local address works, Ollama is
// up but probably bound to 127.0.0.1 only (check OLLAMA_HOST), so we report
// reachable=false.
func (a *Agent) readModels(ctx context.Context) (bool, []ModelInfo) {
	if models, err := a.modelsAt(ctx, a.advertised); err == nil {
		return true, models
	}
	if a.cfg.OllamaLocalURL != a.advertised {
		if models, err := a.modelsAt(ctx, a.cfg.OllamaLocalURL); err == nil {
			return false, models
		}
	}
	return false, []ModelInfo{}
}

// ---- controller -----------------------------------------------------------

func (a *Agent) postJSON(ctx context.Context, path string, payload any) (int, string, error) {
	data, err := json.Marshal(payload) // compact
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ControllerURL+path, bytes.NewReader(data))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return resp.StatusCode, string(b), nil
}

func (a *Agent) register(ctx context.Context) bool {
	status, body, err := a.postJSON(ctx, "/api/agent/register", map[string]string{
		"node": a.cfg.NodeID, "ollama_url": a.advertised,
	})
	if err != nil {
		log.Warn("registration failed", "error", errName(err))
		return false
	}
	switch status {
	case 200, 201:
		log.Info("registered", "node", a.cfg.NodeID, "url", a.advertised)
		return true
	case 401:
		log.Error("controller rejected our token; check AGENT_TOKEN")
	default:
		log.Error("registration rejected", "status", status, "body", body)
	}
	return false
}

type heartbeatPayload struct {
	Node              string      `json:"node"`
	CPU               float64     `json:"cpu"`
	RAM               float64     `json:"ram"`
	OllamaLANReachable bool        `json:"ollama_lan_reachable"`
	Models            []string    `json:"models"`
	ModelDetails      []ModelInfo `json:"model_details"`
}

// heartbeat sends one heartbeat. Returns "ok", "reregister" or "error".
func (a *Agent) heartbeat(ctx context.Context) string {
	reachable, details := a.readModels(ctx)
	names := make([]string, 0, len(details))
	for _, m := range details {
		names = append(names, m.Name)
	}
	var cpuPct float64
	if p, err := cpu.Percent(0, false); err == nil && len(p) > 0 {
		cpuPct = p[0]
	}
	var ramPct float64
	if vm, err := mem.VirtualMemory(); err == nil {
		ramPct = vm.UsedPercent
	}
	status, _, err := a.postJSON(ctx, "/api/agent/heartbeat", heartbeatPayload{
		Node: a.cfg.NodeID, CPU: round1(cpuPct), RAM: round1(ramPct),
		OllamaLANReachable: reachable, Models: names, ModelDetails: details,
	})
	if err != nil {
		log.Warn("heartbeat failed", "error", errName(err))
		return "error"
	}
	switch status {
	case 200:
		return "ok"
	case 404:
		log.Info("controller does not know us (restarted?); re-registering")
		return "reregister"
	}
	log.Warn("heartbeat rejected", "status", status)
	return "error"
}

// sleep waits for d or until ctx is cancelled; returns false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (a *Agent) Run(ctx context.Context) {
	registered := false
	backoff := time.Second
	for ctx.Err() == nil {
		if !registered {
			registered = a.register(ctx)
			if !registered {
				if !sleep(ctx, backoff) {
					return
				}
				if backoff *= 2; backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			backoff = time.Second
		}
		if a.heartbeat(ctx) == "reregister" {
			registered = false
			continue
		}
		if !sleep(ctx, a.cfg.HeartbeatInterval) {
			return
		}
	}
}

func runAgent() int {
	cfg, err := agentConfigFromEnv()
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	NewAgent(cfg).Run(ctx)
	return 0
}
