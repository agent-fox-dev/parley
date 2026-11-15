package pushrule_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

const (
	msgNotUpdated = "not updated: another ref in this push was refused"
	msgRefMoved   = "ref changed since it was read; fetch and try again"
	msgInternal   = "internal error; try again"
)

func TestApply_WritesNoRefOfARequestWithARefNotAtItsBefore(t *testing.T) {
	const unknown = "1111111111111111111111111111111111111111"
	r := newRepo(t)
	protectedNo := pushrule.Decision{Update: move("refs/heads/main", r.head, r.side), Message: "main is protected and cannot be force-pushed or deleted", ErrorType: pushrule.ErrorTypeProtectedBranch}
	for _, tc := range []struct {
		name    string
		first   pushrule.Decision
		outcome string
		want    outcome
	}{
		{"a ref moved since its before", allowed(move("refs/heads/feat", r.base, r.side)).Decisions[0], pushrule.OutcomeRefMoved, refused(msgRefMoved, pushrule.ErrorTypeRefMoved)},
		{"a before no object has", allowed(move("refs/heads/feat", unknown, r.side)).Decisions[0], pushrule.OutcomeRefMoved, refused(msgRefMoved, pushrule.ErrorTypeRefMoved)},
		{"a create of a ref that exists", allowed(create("refs/heads/feat", r.side)).Decisions[0], pushrule.OutcomeRefMoved, refused(msgRefMoved, pushrule.ErrorTypeRefMoved)},
		{"an update of a missing ref", allowed(move("refs/heads/gone", r.head, r.side)).Decisions[0], pushrule.OutcomeRefMoved, refused(msgRefMoved, pushrule.ErrorTypeRefMoved)},
		{"a delete of a missing ref", allowed(remove("refs/heads/gone", r.head)).Decisions[0], pushrule.OutcomeRefMoved, refused(msgRefMoved, pushrule.ErrorTypeRefMoved)},
		{"a delete with a wrong before", allowed(remove("refs/heads/feat", r.base)).Decisions[0], pushrule.OutcomeRefMoved, refused(msgRefMoved, pushrule.ErrorTypeRefMoved)},
		{"a rule refused an update", protectedNo, pushrule.OutcomeRefused, outcomeOf(protectedNo)},
		{"a rule could not decide an update", pushrule.Decision{Update: create("refs/heads/x", r.side), Message: msgInternal, ErrorType: pushrule.ErrorTypeInternal}, pushrule.OutcomeInternal, refused(msgInternal, pushrule.ErrorTypeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log []string
			s := &stage{name: "s", log: &log, dir: r.dir}
			var w pushrule.Writer
			w.Bind(pushrule.WriterDeps{Transact: newStore(t).Tx})
			w.RegisterRefUpdatedStage(s.run)
			refsBefore := gitrepotest.Refs(t, r.dir, "")
			v := pushrule.Verdict{Decisions: []pushrule.Decision{tc.first, allowed(create("refs/heads/ok", r.head)).Decisions[0]}}
			res, err := w.Apply(context.Background(), pushrule.ApplyRequest{Verdict: v, Repo: r.view, Workspace: demo, Caller: ownerKey, Via: pushrule.ViaGit, VersionKind: pushrule.VersionKindPush})
			if err != nil || res.Outcome != tc.outcome || !res.Refused() || len(res.Updates) != 2 {
				t.Fatalf("Apply = %+v, %v, want outcome %s for both updates", res, err, tc.outcome)
			}
			if got := outcomeOf(res.Updates[0].Decision); got != tc.want {
				t.Errorf("the first update: %+v, want %+v", got, tc.want)
			}
			if got := outcomeOf(res.Updates[1].Decision); got != (outcome{Message: msgNotUpdated}) {
				t.Errorf("refs/heads/ok: %+v, want the not-updated message", got)
			}
			if got := gitrepotest.Refs(t, r.dir, ""); !maps.Equal(got, refsBefore) {
				t.Errorf("refs = %v, want them unchanged %v", got, refsBefore)
			}
			if len(log) != 0 {
				t.Errorf("stages called %v, want none", log)
			}
		})
	}
}

