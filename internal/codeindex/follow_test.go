package codeindex_test

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
)

func TestFollow_BuildsAWorkspaceOnceItHasItsRepository(t *testing.T) {
	t.Parallel()
	origin := gitrepotest.NewOrigin(t)
	origin.History()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	w := newComposedWorld(t, hubtest.Config{
		Server:              `external_url = "https://hub.example/"`,
		TOML:                enabled,
		MaintenanceInterval: time.Hour,
		Env:                 map[string]string{"GIT_SSL_CAINFO": origin.CAFile(), secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(key)},
	})
	status, body := w.Call(t, http.MethodPost, "/api/v1/workspaces", w.key, map[string]any{"slug": "demo", "git_url": origin.URL()})
	if status != http.StatusCreated {
		t.Fatalf("POST /workspaces = %d %s, want 201", status, body)
	}

	// No further request: the clone itself asks for the build.
	eventually(t, "the index of the cloned workspace", func() bool { return w.record(t, "demo").IndexedSHA != "" })
	if rec := w.reaches(t, "demo", origin.Head("main")); rec.FilesIndexed == 0 {
		t.Errorf("the record = %+v, want the files of the origin's main indexed", rec)
	}
}

func TestFollow_KeepsOnePendingBuildBesideTheRunningOne(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	a := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	release := s.hold(string(filepath.Separator) + a + string(filepath.Separator))
	t.Cleanup(release)
	w.queue(t, "demo")
	s.waitBegan(t, a)

	// Each commit moves main through the ref writer, which reports it.
	b := w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n"})
	c := w.commit(t, "demo", "main", map[string]string{"c.go": "package a\n"})
	d := w.commit(t, "demo", "main", map[string]string{"d.go": "package a\n"})
	eventually(t, "one build running and one waiting", func() bool {
		return slices.Equal(w.jobs(t, "demo"), []string{"running", "queued"})
	})
	if rec := w.record(t, "demo"); rec.IndexedSHA != "" {
		t.Errorf("while the first build is held the record = %+v, want none yet", rec)
	}

	release()
	w.reaches(t, "demo", d)
	if n := w.builds(t); n["ok"] != 2 || n["partial"] != 0 || n["failed"] != 0 {
		t.Errorf("builds by outcome = %v, want two: the one of %s and the one of %s", n, a, d)
	}
	for _, skipped := range []string{b, c} {
		if slices.Contains(w.entries(t, "demo"), skipped) {
			t.Errorf("the code index directory of demo holds %s, a head no build should have taken", skipped)
		}
	}
}

func TestFollow_BuildsAtStartWhatMovedWhileTheHubWasDown(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	a := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.addWorkspace(t, "other", map[string]string{"o.go": "package o\n"})
	w.indexed(t, "demo")
	other := w.indexed(t, "other")

	w.Stop()
	repo := w.workspaces.RepoPath("demo")
	b := gitrepotest.Commit(t, repo, "while down", map[string]string{"b.go": "package a\n"}, a)
	gitrepotest.SetRef(t, repo, "refs/heads/main", b)
	w.Restart(t)

	w.reaches(t, "demo", b)
	if rec := w.record(t, "other"); !rec.BuiltAt.Equal(*other.BuiltAt) || rec.IndexedSHA != other.IndexedSHA {
		t.Errorf("after the restart the record of other = %+v, want it as it was, %+v", rec, other)
	}
	if n := w.builds(t); n["ok"] != 1 {
		t.Errorf("builds after the restart = %v, want the one of demo", n)
	}
}

func TestFollow_BuildsAWorkspaceWhoseSettingIsTurnedOn(t *testing.T) {
	t.Parallel()
	w := newComposedWorld(t, hubtest.Config{TOML: enabled, MaintenanceInterval: time.Hour})
	head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	set := func(on bool) {
		t.Helper()
		status, body := w.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", w.key, map[string]any{"settings": map[string]any{"code_index": on}})
		if status != http.StatusOK {
			t.Fatalf("PATCH code_index %t = %d %s, want 200", on, status, body)
		}
	}
	set(false)
	w.queue(t, "demo")
	if jobs := w.jobs(t, "demo"); len(jobs) != 0 || w.record(t, "demo").IndexedSHA != "" {
		t.Fatalf("with the setting off demo has the jobs %q and the record %+v, want neither", jobs, w.record(t, "demo"))
	}

	set(true)
	// No commit: the setting itself asks for the build.
	built := w.reaches(t, "demo", head)

	set(true)
	if jobs := w.jobs(t, "demo"); len(jobs) != 0 {
		t.Errorf("turning on a setting that is on queued %q, want nothing", jobs)
	}
	if rec := w.record(t, "demo"); !rec.BuiltAt.Equal(*built.BuiltAt) || w.builds(t)["ok"] != 1 {
		t.Errorf("after the second PATCH the record = %+v and builds = %v, want the one build", rec, w.builds(t))
	}
}

func TestFollow_KeepsNothingOfAFork(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	status, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", w.key, map[string]any{"name": "x", "auto_change": false})
	if status != http.StatusCreated {
		t.Fatalf("create the fork demo--x = %d %s, want 201", status, body)
	}

	w.commit(t, "demo--x", "work", map[string]string{"x.go": "package a\n"})
	w.commit(t, "demo--x", "work", map[string]string{"y.go": "package a\n"})
	w.pass(t)
	if jobs := w.jobs(t, "demo--x"); len(jobs) != 0 {
		t.Errorf("the queue holds the code_index jobs %q of the fork, want none", jobs)
	}
	if got := w.entries(t, "demo--x"); len(got) != 0 {
		t.Errorf("the code index directory holds %q of the fork, want nothing", got)
	}
	if n := w.builds(t); n["ok"] != 1 {
		t.Errorf("builds = %v, want the one of the parent", n)
	}
}
