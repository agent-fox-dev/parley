package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

// syncBuffer is a bytes.Buffer safe for the hub's goroutines to write while
// the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// records parses every JSON line of a log.
func records(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range nonEmptyLines(s) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", l, err)
		}
		out = append(out, rec)
	}
	return out
}

// listening returns the listening line's record, or nil.
func listening(t *testing.T, s string) map[string]any {
	t.Helper()
	for _, rec := range records(t, s) {
		if rec["msg"] == "listening" {
			return rec
		}
	}
	return nil
}

// loopbackConfig makes the hub listen on 127.0.0.1 at a port the system
// chooses; extra sections follow it.
const loopbackConfig = "[server]\nbind = \"127.0.0.1\"\nport = 0\n"

// hubDir is a working directory holding config.toml with text.
func hubDir(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runningHub is a hub started in-process by startHub.
type runningHub struct {
	t       *testing.T
	stdout  *syncBuffer
	stderr  *syncBuffer
	code    chan int
	cancel  context.CancelFunc
	addr    string
	rt      atomic.Pointer[Runtime]
	started time.Time
}

func (h *runningHub) url(path string) string { return "http://" + h.addr + path }

// get sends a GET and returns the status and body.
func (h *runningHub) get(path string) (int, string) {
	h.t.Helper()
	resp, err := http.Get(h.url(path))
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

// stop cancels the hub's context and waits for Run's exit status.
func (h *runningHub) stop() int {
	h.t.Helper()
	h.cancel()
	return h.wait(5 * time.Second)
}

func (h *runningHub) wait(within time.Duration) int {
	h.t.Helper()
	select {
	case c := <-h.code:
		return c
	case <-time.After(within):
		h.t.Fatalf("Run did not return within %v; stderr:\n%s", within, h.stderr.String())
		return -1
	}
}

// startHub runs the hub in a goroutine on dir with env and waits for the
// listening line.
func startHub(t *testing.T, dir string, env map[string]string, opts ...Option) *runningHub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &runningHub{t: t, stdout: &syncBuffer{}, stderr: &syncBuffer{}, code: make(chan int, 1), cancel: cancel}
	o := Options{Env: config.MapEnv(env), WorkDir: dir, Stdout: h.stdout, Stderr: h.stderr,
		OnRuntime: func(rt *Runtime) { h.rt.Store(rt) }}
	for _, opt := range opts {
		opt(&o)
	}
	inner := o.OnRuntime
	o.OnRuntime = func(rt *Runtime) {
		h.rt.Store(rt)
		if inner != nil {
			inner(rt)
		}
	}
	go func() { h.code <- Run(ctx, o) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rec := listening(t, h.stderr.String()); rec != nil {
			h.addr, _ = rec["addr"].(string)
			h.started = time.Now()
			t.Cleanup(func() {
				cancel()
				select {
				case <-h.code:
				case <-time.After(5 * time.Second):
				}
			})
			return h
		}
		select {
		case c := <-h.code:
			cancel()
			t.Fatalf("Run returned %d before listening; stderr:\n%s", c, h.stderr.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	t.Fatalf("no listening line within 10 s; stderr:\n%s", h.stderr.String())
	return nil
}

// runHub runs the hub to completion with a context that is not cancelled
// and returns its exit status and output; for runs that end by themselves.
func runHub(t *testing.T, dir string, env map[string]string, args []string, opts ...Option) (int, string, string) {
	t.Helper()
	var stdout, stderr syncBuffer
	o := Options{Args: args, Env: config.MapEnv(env), WorkDir: dir, Stdout: &stdout, Stderr: &stderr}
	for _, opt := range opts {
		opt(&o)
	}
	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), o) }()
	select {
	case code := <-done:
		return code, stdout.String(), stderr.String()
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not return; stderr:\n%s", stderr.String())
		return -1, "", ""
	}
}

// withCompose adds test registrations to the hub's composition.
func withCompose(fn func(*Composition)) Option {
	return func(o *Options) {
		prev := o.Compose
		o.Compose = func(c *Composition) {
			if prev != nil {
				prev(c)
			}
			fn(c)
		}
	}
}