func TestApply_RunsTheRefUpdatedStagesInOrderInsideTheCommitStep(t *testing.T) {
	r := newRepo(t)
	st := newStore(t)
	var log []string
	first := &stage{name: "first", log: &log, dir: r.dir, write: true, versions: []*pushrule.Version{{WorkspaceSlug: "demo", Number: 4, Version: 2}}}
	second := &stage{name: "second", log: &log, dir: r.dir}
	var w pushrule.Writer
	w.Bind(pushrule.WriterDeps{Transact: st.Tx})
	w.RegisterRefUpdatedStage(first.run)
	w.RegisterRefUpdatedStage(second.run)
	after := commit(t, r.dir, []string{r.head}, "M src/b.go")
	updates := []pushrule.Update{move("refs/heads/feat", r.head, after), create("refs/heads/other", r.side)}
	var hookSaw string
	caller := ownerKey
	caller.CredentialID = "k1"
	session := "s1"
	res, err := w.Apply(context.Background(), pushrule.ApplyRequest{Verdict: allowed(updates...), Repo: r.view, Workspace: demo, Caller: caller,
		SessionID: &session, Via: pushrule.ViaAPI, VersionKind: pushrule.VersionKindCommit,
		BeforeCommit: func(context.Context) error {
			log = append(log, "hook")
			hookSaw = gitrepotest.Git(t, r.dir, "rev-parse", "refs/heads/feat")
			return nil
		}})
	if err != nil || res.Outcome != pushrule.OutcomeAccepted || res.Refused() || len(res.Updates) != 2 {
		t.Fatalf("Apply = %+v, %v, want both updates accepted", res, err)
	}
	if want := []string{"hook", "first", "second"}; !slices.Equal(log, want) || hookSaw != r.head {
		t.Errorf("calls %v with feat at %.7s before the stages, want %v with feat at its before", log, hookSaw, want)
	}
	wantSeen := []string{"refs/heads/feat=" + r.head[:7] + " readable=true", "refs/heads/other= readable=true"}
	for _, s := range []*stage{first, second} {
		if len(s.inputs) != 1 {
			t.Fatalf("stage %s called %d times, want once", s.name, len(s.inputs))
		}
		in := s.inputs[0]
		if in.Workspace != demo || in.Caller.CredentialID != "k1" || in.Caller.CredentialType != pushrule.CredentialAPIKey || in.SessionID == nil || *in.SessionID != "s1" ||
			in.Via != "api" || in.VersionKind != "commit" || len(in.Updates) != 2 || in.Updates[0].Update != updates[0] || !in.Updates[1].Created {
			t.Errorf("stage %s got %+v", s.name, in)
		}
		if !slices.Equal(s.seen, wantSeen) {
			t.Errorf("stage %s saw %v, want %v", s.name, s.seen, wantSeen)
		}
	}
	heads := gitrepotest.Refs(t, r.dir, "refs/heads/")
	if heads["feat"] != after || heads["other"] != r.side {
		t.Errorf("refs = %v, want feat at %.7s and other at %.7s", heads, after, r.side)
	}
	if got := stageRows(t, st); !slices.Equal(got, []string{"refs/heads/feat", "refs/heads/other"}) {
		t.Errorf("stage rows = %v, want the first stage's two rows", got)
	}
	if v := res.Updates[0].Version; v == nil || *v != (pushrule.Version{WorkspaceSlug: "demo", Number: 4, Version: 2}) || res.Updates[1].Version != nil {
		t.Errorf("versions = %v, %v, want {demo 4 2} and none", v, res.Updates[1].Version)
	}
}

