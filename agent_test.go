package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateServiceBaseURLTable(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		bad         bool
	}{
		{"http origin", "http://127.0.0.1:8000", false}, {"https origin", "https://controller.example:443", false},
		{"trailing slash", "http://127.0.0.1:8000/", false}, {"missing", "", true}, {"scheme missing", "127.0.0.1:8000", true},
		{"ftp", "ftp://127.0.0.1:8000", true}, {"credentials", "http://user:pass@127.0.0.1:8000", true},
		{"path", "http://127.0.0.1:8000/api", true}, {"query", "http://127.0.0.1:8000?x=1", true},
		{"fragment", "http://127.0.0.1:8000/#frag", true}, {"port zero", "http://127.0.0.1:0", true},
		{"large port", "http://127.0.0.1:99999", true}, {"bad url", "http://[", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServiceBaseURL("URL", tt.value)
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v bad=%v", err, tt.bad)
			}
		})
	}
}

func setValidAgentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_TOKEN", "a6D1f7H2j9K4m8N3p5Q0r6S1t7V2w9X4")
	t.Setenv("AGENT_TOKENS", "")
	t.Setenv("CONTROLLER_URL", "http://127.0.0.1:8000")
	t.Setenv("NODE_ID", "node-a")
	t.Setenv("OLLAMA_PORT", "11434")
	t.Setenv("HEARTBEAT_INTERVAL", "5")
	t.Setenv("OLLAMA_LOCAL_URL", "http://127.0.0.1:11434")
	t.Setenv("OLLAMA_ADVERTISE_URL", "")
}

func TestAgentConfigValidation(t *testing.T) {
	t.Run("valid defaults", func(t *testing.T) {
		setValidAgentEnv(t)
		cfg, err := agentConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.NodeID != "node-a" || cfg.OllamaPort != 11434 || cfg.HeartbeatInterval != 5*time.Second {
			t.Fatalf("unexpected config %+v", cfg)
		}
	})
	cases := []struct{ name, key, value string }{
		{"short token", "AGENT_TOKEN", "short"}, {"bad controller URL", "CONTROLLER_URL", "file:///etc/passwd"},
		{"node ID path traversal", "NODE_ID", "../../etc"}, {"port zero", "OLLAMA_PORT", "0"},
		{"port too large", "OLLAMA_PORT", "65536"}, {"port not number", "OLLAMA_PORT", "nope"},
		{"NaN heartbeat", "HEARTBEAT_INTERVAL", "NaN"}, {"infinite heartbeat", "HEARTBEAT_INTERVAL", "+Inf"},
		{"zero heartbeat", "HEARTBEAT_INTERVAL", "0"}, {"very long heartbeat", "HEARTBEAT_INTERVAL", "3601"},
		{"bad local URL", "OLLAMA_LOCAL_URL", "http://user:pass@127.0.0.1:11434"},
		{"bad advertise URL", "OLLAMA_ADVERTISE_URL", "http://127.0.0.1:11434/api"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			setValidAgentEnv(t)
			t.Setenv(tt.key, tt.value)
			if _, err := agentConfigFromEnv(); err == nil {
				t.Fatalf("expected %s=%q to be rejected", tt.key, tt.value)
			}
		})
	}
}

func TestParseTimestampTable(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
		valid bool
	}{
		{"RFC3339", "2024-01-02T03:04:05Z", true}, {"nanoseconds", "2024-01-02T03:04:05.123456789Z", true},
		{"offset", "2024-01-02T03:04:05+05:30", true}, {"no timezone legacy", "2024-01-02T03:04:05.123", true},
		{"wrong type", 12, false}, {"empty", "", false}, {"invalid", "yesterday", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTimestamp(tt.value)
			if (got != nil) != tt.valid {
				t.Fatalf("got=%v valid=%v", got, tt.valid)
			}
		})
	}
}

func TestSummarizeModelHandlesBadFields(t *testing.T) {
	got := summarizeModel(map[string]any{
		"name": "llama3", "size": float64(1024), "modified_at": "2024-01-02T03:04:05Z", "digest": "abc",
		"details": map[string]any{"family": "llama", "parameter_size": "8B", "quantization_level": "Q4_K_M", "format": "gguf"},
	})
	if got.Name != "llama3" || got.Size == nil || *got.Size != 1024 || got.ModifiedAt == nil || got.Family != "llama" {
		t.Fatalf("unexpected model summary %+v", got)
	}
	bad := summarizeModel(map[string]any{"name": "x", "size": math.NaN(), "modified_at": false, "details": "invalid"})
	if bad.Size != nil || bad.ModifiedAt != nil || bad.Family != "" {
		t.Fatalf("bad fields should be ignored: %+v", bad)
	}
}

