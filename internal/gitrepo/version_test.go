package gitrepo_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

func TestParseVersion_ReadsMajorAndMinorAndComparesOnThemOnly(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		line         string
		major, minor int
		text         string
		accepted     bool
	}{
		{"git version 2.38.0", 2, 38, "2.38.0", true},
		{"git version 2.38", 2, 38, "2.38", true},
		{"git version 2.39.3 (Apple Git-146)", 2, 39, "2.39.3", true},
		{"git version 2.45.1.windows.1", 2, 45, "2.45.1.windows.1", true},
		{"git version 2.50.0-rc1", 2, 50, "2.50.0-rc1", true},
		{"git version 2.100.0", 2, 100, "2.100.0", true},
		{"git version 3.0", 3, 0, "3.0", true},
		{"git version 2.37.7", 2, 37, "2.37.7", false},
		{"git version 2.9.5", 2, 9, "2.9.5", false},
		{"git version 1.9", 1, 9, "1.9", false},
	} {
		v, err := gitrepo.ParseVersion(c.line)
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", c.line, err)
			continue
		}
		if v.Major != c.major || v.Minor != c.minor || v.Text != c.text {
			t.Errorf("ParseVersion(%q) = %+v, want %d.%d %q", c.line, v, c.major, c.minor, c.text)
		}
		if got := v.AtLeast(2, 38); got != c.accepted {
			t.Errorf("ParseVersion(%q).AtLeast(2, 38) = %v, want %v", c.line, got, c.accepted)
		}
	}
}

func TestParseVersion_RefusesALineWithoutAVersion(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"", "git version", "git version two", "git version 2", "hg version 2.40", "version 2.40.0"} {
		if v, err := gitrepo.ParseVersion(line); err == nil {
			t.Errorf("ParseVersion(%q) = %+v, want an error", line, v)
		}
	}
}

// gitScript writes an executable script named git with body into a new
// directory and returns a path lookup that resolves git to it.
func gitScript(t *testing.T, body string) func(string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return func(string) (string, error) { return path, nil }
}

func TestCheckVersion_ReportsAGitThatCannotBeRead(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 300)
	for _, c := range []struct {
		name, body, want string
	}{
		{"a first line that is no version", "echo " + long + "\n", `cannot read the git version from "` + long[:100] + `"`},
		{"a failing git", "exit 3\n", "git --version failed: exit status 3"},
	} {
		lookPath := gitScript(t, c.body)
		_, err := gitrepo.CheckVersion(context.Background(), lookPath, 10*time.Second)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: CheckVersion error = %v, want one containing %q", c.name, err, c.want)
		}
	}
}

func TestCheckVersion_KillsAGitThatDoesNotAnswerInTime(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	lookPath := gitScript(t, "echo $$ > '"+pidFile+"'\nsleep 60\n")
	start := time.Now()
	// A limit long enough for sh to write its process id under a loaded
	// machine before it is killed.
	_, err := gitrepo.CheckVersion(context.Background(), lookPath, time.Second)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "git --version failed: no answer within 1s") {
		t.Errorf("CheckVersion error = %v, want one saying no answer within 1s", err)
	}
	if elapsed >= 3*time.Second {
		t.Errorf("CheckVersion returned after %v, want less than 3 s", elapsed)
	}
	text, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the script wrote no process id: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatalf("process id %q: %v", text, err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Errorf("signal 0 to the script's process %d = %v, want ESRCH: it still runs", pid, err)
	}
}
