package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

// setBuildValues sets the build values of internal/buildinfo for the test and
// restores them after it. A test that calls it must not run in parallel.
func setBuildValues(t *testing.T, version, commit string) {
	t.Helper()
	oldVersion, oldCommit := buildinfo.Version, buildinfo.Commit
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = oldVersion, oldCommit })
	buildinfo.Version, buildinfo.Commit = version, commit
}

// unsetEnv removes key from the environment for the test and restores it
// after it. A test that calls it must not run in parallel.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	os.Unsetenv(key)
}

// runCLI calls Run with args and returns the exit status, stdout and stderr.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// runCLIWith is runCLI with opts in place of the defaults.
func runCLIWith(t *testing.T, opts options, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, opts)
	return code, stdout.String(), stderr.String()
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
