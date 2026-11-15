package gitserver_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// changeFake is a change service that notes each call, in order, and
// answers with what the test scripted: the draft or open change of each
// branch, the refusal of an open, the change an open makes and the version
// a record makes.
type changeFake struct {
	found    map[string]*gitserver.BranchChange
	openErr  error
	opened   gitserver.OpenedChange
	recorded *pushrule.Version
	calls    []string
}

func (f *changeFake) OpenChangeOfBranch(_ context.Context, source, branch string) (*gitserver.BranchChange, error) {
	f.calls = append(f.calls, "find "+source+":"+branch)
	return f.found[branch], nil
}

func (f *changeFake) RecordPushVersion(_ context.Context, req gitserver.VersionRequest) (*pushrule.Version, error) {
	f.calls = append(f.calls, fmt.Sprintf("record %s#%d from %s:%s@%s %s", req.Workspace, req.Number, req.Source, req.Branch, req.Head, session(req.SessionID)))
	return f.recorded, nil
}

func (f *changeFake) OpenChange(_ context.Context, req gitserver.OpenRequest) (gitserver.OpenedChange, error) {
	f.calls = append(f.calls, fmt.Sprintf("open %s:%s@%s %s", req.Source, req.Branch, req.Head, session(req.SessionID)))
	return f.opened, f.openErr
}

func session(id *string) string {
	if id == nil {
		return "-"
	}
	return *id
}

// The ref updates of an accepted push, as the ref writer reports them.
func created(ref string) pushrule.UpdateResult {
	return pushrule.UpdateResult{Decision: pushrule.Decision{Update: pushrule.Update{Ref: ref, Before: pushrule.ZeroID, After: "c1"}, Allowed: true, Created: true}}
}

func updated(ref string) pushrule.UpdateResult {
	return pushrule.UpdateResult{Decision: pushrule.Decision{Update: pushrule.Update{Ref: ref, Before: "c0", After: "c1"}, Allowed: true}}
}

func deleted(ref string) pushrule.UpdateResult {
	return pushrule.UpdateResult{Decision: pushrule.Decision{Update: pushrule.Update{Ref: ref, Before: "c0", After: pushrule.ZeroID}, Allowed: true, Deleted: true}}
}

func direct(ref string) pushrule.UpdateResult {
	u := updated(ref)
	u.Direct = true
	return u
}

func versioned(ref string, v pushrule.Version) pushrule.UpdateResult {
	u := updated(ref)
	u.Version = &v
	return u
}

var (
	repoWS = workspace.Workspace{Slug: "demo", Kind: access.KindRepository}
	apiKey = gitserver.PushCaller{CredentialType: gitserver.CredentialAPIKey, CredentialID: "k1", UserID: "u1", Level: access.LevelOwner}
	gitPAT = gitserver.PushCaller{CredentialType: gitserver.CredentialPAT, CredentialID: "t1", UserID: "u1", Level: access.LevelOwner,
		PAT: &identity.Principal{Type: identity.TypePAT, CredentialID: "t1", Permissions: []string{"git:write"}}}
)

func forkWS(autoChange bool) workspace.Workspace {
	parent := "demo"
	return workspace.Workspace{Slug: "demo--k3x9w2ab", Kind: access.KindFork, ParentSlug: &parent, AutoChange: &autoChange}
}

func TestChangeMapper_MapsOnlyCreatedOrUpdatedBranchesInCommandOrder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		updates   []pushrule.UpdateResult
		wantCalls []string
		wantLines int
	}{
		{"tags, notes and deletes among branches", []pushrule.UpdateResult{created("refs/tags/v1"), updated("refs/heads/b"), deleted("refs/heads/gone"), created("refs/notes/x"), created("refs/heads/a")},
			[]string{"find demo:b", "open demo:b@c1 -", "find demo:a", "open demo:a@c1 -"}, 2},
		{"only a tag and a delete", []pushrule.UpdateResult{created("refs/tags/v1"), deleted("refs/heads/gone")}, nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := &changeFake{opened: gitserver.OpenedChange{Workspace: "demo", Number: 1}}
			var lines []string
			outcomes := gitserver.NewChangeMapper(fake, nil).Map(context.Background(), gitserver.MappingInput{
				Workspace: repoWS, Caller: apiKey, Options: []string{"parley.change"}, Updates: c.updates,
			}, func(l string) { lines = append(lines, l) })
			if !reflect.DeepEqual(fake.calls, c.wantCalls) {
				t.Errorf("calls %q, want %q", fake.calls, c.wantCalls)
			}
			if len(lines) != c.wantLines || len(outcomes) != c.wantLines {
				t.Fatalf("lines %q and %d outcomes, want %d of each", lines, len(outcomes), c.wantLines)
			}
			for i, o := range outcomes {
				if lines[i] != o.Line() {
					t.Errorf("line %d is %q, want the line of %+v", i, lines[i], o)
				}
			}
			if c.wantLines > 0 && outcomes[0].Branch != "b" {
				t.Errorf("first outcome is for %q, want b", outcomes[0].Branch)
			}
		})
	}
}

