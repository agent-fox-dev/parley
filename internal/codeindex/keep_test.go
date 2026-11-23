package codeindex_test

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
)

func TestKeep_HasTheIndexAfterARestartWithoutBuildingIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n\nfunc Zebra() {}\n", "b.go": "package a\n\nfunc Yak() {}\n"})
	before := w.indexed(t, "demo")

	w.Restart(t)
	w.pass(t)
	after := w.record(t, "demo")
	if after.IndexedSHA != head || !after.BuiltAt.Equal(*before.BuiltAt) || after.FilesIndexed != before.FilesIndexed {
		t.Errorf("after the restart the record = %+v, want it as it was, %+v", after, before)
	}
	files, err := w.index.Files(context.Background(), "demo", "Zebra")
	if err != nil || !slices.Equal(files, []string{"a.go"}) {
		t.Errorf("the index answers %q (%v) for Zebra, want a.go", files, err)
	}
	for outcome, n := range w.builds(t) {
		if n != 0 {
			t.Errorf("parley_code_index_builds_total{outcome=%q} = %v after the restart, want 0", outcome, n)
		}
	}
}

func TestKeep_BuildsAPartialIndexAgainAsABuildAfterARestart(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, "[code_index]\nmax_files = 2\n[scm]\nread_timeout = \"1s\"\n", s.adjust)
	files := map[string]string{}
	for i := range 5 {
		files[fmt.Sprintf("f%d.go", i)] = "package f\n\n// needle\n"
	}
	head := w.addWorkspace(t, "demo", files)
	w.indexed(t, "demo")

	// Every build of demo is held from here on: the index its build made
	// answers without one.
	release := s.hold(demoPart)
	t.Cleanup(release)
	if got, _ := w.search(t, "demo", "needle"); !got.Partial || str(got.IndexedSHA) != head {
		t.Errorf("the search after the build answers %+v, want the partial index of %s", got, head)
	}

	// A partial index is not stored whole: the start queues its build, a
	// job like any other, which a search waits for.
	w.Restart(t)
	s.waitBegan(t, demoPart)
	if jobs := w.jobs(t, "demo"); !slices.Equal(jobs, []string{"running"}) {
		t.Errorf("after the restart the code_index jobs of demo are %q, want the one build running", jobs)
	}
	if st := w.statusOf(t, "demo"); st.State != "building" {
		t.Errorf("after the restart the state of demo is %s, want building", st.State)
	}
	a := w.ask(t, "demo", "code-search", "q=needle", w.key)
	if r := a.refusal(); a.code != http.StatusServiceUnavailable || r.Error.ErrorType != "code_index_not_ready" ||
		r.Error.State != "building" || a.header.Get("Retry-After") != "5" || str(r.Error.HeadSHA) != head {
		t.Errorf("a search during the build = %d %s (Retry-After %q), want 503 code_index_not_ready, state building, Retry-After 5",
			a.code, a.body, a.header.Get("Retry-After"))
	}

	release()
	w.reaches(t, "demo", head)
	if got, _ := w.search(t, "demo", "needle"); !got.Partial || str(got.IndexedSHA) != head {
		t.Errorf("the search after the build answers %+v, want the partial index of %s", got, head)
	}
	if st := w.statusOf(t, "demo"); st.State != "ready" || !st.Partial {
		t.Errorf("after the build the status of demo = %+v, want ready and partial", st)
	}
	if b := w.builds(t); b["partial"] != 1 || b["ok"] != 0 || b["failed"] != 0 {
		t.Errorf("builds by outcome after the restart = %v, want the one partial build", b)
	}
}

func TestKeep_KeepsTheIndexOfAnArchivedWorkspace(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	before := w.indexed(t, "demo")
	tree := w.tree(t, "demo", head)

	if status, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/archive", w.key, nil); status != http.StatusOK {
		t.Fatalf("archive demo = %d %s, want 200", status, body)
	}
	w.pass(t)
	if after := w.record(t, "demo"); after.IndexedSHA != head || !after.BuiltAt.Equal(*before.BuiltAt) {
		t.Errorf("after the archive the record = %+v, want it as it was, %+v", after, before)
	}
	if got := w.tree(t, "demo", head); len(got) != len(tree) || got["a.go"] != tree["a.go"] {
		t.Errorf("after the archive the tree holds %q, want %q", got, tree)
	}
}

