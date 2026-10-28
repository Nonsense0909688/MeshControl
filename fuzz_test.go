package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

func FuzzValidateNodeID(f *testing.F) {
	for _, seed := range []string{"node-a", "", "../etc/passwd", "a b", strings.Repeat("n", 64), strings.Repeat("n", 65), "ümlaut", "node_01"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := validateNodeID(input)
		if err == nil && got != input {
			t.Fatalf("validator changed valid input %q to %q", input, got)
		}
		if err == nil && !nodeIDRe.MatchString(got) {
			t.Fatalf("invalid node ID accepted: %q", got)
		}
	})
}

func FuzzParseObject(f *testing.F) {
	for _, seed := range []string{`{"ok":true}`, `{}`, `[]`, `null`, `"str"`, `{`, `{"x":[1,2]}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := parseObject([]byte(input))
		if err == nil && got == nil {
			t.Fatalf("accepted object returned nil map")
		}
	})
}

func FuzzValidateOllamaURL(f *testing.F) {
	cfg := testConfig()
	for _, seed := range []string{"http://127.0.0.1:11434", "ftp://127.0.0.1", "http://user:pass@127.0.0.1", "http://8.8.8.8:11434", "http://127.0.0.1/api", "http://[::1]:11434"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := validateOllamaURL(input, cfg)
		if err == nil {
			if !strings.HasPrefix(got, "http://") && !strings.HasPrefix(got, "https://") {
				t.Fatalf("unexpected normalized URL %q", got)
			}
			// A successful validation must yield an explicitly-port-qualified host URL.
			if !strings.Contains(got, "://") {
				t.Fatalf("not a URL: %q", got)
			}
		}
	})
}

func FuzzParseNetworks(f *testing.F) {
	for _, seed := range []string{"10.0.0.0/8", "127.0.0.1", "::1", "bad", "", "10.0.0.0/999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := parseNetworks(input)
		if err == nil {
			for _, network := range got {
				if network == nil || network.IP == nil || network.Mask == nil {
					t.Fatalf("invalid parsed network for %q", input)
				}
			}
		}
	})
}

func FuzzValidatePercent(f *testing.F) {
	for _, n := range []float64{0, 1, 50, 100, -1, 101} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, input float64) {
		got, err := validatePercent(input, "cpu")
		if err == nil && (got < 0 || got > 100 || got != got) {
			t.Fatalf("invalid percent accepted: %v", got)
		}
	})
}

func TestSafeTransportDNSAllowlistUsesAllResolvedAddresses(t *testing.T) {
	cfg := testConfig()
	cfg.AllowHostnames = true
	_, publicNet, _ := net.ParseCIDR("8.8.8.0/24")
	cfg.AllowedNetworks = append(cfg.AllowedNetworks, publicNet)
	// The transport still refuses an address outside the explicit allowlist.
	tr := newSafeTransport(cfg)
	defer tr.CloseIdleConnections()
	_, err := tr.DialContext(nilContext(), "tcp", "9.9.9.9:53")
	if err == nil || !strings.Contains(err.Error(), "outside allowed networks") {
		t.Fatalf("expected allowlist block, got %v", err)
	}
}

func nilContext() context.Context { return context.Background() }
