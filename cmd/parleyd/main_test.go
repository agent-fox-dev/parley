package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/cli"
)

func TestRun_ListensAndLogsTheBoundAddress(t *testing.T) {
	t.Parallel()
	r := startRun(t, time.Second)

	host, port, err := net.SplitHostPort(r.addr)
	if err != nil {
		t.Fatalf("addr %q is not host:port: %v", r.addr, err)
	}
	if host != "127.0.0.1" || port == "0" {
		t.Errorf("addr = %q, want 127.0.0.1 with the port the system chose", r.addr)
	}
	res, err := http.Get("http://" + r.addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", res.StatusCode, http.StatusOK)
	}

	r.cancel()
	if code := r.exitCode(t, 5*time.Second); code != 0 {
		t.Errorf("run returned %d after its context was cancelled, want 0", code)
	}
}

func TestRun_ExitsWithOneErrorLineWhenItCannotListen(t *testing.T) {
	t.Parallel()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()
	addr := held.Addr().String()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, addr, time.Second, &stdout, &stderr)

	if code != 1 {
		t.Errorf("run returned %d, want 1", code)
	}
	lines := nonEmptyLines(stderr.String())
	if len(lines) != 1 {
		t.Fatalf("stderr holds %d lines, want 1: %q", len(lines), stderr.String())
	}
	if !strings.Contains(lines[0], addr) {
		t.Errorf("error line %q does not name %s", lines[0], addr)
	}
	if strings.Contains(stderr.String(), "listening") {
		t.Errorf("stderr %q has a listening line", stderr.String())
	}
}

func TestRun_PrintsUsageOnStdoutForHelp(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			// run returns without serving, or the test would hang here.
			code := run(context.Background(), []string{arg}, "127.0.0.1:0", time.Second, &stdout, &stderr)

			if code != 0 {
				t.Errorf("run returned %d, want 0", code)
			}
			if !strings.Contains(stdout.String(), "parleyd") {
				t.Errorf("stdout %q does not name parleyd", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRun_RefusesUnknownFlagsAndArgumentsWithUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--nope"}, {"-x"}, {"serve"}, {"serve", "dir"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, "127.0.0.1:0", time.Second, &stdout, &stderr)

			if code != 2 {
				t.Errorf("run returned %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "Usage: parleyd") {
				t.Errorf("stderr %q does not hold the usage", stderr.String())
			}
			if !strings.Contains(stderr.String(), args[0]) {
				t.Errorf("stderr %q does not name the rejected argument %q", stderr.String(), args[0])
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if strings.Contains(stderr.String(), "listening") {
				t.Errorf("stderr %q has a listening line", stderr.String())
			}
		})
	}
}

// The test signals its own process, which every running run would receive,
// so it must not run in parallel. Each signal is sent only after the
// listening line, when run has registered for it; before that the signal
// would end the test binary.
func TestRun_StopsOnASignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			r := startRun(t, time.Second)

			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatalf("send %v: %v", sig, err)
			}
			if code := r.exitCode(t, time.Second); code != 0 {
				t.Errorf("run returned %d, want 0", code)
			}
			if conn, err := net.Dial("tcp", r.addr); err == nil {
				conn.Close()
				t.Errorf("a connection to %s was accepted after run returned", r.addr)
			}
		})
	}
}

