package codeindex_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/jobs"
)

func TestBuild_WritesTheTreeOfTheHeadAndRecordsTheCommit(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	first := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n\nfunc Alpha() {}\n", "docs/b.md": "# Beta\n"})
	gitrepotest.Git(t, w.workspaces.RepoPath("demo"), "update-ref", "refs/heads/main",
		gitrepotest.Commit(t, w.workspaces.RepoPath("demo"), "run", map[string]string{"extra.txt": "extra\n"}, first))
	head := w.commit(t, "demo", "main", map[string]string{"run.sh": "#!/bin/sh\necho run\n"}, "extra.txt")
	before := time.Now().Add(-time.Second)

	rec := w.indexed(t, "demo")
	want := map[string]string{"a.go": "package a\n\nfunc Alpha() {}\n", "docs/b.md": "# Beta\n", "run.sh": "#!/bin/sh\necho run\n"}
	if got := w.tree(t, "demo", head); !maps.Equal(got, want) {
		t.Errorf("the tree holds %q, want exactly %q", got, want)
	}
	if rec.IndexedSHA != head || rec.FilesIndexed != 3 || rec.Partial || rec.BuiltAt == nil || rec.BuiltAt.Before(before) || rec.Error != "" {
		t.Errorf("the record = %+v, want the head %s, 3 files, not partial, built now and no failure", rec, head)
	}

	next := w.commit(t, "demo", "main", map[string]string{"c.go": "package a\n\nfunc Gamma() {}\n"}, "a.go")
	rec = w.indexed(t, "demo")
	want = map[string]string{"docs/b.md": "# Beta\n", "run.sh": "#!/bin/sh\necho run\n", "c.go": "package a\n\nfunc Gamma() {}\n"}
	if got := w.tree(t, "demo", next); !maps.Equal(got, want) {
		t.Errorf("after the next commit the tree holds %q, want exactly %q", got, want)
	}
	if rec.IndexedSHA != next || rec.FilesIndexed != 3 {
		t.Errorf("after the next commit the record = %+v, want the head %s and 3 files", rec, next)
	}
}

