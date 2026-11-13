// Package e2e builds the programs and runs the binaries, for what only a
// built binary proves.
package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// moduleRoot is the repository root, relative to this package's directory.
const moduleRoot = "../.."

// build builds the program in pkg into dir with the build values set by the
// linker and returns the binary's path.
func build(t *testing.T, dir, pkg, version, commit, buildTime string) string {
	t.Helper()
	const buildinfo = "github.com/agent-fox-dev/parley/internal/buildinfo"
	bin := filepath.Join(dir, filepath.Base(pkg))
	ldflags := strings.Join([]string{
		"-X " + buildinfo + ".Version=" + version,
		"-X " + buildinfo + ".Commit=" + commit,
		"-X " + buildinfo + ".BuildTime=" + buildTime,
	}, " ")
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", bin, pkg)
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

func TestBuild_RunsTheBuiltBinaries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parley := build(t, dir, "./cmd/parley", "9.9.9", "abc1234", "2026-01-02T03:04:05Z")
	parleyd := build(t, dir, "./cmd/parleyd", "9.9.9", "abc1234", "2026-01-02T03:04:05Z")
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "PARLEY_ENDPOINT_URL=")
	})

	t.Run("parley version reports the build values set by the linker", func(t *testing.T) {
		cmd := exec.Command(parley, "version")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("parley version: %v\n%s", err, out)
		}
		var doc struct {
			CLI struct {
				Version string `json:"version"`
				Commit  string `json:"commit"`
			} `json:"cli"`
			Server json.RawMessage `json:"server"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("parley version printed no JSON object: %v\n%s", err, out)
		}
		if doc.CLI.Version != "9.9.9" || doc.CLI.Commit != "abc1234" {
			t.Errorf("cli = %+v, want version 9.9.9 and commit abc1234", doc.CLI)
		}
		if doc.Server != nil {
			t.Errorf("parley version printed a server member without a hub URL: %s", doc.Server)
		}
	})

	t.Run("parley refuses an unknown command with exit status 2 and a usage error", func(t *testing.T) {
		var stdout, stderr strings.Builder
		cmd := exec.Command(parley, "nosuch")
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("parley nosuch: %v, want exit status 2; stderr %q", err, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want it empty", stdout.String())
		}
		var doc struct {
			Error struct {
				Code      *int   `json:"code"`
				Message   string `json:"message"`
				ErrorType string `json:"error_type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(stderr.String()), &doc); err != nil {
			t.Fatalf("stderr is not one JSON document (%v): %q", err, stderr.String())
		}
		if doc.Error.Code == nil || *doc.Error.Code != 0 || doc.Error.ErrorType != "usage_error" || !strings.Contains(doc.Error.Message, "nosuch") {
			t.Errorf("stderr = %q, want a usage_error envelope with code 0 that names nosuch", stderr.String())
		}
	})

	t.Run("parleyd --help succeeds", func(t *testing.T) {
		if out, err := exec.Command(parleyd, "--help").CombinedOutput(); err != nil {
			t.Errorf("parleyd --help: %v\n%s", err, out)
		}
	})
}
