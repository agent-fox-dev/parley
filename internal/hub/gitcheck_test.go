package hub_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
)

// gitDir returns a new directory holding an executable script named git with
// body, for the PATH of a hub's environment; an empty body leaves the
// directory without git.
func gitDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// pathEnv is a hub environment whose PATH is dir alone.
func pathEnv(dir string) config.Env {
	return config.MapEnv(map[string]string{"PATH": dir})
}

// Not parallel: the scripts are written while no other test of the package
// forks, so that none of its children holds a script open for writing when
// the hub runs it.
func TestGitCheck_AcceptsAUsableGitBeforeEveryOtherHook(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "calls")
	bin := gitDir(t, "echo call >> '"+counter+"'\necho 'git version 2.39.3 (Apple Git-146)'\n")

	// The version line is a start report, written right before the listening
	// line, so a later hook sees that git ran by the script's counter file.
	var gitRanFirst atomic.Bool
	compose := func(c *hub.Composition) {
		c.AddStartHook("h", func(context.Context, *hub.Runtime) error {
			calls, _ := os.ReadFile(counter)
			gitRanFirst.Store(len(calls) > 0)
			return nil
		})
	}
	r := startHub(t, hub.Options{WorkDir: workDir(t, localConfig), Env: pathEnv(bin), Compose: compose})

	calls, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("git was not run: %v", err)
	}
	if n := len(nonEmptyLines(string(calls))); n != 1 {
		t.Errorf("git was run %d times, want 1", n)
	}
	if !gitRanFirst.Load() {
		t.Error("a later start hook ran before git was run")
	}
	var checked []map[string]any
	for _, rec := range r.stderr.records(t) {
		if rec["msg"] == "git version checked" {
			checked = append(checked, rec)
		}
		if rec["msg"] == "listening" && len(checked) == 0 {
			t.Errorf("the listening line came before the git version checked line; stderr = %q", r.stderr.String())
		}
	}
	if len(checked) != 1 {
		t.Fatalf("%d lines say git version checked, want 1; stderr = %q", len(checked), r.stderr.String())
	}
	if checked[0]["level"] != "INFO" || checked[0]["git_version"] != "2.39.3" {
		t.Errorf("git version checked line = %v, want level INFO and git_version 2.39.3", checked[0])
	}

	t.Run("the git of the host", func(t *testing.T) {
		r := startHub(t, hub.Options{WorkDir: workDir(t, localConfig)})
		if v, _ := r.stderr.waitForMsg(t, "git version checked")["git_version"].(string); v == "" {
			t.Errorf("the git version checked line has no git_version; stderr = %q", r.stderr.String())
		}
	})
}

// Not parallel at the top, for the reason given above; the rows run in
// parallel once every script is written.
func TestGitCheck_RefusesToStartWithoutAUsableGit(t *testing.T) {
	cases := []struct {
		name, body, want string
		bin              string
	}{
		{name: "no git on the PATH", want: "git not found on PATH; parleyd needs git 2.38 or later"},
		{name: "git 2.30.2", body: "echo 'git version 2.30.2'\n", want: "git 2.30.2 is too old; parleyd needs git 2.38 or later"},
		{name: "git 2.37.7", body: "echo 'git version 2.37.7'\n", want: "git 2.37.7 is too old"},
		{name: "an unreadable version", body: "echo 'gti 2.40'\n", want: `cannot read the git version from "gti 2.40"`},
		{name: "a failing git", body: "exit 3\n", want: "git --version failed: exit status 3"},
	}
	for i := range cases {
		cases[i].bin = gitDir(t, cases[i].body)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var called atomic.Bool
			compose := func(c *hub.Composition) {
				c.AddStartHook("h", func(context.Context, *hub.Runtime) error {
					called.Store(true)
					return nil
				})
			}
			res := runToEnd(t, hub.Options{WorkDir: workDir(t, localConfig), Env: pathEnv(c.bin), Compose: compose})
			if res.code != 1 {
				t.Errorf("exit status %d, want 1", res.code)
			}
			lines := nonEmptyLines(res.stderr)
			if len(lines) != 1 {
				t.Fatalf("stderr holds %d lines, want 1: %q", len(lines), res.stderr)
			}
			rec := decodeLine(t, lines[0])
			if rec["level"] != "ERROR" {
				t.Errorf("level = %v, want ERROR", rec["level"])
			}
			if text, _ := rec["error"].(string); !strings.Contains(text, c.want) {
				t.Errorf("error line %q does not say %q", lines[0], c.want)
			}
			if strings.Contains(res.stderr, "listening") {
				t.Errorf("the hub listened: %q", res.stderr)
			}
			if called.Load() {
				t.Error("a later start hook was called")
			}
		})
	}
}

func TestGitCheck_RunsThatDoNotServeNeedNoGit(t *testing.T) {
	t.Parallel()
	dir := workDir(t, localConfig)
	env := pathEnv(t.TempDir())
	for _, c := range []struct {
		name string
		args []string
		want int
	}{
		{"help", []string{"--help"}, 0},
		{"a usage error", []string{"--no-such-flag"}, 2},
		{"the first bootstrap", []string{"--admin-email=a@x"}, 0},
		{"a reset of the admin token", []string{"--reset-admin-token"}, 0},
		{"a backup", []string{"backup", filepath.Join(t.TempDir(), "snapshot")}, 0},
	} {
		res := runToEnd(t, hub.Options{WorkDir: dir, Env: env, Args: c.args})
		if res.code != c.want {
			t.Errorf("%s: exit status %d, want %d; stderr = %q", c.name, res.code, c.want, res.stderr)
		}
		if strings.Contains(res.stderr, "git not found") {
			t.Errorf("%s: the run looked for git; stderr = %q", c.name, res.stderr)
		}
	}
}
