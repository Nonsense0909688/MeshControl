package main

import (
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig() ControllerConfig {
	_, nets, _ := net.ParseCIDR("127.0.0.0/8")
	_, lan, _ := net.ParseCIDR("10.0.0.0/8")
	_, loop6, _ := net.ParseCIDR("::1/128")
	return ControllerConfig{
		AgentToken: strings.Repeat("agent-secret-", 4), AdminToken: strings.Repeat("admin-secret-", 4),
		OfflineAfter: 10 * time.Second, PurgeAfter: time.Minute,
		AllowedNetworks: []*net.IPNet{nets, lan, loop6}, AllowedNodePorts: map[int]bool{11434: true}, ConnectTimeout: time.Second, ReadTimeout: 5 * time.Second,
		MaxBodyBytes: 1024 * 1024, MaxConcurrentRequests: 64,
		MaxConcurrentPulls: 2, MaxConcurrentPullsPerNode: 1,
		PullRegistries: map[string]bool{"hf.co": true, "registry.ollama.ai": true},
	}
}

func TestParseNetworksTable(t *testing.T) {
	tests := []struct {
		name, input string
		want        int
		bad         bool
	}{
		{"ipv4 CIDR", "10.0.0.0/8", 1, false},
		{"ipv4 host auto mask", "10.0.0.1", 1, false},
		{"ipv6 host auto mask", "::1", 1, false},
		{"multiple", "10.0.0.0/8, 192.168.0.0/16,fc00::/7", 3, false},
		{"spaces and empty", " 10.0.0.0/8, ,192.168.0.0/16 ", 2, false},
		{"bad mask", "10.0.0.0/99", 0, true},
		{"bad ip", "not-an-ip/24", 0, true},
		{"empty", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNetworks(tt.input)
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v, want bad=%v", err, tt.bad)
			}
			if len(got) != tt.want {
				t.Fatalf("len=%d want %d", len(got), tt.want)
			}
		})
	}
}

func TestValidateSecretTable(t *testing.T) {
	good := "a8F1d2C3e4G5h6J7k8M9n0P1q2R3s4T5"
	for _, tt := range []struct {
		name, value string
		bad         bool
	}{
		{"valid", good, false}, {"empty", "", true}, {"short", "1234567890123456789012345678901", true},
		{"placeholder", "replace-with-a-secret-that-is-long-enough-to-look-real", true},
		{"case-insensitive placeholder", "THIS-IS-A-CHANGE-ME-PLACEHOLDER-SECRET-1234567890", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSecret("TOKEN", tt.value)
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v bad=%v", err, tt.bad)
			}
		})
	}
}

func TestValidateNodeIDTable(t *testing.T) {
	for _, tt := range []struct {
		value any
		want  string
		bad   bool
	}{
		{"node", "node", false}, {"a", "a", false}, {"GPU_box-01.local", "GPU_box-01.local", false},
		{"1starts-digit", "1starts-digit", false}, {"", "", true}, {" leading", " leading", true},
		{"trailing space ", "trailing space ", true}, {"bad/slash", "bad/slash", true}, {"bad space", "bad space", true},
		{strings.Repeat("a", 65), strings.Repeat("a", 65), true}, {nil, "", true}, {42, "", true},
	} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(strings.Repeat("x", 0)+toTestName(tt.value)), " ", "_"), func(t *testing.T) {
			got, err := validateNodeID(tt.value)
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v bad=%v", err, tt.bad)
			}
			if err == nil && got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func toTestName(v any) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(strings.Trim(strings.ReplaceAll(strings.TrimSpace(asString(v)), "\n", "\\n"), "\t")), "/", "_"), ".", "_")
}
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "non_string"
}

func TestValidatePercentTable(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
		bad   bool
	}{
		{"zero", float64(0), false}, {"hundred", float64(100), false}, {"middle", float64(27.5), false},
		{"negative", float64(-0.1), true}, {"over", float64(100.1), true}, {"nan", math.NaN(), true},
		{"positive infinity", math.Inf(1), true}, {"negative infinity", math.Inf(-1), true}, {"string", "50", true}, {"nil", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validatePercent(tt.value, "cpu")
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v want bad=%v", err, tt.bad)
			}
		})
	}
}

func TestValidateModelsTable(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
		want  int
		bad   bool
	}{
		{"empty list", []any{}, 0, false}, {"valid", []any{"llama3", "qwen2.5:3b"}, 2, false},
		{"duplicates removed", []any{"llama3", "llama3", " llama3 "}, 1, false},
		{"not list", "llama3", 0, true}, {"null item", []any{nil}, 0, true}, {"empty item", []any{" "}, 0, true},
		{"long item", []any{strings.Repeat("x", maxModelNameLen+1)}, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateModels(tt.value)
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v bad=%v", err, tt.bad)
			}
			if err == nil && len(got) != tt.want {
				t.Fatalf("len=%d want=%d", len(got), tt.want)
			}
		})
	}
	tooMany := make([]any, maxModels+1)
	for i := range tooMany {
		tooMany[i] = "m"
	}
	if _, err := validateModels(tooMany); err == nil {
		t.Fatal("expected too-many-models error")
	}
}