func TestApply_RefusesTheWholeRequestAsInternalWhenTheCommitStepFails(t *testing.T) {
	stageErr := errors.New("stage failed")
	for _, tc := range []struct {
		name     string
		hookErr  error
		wantErr  error
		wantLog  []string
		noStages bool
	}{
		{name: "a stage fails", wantErr: stageErr, wantLog: []string{"first", "failing"}},
		{name: "the hook before the commit fails", hookErr: errInjected, wantErr: errInjected},
		{name: "no store to run the stages in", noStages: true, wantLog: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			st := newStore(t)
			var log []string
			var w pushrule.Writer
			if !tc.noStages {
				w.Bind(pushrule.WriterDeps{Transact: st.Tx})
			}
			w.RegisterRefUpdatedStage((&stage{name: "first", log: &log, dir: r.dir, write: true}).run)
			w.RegisterRefUpdatedStage((&stage{name: "failing", log: &log, dir: r.dir, err: stageErr}).run)
			w.RegisterRefUpdatedStage((&stage{name: "third", log: &log, dir: r.dir}).run)
			refsBefore := gitrepotest.Refs(t, r.dir, "")
			res, err := w.Apply(context.Background(), pushrule.ApplyRequest{Verdict: allowed(move("refs/heads/feat", r.head, r.side)), Repo: r.view,
				Workspace: demo, Caller: ownerKey, Via: pushrule.ViaGit, VersionKind: pushrule.VersionKindPush,
				BeforeCommit: func(context.Context) error { return tc.hookErr }})
			if tc.noStages {
				if err == nil {
					t.Error("Apply without a store returned no error")
				}
			} else if err != tc.wantErr {
				t.Errorf("Apply error = %v, want %v unchanged", err, tc.wantErr)
			}
			if res.Outcome != pushrule.OutcomeInternal || len(res.Updates) != 1 || outcomeOf(res.Updates[0].Decision) != refused(msgInternal, pushrule.ErrorTypeInternal) {
				t.Errorf("Apply = %+v, want every update refused as internal", res)
			}
			if got := gitrepotest.Refs(t, r.dir, ""); !maps.Equal(got, refsBefore) {
				t.Errorf("refs = %v, want them unchanged", got)
			}
			if got := stageRows(t, st); len(got) != 0 {
				t.Errorf("stage rows = %v, want none stored", got)
			}
			if tc.wantLog != nil && !slices.Equal(log, tc.wantLog) {
				t.Errorf("stages called %v, want %v", log, tc.wantLog)
			}
		})
	}
}

func TestApply_ReportsTheForkActivityAndTheNewHeadOnceTheRefsAreWritten(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ws         pushrule.Workspace
		via        string
		update     func(r *repo) pushrule.Update
		refuse     bool
		noRecorder bool
		failing    bool
		activity   []string
		heads      func(r *repo) []string
	}{
		{name: "git on a fork", ws: demoFk, via: "git", activity: []string{demoFk.Slug}},
		{name: "api on a fork", ws: demoFk, via: "api", activity: []string{demoFk.Slug}},
		{name: "land on a fork", ws: demoFk, via: "land"},
		{name: "git on a repository", ws: demo, via: "git"},
		{name: "a refused request on a fork", ws: demoFk, via: "git", refuse: true},
		{name: "the default branch of a repository", ws: demo, via: "land",
			update: func(r *repo) pushrule.Update { return move("refs/heads/main", r.head, r.side) },
			heads:  func(r *repo) []string { return []string{"demo " + r.side} }},
		{name: "the default branch of a fork", ws: demoFk, via: "git", activity: []string{demoFk.Slug},
			update: func(r *repo) pushrule.Update { return move("refs/heads/main", r.head, r.side) }},
		{name: "a recorder that fails", ws: demoFk, via: "git", failing: true, activity: []string{demoFk.Slug}},
		{name: "no recorder", ws: demoFk, via: "git", noRecorder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			u := create("refs/heads/topic", r.side)
			if tc.update != nil {
				u = tc.update(r)
			}
			v := allowed(u)
			if tc.refuse {
				v.Decisions = append(v.Decisions, pushrule.Decision{Update: create("refs/parley/x", r.side), Message: "refs/parley/ is maintained by the hub and cannot be pushed to", ErrorType: pushrule.ErrorTypeHubOwnedRef})
			}
			activity, heads := &reports{}, &reports{}
			if tc.failing {
				activity.err = errInjected
			}
			var w pushrule.Writer
			w.Bind(pushrule.WriterDeps{Heads: heads})
			if !tc.noRecorder {
				w.RegisterForkActivity(activity)
			}
			res, err := w.Apply(context.Background(), pushrule.ApplyRequest{Verdict: v, Repo: r.view, Workspace: tc.ws, Caller: ownerKey, Via: tc.via})
			wantAt := u.After
			if tc.refuse {
				wantAt = gitrepotest.Refs(t, r.dir, "")[u.Ref]
			}
			if err != nil || res.Refused() != tc.refuse {
				t.Errorf("Apply = %+v, %v, want refused %t", res, err, tc.refuse)
			}
			if got := gitrepotest.Refs(t, r.dir, "")[u.Ref]; got != wantAt {
				t.Errorf("%s = %.7s, want %.7s", u.Ref, got, wantAt)
			}
			if !slices.Equal(activity.got, tc.activity) {
				t.Errorf("fork activity %v, want %v", activity.got, tc.activity)
			}
			var wantHeads []string
			if tc.heads != nil {
				wantHeads = tc.heads(r)
			}
			if !slices.Equal(heads.got, wantHeads) {
				t.Errorf("heads set %v, want %v", heads.got, wantHeads)
			}
		})
	}
}

