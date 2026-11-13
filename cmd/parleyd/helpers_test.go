package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
)

// localConfig makes parleyd listen on a port the system chooses, on the
// loopback interface only.
const localConfig = "[server]\nbind = \"127.0.0.1\"\nport = 0\n"

// noEnv is an empty environment.
func noEnv(string) (string, bool) { return "", false }

// workDir returns a new working directory holding config.toml with text.
func workDir(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(text), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	return dir
}

// listDir returns the names in dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// withGrace shortens the time in-flight requests get on a stop.
func withGrace(grace time.Duration) func(*hub.Options) {
	return func(o *hub.Options) { o.RequestGrace = grace }
}

// syncBuffer is a bytes.Buffer that run may write to while the test reads it.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	written chan struct{}
}

func newSyncBuffer() *syncBuffer {
	return &syncBuffer{written: make(chan struct{}, 1)}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	select {
	case b.written <- struct{}{}:
	default:
	}
	return n, err
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lineWith waits up to 5 s for a complete line holding text and returns it.
func (b *syncBuffer) lineWith(t *testing.T, text string) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		s := b.String()
		if i := strings.LastIndexByte(s, '\n'); i >= 0 {
			for _, line := range nonEmptyLines(s[:i]) {
				if strings.Contains(line, text) {
					return line
				}
			}
		}
		select {
		case <-b.written:
		case <-timeout:
			t.Fatalf("no line holding %s on stderr within 5 s; stderr = %q", text, b.String())
		}
	}
}

// nonEmptyLines splits s into its lines, without the empty ones.
func nonEmptyLines(s string) []string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// running is a run started by startRun.
type running struct {
	addr   string
	stderr *syncBuffer
	cancel context.CancelFunc
	// code receives run's exit status, once.
	code chan int
}

// startRun calls run with no arguments in a working directory whose
// configuration listens on 127.0.0.1:0, in a goroutine, waits for its
// listening line and returns the address that line names.
func startRun(t *testing.T, grace time.Duration) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stderr: newSyncBuffer(), cancel: cancel, code: make(chan int, 1)}
	dir := workDir(t, localConfig)
	go func() { r.code <- run(ctx, nil, noEnv, dir, &bytes.Buffer{}, r.stderr, withGrace(grace)) }()
	t.Cleanup(cancel)

	line := r.stderr.lineWith(t, `"msg":"listening"`)
	var rec struct {
		Msg  string `json:"msg"`
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("listening line %q is not a JSON object: %v", line, err)
	}
	r.addr = rec.Addr
	return r
}

// exitCode waits up to within for run to return and gives its status.
func (r *running) exitCode(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case code := <-r.code:
		return code
	case <-time.After(within):
		t.Fatalf("run did not return within %v", within)
		return -1
	}
}

// getJSON sends GET url, decodes its JSON body into v and returns the status.
func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: body is not JSON: %v", url, err)
	}
	return res.StatusCode
}
