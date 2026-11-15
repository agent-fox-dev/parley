package gitserver_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The end-to-end tests of a push: a stock git client against the hub
// served in-process, through the git routes, the push processor, the rule
// engine, the ref writer, the event log and the store, none of them stubbed.

// gitPushEvent is the part of a parley.git.push event these tests read.
type gitPushEvent struct {
	ActorType string
	Ref       string
	Meta      struct {
		Before  *string `json:"before"`
		Created bool    `json:"created"`
		Forced  bool    `json:"forced"`
		Direct  bool    `json:"direct"`
		Via     string  `json:"via"`
	}
}

// pushEvents are the parley.git.push events of the hub's log after seq.
func (w *world) pushEvents(t *testing.T, seq int64) []gitPushEvent {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), seq, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var evs []gitPushEvent
	for _, r := range recs {
		if r.Type != "parley.git.push" {
			continue
		}
		ev := gitPushEvent{ActorType: r.ActorType, Ref: r.ResourceID}
		if err := json.Unmarshal(r.Metadata, &ev.Meta); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// cloneDemo clones demo over the hub's git URL with password and returns
// the working tree, in which commits are made as P.
func (w *world) cloneDemo(t *testing.T, password string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	runGit(t, t.TempDir(), "clone", "--quiet", w.remote(password, "acme/demo.git"), dir)
	runGit(t, dir, "config", "user.name", "P")
	runGit(t, dir, "config", "user.email", "p@example.invalid")
	return dir
}

// writeFiles writes each path of files with its content in the working
// tree dir and stages it.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", path)
	}
}

func TestPushEndToEnd_AgentPushesANewBranchThenForcePushesIt(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	pat, _ := w.pat(t, w.owner, "", "git:read", "git:write")
	dir := w.cloneDemo(t, pat)
	seq := w.lastSeq()

	runGit(t, dir, "checkout", "--quiet", "-b", "fix-retry")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "retry")
	first := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "push", "--quiet", "origin", "fix-retry")
	runGit(t, dir, "commit", "--quiet", "--amend", "--allow-empty", "-m", "retry, amended")
	amended := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "push", "--quiet", "--force", "origin", "fix-retry")

	if got := gitrepotest.Refs(t, w.repo, "refs/heads/")["fix-retry"]; got != amended {
		t.Errorf("hub's refs/heads/fix-retry = %s, want the amended commit %s", got, amended)
	}
	if !readable(w.repo, first) {
		t.Errorf("the branch's first commit %s is no longer readable in the hub's repository", first)
	}
	evs := w.pushEvents(t, seq)
	if len(evs) != 2 {
		t.Fatalf("push events = %+v, want two", evs)
	}
	for i, ev := range evs {
		if ev.Ref != "refs/heads/fix-retry" || ev.Meta.Via != "git" || ev.ActorType != "pat" {
			t.Errorf("event %d = %+v, want refs/heads/fix-retry through git by a pat", i, ev)
		}
	}
	if !evs[0].Meta.Created || evs[0].Meta.Before != nil || evs[0].Meta.Forced {
		t.Errorf("first event = %+v, want created with before null", evs[0].Meta)
	}
	if !evs[1].Meta.Forced || evs[1].Meta.Created {
		t.Errorf("second event = %+v, want forced", evs[1].Meta)
	}
}

