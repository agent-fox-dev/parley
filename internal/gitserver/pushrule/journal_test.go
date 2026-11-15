package pushrule_test

import (
	"context"
	"database/sql"
	"reflect"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

func TestApply_KeepsTheJournalEntryWhenTheEventsCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	st := newStore(t)
	var w pushrule.Writer
	w.Bind(pushrule.WriterDeps{Transact: st.Tx, Events: &emitted{err: errInjected}})
	session := session1
	var duringCommit []pushrule.JournalEntry
	res, err := w.Apply(ctx, pushrule.ApplyRequest{Verdict: allowed(create("refs/heads/a", r.head), create("refs/heads/b", r.side)),
		Repo: r.view, Workspace: demo, Caller: ownerKey, SessionID: &session, Via: pushrule.ViaGit, VersionKind: pushrule.VersionKindPush,
		BeforeCommit: func(context.Context) error { duringCommit = journal(t, st); return nil }})
	w.Finish(ctx, res)
	if err != nil || res.Refused() {
		t.Fatalf("Apply = %+v, %v, want accepted", res, err)
	}
	if heads := gitrepotest.Refs(t, r.dir, "refs/heads/"); heads["a"] != r.head || heads["b"] != r.side {
		t.Errorf("refs = %v, want a at %.7s and b at %.7s", heads, r.head, r.side)
	}
	got := journal(t, st)
	want := []pushrule.JournalEntry{{Workspace: "demo", ActorType: "api_key", ActorID: "u-owner", CredentialID: "key-owner", SessionID: &session, Via: "git",
		Updates: []pushrule.JournalUpdate{
			{Ref: "refs/heads/a", Before: pushrule.ZeroID, After: r.head, Created: true},
			{Ref: "refs/heads/b", Before: pushrule.ZeroID, After: r.side, Created: true},
		}}}
	if len(got) == 1 {
		want[0].ID = got[0].ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("journal = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(duringCommit, want) {
		t.Errorf("journal before the refs moved = %+v, want the entry", duringCommit)
	}
}

func TestReconcile_WritesTheEventsTheJournalOwesAndDropsTheRest(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	st := newStore(t)
	log := eventLog(t, st)
	next := commit(t, r.dir, []string{r.head}, "M src/b.go")
	session := session1
	owed := pushrule.JournalEntry{Workspace: "demo", ActorType: "pat", ActorID: "u-owner", CredentialID: "pat-owner", SessionID: &session, Via: "git",
		Updates: []pushrule.JournalUpdate{
			{Ref: "refs/heads/feat", Before: r.head, After: next},
			{Ref: "refs/heads/e1", Before: pushrule.ZeroID, After: next, Created: true},
		}}
	neverWritten := pushrule.JournalEntry{Workspace: "demo", ActorType: "api_key", ActorID: "u-owner", Via: "git",
		Updates: []pushrule.JournalUpdate{{Ref: "refs/heads/e2", Before: pushrule.ZeroID, After: next, Created: true}}}
	deleted := pushrule.JournalEntry{Workspace: "demo", ActorType: "admin_token", Via: "api",
		Updates: []pushrule.JournalUpdate{{Ref: "refs/heads/gone", Before: r.head, After: pushrule.ZeroID, Deleted: true}}}
	movedSince := pushrule.JournalEntry{Workspace: "demo", ActorType: "api_key", ActorID: "u-owner", Via: "land",
		Updates: []pushrule.JournalUpdate{{Ref: "refs/heads/patch/a", Before: r.base, After: next}}}
	if err := st.Tx(ctx, func(tx *sql.Tx) error {
		for _, e := range []pushrule.JournalEntry{owed, neverWritten, deleted, movedSince} {
			if _, err := pushrule.InsertJournalEntry(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, r.dir, "update-ref", "refs/heads/feat", next)
	gitrepotest.Git(t, r.dir, "update-ref", "refs/heads/e1", next)

	var w pushrule.Writer
	w.Bind(pushrule.WriterDeps{Transact: st.Tx, Events: log, Repos: func(context.Context, string) (*gitrepo.Repo, error) { return r.view, nil }})
	if err := w.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	slug, user := "demo", "u-owner"
	want := []string{
		eventLine("parley.git.push", &slug, "pat", &user, "ref", "refs/heads/feat", &session, metadata("refs/heads/feat", r.head, next, "git")),
		eventLine("parley.git.push", &slug, "pat", &user, "ref", "refs/heads/e1", &session, metadata("refs/heads/e1", pushrule.ZeroID, next, "git", "created")),
		eventLine("parley.git.push", &slug, "admin_token", nil, "ref", "refs/heads/gone", nil, metadata("refs/heads/gone", r.head, pushrule.ZeroID, "api", "deleted")),
	}
	if got := logged(t, log); !slices.Equal(got, want) {
		t.Errorf("events\n%q\nwant\n%q", got, want)
	}
	if j := journal(t, st); len(j) != 0 {
		t.Errorf("journal = %+v, want it empty", j)
	}
	if err := w.Reconcile(ctx); err != nil || len(logged(t, log)) != len(want) {
		t.Errorf("a second Reconcile = %v with %d events, want nothing more written", err, len(logged(t, log)))
	}
}