func TestModelsAtHandlesSuccessErrorsAndMalformedJSON(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		body      string
		wantErr   bool
		wantCount int
	}{
		{"valid list", 200, `{"models":[{"name":"llama3","size":42},{"name":"qwen"}]}`, false, 2},
		{"empty list", 200, `{"models":[]}`, false, 0}, {"wrong status", 503, `{"error":"down"}`, true, 0},
		{"malformed JSON", 200, `{`, true, 0}, {"invalid models type", 200, `{"models":"not-a-list"}`, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			a := &Agent{client: &http.Client{Timeout: time.Second}}
			got, err := a.modelsAt(context.Background(), srv.URL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && len(got) != tt.wantCount {
				t.Fatalf("got %d models want %d", len(got), tt.wantCount)
			}
		})
	}
}

func TestModelsAtRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 8*1024*1024+100))
	}))
	defer srv.Close()
	a := &Agent{client: &http.Client{Timeout: 5 * time.Second}}
	if _, err := a.modelsAt(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "exceeded 8 MiB") {
		t.Fatalf("expected response-size error, got %v", err)
	}
}

func TestAgentPostJSONAddsBearerAuth(t *testing.T) {
	seenAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type=%q", r.Header.Get("Content-Type"))
		}
		var value map[string]any
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			t.Errorf("invalid body: %v", err)
		}
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	a := &Agent{cfg: AgentConfig{ControllerURL: srv.URL, Token: "secret"}, client: &http.Client{Timeout: time.Second}}
	status, body, err := a.postJSON(context.Background(), "/api/test", map[string]any{"node": "node-a"})
	if err != nil || status != 201 || !strings.Contains(body, "ok") {
		t.Fatalf("status=%d body=%q err=%v", status, body, err)
	}
	if seenAuth != "Bearer secret" {
		t.Fatalf("authorization=%q", seenAuth)
	}
}

func TestAgentRunStopsOnContextCancel(t *testing.T) {
	setValidAgentEnv(t)
	cfg, err := agentConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cfg.HeartbeatInterval = time.Millisecond
	a := NewAgent(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Agent.Run did not respect cancelled context")
	}
}

func TestControllerConfigEnvironmentDefaultsAndValidation(t *testing.T) {
	setValidAgentEnv(t)
	t.Setenv("ADMIN_TOKEN", "f5G1h8J3k6M2n9P4q7R0s5T1v8W3x6Y9")
	for _, k := range []string{"OFFLINE_AFTER", "PURGE_AFTER", "CONNECT_TIMEOUT", "READ_TIMEOUT", "ALLOWED_NODE_NETWORKS", "ALLOWED_NODE_PORTS", "ALLOW_HOSTNAMES", "VERIFY_SOURCE_IP", "MAX_BODY_MB", "MAX_CONCURRENT_REQUESTS", "MAX_CONCURRENT_PULLS", "MAX_CONCURRENT_PULLS_PER_NODE", "PULL_REGISTRIES", "AGENT_TOKENS"} {
		t.Setenv(k, "")
	}
	cfg, err := controllerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBodyBytes != 8*1024*1024 || cfg.MaxConcurrentRequests != 256 || cfg.MaxConcurrentPulls != 4 || cfg.MaxConcurrentPullsPerNode != 1 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
	for _, tt := range []struct{ name, key, value string }{
		{"offline too small", "OFFLINE_AFTER", "1"}, {"purge earlier than offline", "PURGE_AFTER", "10"},
		{"connect zero", "CONNECT_TIMEOUT", "0"}, {"read timeout NaN", "READ_TIMEOUT", "NaN"},
		{"bad CIDR", "ALLOWED_NODE_NETWORKS", "10.0.0.0/99"}, {"bad allowed port", "ALLOWED_NODE_PORTS", "0"}, {"body too small", "MAX_BODY_MB", "0"},
		{"body decimal", "MAX_BODY_MB", "1.5"}, {"request concurrency zero", "MAX_CONCURRENT_REQUESTS", "0"},
		{"pull limit zero", "MAX_CONCURRENT_PULLS", "0"}, {"per-node above global", "MAX_CONCURRENT_PULLS_PER_NODE", "5"},
		{"bad hostname boolean", "ALLOW_HOSTNAMES", "maybe"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setValidAgentEnv(t)
			t.Setenv("ADMIN_TOKEN", "f5G1h8J3k6M2n9P4q7R0s5T1v8W3x6Y9")
			for _, k := range []string{"OFFLINE_AFTER", "PURGE_AFTER", "CONNECT_TIMEOUT", "READ_TIMEOUT", "ALLOWED_NODE_NETWORKS", "ALLOWED_NODE_PORTS", "ALLOW_HOSTNAMES", "VERIFY_SOURCE_IP", "MAX_BODY_MB", "MAX_CONCURRENT_REQUESTS", "MAX_CONCURRENT_PULLS", "MAX_CONCURRENT_PULLS_PER_NODE", "PULL_REGISTRIES", "AGENT_TOKENS"} {
				t.Setenv(k, "")
			}
			t.Setenv(tt.key, tt.value)
			if _, err := controllerConfigFromEnv(); err == nil {
				t.Fatalf("expected %s=%q to fail", tt.key, tt.value)
			}
		})
	}
}