func TestValidateModelDetailsSanitizes(t *testing.T) {
	got, err := validateModelDetails([]any{
		map[string]any{"name": "llama3", "size": float64(123), "digest": " abc ", "family": strings.Repeat("x", 200)},
		map[string]any{"name": "", "digest": "ignored"}, "invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	if got["llama3"].Size == nil || *got["llama3"].Size != 123 {
		t.Fatalf("bad size: %+v", got["llama3"].Size)
	}
	if got["llama3"].Digest == nil || *got["llama3"].Digest != "abc" {
		t.Fatalf("bad digest: %+v", got["llama3"].Digest)
	}
	if got["llama3"].Family == nil || len(*got["llama3"].Family) != 128 {
		t.Fatalf("family was not capped")
	}
	if _, err := validateModelDetails("wrong type"); err == nil {
		t.Fatal("expected error for wrong outer type")
	}
}

func TestValidateOllamaURLTable(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedNodePorts[80] = true // this test also covers implicit HTTP port normalisation
	for _, tt := range []struct {
		name, value string
		bad         bool
	}{
		{"ipv4", "http://127.0.0.1:11434", false}, {"ipv4 default port", "http://127.0.0.1", false},
		{"ipv6", "http://[::1]:11434", false}, {"https", "https://127.0.0.1:11434/", false},
		{"scheme missing", "127.0.0.1:11434", true}, {"ftp", "ftp://127.0.0.1:21", true},
		{"credentials", "http://user:pass@127.0.0.1:11434", true}, {"path", "http://127.0.0.1:11434/api", true},
		{"query", "http://127.0.0.1:11434?x=1", true}, {"fragment", "http://127.0.0.1:11434/#hi", true},
		{"public host", "http://8.8.8.8:11434", true}, {"disallowed port", "http://127.0.0.1:8080", true}, {"port zero", "http://127.0.0.1:0", true},
		{"bad port", "http://127.0.0.1:99999", true}, {"empty", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateOllamaURL(tt.value, cfg)
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v bad=%v", err, tt.bad)
			}
		})
	}
	cfg.AllowHostnames = false
	if _, err := validateOllamaURL("http://localhost:11434", cfg); err == nil {
		t.Fatal("hostname should be disabled by default")
	}
}

func TestValidatePullModelTable(t *testing.T) {
	cfg := testConfig()
	for _, tt := range []struct {
		name, value string
		bad         bool
	}{
		{"simple", "llama3.2:3b", false}, {"library path", "library/llama3", false},
		{"allowed registry", "hf.co/org/repo", false}, {"disallowed registry", "evil.example/org/repo", true},
		{"double dot", "../secret", true}, {"double slash", "org//repo", true},
		{"spaces", "bad model", true}, {"empty", "", true}, {"unsupported punctuation", "model;foo", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validatePullModel(tt.value, cfg)
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v want bad=%v", err, tt.bad)
			}
		})
	}
}

func TestNormModelTable(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"llama3", "llama3:latest"}, {" llama3 ", "llama3:latest"}, {"llama3:latest", "llama3:latest"},
		{"org/llama3", "org/llama3:latest"}, {"org/llama3:q4", "org/llama3:q4"},
	} {
		if got := normModel(tt.in); got != tt.want {
			t.Errorf("normModel(%q)=%q want %q", tt.in, got, tt.want)
		}
	}
}

func TestRegistryStateTransitionsAndPurge(t *testing.T) {
	now := time.Unix(100, 0)
	cfg := testConfig()
	cfg.OfflineAfter, cfg.PurgeAfter = 10*time.Second, 20*time.Second
	r := NewRegistry(cfg, func() time.Time { return now })
	if !r.Register("node-a", "http://10.0.0.1:11434") {
		t.Fatal("first registration should create")
	}
	if r.Register("node-a", "http://10.0.0.1:11434") {
		t.Fatal("second registration should update")
	}
	if ok := r.Heartbeat("node-a", 25, 40, true, []string{"llama3"}, map[string]ModelDetail{}); !ok {
		t.Fatal("heartbeat rejected")
	}
	v, ok := r.Get("node-a")
	if !ok || !v.Online || v.Status != "online" || len(v.Models) != 1 {
		t.Fatalf("unexpected node view: %+v", v)
	}
	now = now.Add(11 * time.Second)
	v, ok = r.Get("node-a")
	if !ok || v.Online || v.Status != "offline" {
		t.Fatalf("expected offline state: %+v", v)
	}
	now = now.Add(10 * time.Second)
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("expected purge, got %d nodes", len(got))
	}
}