func TestApply_WritesALandVerdictWithoutAskingTheEngine(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	for _, tc := range []struct {
		u    pushrule.Update
		want outcome
	}{
		{create("refs/heads/n", r.head), outcome{Allowed: true, Created: true}},
		{remove("refs/heads/feat", r.head), outcome{Allowed: true, Deleted: true}},
		{move("refs/heads/feat", r.base, r.head), outcome{Allowed: true}},
		{move("refs/heads/feat", r.head, r.side), outcome{Allowed: true, Forced: true}},
	} {
		v, err := pushrule.AllowedVerdict(ctx, r.view, []pushrule.Update{tc.u})
		if err != nil || v.Refused() || len(v.Decisions) != 1 || v.Decisions[0].Update != tc.u || outcomeOf(v.Decisions[0]) != tc.want {
			t.Errorf("AllowedVerdict(%s %.7s..%.7s) = %+v, %v, want %+v", tc.u.Ref, tc.u.Before, tc.u.After, v, err, tc.want)
		}
	}

	lookup, cp, files := &tracking{}, &controller{}, &versionFiles{}
	e := engine(pushrule.EngineDeps{Settings: settings{Settings: pushrule.Settings{ProtectedBranches: []string{"main"}}}, Tracking: lookup, VersionFiles: files})
	e.RegisterController(cp)
	var log []string
	s := &stage{name: "s", log: &log, dir: r.dir}
	var w pushrule.Writer
	w.Bind(pushrule.WriterDeps{Transact: newStore(t).Tx})
	w.RegisterRefUpdatedStage(s.run)
	land := func(after string) pushrule.Result {
		t.Helper()
		v, err := pushrule.AllowedVerdict(ctx, r.view, []pushrule.Update{move("refs/heads/main", r.head, after)})
		if err != nil {
			t.Fatal(err)
		}
		res, err := w.Apply(ctx, pushrule.ApplyRequest{Verdict: v, Repo: r.view, Workspace: demo, Caller: adminToken, Via: pushrule.ViaLand, VersionKind: pushrule.VersionKindNone})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	b := commit(t, r.dir, []string{r.head}, "M src/b.go")
	if res := land(b); res.Outcome != pushrule.OutcomeAccepted || res.Updates[0].Direct {
		t.Errorf("the land = %+v, want accepted and not direct", res)
	}
	if got := gitrepotest.Git(t, r.dir, "rev-parse", "main"); got != b {
		t.Errorf("main = %.7s, want %.7s", got, b)
	}
	if len(s.inputs) != 1 || s.inputs[0].Via != "land" || s.inputs[0].Updates[0].Direct {
		t.Errorf("stage inputs %+v, want one call with via land and direct false", s.inputs)
	}
	if asked, forwarded := cp.counts(); len(lookup.asked)+len(files.asked)+asked+forwarded != 0 {
		t.Errorf("the engine's collaborators were asked: %v %v %d %d", lookup.asked, files.asked, asked, forwarded)
	}
	if res := land(r.side); res.Outcome != pushrule.OutcomeRefMoved {
		t.Errorf("a land from a stale before = %+v, want ref_moved", res)
	}
	if got := gitrepotest.Git(t, r.dir, "rev-parse", "main"); got != b {
		t.Errorf("main = %.7s after the stale land, want %.7s", got, b)
	}
}

func TestApply_RunsTheForwardsInCommandOrderBeforeAnyRefIsWritten(t *testing.T) {
	r := newRepo(t)
	f := &forwards{dir: r.dir}
	var w pushrule.Writer
	v := allowed(move("refs/heads/patch/a", r.head, r.side), move("refs/heads/feat", r.head, r.side), create("refs/heads/patch/b", r.side))
	v.Decisions[0].Forward = f.of("refs/heads/patch/a", nil, false)
	v.Decisions[2].Forward = f.of("refs/heads/patch/b", nil, false)
	res, err := w.Apply(context.Background(), pushrule.ApplyRequest{Verdict: v, Repo: r.view, Workspace: demoCP, Caller: ownerKey, Via: pushrule.ViaGit})
	if err != nil || res.Outcome != pushrule.OutcomeAccepted {
		t.Fatalf("Apply = %+v, %v, want accepted", res, err)
	}
	if want := []string{"refs/heads/patch/a=" + r.head, "refs/heads/patch/b="}; !slices.Equal(f.runs, want) {
		t.Errorf("forwards ran %v, want %v: each once, in command order, with its ref at its before", f.runs, want)
	}
	if heads := gitrepotest.Refs(t, r.dir, "refs/heads/"); heads["patch/a"] != r.side || heads["feat"] != r.side || heads["patch/b"] != r.side {
		t.Errorf("refs = %v, want patch/a, feat and patch/b at %.7s", heads, r.side)
	}

	refsBefore := gitrepotest.Refs(t, r.dir, "")
	v = allowed(move("refs/heads/patch/a", r.side, r.head))
	v.Decisions[0].Forward = f.of("refs/heads/patch/a", nil, false)
	v.Decisions = append(v.Decisions, pushrule.Decision{Update: create("refs/parley/x", r.side), Message: "refs/parley/ is maintained by the hub and cannot be pushed to", ErrorType: pushrule.ErrorTypeHubOwnedRef})
	if res, err := w.Apply(context.Background(), pushrule.ApplyRequest{Verdict: v, Repo: r.view, Workspace: demoCP, Caller: ownerKey, Via: pushrule.ViaGit}); err != nil || res.Outcome != pushrule.OutcomeRefused {
		t.Errorf("Apply of a refused verdict = %+v, %v, want refused", res, err)
	}
	if len(f.runs) != 2 {
		t.Errorf("forwards ran %v after a refused verdict, want no further run", f.runs)
	}
	if got := gitrepotest.Refs(t, r.dir, ""); !maps.Equal(got, refsBefore) {
		t.Errorf("refs = %v, want them unchanged", got)
	}
}

func TestApply_RefusesTheWholeRequestWhenAForwardFailsOrOutlastsItsLimit(t *testing.T) {
	if pushrule.DefaultForwardLimit != 120*time.Second {
		t.Errorf("the default forward limit is %v, want 2m0s", pushrule.DefaultForwardLimit)
	}
	for _, tc := range []struct {
		name    string
		err     error
		blocks  bool
		message string
	}{
		{name: "origin rejects the second forward", err: errors.New("origin rejected push: non-fast-forward"), message: "origin rejected push: non-fast-forward"},
		{name: "the second forward outlasts its limit", blocks: true, message: "origin rejected push: timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			f := &forwards{dir: r.dir}
			var log []string
			var w pushrule.Writer
			w.Bind(pushrule.WriterDeps{Transact: newStore(t).Tx, ForwardLimit: 50 * time.Millisecond})
			w.RegisterRefUpdatedStage((&stage{name: "s", log: &log, dir: r.dir}).run)
			v := allowed(move("refs/heads/patch/a", r.head, r.side), create("refs/heads/patch/b", r.side), create("refs/heads/patch/c", r.side))
			v.Decisions[0].Forward = f.of("refs/heads/patch/a", nil, false)
			v.Decisions[1].Forward = f.of("refs/heads/patch/b", tc.err, tc.blocks)
			v.Decisions[2].Forward = f.of("refs/heads/patch/c", nil, false)
			refsBefore := gitrepotest.Refs(t, r.dir, "")
			res, err := w.Apply(context.Background(), pushrule.ApplyRequest{Verdict: v, Repo: r.view, Workspace: demoCP, Caller: ownerKey, Via: pushrule.ViaGit})
			if err != nil || res.Outcome != pushrule.OutcomeRefused || !res.Refused() || len(res.Updates) != 3 {
				t.Fatalf("Apply = %+v, %v, want the request refused", res, err)
			}
			want := []outcome{{Message: msgNotUpdated}, refused(tc.message, pushrule.ErrorTypePatchBranch), {Message: msgNotUpdated}}
			for i, u := range res.Updates {
				if got := outcomeOf(u.Decision); got != want[i] {
					t.Errorf("%s: %+v, want %+v", u.Ref, got, want[i])
				}
			}
			if want := []string{"refs/heads/patch/a=" + r.head, "refs/heads/patch/b="}; !slices.Equal(f.runs, want) {
				t.Errorf("forwards ran %v, want %v: the earlier one once and left as it is, no later one", f.runs, want)
			}
			if tc.blocks && f.ctxErr == nil {
				t.Error("the forward that outlasted its limit did not see its context end")
			}
			if got := gitrepotest.Refs(t, r.dir, ""); !maps.Equal(got, refsBefore) {
				t.Errorf("refs = %v, want them unchanged %v", got, refsBefore)
			}
			if len(log) != 0 {
				t.Errorf("stages called %v, want none", log)
			}
		})
	}
}
