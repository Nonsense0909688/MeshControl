package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func BenchmarkRegistrySnapshot(b *testing.B) {
	r := NewRegistry(testConfig(), time.Now)
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("node-%04d", i)
		r.Register(name, "http://10.0.0.1:11434")
		r.Heartbeat(name, float64(i%100), float64((i+1)%100), true, []string{"llama3", "qwen2.5:3b"}, nil)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Snapshot()
	}
}

func BenchmarkParseObject(b *testing.B) {
	body := []byte(`{"model":"llama3","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseObject(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHealthHandler(b *testing.B) {
	_, network, _ := net.ParseCIDR("127.0.0.0/8")
	cfg := testConfig()
	cfg.AllowedNetworks = []*net.IPNet{network}
	s := NewServer(cfg, "web")
	defer s.client.CloseIdleConnections()
	h := s.Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status=%d", rec.Code)
		}
	}
}
