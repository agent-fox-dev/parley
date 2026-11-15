package pushrule_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

func TestFinish_WritesOneEventPerUpdateWithTheSurfaceTheWriteCameThrough(t *testing.T) {
	ctx := context.Background()
	for _, via := range []string{"git", "api", "land"} {
		t.Run(via, func(t *testing.T) {
			r := newRepo(t)
			st, ev := newStore(t), &emitted{}
			var w pushrule.Writer
			w.Bind(pushrule.WriterDeps{Transact: st.Tx, Events: ev})
			session := session1
			res, err := w.Apply(ctx, pushrule.ApplyRequest{Verdict: allowed(create("refs/heads/n", r.head), remove("refs/heads/feat", r.head)),
				Repo: r.view, Workspace: demoFk, Caller: ownerPAT, SessionID: &session, Via: via})
			if err != nil || res.Refused() {
				t.Fatalf("Apply = %+v, %v, want accepted", res, err)
			}
			w.Finish(ctx, res)
			slug, user := demoFk.Slug, "u-owner"
			want := []string{
				eventLine("parley.git.push", &slug, "pat", &user, "ref", "refs/heads/n", &session, metadata("refs/heads/n", pushrule.ZeroID, r.head, via, "created")) + " parent=demo",
				eventLine("parley.git.push", &slug, "pat", &user, "ref", "refs/heads/feat", &session, metadata("refs/heads/feat", r.head, pushrule.ZeroID, via, "deleted")) + " parent=demo",
			}
			if got := ev.got(t); !slices.Equal(got, want) {
				t.Errorf("events\n%q\nwant\n%q", got, want)
			}
			if j := journal(t, st); len(j) != 0 {
				t.Errorf("journal = %+v, want it empty once the events are written", j)
			}
		})
	}

	t.Run("a surface the writer does not know", func(t *testing.T) {
		r := newRepo(t)
		st, ev := newStore(t), &emitted{}
		var w pushrule.Writer
		w.Bind(pushrule.WriterDeps{Transact: st.Tx, Events: ev})
		refsBefore := gitrepotest.Git(t, r.dir, "for-each-ref")
		res, err := w.Apply(ctx, pushrule.ApplyRequest{Verdict: allowed(create("refs/heads/n", r.head)), Repo: r.view, Workspace: demo, Caller: ownerKey, Via: "cli"})
		if err == nil || !res.Refused() {
			t.Errorf("Apply with via cli = %+v, %v, want an error and nothing written", res, err)
		}
		w.Finish(ctx, res)
		if gitrepotest.Git(t, r.dir, "for-each-ref") != refsBefore || len(ev.got(t)) != 0 || len(journal(t, st)) != 0 {
			t.Error("Apply with via cli moved a ref, wrote an event or kept a journal entry")
		}
	})
}

func TestFinish_TellsEveryPostPushListenerInTheBackground(t *testing.T) {
	ctx := context.Background()
	s1, s2 := session1, session2
	for _, tc := range []struct {
		via     string
		session *string
	}{{"git", &s1}, {"api", &s2}, {"land", nil}} {
		t.Run(tc.via, func(t *testing.T) {
			r := newRepo(t)
			a := newListener()
			var w pushrule.Writer
			w.Bind(pushrule.WriterDeps{Transact: newStore(t).Tx, Events: &emitted{}})
			w.RegisterPostPushListener(a.listen)
			w.RegisterPostPushListener(func(context.Context, pushrule.PostPush) error { panic("listener B fails") })
			v := allowed(create("refs/heads/n", r.head))
			res, err := w.Apply(ctx, pushrule.ApplyRequest{Verdict: v, Repo: r.view, Workspace: demo, Caller: ownerKey, SessionID: tc.session, Via: tc.via})
			if err != nil || res.Refused() {
				t.Fatalf("Apply = %+v, %v, want accepted", res, err)
			}
			finished := make(chan struct{})
			go func() { w.Finish(ctx, res); close(finished) }()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("the event step waited for a listener")
			}
			close(a.release)
			got := a.next(t)
			if got.Workspace != "demo" || len(got.Updates) != 1 || got.Updates[0].Update != v.Decisions[0].Update || outcomeOf(got.Updates[0]) != outcomeOf(v.Decisions[0]) || got.Via != tc.via || !slices.Equal(deref(got.SessionID), deref(tc.session)) {
				t.Errorf("listener A was told %+v, want demo, the create, %s and session %v", got, tc.via, deref(tc.session))
			}
			if at := gitrepotest.Refs(t, r.dir, "refs/heads/")["n"]; at != r.head {
				t.Errorf("refs/heads/n = %.7s, want %.7s", at, r.head)
			}

			refusedV := allowed(create("refs/heads/m", r.head))
			refusedV.Decisions = append(refusedV.Decisions, pushrule.Decision{Update: create("refs/parley/x", r.head), Message: "refs/parley/ is maintained by the hub and cannot be pushed to", ErrorType: pushrule.ErrorTypeHubOwnedRef})
			res, _ = w.Apply(ctx, pushrule.ApplyRequest{Verdict: refusedV, Repo: r.view, Workspace: demo, Caller: ownerKey, Via: tc.via})
			w.Finish(ctx, res)
			res, _ = w.Apply(ctx, pushrule.ApplyRequest{Verdict: allowed(create("refs/heads/o", r.head)), Repo: r.view, Workspace: demo, Caller: ownerKey, Via: tc.via})
			w.Finish(ctx, res)
			if got := a.next(t); got.Updates[0].Ref != "refs/heads/o" || len(a.called) != 0 {
				t.Errorf("listener A was told of %s with %d calls pending, want only the accepted refs/heads/o", got.Updates[0].Ref, len(a.called))
			}
		})
	}
}

// deref is the session s names as a list, empty for none.
func deref(s *string) []string {
	if s == nil {
		return nil
	}
	return []string{*s}
}
