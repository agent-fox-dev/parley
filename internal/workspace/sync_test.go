package workspace_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestSync_FetchesWithCurrentCredential(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	rec, _ := s.workspace(t, syncSlug)
	// Each step changes the workspace's secrets ("" removes one) before its
	// sync, which must fetch with the credential they hold then.
	steps := []struct {
		name    string
		secrets map[string]string
		want    workspace.Credential
	}{
		{"a token", map[string]string{workspace.SecretGitPAT: "A"},
			workspace.Credential{Username: "x-token-auth", Password: "A"}},
		{"a user name and password", map[string]string{workspace.SecretGitPAT: "", workspace.SecretGitUsername: "u", workspace.SecretGitPassword: "p"},
			workspace.Credential{Username: "u", Password: "p"}},
		{"no credential", map[string]string{workspace.SecretGitUsername: "", workspace.SecretGitPassword: ""},
			workspace.Credential{}},
	}
	for i, step := range steps {
		for name, value := range step.secrets {
			if value == "" {
				s.deleteSecret(t, syncSlug, name)
			} else {
				s.putSecret(t, syncSlug, name, value)
			}
		}
		s.sync(t, workspace.SyncOptions{})
		calls := s.fetches.recorded()
		if len(calls) != i+1 {
			t.Fatalf("%s: %d fetches after %d syncs, want one per sync", step.name, len(calls), i+1)
		}
		got := calls[i]
		if got.cred != step.want || got.url != deref(rec.GitURL) || got.dir != s.copy {
			t.Errorf("%s: fetch of %s from %s with %+v; want %s from %s with %+v",
				step.name, got.dir, got.url, got.cred, s.copy, deref(rec.GitURL), step.want)
		}
	}
}

func TestSync_CreatesNewTagsAndKeepsExisting(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	origin := s.origin.Dir()
	// The cases run in order on one workspace; each sync must leave every
	// tag the hub had at its value and add each tag origin has and the hub
	// lacks at origin's value, an annotated tag as its tag object.
	cases := []struct {
		name   string
		change func(t *testing.T)
	}{
		{"origin has a new tag and a new annotated tag", func(t *testing.T) {
			s.origin.Tag("v2", "dev")
			gitrepotest.Git(t, origin, "tag", "--annotate", "--message=v3", "v3", "main")
		}},
		{"origin re-points v1 to another commit", func(t *testing.T) {
			gitrepotest.Git(t, origin, "update-ref", "refs/tags/v1", "refs/heads/dev")
		}},
		{"origin deletes v1", func(t *testing.T) {
			gitrepotest.Git(t, origin, "update-ref", "-d", "refs/tags/v1")
		}},
		{"a tag exists only in the hub", func(t *testing.T) {
			gitrepotest.Git(t, s.copy, "update-ref", "refs/tags/hubonly", "refs/heads/main")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.change(t)
			before := refsUnder(t, s.copy, "refs/tags/")
			created := map[string]string{}
			for name, id := range refsUnder(t, origin, "refs/tags/") {
				if _, had := before[name]; !had {
					created[name] = id
				}
			}
			r := s.sync(t, workspace.SyncOptions{})
			want := maps.Clone(before)
			maps.Copy(want, created)
			if got := refsUnder(t, s.copy, "refs/tags/"); !maps.Equal(got, want) {
				t.Errorf("the hub's tags = %v, want %v", got, want)
			}
			if !maps.Equal(r.CreatedTags, created) {
				t.Errorf("CreatedTags = %v, want %v", r.CreatedTags, created)
			}
		})
	}
}

