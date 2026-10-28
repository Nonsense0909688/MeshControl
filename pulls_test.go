package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadPullLineTable(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		max         int
		want        string
		bad         bool
	}{
		{"newline", "hello\n", 10, "hello\n", false}, {"eof final line", "last", 10, "last", false},
		{"empty line", "\n", 10, "\n", false}, {"at limit", "12345\n", 6, "12345\n", false},
		{"over limit", strings.Repeat("x", 100), 10, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readPullLine(bufio.NewReaderSize(strings.NewReader(tt.input), 4), tt.max)
			if (err != nil) != tt.bad {
				t.Fatalf("err=%v bad=%v", err, tt.bad)
			}
			if !tt.bad && string(got) != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestPullManagerLimitsDuplicatesAndCancellation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	cfg := testConfig()
	port, _ := strconv.Atoi(strings.Split(upstream.URL[strings.LastIndex(upstream.URL, ":")+1:], "/")[0])
	cfg.AllowedNodePorts[port] = true
	cfg.MaxConcurrentPulls = 2
	cfg.MaxConcurrentPullsPerNode = 1
	pm := NewPullManager(cfg, time.Now)
	first, ok, reason := pm.StartDetailed("node-a", upstream.URL, "llama3")
	if !ok || reason != "" || first.ID == "" {
		t.Fatalf("first start: ok=%v reason=%q job=%+v", ok, reason, first)
	}
	if _, ok, reason := pm.StartDetailed("node-a", upstream.URL, "llama3:latest"); ok || reason != "duplicate" {
		t.Fatalf("duplicate accepted: ok=%v reason=%q", ok, reason)
	}
	if _, ok, reason := pm.StartDetailed("node-a", upstream.URL, "qwen"); ok || reason != "node_limit" {
		t.Fatalf("per-node cap failed: ok=%v reason=%q", ok, reason)
	}
	second, ok, reason := pm.StartDetailed("node-b", upstream.URL, "qwen")
	if !ok || reason != "" {
		t.Fatalf("second node start: ok=%v reason=%q", ok, reason)
	}
	if _, ok, reason := pm.StartDetailed("node-c", upstream.URL, "phi"); ok || reason != "global_limit" {
		t.Fatalf("global cap failed: ok=%v reason=%q", ok, reason)
	}
	if state := pm.Cancel(first.ID); state != "ok" {
		t.Fatalf("cancel status=%s", state)
	}
	if state := pm.Cancel(first.ID); state != "ok" && state != "finished" {
		t.Fatalf("second cancel status=%s", state)
	}
	if state := pm.Cancel("missing"); state != "missing" {
		t.Fatalf("missing cancel status=%s", state)
	}
	if state := pm.Cancel(second.ID); state != "ok" {
		t.Fatalf("second cancel status=%s", state)
	}
	waitForPullsToStop(t, pm)
	if state := pm.Cancel(first.ID); state != "finished" {
		t.Fatalf("finished cancel status=%s", state)
	}
}

func waitForPullsToStop(t *testing.T, pm *PullManager) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allDone := true
		for _, job := range pm.Snapshot() {
			if job.State == "running" {
				allDone = false
				break
			}
		}
		if allDone {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pull jobs did not stop in time")
}

func TestPullManagerRejectsNon200AndReportsSuccess(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"upstream error", 500, `{"error":"no space"}`, "error"},
		{"success frame", 200, `{"status":"success"}` + "\n", "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer upstream.Close()
			config := testConfig()
			port, _ := strconv.Atoi(strings.Split(upstream.URL[strings.LastIndex(upstream.URL, ":")+1:], "/")[0])
			config.AllowedNodePorts[port] = true
			pm := NewPullManager(config, time.Now)
			job, ok, reason := pm.StartDetailed("node-a", upstream.URL, "llama3")
			if !ok || reason != "" {
				t.Fatalf("start failed: %v %s", ok, reason)
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				for _, got := range pm.Snapshot() {
					if got.ID == job.ID && got.State != "running" {
						if got.State != tt.want {
							t.Fatalf("state=%q want=%q error=%v", got.State, tt.want, got.Error)
						}
						if tt.want == "error" && (got.Error == nil || !strings.Contains(*got.Error, "no space")) {
							t.Fatalf("missing upstream error: %+v", got)
						}
						return
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Fatal("pull did not finish")
		})
	}
}

func TestPullErrorTextIsBoundedAndFallbacks(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"json error", `{"error":"failed"}`, 500, "failed"}, {"non-json", "bad", 503, "node returned HTTP 503"},
		{"empty json", `{}`, 400, "node returned HTTP 400"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.status, Body: ioNopCloser(strings.NewReader(tt.body))}
			got := pullErrorText(resp)
			if got != tt.want {
				t.Fatalf("got=%q want=%q", got, tt.want)
			}
		})
	}
}

func ioNopCloser(r *strings.Reader) io.ReadCloser { return io.NopCloser(r) }

func TestPullManagerCancelAndSnapshotConcurrency(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	cfg := testConfig()
	port, _ := strconv.Atoi(strings.Split(upstream.URL[strings.LastIndex(upstream.URL, ":")+1:], "/")[0])
	cfg.AllowedNodePorts[port] = true
	pm := NewPullManager(cfg, time.Now)
	job, ok := pm.Start("node-a", upstream.URL, "llama3")
	if !ok {
		t.Fatal("pull did not start")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			_ = pm.Snapshot()
			_ = pm.wasCancelled(job.ID)
		}
	}()
	if state := pm.Cancel(job.ID); state != "ok" {
		t.Fatalf("cancel=%s", state)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("snapshot stress stalled")
	}
	waitForPullsToStop(t, pm)
}

func TestPullManagerPrunesCompletedHistoryButKeepsRunningJobs(t *testing.T) {
	pm := NewPullManager(testConfig(), time.Now)
	now := time.Now()
	pm.mu.Lock()
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("done-%02d", i)
		finished := epoch(now.Add(time.Duration(i) * time.Second))
		pm.jobs[id] = &pullEntry{job: PullJob{ID: id, Node: "node-a", Model: "m", State: "success", StartedAt: finished, FinishedAt: &finished}}
	}
	pm.jobs["running"] = &pullEntry{job: PullJob{ID: "running", Node: "node-b", Model: "large", State: "running", StartedAt: epoch(now)}}
	pm.jobs["active"] = &pullEntry{job: PullJob{ID: "active", Node: "node-c", Model: "waiting", State: "running", StartedAt: epoch(now)}}
	pm.mu.Unlock()
	pm.finish("running", "error", "test completion")
	jobs := pm.Snapshot()
	finishedCount, runningFound := 0, false
	for _, job := range jobs {
		if job.State == "running" {
			runningFound = true
		} else {
			finishedCount++
		}
	}
	if !runningFound {
		t.Fatal("running/newly-finished job should remain available")
	}
	if finishedCount != maxPullJobs {
		t.Fatalf("finished history count=%d want=%d", finishedCount, maxPullJobs)
	}
}
