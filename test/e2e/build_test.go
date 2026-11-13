// Package e2e builds the programs and runs the binaries, for what only a
// built binary proves.
package e2e

import (
	"encoding/json"
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

func TestBuild_ReportsTheBuildValuesSetByTheLinker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parley := build(t, dir, "./cmd/parley", "9.9.9", "abc1234", "2026-01-02T03:04:05Z")
	parleyd := build(t, dir, "./cmd/parleyd", "9.9.9", "abc1234", "2026-01-02T03:04:05Z")

	cmd := exec.Command(parley, "version")
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "PARLEY_ENDPOINT_URL=")
	})
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

	if out, err := exec.Command(parleyd, "--help").CombinedOutput(); err != nil {
		t.Errorf("parleyd --help: %v\n%s", err, out)
	}
}
