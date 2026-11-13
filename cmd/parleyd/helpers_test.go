package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

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

// firstLine waits up to 5 s for a complete first line and returns it.
func (b *syncBuffer) firstLine(t *testing.T) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		if line, _, ok := strings.Cut(b.String(), "\n"); ok {
			return line
		}
		select {
		case <-b.written:
		case <-timeout:
			t.Fatalf("no complete line on stderr within 5 s; stderr = %q", b.String())
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

// startRun calls run with no arguments on 127.0.0.1:0 in a goroutine, waits
// for its listening line and returns the address that line names.
func startRun(t *testing.T, grace time.Duration) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stderr: newSyncBuffer(), cancel: cancel, code: make(chan int, 1)}
	go func() { r.code <- run(ctx, nil, "127.0.0.1:0", grace, &bytes.Buffer{}, r.stderr) }()
	t.Cleanup(cancel)

	line := r.stderr.firstLine(t)
	var rec struct {
		Msg  string `json:"msg"`
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("first stderr line %q is not a JSON object: %v", line, err)
	}
	if rec.Msg != "listening" {
		t.Fatalf("first stderr line %q has msg %q, want %q", line, rec.Msg, "listening")
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