func TestSync_PerBranchResults(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	set := s.branchSet(t)
	base, theirs, hubAhead, hubDiv := set.base, set.theirs, set.hubAhead, set.hubDiv
	up := workspace.BranchUpToDate
	steps := []struct {
		reset bool
		want  map[string]workspace.BranchOutcome
		heads map[string]string
	}{
		{false,
			map[string]workspace.BranchOutcome{"main": up, "dev": up, "new-b": workspace.BranchCreated, "same": up,
				"ff": workspace.BranchFastForward, "ahead-b": workspace.BranchAhead, "div": workspace.BranchDiverged},
			map[string]string{"new-b": theirs["new-b"], "ff": theirs["ff"], "same": base, "ahead-b": hubAhead, "div": hubDiv}},
		{true,
			map[string]workspace.BranchOutcome{"main": up, "dev": up, "new-b": up, "same": up,
				"ff": up, "ahead-b": workspace.BranchAhead, "div": workspace.BranchReset},
			map[string]string{"div": theirs["div"], "ahead-b": hubAhead}},
	}
	for _, step := range steps {
		r := s.sync(t, workspace.SyncOptions{ResetToOrigin: step.reset})
		if got := outcomes(r); !maps.Equal(got, step.want) {
			t.Errorf("reset %t: results = %v, want %v", step.reset, got, step.want)
		}
		for name, want := range step.heads {
			if got := ref(t, s.copy, "refs/heads/"+name); got != want {
				t.Errorf("reset %t: the hub's %s = %s, want %s", step.reset, name, got, want)
			}
		}
	}
	if slices.Contains(strings.Fields(gitrepotest.Git(t, s.copy, "rev-list", theirs["div"])), hubDiv) {
		t.Errorf("the hub's former commit on div is still an ancestor of its head after the reset")
	}
}

func TestSync_LeavesHubOnlyBranchesAlone(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	s.origin.Branch("gone", "main")
	s.sync(t, workspace.SyncOptions{})
	gitrepotest.Git(t, s.origin.Dir(), "update-ref", "-d", "refs/heads/gone")
	grow(t, s.copy, "local-only", s.origin.Head("main"), "hub")
	before := refsUnder(t, s.copy, "refs/heads/")
	for _, reset := range []bool{false, true} {
		r := s.sync(t, workspace.SyncOptions{ResetToOrigin: reset})
		for _, name := range []string{"local-only", "gone"} {
			if got := ref(t, s.copy, "refs/heads/"+name); got != before[name] {
				t.Errorf("reset %t: the hub's %s = %q, want %s unchanged", reset, name, got, before[name])
			}
			if _, listed := outcomes(r)[name]; listed {
				t.Errorf("reset %t: %s is in branches, which lists only origin's", reset, name)
			}
		}
	}
}

func TestSync_BranchesListShape(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	origin := s.origin.Dir()
	gitrepotest.Git(t, origin, "branch", "release/1.x", "main")
	grow(t, origin, "exp", s.origin.Head("main"), "origin")
	gitrepotest.Git(t, origin, "branch", "a/b", "dev")
	gitrepotest.Git(t, origin, "update-ref", "-d", "refs/heads/dev")
	theirs := refsUnder(t, origin, "refs/heads/")
	r := s.sync(t, workspace.SyncOptions{})
	// The entries as the route marshals them.
	var entries []map[string]string
	if b, err := json.Marshal(r.Branches); err != nil || json.Unmarshal(b, &entries) != nil {
		t.Fatalf("branches do not marshal: %v (%s)", err, b)
	}
	var names []string
	commit := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, e := range entries {
		name := e["name"]
		names = append(names, name)
		want := map[string]string{"name": name, "result": e["result"],
			"local_sha": ref(t, s.copy, "refs/heads/"+name), "origin_sha": theirs[name]}
		if !maps.Equal(e, want) || !commit.MatchString(e["local_sha"]) || !commit.MatchString(e["origin_sha"]) {
			t.Errorf("entry %v, want %v with full commit ids", e, want)
		}
	}
	if want := []string{"a/b", "exp", "main", "release/1.x"}; !slices.Equal(names, want) {
		t.Errorf("branches are %q, want %q", names, want)
	}

	for name := range theirs {
		gitrepotest.Git(t, origin, "update-ref", "-d", "refs/heads/"+name)
	}
	r = s.sync(t, workspace.SyncOptions{})
	if b, err := json.Marshal(r.Branches); err != nil || string(b) != "[]" {
		t.Errorf("branches of an origin without branches marshal as %s (%v), want []", b, err)
	}
}

