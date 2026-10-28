package main

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envBoolChecked(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid %s: expected true/false", key)
	}
}

func envFloat(key string, def float64) (float64, error) {
	f, err := strconv.ParseFloat(envOr(key, strconv.FormatFloat(def, 'f', -1, 64)), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return f, nil
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// str returns m[key] if it is a string, else "". Safe on nil maps.
func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// errName returns just the error's type, so tokens/URLs never end up in logs.
func errName(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return fmt.Sprintf("%T", ue.Err)
	}
	return fmt.Sprintf("%T", err)
}

// idleTimer cancels a request if nothing happens for d. touch() on every chunk
// gives "read timeout between bytes" semantics, like Python requests.
type idleTimer struct {
	t     *time.Timer
	d     time.Duration
	fired atomic.Bool
}

func newIdle(d time.Duration, cancel func()) *idleTimer {
	it := &idleTimer{d: d}
	it.t = time.AfterFunc(d, func() { it.fired.Store(true); cancel() })
	return it
}
func (i *idleTimer) touch() { i.t.Reset(i.d) }
func (i *idleTimer) stop()  { i.t.Stop() }
