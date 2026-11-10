// Package e2e holds end-to-end tests that build and run the real binaries.
package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// moduleRoot is the repository root, two directories above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func buildBinary(t *testing.T, dir, pkg, ldflags string) string {
	t.Helper()
	out := filepath.Join(dir, filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, pkg)
	cmd.Dir = moduleRoot(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
	return out
}

func envWithout(name string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, name+"=") {
			env = append(env, kv)
		}
	}
	return env
}

func TestBuild_LinkerFlagsSetTheReportedBuild(t *testing.T) {
	const pkgPath = "github.com/agent-fox-dev/parley/internal/buildinfo"
	ldflags := "-X " + pkgPath + ".Version=9.9.9 -X " + pkgPath + ".Commit=abc1234 -X " + pkgPath + ".BuildTime=2026-01-02T03:04:05Z"
	dir := t.TempDir()
	parley := buildBinary(t, dir, "./cmd/parley", ldflags)
	parleyd := buildBinary(t, dir, "./cmd/parleyd", ldflags)

	cmd := exec.Command(parley, "version")
	cmd.Env = envWithout("PARLEY_ENDPOINT_URL")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("parley version: %v", err)
	}
	var doc struct {
		CLI struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		} `json:"cli"`
		Server json.RawMessage `json:"server"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parley version printed %q: %v", out, err)
	}
	if doc.CLI.Version != "9.9.9" || doc.CLI.Commit != "abc1234" {
		t.Errorf("cli = %+v, want version 9.9.9 and commit abc1234", doc.CLI)
	}
	if doc.Server != nil {
		t.Errorf("server = %s, want no server member", doc.Server)
	}

	if err := exec.Command(parleyd, "--help").Run(); err != nil {
		t.Errorf("parleyd --help: %v, want exit status 0", err)
	}
}

func TestBuild_AnUnknownCommandIsAUsageError(t *testing.T) {
	parley := buildBinary(t, t.TempDir(), "./cmd/parley", "")
	cmd := exec.Command(parley, "nosuch")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("exit %v, want 2", err)
	}
	var doc struct {
		Error struct {
			Code      int    `json:"code"`
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr.String()), &doc); err != nil || doc.Error.ErrorType != "usage_error" || doc.Error.Code != 0 {
		t.Errorf("stderr %q", stderr.String())
	}
}
