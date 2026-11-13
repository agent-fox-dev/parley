package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
)

// localConfig makes a hub listen on a port the system chooses, on the
// loopback interface only.
const localConfig = "[server]\nbind = \"127.0.0.1\"\nport = 0\n"

// syncBuffer is a bytes.Buffer that a hub may write to while the test reads
// it.
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

// records decodes every complete line written so far as a JSON object.
func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	text := b.String()
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	} else {
		text = ""
	}
	var recs []map[string]any
	for _, line := range nonEmptyLines(text) {
		recs = append(recs, decodeLine(t, line))
	}
	return recs
}

// waitForMsg waits up to 5 s for a line whose msg is msg and returns it.
func (b *syncBuffer) waitForMsg(t *testing.T, msg string) map[string]any {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		for _, rec := range b.records(t) {
			if rec["msg"] == msg {
				return rec
			}
		}
		select {
		case <-b.written:
		case <-timeout:
			t.Fatalf("no line with msg %q within 5 s; stderr = %q", msg, b.String())
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

// decodeLine parses one log line as a JSON object.
func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("line %q is not a JSON object: %v", line, err)
	}
	return rec
}

// writeFile writes text to path, creating its directory.
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
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

// freePort returns a TCP port on 127.0.0.1 that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// running is a hub started by startHub or runHub.
type running struct {
	stderr *syncBuffer
	cancel context.CancelFunc
	// code receives Run's exit status, once.
	code chan int
	// addr is the listening line's addr, once startHub has returned.
	addr string
	// listening is the listening line, once startHub has returned.
	listening map[string]any
}

// runHub calls hub.Run with opts in a goroutine, with stderr captured, and
// does not wait for it to listen.
func runHub(t *testing.T, opts hub.Options) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stderr: newSyncBuffer(), cancel: cancel, code: make(chan int, 1)}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	opts.Stderr = r.stderr
	go func() { r.code <- hub.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.code:
		case <-time.After(5 * time.Second):
			t.Errorf("hub did not stop within 5 s of the test's end")
		}
	})
	return r
}

// startHub runs a hub with opts and waits for its listening line.
func startHub(t *testing.T, opts hub.Options) *running {
	t.Helper()
	r := runHub(t, opts)
	r.listening = r.stderr.waitForMsg(t, "listening")
	r.addr, _ = r.listening["addr"].(string)
	return r
}

// stop cancels the hub's context and returns its exit status.
func (r *running) stop(t *testing.T) int {
	t.Helper()
	r.cancel()
	return r.wait(t, 5*time.Second)
}

// wait returns Run's exit status, failing the test when Run has not
// returned within the given time.
func (r *running) wait(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case code := <-r.code:
		r.code <- code // the cleanup reads it again
		return code
	case <-time.After(within):
		t.Fatalf("Run did not return within %v", within)
		return -1
	}
}

// url is the hub's URL for path.
func (r *running) url(path string) string {
	return "http://" + r.addr + path
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

// callLog records calls in order, from any goroutine.
type callLog struct {
	mu    sync.Mutex
	names []string
}

func (l *callLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

func (l *callLog) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.names)
}

// eventually polls cond every few milliseconds for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5 s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// itoa formats a port for a configuration file.
func itoa(n int) string { return strconv.Itoa(n) }