func TestSync_MovesProtectedBranches(t *testing.T) {
	t.Parallel()
	for _, ownerPush := range []bool{true, false} {
		t.Run(fmt.Sprintf("protect.owner_push %t", ownerPush), func(t *testing.T) {
			t.Parallel()
			s := newSyncWorld(t)
			s.patch(t, s.uKey, syncSlug, map[string]any{"settings": map[string]any{
				"protect.owner_push": ownerPush, "protected_branches": []string{"main", "release/*"}}}).decode(t, http.StatusOK)
			origin := s.origin.Dir()
			// The cases run in order on the one workspace.
			cases := []struct {
				name, branch string
				reset        bool
				want         workspace.BranchOutcome
				change       func()
			}{
				{"origin adds a commit to main", "main", false, workspace.BranchFastForward, func() {
					grow(t, origin, "main", s.origin.Head("main"), "origin")
				}},
				{"main diverged", "main", true, workspace.BranchReset, func() {
					base := s.origin.Head("main")
					grow(t, origin, "main", base, "origin")
					grow(t, s.copy, "main", base, "hub")
				}},
				{"origin adds release/2", "release/2", false, workspace.BranchCreated, func() {
					grow(t, origin, "release/2", s.origin.Head("main"), "origin")
				}},
			}
			for _, c := range cases {
				c.change()
				r := s.sync(t, workspace.SyncOptions{ResetToOrigin: c.reset})
				if got := outcomes(r)[c.branch]; got != c.want {
					t.Errorf("%s: %s is %q, want %q", c.name, c.branch, got, c.want)
				}
				if got, want := ref(t, s.copy, "refs/heads/"+c.branch), s.origin.Head(c.branch); got != want {
					t.Errorf("%s: the hub's %s = %s, want origin's %s", c.name, c.branch, got, want)
				}
			}
		})
	}
}

func TestSync_ProtectedBranchAheadIsNormal(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	mine := grow(t, s.copy, "main", s.origin.Head("main"), "hub")
	for _, reset := range []bool{false, true} {
		r := s.sync(t, workspace.SyncOptions{ResetToOrigin: reset})
		i := slices.IndexFunc(r.Branches, func(b workspace.BranchResult) bool { return b.Name == "main" })
		if i < 0 || r.Branches[i].Result != workspace.BranchAhead || r.Branches[i].LocalSHA != mine {
			t.Errorf("reset %t: branches = %+v, want main ahead at %s", reset, r.Branches, mine)
		}
		if got := ref(t, s.copy, "refs/heads/main"); got != mine {
			t.Errorf("reset %t: the hub's main = %s, want %s unchanged", reset, got, mine)
		}
	}
}