func TestRegistryRegistrationSourceBinding(t *testing.T) {
	r := NewRegistry(testConfig(), time.Now)
	created, err := r.RegisterFrom("node-a", "http://10.0.0.1:11434", "10.0.0.1")
	if err != nil || !created {
		t.Fatalf("initial register: created=%v err=%v", created, err)
	}
	if _, err := r.RegisterFrom("node-a", "http://10.0.0.2:11434", "10.0.0.2"); err == nil {
		t.Fatal("expected active node takeover to be rejected")
	}
	if ok, err := r.HeartbeatFrom("node-a", "10.0.0.2", 0, 0, true, nil, nil); err == nil || ok {
		t.Fatalf("unexpected heartbeat result ok=%v err=%v", ok, err)
	}
	if ok, err := r.HeartbeatFrom("node-a", "10.0.0.1", 1, 2, true, []string{"m"}, nil); err != nil || !ok {
		t.Fatalf("correct source heartbeat failed: %v %v", ok, err)
	}
}

func TestRegistryRegistrationCanRebindAfterOffline(t *testing.T) {
	now := time.Unix(200, 0)
	cfg := testConfig()
	cfg.OfflineAfter = 5 * time.Second
	r := NewRegistry(cfg, func() time.Time { return now })
	_, _ = r.RegisterFrom("node-a", "http://10.0.0.1:11434", "10.0.0.1")
	now = now.Add(6 * time.Second)
	if _, err := r.RegisterFrom("node-a", "http://10.0.0.2:11434", "10.0.0.2"); err != nil {
		t.Fatalf("offline node should be allowed to rebind: %v", err)
	}
}

func TestCandidatesRoundRobinAndEligibility(t *testing.T) {
	r := NewRegistry(testConfig(), time.Now)
	for _, n := range []string{"a", "b", "c"} {
		r.Register(n, "http://10.0.0.1:11434")
		reachable := n != "c"
		r.Heartbeat(n, 10, 20, reachable, []string{"llama3"}, nil)
	}
	hosting, eligible := r.CandidatesFor("llama3:latest")
	if len(hosting) != 3 || len(eligible) != 2 {
		t.Fatalf("hosting=%d eligible=%d", len(hosting), len(eligible))
	}
	first := eligible[0].Node
	_, next := r.CandidatesFor("llama3")
	if len(next) != 2 || next[0].Node == first {
		t.Fatalf("round-robin did not rotate: first %q next %+v", first, next)
	}
}

func TestRegistrySnapshotSortsAndCopies(t *testing.T) {
	r := NewRegistry(testConfig(), time.Now)
	for _, n := range []string{"z-node", "a-node", "m-node"} {
		r.Register(n, "http://10.0.0.1:11434")
	}
	got := r.Snapshot()
	if got[0].Node != "a-node" || got[1].Node != "m-node" || got[2].Node != "z-node" {
		t.Fatalf("snapshot not sorted: %+v", got)
	}
	got[0].Models = append(got[0].Models, "mutated")
	v, _ := r.Get("a-node")
	if len(v.Models) != 0 {
		t.Fatal("snapshot leaked mutable slice into registry")
	}
}

func TestRegistryConcurrentStress(t *testing.T) {
	r := NewRegistry(testConfig(), time.Now)
	const workers, loops = 80, 120
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			node := "node-" + strings.Repeat("x", w%3+1) + "-" + strings.Repeat("a", w%5+1)
			for i := 0; i < loops; i++ {
				r.Register(node, "http://10.0.0.1:11434")
				r.Heartbeat(node, float64(i%100), float64((i+1)%100), true, []string{"llama3", "qwen"}, nil)
				_ = r.Snapshot()
				_, _ = r.Get(node)
				_, _ = r.CandidatesFor("llama3")
			}
		}()
	}
	wg.Wait()
	if len(r.Snapshot()) == 0 {
		t.Fatal("stress left registry empty")
	}
}

func TestIPAllowedBoundaryCases(t *testing.T) {
	_, network, _ := net.ParseCIDR("10.0.0.0/8")
	if !ipAllowed(net.ParseIP("10.1.2.3"), []*net.IPNet{network}) {
		t.Fatal("expected allowed IP")
	}
	if ipAllowed(net.ParseIP("11.1.2.3"), []*net.IPNet{network}) {
		t.Fatal("unexpectedly allowed out-of-network IP")
	}
	if ipAllowed(nil, []*net.IPNet{network}) {
		t.Fatal("nil IP must not be allowed")
	}
}
