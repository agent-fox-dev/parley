package awareness_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestWorker_IndexesAnOpenedChangeOnlyAfterAPass(t *testing.T) {
	var comp *awareness.Component
	var workspaces *workspace.Service
	h := hubtest.StartWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
		comp, workspaces = c.Awareness(), c.Workspaces()
		// The worker is under the test's control: its own passes come an
		// hour apart.
		c.AdjustAwareness(func(d *awareness.Deps) { d.IndexInterval = time.Hour })
	}})
	owner := h.CreateUser(t, "owner")
	key := h.IssueKey(t, owner)
	org := h.CreateOrg(t, "acme")
	h.AddMember(t, org, owner)
	dir := workspaces.RepoPath("demo")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	base := commit(t, dir, "main", "", map[string]string{"api/a.go": "a\n"})
	commit(t, dir, "feat", base, map[string]string{"api/a.go": "feat\n"})
	now := time.Now().UTC()
	rec := workspace.Workspace{
		Slug: "demo", Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/demo.git"), DefaultBranch: new("main"), OwnerID: owner, OrgID: org,
		Status: workspace.StatusReady, DisplayName: "demo", HeadSHA: &base,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	txOf(t, h.Store, func(tx *sql.Tx) error { return workspace.Insert(context.Background(), tx, rec) })

	status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes", key,
		map[string]any{"source": map[string]any{"branch": "feat"}, "title": "Rework a", "intent": "Make a better."})
	if status != http.StatusCreated {
		t.Fatalf("POST changes = %d %s, want 201", status, raw)
	}
	var opened struct{ Number int64 }
	if err := json.Unmarshal(raw, &opened); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, err := comp.InFlight(ctx, nil, "demo", "main"); err != nil || len(got) != 0 {
		t.Fatalf("in flight before a pass = %v, %v; want none", got, err)
	}

	if err := comp.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := comp.InFlight(ctx, nil, "demo", "main"); err != nil || !slices.Equal(got, []int64{opened.Number}) {
		t.Errorf("in flight after a pass = %v, %v; want [%d]", got, err, opened.Number)
	}
	if got, err := comp.Files(ctx, nil, "demo", opened.Number); err != nil || !slices.Equal(got, []string{"api/a.go"}) {
		t.Errorf("files = %v, %v; want [api/a.go]", got, err)
	}
}

func TestWorker_AppliesEachEventOnceAcrossARestartAndAFailedCommit(t *testing.T) {
	f := newFixture(t)
	base := commit(t, f.repo, "main", "", map[string]string{"a.go": "a\n", "b.go": "b\n"})
	var versions []awareness.Change
	for i, files := range []map[string]string{
		{"a.go": "1\n"}, {"b.go": "1\n"}, {"a.go": "2\n", "c.go": "c\n"}, {"b.go": "2\n", "d.go": "d\n"}, {"e.go": "e\n"},
	} {
		head := commit(t, f.repo, "v", base, files)
		versions = append(versions, awareness.Change{Number: int64(i%2 + 1), Version: int64(i/2 + 1), BaseSHA: &base, HeadSHA: head})
	}

	whole := newFixtureOn(t, f.repo)
	for _, v := range versions {
		whole.version(v)
	}
	whole.pass()

	f.version(versions[0])
	f.version(versions[1])
	f.pass()
	f.restart()
	var fifth int64
	for _, v := range versions[2:] {
		fifth = f.version(v)
	}
	f.pass()
	f.pass()

	if got := f.position(); got != fifth {
		t.Errorf("position = %d, want %d, the fifth event's", got, fifth)
	}
	if got, want := f.dump(), whole.dump(); !slices.Equal(got, want) {
		t.Errorf("index after a restart = %v, want that of a run without it, %v", got, want)
	}
	if got := f.changes.readCount(); got != 5 {
		t.Errorf("the change reader was asked %d times, want 5, once per event", got)
	}

	before := f.dump()
	head := commit(t, f.repo, "v", base, map[string]string{"f.go": "f\n"})
	sixth := f.version(awareness.Change{Number: 1, Version: 4, BaseSHA: &base, HeadSHA: head})
	awareness.FailNextCommit(f.comp, errors.New("disk I/O error"))
	if err := f.comp.Pass(context.Background()); err == nil {
		t.Error("a pass whose transaction failed returned no error")
	}
	if got := f.position(); got != fifth {
		t.Errorf("position after a failed transaction = %d, want %d", got, fifth)
	}
	if got := f.dump(); !slices.Equal(got, before) {
		t.Errorf("index after a failed transaction = %v, want it unchanged, %v", got, before)
	}

	f.pass()
	if got := f.position(); got != sixth {
		t.Errorf("position after the next pass = %d, want %d", got, sixth)
	}
	if got := f.files(1); !slices.Equal(got, []string{"f.go"}) {
		t.Errorf("files of 1 after the next pass = %v, want [f.go]", got)
	}
}