func TestPushEndToEnd_OwnerPushesTheProtectedBranchDirectlyAndIsRefusedWithABoundToken(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	dir := w.cloneDemo(t, w.ownerKey)
	seq := w.lastSeq()

	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "direct")
	direct := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "push", "--quiet", "origin", "main")
	rec, _, err := workspace.BySlug(context.Background(), w.Store.Main(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := gitrepotest.Refs(t, w.repo, "refs/heads/")["main"]; got != direct || rec.HeadSHA == nil || *rec.HeadSHA != direct {
		t.Errorf("main = %s, head_sha = %v, want both %s", got, rec.HeadSHA, direct)
	}
	if evs := w.pushEvents(t, seq); len(evs) != 1 || !evs[0].Meta.Direct {
		t.Fatalf("push events = %+v, want one direct push", evs)
	}

	bound, _ := w.pat(t, w.owner, "demo", "git:read", "git:write")
	runGit(t, dir, "remote", "set-url", "origin", w.remote(bound, "acme/demo.git"))
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "second")
	second := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "branch", "feature-x")
	refsBefore, seqBefore := gitrepotest.Refs(t, w.repo, "refs/"), w.lastSeq()
	out, err := clientGit(t, dir, nil, "push", "origin", "main", "feature-x")
	if err == nil {
		t.Fatalf("git push with the bound PAT succeeded, want it refused:\n%s", out)
	}
	for _, want := range []string{
		"main is protected; push another branch and open a change (git push -o parley.change)",
		"not updated: another ref in this push was refused",
		"remote: error: refs/heads/main:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("git's output lacks %q:\n%s", want, out)
		}
	}
	if got := gitrepotest.Refs(t, w.repo, "refs/"); !maps.Equal(got, refsBefore) {
		t.Errorf("hub refs = %v after the refused push, want %v", got, refsBefore)
	}
	if out, err := clientGit(t, t.TempDir(), nil, "fetch", "--quiet", w.remote(w.ownerKey, "acme/demo.git"), second); err == nil {
		t.Errorf("git fetch of the refused commit %s succeeded:\n%s", second, out)
	}
	if got := w.lastSeq(); got != seqBefore {
		t.Errorf("the event log moved from %d to %d on a refused push", seqBefore, got)
	}
}

func TestPushEndToEnd_PathScopedTokenIsHeldToItsPaths(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	stream := "commit refs/heads/main\ncommitter P <p@example.invalid> now\ndata 6\nfiles\nfrom refs/heads/main^0\n" +
		"M 100644 inline src/main.go\ndata 13\npackage main\n\nM 100644 inline docs/guide.md\ndata 8\n# Guide\n\n"
	if _, err := gitrepotest.Run(w.repo, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	token := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:read", "git:write"}, Paths: []string{"docs/**"}, CreatedAt: w.clock()})
	dir := w.cloneDemo(t, token)
	seq := w.lastSeq()

	runGit(t, dir, "checkout", "--quiet", "-b", "docs-fix")
	writeFiles(t, dir, map[string]string{"docs/guide.md": "# Guide, fixed\n", "src/main.go": "package main // fixed\n"})
	runGit(t, dir, "commit", "--quiet", "-m", "fix")
	out, err := clientGit(t, dir, nil, "push", "origin", "docs-fix")
	if err == nil || !strings.Contains(out, "path src/main.go is outside this token's allowed paths") {
		t.Fatalf("first push: err %v, output:\n%s\nwant it refused for src/main.go", err, out)
	}
	if _, found := gitrepotest.Refs(t, w.repo, "refs/heads/")["docs-fix"]; found {
		t.Error("refs/heads/docs-fix exists after the refused push")
	}
	if evs := w.pushEvents(t, seq); len(evs) != 0 {
		t.Errorf("push events = %+v after the refused push, want none", evs)
	}

	runGit(t, dir, "checkout", "--quiet", "HEAD~1", "--", "src/main.go")
	runGit(t, dir, "commit", "--quiet", "--amend", "-m", "fix docs")
	runGit(t, dir, "push", "--quiet", "origin", "docs-fix")
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	if got := gitrepotest.Refs(t, w.repo, "refs/heads/")["docs-fix"]; got != head {
		t.Errorf("hub's refs/heads/docs-fix = %q, want the amended commit %s", got, head)
	}
	if evs := w.pushEvents(t, seq); len(evs) != 1 || evs[0].Ref != "refs/heads/docs-fix" {
		t.Errorf("push events = %+v, want exactly one for refs/heads/docs-fix", evs)
	}
}
