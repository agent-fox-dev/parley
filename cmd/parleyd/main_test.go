package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/config"
	phub "github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/server"
)

var noEnv = config.MapEnv(nil)

// configDir is a working directory whose config.toml makes the hub listen
// on 127.0.0.1 at port.
func configDir(t *testing.T, port int) string {
	t.Helper()
	dir := t.TempDir()
	text := fmt.Sprintf("[server]\nbind = \"127.0.0.1\"\nport = %d\n", port)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// withGrace sets the hub's request grace.
func withGrace(d time.Duration) phub.Option {
	return func(o *phub.Options) { o.RequestGrace = d }
}

// withMounts registers extra routes on the hub's server.
func withMounts(mounts ...func(*server.Server)) phub.Option {
	return func(o *phub.Options) {
		o.Compose = func(c *phub.Composition) {
			c.OnServer(func(_ *phub.Runtime, srv *server.Server) error {
				for _, m := range mounts {
					m(srv)
				}
				return nil
			})
		}
	}
}

// listDir lists a directory's entries.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// syncBuffer is a bytes.Buffer safe for the run goroutine to write while the
// test reads it.
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

type runningHub struct {
	addr   string
	stderr *syncBuffer
	code   <-chan int
	cancel context.CancelFunc
}

// startHub calls run in a goroutine with no arguments, configured for
// 127.0.0.1:0, and waits for its listening line.
func startHub(t *testing.T, grace time.Duration, mounts ...func(*server.Server)) runningHub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &syncBuffer{}
	code := make(chan int, 1)
	dir := configDir(t, 0)
	go func() { code <- run(ctx, nil, noEnv, dir, io.Discard, stderr, withGrace(grace), withMounts(mounts...)) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if line, ok := firstLine(stderr.String()); ok {
			var rec struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				cancel()
				t.Fatalf("first stderr line %q is not JSON: %v", line, err)
			}
			if rec.Msg != "listening" {
				cancel()
				t.Fatalf("first stderr line msg = %q, want listening", rec.Msg)
			}
			t.Cleanup(cancel)
			return runningHub{addr: rec.Addr, stderr: stderr, code: code, cancel: cancel}
		}
		select {
		case c := <-code:
			cancel()
			t.Fatalf("run returned %d before listening; stderr: %s", c, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("no listening line on stderr within 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func firstLine(s string) (string, bool) {
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return "", false
	}
	return s[:i], true
}

func nonEmptyLines(s string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines = append(lines, sc.Text())
		}
	}
	return lines
}

func waitCode(t *testing.T, code <-chan int, within time.Duration) int {
	t.Helper()
	select {
	case c := <-code:
		return c
	case <-time.After(within):
		t.Fatalf("run did not return within %v", within)
		return -1
	}
}

func TestRun_ListensAndLogsTheBoundAddress(t *testing.T) {
	hub := startHub(t, time.Second)
	host, port, err := net.SplitHostPort(hub.addr)
	if err != nil {
		t.Fatalf("addr %q is not host:port: %v", hub.addr, err)
	}
	if host != "127.0.0.1" || port == "0" {
		t.Fatalf("addr = %q, want 127.0.0.1 with the chosen port", hub.addr)
	}
	resp, err := http.Get("http://" + hub.addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	hub.cancel()
	if c := waitCode(t, hub.code, 2*time.Second); c != 0 {
		t.Fatalf("run returned %d, want 0", c)
	}
}

func TestRun_ExitsOneWithOneLineWhenTheAddressCannotBeBound(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	addr := held.Addr().String()

	var stdout, stderr bytes.Buffer
	port := held.Addr().(*net.TCPAddr).Port
	code := run(context.Background(), nil, noEnv, configDir(t, port), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run returned %d, want 1", code)
	}
	lines := nonEmptyLines(stderr.String())
	if len(lines) != 1 {
		t.Fatalf("stderr holds %d lines, want 1: %q", len(lines), stderr.String())
	}
	if !strings.Contains(lines[0], addr) {
		t.Errorf("error line %q does not name %s", lines[0], addr)
	}
	if strings.Contains(stderr.String(), "listening") {
		t.Errorf("stderr %q holds a listening line", stderr.String())
	}
}

func TestRun_HelpPrintsUsageOnStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--help"}, noEnv, t.TempDir(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run returned %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "parleyd") {
		t.Errorf("stdout %q does not name parleyd", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun_RejectsUnknownFlagsAndPositionalArguments(t *testing.T) {
	tests := []struct {
		args     []string
		rejected string
	}{
		{[]string{"--nope"}, "nope"},
		{[]string{"-x"}, "x"},
		{[]string{"serve"}, "serve"},
		{[]string{"serve", "dir"}, "serve"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, noEnv, t.TempDir(), &stdout, &stderr)
			if code != 2 {
				t.Fatalf("run returned %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "parleyd") {
				t.Errorf("stderr %q holds no usage naming parleyd", stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.rejected) {
				t.Errorf("stderr %q does not name the rejected argument %q", stderr.String(), tt.rejected)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if strings.Contains(stderr.String(), "listening") {
				t.Errorf("stderr %q holds a listening line", stderr.String())
			}
		})
	}
}

func TestRun_StopsOnSignalAndExitsZero(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			hub := startHub(t, time.Second)
			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			if c := waitCode(t, hub.code, time.Second); c != 0 {
				t.Fatalf("run returned %d after %v, want 0", c, sig)
			}
			if c, err := net.Dial("tcp", hub.addr); err == nil {
				c.Close()
				t.Fatalf("a connection to %s was accepted after run returned", hub.addr)
			}
		})
	}
}