// The test signals its own process, so it must not run in parallel; see
// TestRun_StopsOnASignal.
func TestRun_ExitsZeroWhenTheGraceExpires(t *testing.T) {
	// main passes this grace; the test shortens it to keep the wait short.
	if defaultShutdownGrace != 15*time.Second {
		t.Errorf("main's shutdown grace = %v, want 15s", defaultShutdownGrace)
	}
	const grace = 50 * time.Millisecond
	r := startRun(t, grace)

	// A request whose header never ends stays in flight until the grace has
	// passed and the server closes its connection.
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatalf("dial %s: %v", r.addr, err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: parleyd\r\n"); err != nil {
		t.Fatalf("write a partial request: %v", err)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	if code := r.exitCode(t, grace+time.Second); code != 0 {
		t.Errorf("run returned %d, want 0", code)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if got, err := io.ReadAll(conn); len(got) != 0 || err != nil {
		t.Errorf("unfinished request read %q (error %v), want its connection closed with no response", got, err)
	}
}

// An operator starts the hub, a developer reads its version with the real
// client, and the operator stops the hub with SIGTERM. The test signals its
// own process, so it must not run in parallel; see TestRun_StopsOnASignal.
func TestRun_ServesTheClientUntilStopped(t *testing.T) {
	t.Setenv("PARLEY_ENDPOINT_URL", "")
	os.Unsetenv("PARLEY_ENDPOINT_URL")
	r := startRun(t, time.Second)
	hub := "http://" + r.addr

	for path, want := range map[string]string{"/healthz": "ok", "/readyz": "ready"} {
		var doc struct{ Status string }
		if status := getJSON(t, hub+path, &doc); status != http.StatusOK || doc.Status != want {
			t.Errorf("GET %s = %d %q, want 200 %q", path, status, doc.Status, want)
		}
	}
	var served map[string]any
	if status := getJSON(t, hub+"/version", &served); status != http.StatusOK {
		t.Fatalf("GET /version status = %d, want 200", status)
	}

	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"version", "--endpoint-url", hub}, &stdout, &stderr); code != 0 {
		t.Errorf("parley version returned %d, want 0; stderr = %q", code, stderr.String())
	}
	var printed struct{ Server map[string]any }
	if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil {
		t.Fatalf("parley version printed %q, not a JSON document: %v", stdout.String(), err)
	}
	if !reflect.DeepEqual(printed.Server, served) {
		t.Errorf("parley version printed server %v, want the hub's answer %v", printed.Server, served)
	}
	if printed.Server["go_version"] != runtime.Version() {
		t.Errorf("server go_version = %v, want %s", printed.Server["go_version"], runtime.Version())
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	if code := r.exitCode(t, time.Second); code != 0 {
		t.Errorf("run returned %d after SIGTERM, want 0", code)
	}
	if conn, err := net.Dial("tcp", r.addr); err == nil {
		conn.Close()
		t.Errorf("a connection to %s was accepted after run returned", r.addr)
	}
}

func TestRun_AnswersAnUnknownPathWithTheErrorEnvelopeAndLogsIt(t *testing.T) {
	r := startRun(t, time.Second)

	res, err := http.Get("http://" + r.addr + "/api/v1/no-such-thing?token=s3cr3t")
	if err != nil {
		t.Fatalf("GET /api/v1/no-such-thing: %v", err)
	}
	defer res.Body.Close()
	var doc struct {
		Error struct {
			Code      int    `json:"code"`
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if res.StatusCode != http.StatusNotFound || doc.Error.Code != 404 || doc.Error.ErrorType != "not_found" {
		t.Errorf("answer = %d %+v, want 404 with code 404 and error_type not_found", res.StatusCode, doc.Error)
	}
	id := res.Header.Get("X-Request-ID")

	r.cancel()
	if code := r.exitCode(t, 5*time.Second); code != 0 {
		t.Fatalf("run returned %d after a stop, want 0", code)
	}
	var found int
	for _, line := range nonEmptyLines(r.stderr.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not a JSON object: %v", line, err)
		}
		if rec["msg"] != "request" {
			continue
		}
		found++
		if rec["path"] != "/api/v1/no-such-thing" || rec["status"] != float64(404) || rec["request_id"] != id {
			t.Errorf("access-log line = %v, want path /api/v1/no-such-thing, status 404, request_id %q", rec, id)
		}
		if strings.Contains(line, "s3cr3t") {
			t.Errorf("access-log line %q holds the query string", line)
		}
	}
	if found != 1 {
		t.Errorf("stderr holds %d access-log lines, want 1: %q", found, r.stderr.String())
	}
}