func TestSync_RecordsCompletion(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	origin := s.origin.Dir()
	// The cases run in order on the one workspace; main is the result the
	// default branch has, "" when origin no longer has it.
	cases := []struct {
		name, main string
		change     func()
	}{
		{"all branches up_to_date", "up_to_date", func() {}},
		{"main ahead", "ahead", func() { grow(t, s.copy, "main", s.origin.Head("main"), "hub") }},
		{"a branch diverged", "ahead", func() {
			base := s.origin.Head("dev")
			grow(t, origin, "dev", base, "origin")
			grow(t, s.copy, "dev", base, "hub")
		}},
		{"main fast_forward", "fast_forward", func() {
			gitrepotest.Git(t, s.copy, "update-ref", "refs/heads/main", s.origin.Head("main"))
			grow(t, origin, "main", s.origin.Head("main"), "origin")
		}},
		{"origin has deleted the default branch", "", func() {
			gitrepotest.Git(t, origin, "update-ref", "-d", "refs/heads/main")
		}},
	}
	for i, c := range cases {
		c.change()
		if _, err := s.Store.Main().Exec(`UPDATE workspaces SET sync_error = 'old' WHERE slug = ?`, syncSlug); err != nil {
			t.Fatal(err)
		}
		s.advance(time.Minute)
		before, _ := s.workspace(t, syncSlug)
		obj, _ := syncBranches(t, s.postSync(t, s.uKey, syncSlug, ""))
		rec, _ := s.workspace(t, syncSlug)
		at := s.clock().UTC().Truncate(time.Second)
		head := ref(t, s.copy, "refs/heads/main")
		var originHead *string
		if id := s.origin.Head("main"); id != "" {
			originHead = &id
		}
		if rec.LastSyncAt == nil || !rec.LastSyncAt.Equal(at) || rec.SyncError != nil || deref(rec.HeadSHA) != head ||
			!reflect.DeepEqual(rec.OriginHeadSHA, originHead) || !rec.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("%s: the record has last_sync_at %v, sync_error %v, head_sha %v, origin_head_sha %v, updated_at %v; "+
				"want %v, nil, %s, %v, %v unchanged", c.name, rec.LastSyncAt, deref(rec.SyncError), deref(rec.HeadSHA),
				deref(rec.OriginHeadSHA), rec.UpdatedAt, at, head, deref(originHead), before.UpdatedAt)
		}
		var wantOrigin any
		if originHead != nil {
			wantOrigin = *originHead
		}
		got := map[string]any{"last_sync_at": obj["last_sync_at"], "sync_error": obj["sync_error"],
			"head_sha": obj["head_sha"], "origin_head_sha": obj["origin_head_sha"]}
		want := map[string]any{"last_sync_at": at.Format(time.RFC3339), "sync_error": nil, "head_sha": head, "origin_head_sha": wantOrigin}
		if !maps.Equal(got, want) {
			t.Errorf("%s: the answer carries %v, want %v", c.name, got, want)
		}
		events := s.eventsOf(t, syncEvent)
		if len(events) != i+1 {
			t.Fatalf("%s: %d sync events after %d syncs", c.name, len(events), i+1)
		}
		var meta map[string]any
		if err := json.Unmarshal(events[i].Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if list, ok := meta[c.main].([]any); c.main != "" && c.main != "up_to_date" && !slices.Contains(list, any("main")) {
			t.Errorf("%s: the event's metadata %v does not list main under %s (%v)", c.name, meta, c.main, ok)
		}
	}
}

func TestSync_EmitsOneEvent(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	set := s.branchSet(t)
	tag := s.origin.Tag("v2", "dev")
	steps := []struct {
		lists   map[string][]any
		updated []any
	}{
		{map[string][]any{"created": {"new-b"}, "fast_forward": {"ff"}, "ahead": {"ahead-b"}, "diverged": {"div"}, "reset": {}},
			[]any{
				map[string]any{"ref": "refs/heads/ff", "before": set.base, "after": set.theirs["ff"]},
				map[string]any{"ref": "refs/heads/new-b", "before": nil, "after": set.theirs["new-b"]},
				map[string]any{"ref": "refs/tags/v2", "before": nil, "after": tag},
			}},
		// Nothing left to move: only the branches that stay apart are listed.
		{map[string][]any{"created": {}, "fast_forward": {}, "ahead": {"ahead-b"}, "diverged": {"div"}, "reset": {}}, []any{}},
	}
	for i, step := range steps {
		total := s.eventCount(t)
		s.postSync(t, s.uKey, syncSlug, "reset_to_origin=false").decode(t, http.StatusOK)
		events := s.eventsOf(t, syncEvent)
		if n := s.eventCount(t); n != total+1 || len(events) != i+1 {
			t.Fatalf("sync %d: the log grew by %d events, %d of them sync events; want one", i+1, n-total, len(events))
		}
		ev := events[i]
		if deref(ev.Workspace) != syncSlug || ev.ResourceType != "workspace" || ev.ResourceID != syncSlug ||
			ev.ActorType != event.ActorAPIKey || deref(ev.ActorID) != s.u {
			t.Errorf("sync %d: event of %v, resource %s %s, actor %s %v; want %s, workspace %s, api_key %s",
				i+1, deref(ev.Workspace), ev.ResourceType, ev.ResourceID, ev.ActorType, deref(ev.ActorID), syncSlug, syncSlug, s.u)
		}
		var meta map[string]any
		if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"updated": step.updated}
		for name, list := range step.lists {
			want[name] = list
		}
		if !reflect.DeepEqual(meta, want) {
			t.Errorf("sync %d: metadata = %v, want %v", i+1, meta, want)
		}
	}
}