func TestBuild_KeepsInTheTreeTheCommitsFilesAndNothingElse(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", map[string]string{"seed.txt": "seed\n"})
	repo := w.workspaces.RepoPath("demo")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repo, "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	const crlf = "one\r\ntwo\r\n"
	stream := "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\n" + data("links") + "from refs/heads/main^0\n" +
		"M 120000 inline out\n" + data("/etc/hosts") + "M 120000 inline loop\n" + data(".") +
		"M 160000 " + strings.Repeat("5", 40) + " vendor/lib\n" +
		"M 100644 inline .gitattributes\n" + data("*.txt filter=upper eol=lf text\n") +
		"M 100644 inline win.txt\n" + data(crlf) + "M 100755 inline run.sh\n" + data("#!/bin/sh\n") + "\n"
	if out, err := gitrepotest.Run(repo, stream, "fast-import", "--quiet"); err != nil {
		t.Fatalf("fast-import: %v: %s", err, out)
	}
	gitrepotest.Git(t, repo, "config", "filter.upper.smudge", "tr a-z A-Z")
	head := w.head(t, "demo")

	w.indexed(t, "demo")
	got := w.tree(t, "demo", head)
	want := map[string]string{"seed.txt": "seed\n", "out": "-> /etc/hosts", "loop": "-> .", "win.txt": crlf,
		".gitattributes": "*.txt filter=upper eol=lf text\n", "run.sh": "#!/bin/sh\n"}
	// The submodule entry is left out, with or without its directory.
	delete(got, "vendor/lib")
	delete(got, "vendor")
	if !maps.Equal(got, want) {
		t.Errorf("the tree holds %q, want exactly %q", got, want)
	}
	if fi, err := os.Stat(filepath.Join(w.index.TreeDir("demo", head), "run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh has the mode %v (%v), want it executable", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(w.index.TreeDir("demo", head), ".git")); !os.IsNotExist(err) {
		t.Errorf("the tree holds a .git (%v), want none", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the repository's post-checkout hook ran (%v), want no hook run", err)
	}
}

func TestBuild_KeepsAPartialIndexWhenABoundStopsIt(t *testing.T) {
	t.Parallel()
	five := map[string]string{}
	for i := range 5 {
		five[fmt.Sprintf("f%d.go", i)] = fmt.Sprintf("package f\n\nfunc F%d() {}\n", i) + strings.Repeat("// padding of the file\n", 180)
	}
	for _, tc := range []struct {
		name, toml string
		// reported makes the index report a bound, as the library's own
		// does when its wall time is over.
		reported bool
	}{
		{"max_files", "[code_index]\nmax_files = 2\n", false},
		{"max_size", "[code_index]\nmax_size = \"1KB\"\n", false},
		{"build_timeout", "[code_index]\nbuild_timeout = \"1s\"\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSeam()
			s.partial = tc.reported
			w := newWorld(t, tc.toml, s.adjust)
			head := w.addWorkspace(t, "demo", five)

			rec := w.indexed(t, "demo")
			if rec.IndexedSHA != head || !rec.Partial || rec.BuiltAt == nil || rec.Error != "" || rec.FilesIndexed > 5 {
				t.Errorf("the record = %+v, want a partial index of the head %s and no failure", rec, head)
			}
			if tc.name == "max_files" && rec.FilesIndexed != 2 {
				t.Errorf("files_indexed = %d, want the 2 the bound allows", rec.FilesIndexed)
			}
			if b := w.builds(t); b["partial"] != 1 || b["ok"] != 0 || b["failed"] != 0 {
				t.Errorf("builds by outcome = %v, want one partial", b)
			}
			s.mu.Lock()
			timeBound := s.opened[len(s.opened)-1].MaxBuildTime
			s.mu.Unlock()
			if tc.name == "build_timeout" && timeBound != time.Second {
				t.Errorf("the index was made with the wall time bound %s, want the section's 1s", timeBound)
			}

			// The head stays, so the partial index is not built again.
			w.queue(t, "demo")
			eventually(t, "the queued build to end", func() bool { return len(w.jobs(t, "demo")) == 0 })
			if again := w.record(t, "demo"); !again.BuiltAt.Equal(*rec.BuiltAt) || w.builds(t)["partial"] != 1 {
				t.Errorf("after one more queued build the record was built at %s and builds are %v; want %s and still one",
					again.BuiltAt, w.builds(t), rec.BuiltAt)
			}
		})
	}
}

// clockedWorld is a world whose job queue runs on a clock the test moves,
// and passes only when the test runs a pass.
type clockedWorld struct {
	*world
	clock *hubtest.JobClock
	jobq  *jobs.Queue
}

func newClockedWorld(t *testing.T, s *seam) *clockedWorld {
	t.Helper()
	c := &clockedWorld{clock: hubtest.NewJobClock(time.Now)}
	// An hour between maintenance passes: every build is queued by the test
	// or by the notice of a commit, which the test waits for (noticed), so
	// that no late notice makes a failed build due again behind its back.
	c.world = newComposedWorld(t, hubtest.Config{TOML: enabled, MaintenanceInterval: time.Hour, Compose: func(h *hub.Composition) {
		c.jobq = h.Jobs()
		h.AdjustJobs(func(d *jobs.Deps) { d.Clock, d.NoWake = c.clock, true })
	}}, s.adjust)
	return c
}

// noticed commits put to main of slug and waits until the ref writer's
// notice of it has queued the build.
func (c *clockedWorld) noticed(t *testing.T, slug string, put map[string]string) string {
	t.Helper()
	sha := c.commit(t, slug, "main", put)
	eventually(t, "the notice of the commit to queue a build of "+slug, func() bool {
		return slices.Contains(c.jobs(t, slug), jobs.StatusQueued)
	})
	return sha
}

// run runs one pass of the queue and waits until no build of slug runs.
func (c *clockedWorld) run(t *testing.T, slug string) {
	t.Helper()
	if err := c.jobq.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the builds of "+slug+" to end", func() bool {
		for _, status := range c.jobs(t, slug) {
			if status == jobs.StatusRunning {
				return false
			}
		}
		return true
	})
}