func TestKeep_RemovesEverythingOfADeletedWorkspace(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	// Held before the commit that starts it, so the build cannot slip by.
	held := string(filepath.Separator) + "demo" + string(filepath.Separator)
	release := s.hold(held)
	t.Cleanup(release)
	w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n"})
	s.waitBegan(t, held)

	if status, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/archive", w.key, nil); status != http.StatusOK {
		t.Fatalf("archive demo = %d %s, want 200", status, body)
	}
	if status, body := w.Call(t, http.MethodDelete, "/api/v1/workspaces/demo", w.key, nil); status >= 300 {
		t.Fatalf("delete demo = %d %s, want it deleted", status, body)
	}
	if jobs := w.jobs(t, "demo"); len(jobs) != 0 {
		t.Errorf("after the delete the queue holds the code_index jobs %q of demo, want none", jobs)
	}
	if rec := w.record(t, "demo"); rec.IndexedSHA != "" || rec.Workspace != "" {
		t.Errorf("after the delete the record of demo = %+v, want none", rec)
	}
	s.waitEnded(t, held)
	w.pass(t)
	if got := w.entries(t, "demo"); len(got) != 0 {
		t.Errorf("after the delete and a maintenance pass the code index directory holds %q of demo, want nothing", got)
	}

	// The slug is taken again by another repository.
	release()
	again := w.addWorkspace(t, "demo", map[string]string{"other.go": "package other\n"})
	w.indexed(t, "demo")
	if got := w.tree(t, "demo", again); len(got) != 1 || got["other.go"] == "" {
		t.Errorf("the tree of the new demo holds %q, want its own file alone", got)
	}
	if got := w.entries(t, "demo"); !slices.Equal(got, []string{again}) {
		t.Errorf("the directory of the new demo holds %q, want the build of its head, %s, alone", got, again)
	}
}

func TestKeep_RemovesTheIndexOfAWorkspaceWhoseSettingIsTurnedOff(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	// Held before the commit that starts it, so the build cannot slip by.
	held := string(filepath.Separator) + "demo" + string(filepath.Separator)
	release := s.hold(held)
	t.Cleanup(release)
	w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n"})
	s.waitBegan(t, held)
	// One more waits beside the build that runs.
	w.commit(t, "demo", "main", map[string]string{"c.go": "package a\n"})
	eventually(t, "a build waiting beside the running one", func() bool { return len(w.jobs(t, "demo")) == 2 })

	status, body := w.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", w.key, map[string]any{"settings": map[string]any{"code_index": false}})
	if status != http.StatusOK {
		t.Fatalf("PATCH code_index false = %d %s, want 200", status, body)
	}
	s.waitEnded(t, held)
	w.pass(t)
	check := func(when string) {
		t.Helper()
		if got := w.entries(t, "demo"); len(got) != 0 {
			t.Errorf("%s the code index directory holds %q of demo, want nothing", when, got)
		}
		if jobs := w.jobs(t, "demo"); len(jobs) != 0 {
			t.Errorf("%s the queue holds the code_index jobs %q of demo, want none", when, jobs)
		}
		if rec := w.record(t, "demo"); rec.Workspace != "" {
			t.Errorf("%s the record of demo = %+v, want none", when, rec)
		}
	}
	check("after one maintenance pass")
	w.pass(t)
	check("after a second pass")
	if n := w.builds(t); n["failed"] != 0 {
		t.Errorf("builds by outcome = %v, want the build that was ended counted under none", n)
	}
}

func TestKeep_LoadsAnIndexWithoutHoldingUpTheBuilds(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n\nfunc Zebra() {}\n"})
	w.addWorkspace(t, "other", map[string]string{"o.go": "package o\n"})
	w.indexed(t, "demo")

	// The load of demo's index is held, as a slow one is.
	held := string(filepath.Separator) + "demo" + string(filepath.Separator)
	release := s.hold(held)
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(context.Background())
	loaded := make(chan error, 1)
	go func() {
		_, err := w.index.Files(ctx, "demo", "Zebra")
		loaded <- err
	}()
	s.waitBegan(t, held)

	// A build of another workspace settles meanwhile.
	w.indexed(t, "other")

	// The caller gives up: the stored index is not the one at fault.
	cancel()
	if err := <-loaded; err == nil {
		t.Error("the load whose caller gave up answered, want its error")
	}
	release()
	files, err := w.index.Files(context.Background(), "demo", "Zebra")
	if err != nil || !slices.Equal(files, []string{"a.go"}) {
		t.Errorf("after a load that was given up the index answers %q (%v), want a.go from the index that was stored", files, err)
	}
	if n := w.builds(t); n["ok"] != 2 {
		t.Errorf("builds = %v, want the two there were: a load that was given up builds nothing", n)
	}
}
