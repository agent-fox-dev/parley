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
)

// localConfig makes parleyd listen on a port the system chooses, on the
// loopback interface only.
const localConfig = "[server]\nbind = \"127.0.0.1\"\nport = 0\n"

// noEnv is an empty environment.
func noEnv(string) (string, bool) { return "", false }

// writeFile writes text to the file at path.
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// workDir returns a new working directory holding config.toml with text.
func workDir(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.toml"), text)
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
	addr string
	// listening is the run's listening line.
	listening map[string]any
	cancel    context.CancelFunc
	// code receives run's exit status, once.
	code chan int
}

// startRun calls run with no arguments, the environment env and the working
// directory dir, in a goroutine, waits for its listening line and returns
// the address that line names.
func startRun(t *testing.T, env func(string) (string, bool), dir string) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, code: make(chan int, 1)}
	stderr := newSyncBuffer()
	go func() { r.code <- run(ctx, nil, env, dir, &bytes.Buffer{}, stderr) }()
	t.Cleanup(cancel)

	line := stderr.lineWith(t, `"msg":"listening"`)
	if err := json.Unmarshal([]byte(line), &r.listening); err != nil {
		t.Fatalf("listening line %q is not a JSON object: %v", line, err)
	}
	r.addr, _ = r.listening["addr"].(string)
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