func TestBuild_LeavesThePreviousIndexWhenItFailsAndTriesAgain(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newClockedWorld(t, s)
	a := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.queue(t, "demo")
	w.run(t, "demo")
	built := w.record(t, "demo")
	if built.IndexedSHA != a {
		t.Fatalf("the record = %+v, want the index of %s", built, a)
	}

	b := w.noticed(t, "demo", map[string]string{"b.go": "package a\n"})
	s.failWith(func(string) error { return errors.New("the disk went away") })
	w.run(t, "demo")
	rec := w.record(t, "demo")
	if rec.IndexedSHA != a || !rec.BuiltAt.Equal(*built.BuiltAt) || rec.FilesIndexed != built.FilesIndexed {
		t.Errorf("after the failed build the record = %+v, want the index of %s as it was", rec, a)
	}
	if rec.FailedSHA != b || !strings.Contains(rec.Error, "the disk went away") || rec.FailedAt == nil {
		t.Errorf("after the failed build the record = %+v, want the failure of %s kept", rec, b)
	}
	if got := w.tree(t, "demo", a); got["a.go"] != "package a\n" {
		t.Errorf("the tree of %s holds %q, want it as it was", a, got)
	}
	if jobs := w.jobs(t, "demo"); len(jobs) != 1 || jobs[0] != "failed" {
		t.Errorf("the jobs of demo = %q, want the one build waiting for its next attempt", jobs)
	}
	if n := w.builds(t); n["failed"] != 1 || n["ok"] != 1 {
		t.Errorf("builds by outcome = %v, want one ok and one failed", n)
	}

	// A pass before the delay is over does not run it again.
	w.run(t, "demo")
	if w.builds(t)["failed"] != 1 || w.record(t, "demo").IndexedSHA != a {
		t.Errorf("before its delay the build ran again: %v, %+v", w.builds(t), w.record(t, "demo"))
	}
	s.failWith(nil)
	w.clock.Advance(4 * time.Second)
	w.run(t, "demo")
	if rec := w.reaches(t, "demo", b); rec.Error != "" || rec.FailedSHA != "" {
		t.Errorf("after the retry the record = %+v, want the index of %s and no failure", rec, b)
	}
}

func TestBuild_BuildsANewHeadWithoutWaitingForAFailedBuildsRetry(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newClockedWorld(t, s)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	s.failWith(func(string) error { return errors.New("no luck") })
	w.queue(t, "demo")
	w.run(t, "demo")
	if jobs := w.jobs(t, "demo"); len(jobs) != 1 || jobs[0] != "failed" {
		t.Fatalf("the jobs of demo = %q, want the failed build waiting", jobs)
	}

	s.failWith(nil)
	// The notice of the new head makes the failed build due; the clock has
	// not moved.
	c := w.noticed(t, "demo", map[string]string{"c.go": "package a\n"})
	w.run(t, "demo")
	if rec := w.reaches(t, "demo", c); rec.Error != "" {
		t.Errorf("the record = %+v, want the index of the new head %s", rec, c)
	}
}

func TestBuild_RunsAtMostTheConfiguredWorkersAtOnce(t *testing.T) {
	t.Parallel()
	s := newSeam()
	release := map[string]func(){}
	w := newWorld(t, "[code_index]\nworkers = 2\n", s.adjust)
	slugs := []string{"w1", "w2", "w3", "w4", "w5"}
	for _, slug := range slugs {
		w.addWorkspace(t, slug, map[string]string{"a.go": "package a\n"})
		release[slug] = s.hold(string(filepath.Separator) + slug + string(filepath.Separator))
		t.Cleanup(release[slug])
	}
	// The maintenance pass queues the build of each: queueing one here too
	// could add a second beside a build that pass had already started.
	count := func() (running []string, queued int) {
		for _, slug := range slugs {
			for _, status := range w.jobs(t, slug) {
				switch status {
				case jobs.StatusRunning:
					running = append(running, slug)
				case jobs.StatusQueued:
					queued++
				}
			}
		}
		return running, queued
	}
	eventually(t, "two builds to run and three to wait", func() bool { r, q := count(); return len(r) == 2 && q == 3 })
	// The queue passes again and again; no third build may begin.
	time.Sleep(300 * time.Millisecond)
	running, queued := count()
	if len(running) != 2 || queued != 3 {
		t.Fatalf("%d builds run (%v) and %d wait, want 2 and 3", len(running), running, queued)
	}

	release[running[0]]()
	eventually(t, "a third build to begin", func() bool {
		r, q := count()
		return len(r) == 2 && q == 2
	})
	for _, slug := range slugs {
		release[slug]()
	}
	for _, slug := range slugs {
		w.reaches(t, slug, w.head(t, slug))
	}
}