func TestControllerConfigRejectsSharedAdminAndAgentSecret(t *testing.T) {
	setValidAgentEnv(t)
	secret := "9a8B7c6D5e4F3g2H1i0J9k8L7m6N5o4P"
	t.Setenv("AGENT_TOKEN", secret)
	t.Setenv("ADMIN_TOKEN", secret)
	if _, err := controllerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "must be different") {
		t.Fatalf("expected distinct-token error, got %v", err)
	}
}

func TestParseAgentTokensAndPerNodeCredentialConfig(t *testing.T) {
	goodA := "a6D1f7H2j9K4m8N3p5Q0r6S1t7V2w9X4"
	goodB := "b7E2g8J3k0L5n9P4q6R1s7T2v8W3x0Y5"
	for _, tt := range []struct {
		name, raw string
		bad       bool
	}{
		{"two nodes", "node-a:" + goodA + ",node-b:" + goodB, false},
		{"missing separator", "node-a" + goodA, true},
		{"invalid node", "bad/node:" + goodA, true},
		{"short secret", "node-a:short", true},
		{"duplicate node", "node-a:" + goodA + ",node-a:" + goodB, true},
		{"duplicate secret", "node-a:" + goodA + ",node-b:" + goodA, true},
		{"empty map", " , ", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAgentTokens(tt.raw)
			if (err != nil) != tt.bad {
				t.Fatalf("map=%v err=%v bad=%v", got, err, tt.bad)
			}
			if err == nil && len(got) != 2 {
				t.Fatalf("got %d tokens, want 2", len(got))
			}
		})
	}
	setValidAgentEnv(t)
	t.Setenv("AGENT_TOKEN", "")
	t.Setenv("AGENT_TOKENS", "node-a:"+goodA+",node-b:"+goodB)
	t.Setenv("ADMIN_TOKEN", "f5G1h8J3k6M2n9P4q7R0s5T1v8W3x6Y9")
	cfg, err := controllerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentToken != "" || len(cfg.AgentTokens) != 2 || cfg.AgentTokens["node-a"] != goodA {
		t.Fatalf("bad per-node config: %+v", cfg)
	}
	t.Setenv("AGENT_TOKEN", goodA)
	if _, err := controllerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "either AGENT_TOKEN or AGENT_TOKENS") {
		t.Fatalf("expected mixed-mode error, got %v", err)
	}
}

func TestControllerTLSPolicy(t *testing.T) {
	for _, key := range []string{"TLS_CERT_FILE", "TLS_KEY_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("ALLOW_INSECURE_HTTP", "false")
	t.Setenv("CONTROLLER_HOST", "127.0.0.1")
	if cert, key, err := controllerTLSFromEnv(); err != nil || cert != "" || key != "" {
		t.Fatalf("loopback HTTP config: cert=%q key=%q err=%v", cert, key, err)
	}
	t.Setenv("CONTROLLER_HOST", "0.0.0.0")
	if _, _, err := controllerTLSFromEnv(); err == nil {
		t.Fatal("expected non-loopback HTTP to be rejected by default")
	}
	t.Setenv("ALLOW_INSECURE_HTTP", "maybe")
	if _, _, err := controllerTLSFromEnv(); err == nil {
		t.Fatal("expected malformed ALLOW_INSECURE_HTTP boolean to fail")
	}
	t.Setenv("ALLOW_INSECURE_HTTP", "true")
	if cert, key, err := controllerTLSFromEnv(); err != nil || cert != "" || key != "" {
		t.Fatalf("explicit insecure HTTP config: cert=%q key=%q err=%v", cert, key, err)
	}
	t.Setenv("ALLOW_INSECURE_HTTP", "false")
	t.Setenv("TLS_CERT_FILE", filepath.Join(t.TempDir(), "cert.pem"))
	if _, _, err := controllerTLSFromEnv(); err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("expected TLS pair error, got %v", err)
	}
}

func TestControllerTLSLoadsValidPair(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_HOST", "0.0.0.0")
	t.Setenv("ALLOW_INSECURE_HTTP", "false")
	t.Setenv("TLS_CERT_FILE", certFile)
	t.Setenv("TLS_KEY_FILE", keyFile)
	cert, keyPath, err := controllerTLSFromEnv()
	if err != nil || cert != certFile || keyPath != keyFile {
		t.Fatalf("valid TLS pair failed: cert=%q key=%q err=%v", cert, keyPath, err)
	}
}
