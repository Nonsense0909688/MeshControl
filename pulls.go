package main

// Model pulls run as background jobs, so a pull survives page navigation.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

const maxPullJobs = 50

type PullJob struct {
	ID         string   `json:"id"`
	Node       string   `json:"node"`
	Model      string   `json:"model"`
	State      string   `json:"state"` // running | success | error | cancelled
	Status     string   `json:"status"`
	Completed  int64    `json:"completed"`
	Total      int64    `json:"total"`
	Error      *string  `json:"error"`
	StartedAt  float64  `json:"started_at"`
	FinishedAt *float64 `json:"finished_at"`
}

type pullEntry struct {
	job       PullJob
	cancel    context.CancelFunc
	cancelled bool
}

type pullError struct{ msg string }

func (e *pullError) Error() string { return e.msg }

// PullManager runs "ollama pull" on a node as a background job and tracks
// progress. The controller streams Ollama's progress frames into a small
// in-memory job record; the UI polls it.
type PullManager struct {
	cfg    ControllerConfig
	clock  func() time.Time
	client *http.Client
	mu     sync.Mutex
	jobs   map[string]*pullEntry
}

func NewPullManager(cfg ControllerConfig, clock func() time.Time) *PullManager {
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: cfg.ConnectTimeout}).DialContext}
	return &PullManager{cfg: cfg, clock: clock, client: &http.Client{Transport: tr}, jobs: map[string]*pullEntry{}}
}

// Start begins a pull. ok is false if the same pull is already running.
func (pm *PullManager) Start(node, url, model string) (PullJob, bool) {
	pm.mu.Lock()
	for _, e := range pm.jobs {
		if e.job.State == "running" && e.job.Node == node && normModel(e.job.Model) == normModel(model) {
			pm.mu.Unlock()
			return PullJob{}, false
		}
	}
	idb := make([]byte, 6)
	_, _ = rand.Read(idb)
	jid := hex.EncodeToString(idb)
	ctx, cancel := context.WithCancel(context.Background())
	e := &pullEntry{job: PullJob{
		ID: jid, Node: node, Model: model, State: "running", Status: "starting",
		StartedAt: epoch(pm.clock()),
	}, cancel: cancel}
	pm.jobs[jid] = e
	snap := e.job
	pm.mu.Unlock()
	go pm.run(ctx, cancel, jid, url, model)
	return snap, true
}

func (pm *PullManager) Snapshot() []PullJob {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make([]PullJob, 0, len(pm.jobs))
	for _, e := range pm.jobs {
		out = append(out, e.job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// Cancel returns "ok", "missing" or "finished".
func (pm *PullManager) Cancel(jid string) string {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	e := pm.jobs[jid]
	if e == nil {
		return "missing"
	}
	if e.job.State != "running" {
		return "finished"
	}
	e.cancelled = true
	e.cancel() // aborts the in-flight request and unblocks the worker's read
	return "ok"
}

func (pm *PullManager) wasCancelled(jid string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	e := pm.jobs[jid]
	return e != nil && e.cancelled
}

func (pm *PullManager) update(jid string, fn func(*PullJob)) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if e := pm.jobs[jid]; e != nil {
		fn(&e.job)
	}
}

func (pm *PullManager) finish(jid, state, errMsg string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	e := pm.jobs[jid]
	if e == nil {
		return
	}
	e.job.State = state
	if errMsg != "" {
		e.job.Error = &errMsg
	}
	now := epoch(pm.clock())
	e.job.FinishedAt = &now
	if state == "success" {
		e.job.Status = "success"
	}
	var done []*pullEntry
	for _, x := range pm.jobs {
		if x.job.State != "running" {
			done = append(done, x)
		}
	}
	if extra := len(done) - maxPullJobs; extra > 0 {
		sort.Slice(done, func(i, j int) bool { return *done[i].job.FinishedAt < *done[j].job.FinishedAt })
		for _, old := range done[:extra] {
			delete(pm.jobs, old.job.ID)
		}
	}
}

func (pm *PullManager) run(ctx context.Context, cancel context.CancelFunc, jid, url, model string) {
	defer cancel()
	idle := newIdle(pm.cfg.ReadTimeout, cancel)
	defer idle.stop()
	succeeded, err := pm.stream(ctx, idle, jid, url, model)
	var pe *pullError
	switch {
	case pm.wasCancelled(jid):
		// Cancelling aborts the stream, which surfaces as assorted read errors; expected.
		pm.finish(jid, "cancelled", "")
	case errors.As(err, &pe):
		pm.finish(jid, "error", pe.msg)
	case err != nil:
		log.Warn("pull failed", "model", model, "error", errName(err))
		pm.finish(jid, "error", "lost connection to the node")
	case succeeded:
		pm.finish(jid, "success", "")
	default:
		pm.finish(jid, "error", "the node ended the download before it completed")
	}
}

func pullErrorText(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var v struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(b, &v) == nil {
		if s, ok := v.Error.(string); ok && s != "" {
			return truncRunes(s, 300)
		}
	}
	return fmt.Sprintf("node returned HTTP %d", resp.StatusCode)
}

func (pm *PullManager) stream(ctx context.Context, idle *idleTimer, jid, url, model string) (bool, error) {
	// The body is built here, never forwarded from the client.
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := pm.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, &pullError{pullErrorText(resp)}
	}

	layers := map[string][2]int64{} // digest -> {total, completed}
	succeeded := false
	rd := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, rerr := rd.ReadBytes('\n')
		idle.touch()
		if len(bytes.TrimSpace(line)) > 0 {
			var frame map[string]any
			if json.Unmarshal(line, &frame) == nil && frame != nil {
				if msg := str(frame, "error"); msg != "" {
					return succeeded, &pullError{truncRunes(msg, 300)}
				}
				status := truncRunes(str(frame, "status"), 120)
				digest := str(frame, "digest")
				total, _ := frame["total"].(float64)
				done, _ := frame["completed"].(float64)
				if digest != "" && total > 0 && total == math.Trunc(total) {
					c := int64(0)
					if done >= 0 && done == math.Trunc(done) {
						c = int64(done)
					}
					layers[digest] = [2]int64{int64(total), c}
				}
				var sumDone, sumTotal int64
				for _, l := range layers {
					sumTotal += l[0]
					sumDone += l[1]
				}
				pm.update(jid, func(j *PullJob) {
					j.Completed, j.Total = sumDone, sumTotal
					if status != "" {
						j.Status = status
					}
				})
				if status == "success" {
					succeeded = true
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return succeeded, nil
			}
			return succeeded, rerr
		}
	}
}