func TestChangeMapper_DecidesTheOutcomeOfEachBranch(t *testing.T) {
	t.Parallel()
	const s = "3f2c8a1e-5b7d-4c9e-8f10-2a3b4c5d6e7f"
	type mapCase struct {
		name     string
		ws       workspace.Workspace
		caller   gitserver.PushCaller
		options  []string
		session  *string
		updates  []pushrule.UpdateResult
		found    map[string]*gitserver.BranchChange
		openErr  error
		recorded *pushrule.Version
		calls    []string
		outcomes []gitserver.Outcome
	}
	opened := gitserver.Outcome{Branch: "b", Kind: gitserver.OutcomeOpened, Workspace: "demo", Number: 1, Version: 1}
	none := func(branch string) gitserver.Outcome { return gitserver.Outcome{Branch: branch} }
	directWarning := gitserver.Outcome{Branch: "main", Kind: gitserver.OutcomeWarning, Reason: "branch is protected and was pushed directly"}
	var cases []mapCase
	// A version the ref writer reported is the outcome whatever the push
	// asks, and nothing is looked up, opened or recorded for it.
	for _, opts := range [][]string{nil, {"parley.change"}, {"parley.target=other"}} {
		for _, caller := range []gitserver.PushCaller{apiKey, gitPAT} {
			cases = append(cases, mapCase{
				name: fmt.Sprintf("reported version, options %q, %s", opts, caller.CredentialType), ws: repoWS, caller: caller, options: opts,
				updates:  []pushrule.UpdateResult{versioned("refs/heads/b", pushrule.Version{WorkspaceSlug: "demo", Number: 7, Version: 3})},
				outcomes: []gitserver.Outcome{{Branch: "b", Kind: gitserver.OutcomeVersion, Workspace: "demo", Number: 7, Version: 3}},
			})
		}
	}
	// A push that asks for a change opens one for a branch without a draft
	// or open change.
	for _, opts := range [][]string{{"parley.change"}, {"parley.target=release"}, {"parley.change", "parley.target=release"}} {
		cases = append(cases, mapCase{
			name: fmt.Sprintf("asked for a change with %q", opts), ws: repoWS, caller: apiKey, options: opts,
			updates: []pushrule.UpdateResult{created("refs/heads/b")},
			calls:   []string{"find demo:b", "open demo:b@c1 -"}, outcomes: []gitserver.Outcome{opened},
		})
	}
	// A push without a request for a change opens one only in a task fork
	// that opens changes on its own.
	cases = append(cases,
		mapCase{name: "fork with auto_change, no options", ws: forkWS(true), caller: apiKey,
			updates: []pushrule.UpdateResult{created("refs/heads/b")},
			calls:   []string{"find demo--k3x9w2ab:b", "open demo--k3x9w2ab:b@c1 -"}, outcomes: []gitserver.Outcome{opened}},
		mapCase{name: "fork without auto_change, no options", ws: forkWS(false), caller: apiKey,
			updates: []pushrule.UpdateResult{created("refs/heads/b")},
			calls:   []string{"find demo--k3x9w2ab:b"}, outcomes: []gitserver.Outcome{none("b")}},
	)
	for _, ws := range []workspace.Workspace{repoWS, forkWS(false)} {
		for _, opts := range [][]string{nil, {"parley.title=x"}, {"parley.draft"}, {"parley.session=" + s}} {
			var sess *string
			if len(opts) == 1 && opts[0] == "parley.session="+s {
				sess = ptr(s)
			}
			cases = append(cases, mapCase{
				name: fmt.Sprintf("no change asked in %s with %q", ws.Slug, opts), ws: ws, caller: apiKey, options: opts, session: sess,
				updates: []pushrule.UpdateResult{updated("refs/heads/b")},
				calls:   []string{"find " + ws.Slug + ":b"}, outcomes: []gitserver.Outcome{none("b")},
			})
		}
	}
	// A direct push of a protected branch opens nothing, and is warned of
	// only when the push asked for a change.
	for _, ws := range []workspace.Workspace{repoWS, forkWS(true)} {
		for _, opts := range [][]string{{"parley.change"}, {"parley.target=release"}, nil} {
			want := none("main")
			if opts != nil {
				want = directWarning
			}
			cases = append(cases, mapCase{
				name: fmt.Sprintf("direct push in %s with %q", ws.Slug, opts), ws: ws, caller: apiKey, options: opts,
				updates: []pushrule.UpdateResult{direct("refs/heads/main")},
				calls:   []string{"find " + ws.Slug + ":main"}, outcomes: []gitserver.Outcome{want},
			})
		}
	}
	cases = append(cases,
		mapCase{name: "a direct push beside a branch that asks for a change", ws: repoWS, caller: apiKey, options: []string{"parley.change"},
			updates: []pushrule.UpdateResult{direct("refs/heads/main"), created("refs/heads/b")},
			calls:   []string{"find demo:main", "find demo:b", "open demo:b@c1 -"}, outcomes: []gitserver.Outcome{directWarning, opened}},
		// A draft or open change for which the ref writer reported no
		// version already has the pushed head.
		mapCase{name: "a change whose head is already the pushed one", ws: repoWS, caller: apiKey, options: []string{"parley.change"},
			updates: []pushrule.UpdateResult{updated("refs/heads/b")}, found: map[string]*gitserver.BranchChange{"b": {Workspace: "demo", Number: 4}},
			calls: []string{"find demo:b"}, outcomes: []gitserver.Outcome{none("b")}},
		// An open another push won becomes a version of its change.
		mapCase{name: "a lost race to open", ws: repoWS, caller: apiKey, options: []string{"parley.change"},
			updates: []pushrule.UpdateResult{created("refs/heads/b")}, openErr: &gitserver.ChangeExistsError{Workspace: "demo", Number: 9},
			recorded: &pushrule.Version{WorkspaceSlug: "demo", Number: 9, Version: 2},
			calls:    []string{"find demo:b", "open demo:b@c1 -", "record demo#9 from demo:b@c1 -"},
			outcomes: []gitserver.Outcome{{Branch: "b", Kind: gitserver.OutcomeVersion, Workspace: "demo", Number: 9, Version: 2}}},
		mapCase{name: "a lost race to a change that already has the head", ws: repoWS, caller: apiKey, options: []string{"parley.change"},
			updates: []pushrule.UpdateResult{created("refs/heads/b")}, openErr: &gitserver.ChangeExistsError{Workspace: "demo", Number: 9},
			calls:    []string{"find demo:b", "open demo:b@c1 -", "record demo#9 from demo:b@c1 -"},
			outcomes: []gitserver.Outcome{none("b")}},
		// The session of the push goes with every open and record.
		mapCase{name: "the session of an open", ws: repoWS, caller: apiKey, options: []string{"parley.change", "parley.session=" + s}, session: ptr(s),
			updates: []pushrule.UpdateResult{created("refs/heads/b")},
			calls:   []string{"find demo:b", "open demo:b@c1 " + s}, outcomes: []gitserver.Outcome{opened}},
		mapCase{name: "the session of a lost race", ws: repoWS, caller: apiKey, options: []string{"parley.change", "parley.session=" + s}, session: ptr(s),
			updates: []pushrule.UpdateResult{created("refs/heads/b")}, openErr: &gitserver.ChangeExistsError{Workspace: "demo", Number: 9},
			recorded: &pushrule.Version{WorkspaceSlug: "demo", Number: 9, Version: 2},
			calls:    []string{"find demo:b", "open demo:b@c1 " + s, "record demo#9 from demo:b@c1 " + s},
			outcomes: []gitserver.Outcome{{Branch: "b", Kind: gitserver.OutcomeVersion, Workspace: "demo", Number: 9, Version: 2}}},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := &changeFake{found: c.found, openErr: c.openErr, recorded: c.recorded, opened: gitserver.OpenedChange{Workspace: "demo", Number: 1}}
			var lines []string
			got := gitserver.NewChangeMapper(fake, nil).Map(context.Background(), gitserver.MappingInput{
				Workspace: c.ws, Caller: c.caller, Options: c.options, SessionID: c.session, Updates: c.updates,
			}, func(l string) { lines = append(lines, l) })
			if !reflect.DeepEqual(fake.calls, c.calls) {
				t.Errorf("calls %q, want %q", fake.calls, c.calls)
			}
			if !reflect.DeepEqual(got, c.outcomes) {
				t.Errorf("outcomes %+v, want %+v", got, c.outcomes)
			}
			var want []string
			for _, o := range c.outcomes {
				if l := o.Line(); l != "" {
					want = append(want, l)
				}
			}
			if !reflect.DeepEqual(lines, want) {
				t.Errorf("lines %q, want %q", lines, want)
			}
		})
	}
}