func TestSync_NotifiesBranchMoveObservers(t *testing.T) {
	t.Parallel()
	want := map[string]workspace.BranchOutcome{"main": workspace.BranchUpToDate, "dev": workspace.BranchUpToDate,
		"same": workspace.BranchUpToDate, "new-b": workspace.BranchCreated, "ff": workspace.BranchFastForward,
		"ahead-b": workspace.BranchAhead, "div": workspace.BranchReset}
	for _, observers := range []int{2, 0} {
		t.Run(fmt.Sprintf("%d observers", observers), func(t *testing.T) {
			t.Parallel()
			s := newSyncWorld(t)
			set := s.branchSet(t)
			var registered []*recordingObserver
			for range observers {
				registered = append(registered, s.observe(t, nil))
			}
			r := s.sync(t, workspace.SyncOptions{ResetToOrigin: true})
			if got := outcomes(r); !maps.Equal(got, want) {
				t.Errorf("results = %v, want %v", got, want)
			}
			// In the order of the branches, each told after the ref moved.
			moves := []workspace.BranchMove{
				{Workspace: syncSlug, Branch: "div", Before: set.hubDiv, After: set.theirs["div"], Cause: "sync"},
				{Workspace: syncSlug, Branch: "ff", Before: set.base, After: set.theirs["ff"], Cause: "sync"},
				{Workspace: syncSlug, Branch: "new-b", Before: "", After: set.theirs["new-b"], Cause: "sync"},
			}
			for i, o := range registered {
				got, heads := o.recorded()
				if !slices.Equal(got, moves) {
					t.Errorf("observer %d was told %+v, want %+v", i, got, moves)
				}
				for j, m := range got {
					if heads[j] != m.After {
						t.Errorf("observer %d: the hub's %s was %s when told of the move to %s", i, m.Branch, heads[j], m.After)
					}
				}
			}
		})
	}
}

func TestSync_ObserverFailureDoesNotFailSync(t *testing.T) {
	t.Parallel()
	var log hubtest.LogBuffer
	s := newSyncWorld(t, withLog(&log))
	base := s.origin.Head("main")
	gitrepotest.Git(t, s.copy, "branch", "ff", base)
	theirs := grow(t, s.origin.Dir(), "ff", base, "origin")
	s.observe(t, errors.New("observer out of order"))
	recorder := s.observe(t, nil)
	_, branches := syncBranches(t, s.postSync(t, s.uKey, syncSlug, ""))
	if got := outcomes(workspace.SyncResult{Branches: branches})["ff"]; got != workspace.BranchFastForward {
		t.Errorf("ff is %q, want fast_forward", got)
	}
	if got := ref(t, s.copy, "refs/heads/ff"); got != theirs {
		t.Errorf("the hub's ff = %s, want origin's %s", got, theirs)
	}
	if moves, _ := recorder.recorded(); len(moves) != 1 || moves[0].Branch != "ff" {
		t.Errorf("the observer after the failing one was told %+v, want the move of ff", moves)
	}
	var lines []string
	for line := range strings.Lines(log.String()) {
		if strings.Contains(line, "observer out of order") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "branch=ff") {
		t.Errorf("the log has %q about the failure, want one line naming the branch ff", lines)
	}
}
