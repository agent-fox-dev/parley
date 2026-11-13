package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/cli"
)

// usageNames are what the usage must name: both flags and the backup
// command.
var usageNames = []string{"--admin-email", "--reset-admin-token", "backup <directory>"}

func TestRun_PrintsUsageOnStdoutForHelp(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var stdout, stderr bytes.Buffer
			// run returns without serving, or the test would hang here.
			code := run(context.Background(), []string{arg}, noEnv, dir, &stdout, &stderr)

			if code != 0 {
				t.Errorf("run returned %d, want 0", code)
			}
			for _, name := range append([]string{"parleyd"}, usageNames...) {
				if !strings.Contains(stdout.String(), name) {
					t.Errorf("stdout %q does not name %s", stdout.String(), name)
				}
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
			if names := listDir(t, dir); len(names) != 0 {
				t.Errorf("the working directory holds %v, want nothing", names)
			}
		})
	}
}

func TestRun_RefusesUnknownFlagsAndArgumentsWithUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--nope"}, {"-x"}, {"serve"}, {"serve", "dir"},
		{"--admin-email"}, {"backup"}, {"backup", "a", "b"}, {"backup", "a", "--nope"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, noEnv, dir, &stdout, &stderr)

			if code != 2 {
				t.Errorf("run returned %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "Usage: parleyd") {
				t.Errorf("stderr %q does not hold the usage", stderr.String())
			}
			for _, name := range usageNames {
				if !strings.Contains(stderr.String(), name) {
					t.Errorf("stderr %q does not name %s", stderr.String(), name)
				}
			}
			if names := listDir(t, dir); len(names) != 0 {
				t.Errorf("the working directory holds %v, want nothing", names)
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

func TestRun_HandsTheHubTheEnvironmentAndTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	// A relative XDG_CONFIG_HOME is taken relative to the working directory,
	// so the listening line names a path only both together give.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "etc", "config.toml"), localConfig)
	env := func(name string) (string, bool) {
		if name == "XDG_CONFIG_HOME" {
			return "etc", true
		}
		return "", false
	}
	r := startRun(t, env, dir)

	if got, want := r.listening["config"], filepath.Join(dir, "etc", "config.toml"); got != want {
		t.Errorf("listening config = %v, want %s", got, want)
	}
	if got, want := r.listening["data_dir"], filepath.Join(dir, "data"); got != want {
		t.Errorf("listening data_dir = %v, want %s", got, want)
	}
	r.cancel()
	if code := r.exitCode(t, 5*time.Second); code != 0 {
		t.Errorf("run returned %d after its context was cancelled, want 0", code)
	}
}

// An operator starts the hub, a developer reads its version with the real
// client, and the operator stops the hub with SIGTERM. The test signals its
// own process, which every running run would receive, so it must not run in
// parallel. The signal is sent only after the listening line, when run has
// registered for it; before that it would end the test binary.
func TestRun_ServesTheClientUntilStopped(t *testing.T) {
	t.Setenv("PARLEY_ENDPOINT_URL", "")
	os.Unsetenv("PARLEY_ENDPOINT_URL")
	r := startRun(t, noEnv, workDir(t, localConfig))
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