func TestRun_ExitsZeroWhenTheGracePassesWithARequestInFlight(t *testing.T) {
	if phub.DefaultRequestGrace != 15*time.Second {
		t.Errorf("default shutdown grace = %v, want 15 s", phub.DefaultRequestGrace)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	hub := startHub(t, 100*time.Millisecond, func(s *server.Server) {
		s.HandleFunc("GET /stuck", func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
		})
	})
	go func() {
		if resp, err := http.Get("http://" + hub.addr + "/stuck"); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if c := waitCode(t, hub.code, 1100*time.Millisecond); c != 0 {
		t.Fatalf("run returned %d after the grace passed, want 0", c)
	}
}

func TestHub_StartReadVersionAndStop(t *testing.T) {
	t.Setenv("PARLEY_ENDPOINT_URL", "")
	hub := startHub(t, time.Second)
	base := "http://" + hub.addr

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}

	resp, err := http.Get(base + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	var want map[string]any
	err = json.NewDecoder(resp.Body).Decode(&want)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("GET /version body: %v", err)
	}

	var stdout, cliStderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"version", "--endpoint-url", base}, &stdout, &cliStderr); code != 0 {
		t.Fatalf("parley version returned %d; stderr: %s", code, cliStderr.String())
	}
	var doc struct {
		Server map[string]any `json:"server"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("parley version printed %q: %v", stdout.String(), err)
	}
	if !reflect.DeepEqual(doc.Server, want) {
		t.Errorf("server = %v, want the body GET /version returned: %v", doc.Server, want)
	}
	if doc.Server["go_version"] != runtime.Version() {
		t.Errorf("server.go_version = %v, want %s", doc.Server["go_version"], runtime.Version())
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if c := waitCode(t, hub.code, time.Second); c != 0 {
		t.Fatalf("run returned %d after SIGTERM, want 0", c)
	}
	if c, err := net.Dial("tcp", hub.addr); err == nil {
		c.Close()
		t.Fatalf("a connection to %s was accepted after the hub stopped", hub.addr)
	}
}

func TestRun_RefusesFlagsAndSubcommandsItDoesNotHave(t *testing.T) {
	for _, args := range [][]string{{"--nope"}, {"-x"}, {"serve"}, {"--admin-email"}, {"backup"}, {"backup", "a", "b"}, {"backup", "a", "--nope"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := t.TempDir()
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, noEnv, dir, &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			for _, want := range []string{"--admin-email", "--reset-admin-token", "backup <directory>"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("usage on stderr lacks %q", want)
				}
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q", stdout.String())
			}
			if names := listDir(t, dir); len(names) != 0 {
				t.Errorf("created %v", names)
			}
		})
	}
}

func TestRun_HelpNamesTheFlagsAndTheBackupCommand(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, noEnv, dir, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	for _, want := range []string{"--admin-email", "--reset-admin-token", "backup <directory>"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("usage on stdout lacks %q", want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
	if names := listDir(t, dir); len(names) != 0 {
		t.Errorf("created %v", names)
	}
}
