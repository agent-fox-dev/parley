package workspace_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
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
			before := gitrepotest.Refs(t, s.copy, "refs/tags/")
			created := map[string]string{}
			for name, id := range gitrepotest.Refs(t, origin, "refs/tags/") {
				if _, had := before[name]; !had {
					created[name] = id
				}
			}
			r := s.sync(t, workspace.SyncOptions{})
			want := maps.Clone(before)
			maps.Copy(want, created)
			if got := gitrepotest.Refs(t, s.copy, "refs/tags/"); !maps.Equal(got, want) {
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
	before := gitrepotest.Refs(t, s.copy, "refs/heads/")
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

func TestSync_ReportsOriginRefsAHubOnlyRefIsInTheWayOf(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	main := s.origin.Head("main")
	// The hub alone has feat, topic/a and the tag v9; origin has feat/x,
	// topic and v9/rc, which git cannot create beside them, and a new
	// branch and tag that it can.
	hubFeat, hubTopic := grow(t, s.copy, "feat", main, "hub"), grow(t, s.copy, "topic/a", main, "hub")
	gitrepotest.Git(t, s.copy, "update-ref", "refs/tags/v9", main)
	originFeat, originTopic := s.origin.Branch("feat/x", "dev"), s.origin.Branch("topic", "dev")
	s.origin.Tag("v9/rc", "dev")
	newB, v10 := s.origin.Branch("new-b", "dev"), s.origin.Tag("v10", "dev")
	steps := []struct {
		reset   bool
		newB    workspace.BranchOutcome
		created map[string]string
	}{
		{false, workspace.BranchCreated, map[string]string{"v10": v10}},
		{true, workspace.BranchUpToDate, map[string]string{}},
	}
	for i, step := range steps {
		r := s.sync(t, workspace.SyncOptions{ResetToOrigin: step.reset})
		want := map[string]workspace.BranchResult{
			"feat/x": {Name: "feat/x", Result: workspace.BranchNameConflict, OriginSHA: originFeat},
			"topic":  {Name: "topic", Result: workspace.BranchNameConflict, OriginSHA: originTopic},
			"new-b":  {Name: "new-b", Result: step.newB, LocalSHA: newB, OriginSHA: newB},
		}
		for _, b := range r.Branches {
			if w, ok := want[b.Name]; ok && b != w {
				t.Errorf("sync %d: branch %s = %+v, want %+v", i+1, b.Name, b, w)
			}
		}
		if got := outcomes(r); len(got) != len(want)+2 {
			t.Errorf("sync %d: results = %v, want feat/x, topic, new-b, main and dev", i+1, got)
		}
		heads := gitrepotest.Refs(t, s.copy, "refs/heads/")
		_, hasFeatX := heads["feat/x"]
		_, hasTopic := heads["topic"]
		if heads["feat"] != hubFeat || heads["topic/a"] != hubTopic || hasFeatX || hasTopic || heads["new-b"] != newB {
			t.Errorf("sync %d: the hub's branches = %v, want feat and topic/a unchanged, no feat/x or topic, new-b at %s", i+1, heads, newB)
		}
		tags := gitrepotest.Refs(t, s.copy, "refs/tags/")
		if _, hasRC := tags["v9/rc"]; tags["v9"] != main || hasRC || tags["v10"] != v10 {
			t.Errorf("sync %d: the hub's tags = %v, want v9 unchanged, no v9/rc, v10 at %s", i+1, tags, v10)
		}
		if !maps.Equal(r.CreatedTags, step.created) {
			t.Errorf("sync %d: CreatedTags = %v, want %v", i+1, r.CreatedTags, step.created)
		}
		if r.Workspace.LastSyncAt == nil || r.Workspace.SyncError != nil {
			t.Errorf("sync %d: the record's last_sync_at %v, sync_error %v; want a completion", i+1, r.Workspace.LastSyncAt, deref(r.Workspace.SyncError))
		}
		events := s.eventsOf(t, syncEvent)
		var meta map[string]any
		if len(events) != i+1 || json.Unmarshal(events[i].Metadata, &meta) != nil {
			t.Fatalf("sync %d: %d sync events, want %d", i+1, len(events), i+1)
		}
		if got, want := meta["name_conflict"], []any{"feat/x", "topic"}; !reflect.DeepEqual(got, want) {
			t.Errorf("sync %d: the event's name_conflict = %v, want %v", i+1, got, want)
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
	theirs := gitrepotest.Refs(t, origin, "refs/heads/")
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

func TestSync_TreatsProtectedBranchesLikeAnyOther(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	origin := s.origin.Dir()
	for _, ownerPush := range []bool{true, false} {
		s.patch(t, s.uKey, syncSlug, map[string]any{"settings": map[string]any{
			"protect.owner_push": ownerPush, "protected_branches": []string{"main", "release/*"}}}).decode(t, http.StatusOK)
		created := fmt.Sprintf("release/%t", ownerPush)
		// The rows run in order on the one workspace; change returns the head
		// the hub's branch must have after the sync, "" for origin's.
		rows := []struct {
			branch string
			reset  bool
			want   workspace.BranchOutcome
			change func() string
		}{
			{"main", false, workspace.BranchFastForward, func() string {
				gitrepotest.Git(t, s.copy, "update-ref", "refs/heads/main", s.origin.Head("main"))
				grow(t, origin, "main", s.origin.Head("main"), "origin")
				return ""
			}},
			{"main", true, workspace.BranchReset, func() string {
				base := s.origin.Head("main")
				grow(t, origin, "main", base, "origin")
				grow(t, s.copy, "main", base, "hub")
				return ""
			}},
			{created, false, workspace.BranchCreated, func() string {
				grow(t, origin, created, s.origin.Head("main"), "origin")
				return ""
			}},
			{"main", true, workspace.BranchAhead, func() string {
				return grow(t, s.copy, "main", s.origin.Head("main"), "hub")
			}},
		}
		for _, row := range rows {
			want := row.change()
			r := s.sync(t, workspace.SyncOptions{ResetToOrigin: row.reset})
			if want == "" {
				want = s.origin.Head(row.branch)
			}
			if got := outcomes(r)[row.branch]; got != row.want || r.Workspace.LastSyncAt == nil || r.Workspace.SyncError != nil {
				t.Errorf("owner_push %t: %s is %q (last_sync_at %v, sync_error %v), want %q in a completed sync",
					ownerPush, row.branch, got, r.Workspace.LastSyncAt, deref(r.Workspace.SyncError), row.want)
			}
			if got := ref(t, s.copy, "refs/heads/"+row.branch); got != want {
				t.Errorf("owner_push %t, %s: the hub's %s = %s, want %s", ownerPush, row.want, row.branch, got, want)
			}
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
		{map[string][]any{"created": {"new-b"}, "fast_forward": {"ff"}, "ahead": {"ahead-b"}, "diverged": {"div"}, "reset": {}, "name_conflict": {}},
			[]any{
				map[string]any{"ref": "refs/heads/ff", "before": set.base, "after": set.theirs["ff"]},
				map[string]any{"ref": "refs/heads/new-b", "before": nil, "after": set.theirs["new-b"]},
				map[string]any{"ref": "refs/tags/v2", "before": nil, "after": tag},
			}},
		// Nothing left to move: only the branches that stay apart are listed.
		{map[string][]any{"created": {}, "fast_forward": {}, "ahead": {"ahead-b"}, "diverged": {"div"}, "reset": {}, "name_conflict": {}}, []any{}},
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

func TestSync_FetchFailureChangesNothingButTheScrubbedSyncError(t *testing.T) {
	t.Parallel()
	refusing := gitrepotest.NewOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", "right"))
	refusing.History()
	s := newSyncWorld(t, withGitOrigin(refusing))
	s.sync(t, workspace.SyncOptions{})
	if calls := s.fetches.recorded(); len(calls) != 1 || calls[0].deadline != 2*time.Minute {
		t.Errorf("fetches %+v, want one with the deadline 2m", calls)
	}
	s.putSecret(t, syncSlug, workspace.SecretGitPAT, "s3cr3t")
	// Origins that moved on since: a fetch that got through would change
	// the copy's refs.
	for _, o := range []*gitrepotest.Origin{s.origin, refusing} {
		o.Commit("main", "after the last sync")
		o.Tag("v2", "dev")
	}
	closing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closing.Close() })
	go func() {
		for {
			conn, err := closing.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	// The cases run in order on the one workspace. url, when set, is
	// origin's URL from the case on; fetchErr is what the hub's fetcher
	// fails with instead of fetching, and fault the hub's write or read that
	// fails; cause is a part of the failure text.
	cases := []struct {
		name, url, fault, cause string
		fetchErr                error
	}{
		{name: "the origin path does not exist", url: filepath.Join(t.TempDir(), "gone.git"),
			cause: "does not appear to be a git repository"},
		{name: "origin answers 401 to the credential", url: refusing.URL(), cause: "Authentication failed"},
		{name: "origin closes the connection", url: "https://" + closing.Addr().String() + "/r.git",
			cause: closing.Addr().String()},
		{name: "the fetch reaches its deadline", fetchErr: fmt.Errorf("git fetch: %w (2m0s)", gitrepo.ErrDeadline),
			cause: gitrepo.ErrDeadline.Error()},
		{name: "the credential in a URL", cause: "https://example.test/r.git/': The requested URL returned error: 403",
			fetchErr: errors.New("fatal: unable to access 'https://x-token-auth:s3cr3t@example.test/r.git/': The requested URL returned error: 403")},
		{name: "the token alone", fetchErr: errors.New("remote: the token s3cr3t is not valid"), cause: "remote: the token"},
		{name: "a 5000-byte text", fetchErr: errors.New(strings.Repeat("e", 4993) + " s3cr3t"), cause: "eeee"},
		// No credential is not an anonymous fetch: nothing is fetched.
		{name: "the secrets cannot be read", fault: faultSecretRead, cause: "the secret read failed"},
	}
	for _, c := range cases {
		if c.url != "" {
			s.setGitURL(t, syncSlug, c.url)
		}
		if _, err := s.Store.Main().Exec(`UPDATE workspaces SET sync_error = NULL WHERE slug = ?`, syncSlug); err != nil {
			t.Fatal(err)
		}
		s.fetches.failWith(c.fetchErr)
		s.fail(c.fault)
		before, _ := s.workspace(t, syncSlug)
		refs := gitrepotest.Refs(t, s.copy, "")
		events, fetches := s.eventCount(t), len(s.fetches.recorded())
		a := s.postSync(t, s.uKey, syncSlug, "")
		errorType, message := a.refusal(t)
		rec, _ := s.workspace(t, syncSlug)
		if a.status != http.StatusBadGateway || errorType != "upstream_error" || message != deref(rec.SyncError) {
			t.Errorf("%s: answer %d %s with %q, sync_error %q; want 502 upstream_error with the same text", c.name,
				a.status, errorType, message, deref(rec.SyncError))
		}
		if text := deref(rec.SyncError); !strings.HasPrefix(text, "origin fetch failed: ") || !strings.Contains(text, c.cause) ||
			strings.Contains(text, "s3cr3t") || strings.Contains(text, "@example.test") || len(text) > 1024 {
			t.Errorf("%s: sync_error %q (%d bytes), want the scrubbed fetch failure naming %q", c.name, text, len(text), c.cause)
		}
		if got := gitrepotest.Refs(t, s.copy, ""); !maps.Equal(got, refs) {
			t.Errorf("%s: the hub's refs = %v, want %v unchanged", c.name, got, refs)
		}
		// Nothing but sync_error is written.
		rec.SyncError = before.SyncError
		if !reflect.DeepEqual(rec, before) {
			t.Errorf("%s: the record is %+v, want %+v but for sync_error", c.name, rec, before)
		}
		if n := s.eventCount(t); n != events {
			t.Errorf("%s: the log grew by %d events, want none", c.name, n-events)
		}
		want := 1
		if c.fault != "" {
			want = 0
		}
		if n := len(s.fetches.recorded()) - fetches; n != want {
			t.Errorf("%s: %d fetches, want %d", c.name, n, want)
		}
	}
}

func TestSync_ReevaluatesBranchThatMoved(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	observer := s.observe(t, nil)
	origin, base := s.origin.Dir(), s.origin.Head("main")
	// Each branch is behind origin's head (theirs, a child of base) or absent
	// in the hub, and another writer moves it once, between the sync's
	// evaluation and its swap; the sync must keep the writer's head and
	// report the result against it.
	cases := []struct {
		branch string
		inHub  bool
		move   func(theirs string) string
		want   workspace.BranchOutcome
	}{
		{"to-ahead", true, func(theirs string) string { return grow(t, s.copy, "to-ahead", theirs, "writer") },
			workspace.BranchAhead},
		{"to-diverged", true, func(string) string { return grow(t, s.copy, "to-diverged", base, "writer") },
			workspace.BranchDiverged},
		{"to-origin", true, func(theirs string) string {
			gitrepotest.Git(t, s.copy, "update-ref", "refs/heads/to-origin", theirs)
			return theirs
		}, workspace.BranchUpToDate},
		{"absent", false, func(string) string {
			gitrepotest.Git(t, s.copy, "update-ref", "refs/heads/absent", base)
			return base
		}, workspace.BranchFastForward},
	}
	theirs, moved := map[string]string{}, map[string]string{}
	for _, c := range cases {
		theirs[c.branch] = grow(t, origin, c.branch, base, "origin")
		if c.inHub {
			gitrepotest.Git(t, s.copy, "branch", c.branch, base)
		}
		s.swaps.set(c.branch, func(call int) {
			if call == 1 {
				moved[c.branch] = c.move(theirs[c.branch])
			}
		})
	}
	r := s.sync(t, workspace.SyncOptions{})
	for _, c := range cases {
		head := moved[c.branch]
		if c.want == workspace.BranchFastForward {
			head = theirs[c.branch]
		}
		i := slices.IndexFunc(r.Branches, func(b workspace.BranchResult) bool { return b.Name == c.branch })
		if i < 0 || r.Branches[i].Result != c.want || r.Branches[i].LocalSHA != head {
			t.Errorf("%s: branches = %+v, want %s at %s", c.branch, r.Branches, c.want, head)
		}
		if got := ref(t, s.copy, "refs/heads/"+c.branch); got != head {
			t.Errorf("%s: the hub's head = %s, want %s", c.branch, got, head)
		}
	}
	// Only the move that succeeded is told and listed, from the head the
	// writer left.
	want := workspace.BranchMove{Workspace: syncSlug, Branch: "absent", Before: base, After: theirs["absent"], Cause: "sync"}
	if got, _ := observer.recorded(); !slices.Equal(got, []workspace.BranchMove{want}) {
		t.Errorf("the observer was told %+v, want only %+v", got, want)
	}
	events := s.eventsOf(t, syncEvent)
	var meta struct{ Updated []map[string]any }
	if len(events) != 1 || json.Unmarshal(events[0].Metadata, &meta) != nil {
		t.Fatalf("sync events %+v, want one", events)
	}
	wantUpdated := []map[string]any{{"ref": "refs/heads/absent", "before": base, "after": theirs["absent"]}}
	if !reflect.DeepEqual(meta.Updated, wantUpdated) {
		t.Errorf("the event's updated = %v, want %v", meta.Updated, wantUpdated)
	}
}

func TestSync_GivesUpAfterFiveEvaluations(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	origin, base := s.origin.Dir(), s.origin.Head("main")
	for _, name := range []string{"aa", "zz"} {
		grow(t, origin, name, base, "origin")
		gitrepotest.Git(t, s.copy, "branch", name, base)
	}
	// The writer moves zz to a commit of its own on base before every swap,
	// so that each evaluation finds zz diverged again and moves on a stale
	// head. The hook runs in the hub's request, away from the test's
	// goroutine.
	s.swaps.set("zz", func(call int) {
		id := gitFrom(t, s.copy, "", "commit-tree", base+"^{tree}", "-p", base, "-m", fmt.Sprintf("writer %d", call))
		gitFrom(t, s.copy, "", "update-ref", "refs/heads/zz", id)
	})
	before, _ := s.workspace(t, syncSlug)
	events := s.eventCount(t)
	a := s.postSync(t, s.uKey, syncSlug, "reset_to_origin=true")
	if errorType, message := a.refusal(t); a.status != http.StatusInternalServerError || errorType != "internal" ||
		!strings.Contains(message, "zz") {
		t.Errorf("answer = %d %s, want 500 internal naming zz", a.status, a.body)
	}
	if n := s.swaps.count("zz"); n != 5 {
		t.Errorf("zz was evaluated for a swap %d times, want 5", n)
	}
	if rec, _ := s.workspace(t, syncSlug); !reflect.DeepEqual(rec, before) {
		t.Errorf("the record is %+v, want %+v unchanged", rec, before)
	}
	if n := s.eventCount(t); n != events {
		t.Errorf("the log grew by %d events, want none", n-events)
	}
	if got, want := ref(t, s.copy, "refs/heads/aa"), s.origin.Head("aa"); got != want {
		t.Errorf("the hub's aa = %s, want origin's %s, moved before zz", got, want)
	}
}

func TestSync_TwoConcurrentSyncs(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	origin, base := s.origin.Dir(), s.origin.Head("main")
	branches := []string{"b0", "b1", "b2", "b3", "b4"}
	for _, name := range branches {
		gitrepotest.Git(t, origin, "branch", name, base)
	}
	s.sync(t, workspace.SyncOptions{})
	heads := gitrepotest.Refs(t, origin, "refs/heads/")
	// In each round the first sync to evaluate a branch waits there for the
	// other's evaluation of it (or 5 s), so that both syncs have classified
	// the branch before either moves it, and the race shows in every round
	// rather than by chance.
	var mu sync.Mutex
	paired := map[string]chan struct{}{}
	for _, name := range branches {
		s.swaps.set(name, func(int) {
			mu.Lock()
			ch, seen := paired[name]
			switch {
			case !seen:
				ch = make(chan struct{})
				paired[name] = ch
			case ch != nil:
				close(ch)
				paired[name], ch = nil, nil
			}
			mu.Unlock()
			if ch != nil {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
				}
			}
		})
	}
	for round := range 2 {
		mu.Lock()
		clear(paired)
		mu.Unlock()
		if rec, _ := s.workspace(t, syncSlug); rec.Status != workspace.StatusReady {
			t.Fatalf("round %d: status %s before the syncs, want ready", round, rec.Status)
		}
		// A new commit on each branch, all in one fast-import.
		var stream strings.Builder
		for _, name := range branches {
			msg := fmt.Sprintf("round %d on %s", round, name)
			fmt.Fprintf(&stream, "commit refs/heads/%s\ncommitter Origin <origin@example.invalid> now\ndata %d\n%s\nfrom %s\n\n",
				name, len(msg), msg, heads[name])
		}
		gitFrom(t, origin, stream.String(), "fast-import", "--quiet", "--date-format=now")
		heads = gitrepotest.Refs(t, origin, "refs/heads/")

		var answers [2]answer
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range answers {
			wg.Go(func() {
				<-start
				answers[i] = s.postSync(t, s.uKey, syncSlug, "")
			})
		}
		close(start)
		wg.Wait()
		results := map[string][]workspace.BranchOutcome{}
		for i, a := range answers {
			if a.status != http.StatusOK {
				t.Fatalf("round %d: sync %d answered %d %s, want 200", round, i, a.status, a.body)
			}
			_, list := syncBranches(t, a)
			for _, b := range list {
				results[b.Name] = append(results[b.Name], b.Result)
			}
		}
		for name, got := range results {
			slices.Sort(got)
			want := []workspace.BranchOutcome{workspace.BranchUpToDate, workspace.BranchUpToDate}
			if slices.Contains(branches, name) {
				want = []workspace.BranchOutcome{workspace.BranchFastForward, workspace.BranchUpToDate}
			}
			if !slices.Equal(got, want) {
				t.Errorf("round %d: %s was reported %v, want %v", round, name, got, want)
			}
		}
		if got := gitrepotest.Refs(t, s.copy, "refs/heads/"); !maps.Equal(got, heads) {
			t.Fatalf("round %d: the hub's branches = %v, want origin's %v", round, got, heads)
		}
	}
	if rec, _ := s.workspace(t, syncSlug); rec.Status != workspace.StatusReady {
		t.Errorf("status %s after the syncs, want ready", rec.Status)
	}
}

func TestSync_CompletionRecordsTheCopysHeads(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	older := s.origin.Commit("main", "older")
	// A second sync runs while the first is about to move main, fetches a
	// newer main and completes first; the first sync's completion must not
	// record the older origin head it fetched.
	s.swaps.set("main", func(call int) {
		if call == 1 {
			s.origin.Commit("main", "newer")
			s.sync(t, workspace.SyncOptions{})
		}
	})
	r := s.sync(t, workspace.SyncOptions{})
	newer := s.origin.Head("main")
	if got := outcomes(r)["main"]; got != workspace.BranchAhead || newer == older {
		t.Fatalf("the first sync found main %q, want ahead of the %s it fetched", got, older)
	}
	rec, _ := s.workspace(t, syncSlug)
	if deref(rec.HeadSHA) != newer || deref(rec.OriginHeadSHA) != newer || deref(r.Workspace.OriginHeadSHA) != newer {
		t.Errorf("head_sha %s, origin_head_sha %s (answer %s); want both %s, as the copy holds them",
			deref(rec.HeadSHA), deref(rec.OriginHeadSHA), deref(r.Workspace.OriginHeadSHA), newer)
	}
}

func TestSync_SkipsCarryPatchBranches(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	origin, base := s.origin.Dir(), s.origin.Head("main")
	gitrepotest.Git(t, origin, "update-ref", "-d", "refs/heads/dev")
	for _, name := range []string{"integration", "patch/a", "feat"} {
		gitrepotest.Git(t, s.copy, "branch", name, base)
	}
	for _, name := range []string{"main", "integration", "patch/a", "patch/b", "feat"} {
		grow(t, origin, name, base, "origin")
	}
	hubIntegration := grow(t, s.copy, "integration", base, "hub")
	theirs := gitrepotest.Refs(t, origin, "refs/heads/")
	provider := &excluding{names: []string{"patch/a", "patch/b"}}
	s.svc.RegisterExcludedBranchProvider(provider)
	observer := s.observe(t, nil)
	s.setMode(t, workspace.ModeCarryPatch, "integration")

	r := s.sync(t, workspace.SyncOptions{ResetToOrigin: true})
	var names []string
	for _, b := range r.Branches {
		names = append(names, b.Name)
	}
	if want := []string{"feat", "main"}; !slices.Equal(names, want) {
		t.Errorf("branches are %q, want %q", names, want)
	}
	for name, want := range map[string]string{"integration": hubIntegration, "patch/a": base, "patch/b": ""} {
		if got := ref(t, s.copy, "refs/heads/"+name); got != want {
			t.Errorf("the hub's %s = %q, want %q unchanged", name, got, want)
		}
		if got := ref(t, s.copy, "refs/remotes/origin/"+name); got != theirs[name] {
			t.Errorf("the hub's origin head of %s = %q, want origin's %s", name, got, theirs[name])
		}
	}
	if got := provider.asked(); !slices.Equal(got, []string{syncSlug}) {
		t.Errorf("the provider was asked for %q, want once for %s", got, syncSlug)
	}
	moves, _ := observer.recorded()
	var told []string
	for _, m := range moves {
		told = append(told, m.Branch)
	}
	if !slices.Equal(told, names) {
		t.Errorf("the observer was told of %q, want %q", told, names)
	}
	events := s.eventsOf(t, syncEvent)
	if len(events) != 1 {
		t.Fatalf("%d sync events, want one", len(events))
	}
	for _, name := range []string{"integration", "patch/"} {
		if strings.Contains(string(events[0].Metadata), name) {
			t.Errorf("the event's metadata %s names %s", events[0].Metadata, name)
		}
	}

	// The same refs in a standard workspace: nothing is left out and the
	// provider is not asked.
	s.setMode(t, workspace.ModeStandard, "integration")
	r = s.sync(t, workspace.SyncOptions{ResetToOrigin: true})
	want := map[string]workspace.BranchOutcome{"feat": workspace.BranchUpToDate, "main": workspace.BranchUpToDate,
		"integration": workspace.BranchReset, "patch/a": workspace.BranchFastForward, "patch/b": workspace.BranchCreated}
	if got := outcomes(r); !maps.Equal(got, want) {
		t.Errorf("standard: results = %v, want %v", got, want)
	}
	if got := provider.asked(); len(got) != 1 {
		t.Errorf("standard: the provider was asked %d times in all, want only by the carry-patch sync", len(got))
	}
}

func TestSync_ExcludedBranchProviderFailure(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	s.setMode(t, workspace.ModeCarryPatch, "integration")
	s.origin.Commit("main", "after the clone")
	s.svc.RegisterExcludedBranchProvider(&excluding{err: errors.New("cannot read https://x:s3cr3t@example.test/patches.git")})
	before, _ := s.workspace(t, syncSlug)
	heads := gitrepotest.Refs(t, s.copy, "refs/heads/")
	events := s.eventCount(t)
	a := s.postSync(t, s.uKey, syncSlug, "")
	if errorType, message := a.refusal(t); a.status != http.StatusInternalServerError || errorType != "internal" ||
		!strings.Contains(message, "excluded-branch provider") || strings.Contains(message, "s3cr3t") {
		t.Errorf("answer = %d %s, want 500 internal naming the scrubbed provider failure", a.status, a.body)
	}
	if got := gitrepotest.Refs(t, s.copy, "refs/heads/"); !maps.Equal(got, heads) {
		t.Errorf("the hub's branches = %v, want %v unchanged", got, heads)
	}
	if rec, _ := s.workspace(t, syncSlug); !reflect.DeepEqual(rec, before) {
		t.Errorf("the record is %+v, want %+v unchanged", rec, before)
	}
	if n := s.eventCount(t); n != events {
		t.Errorf("the log grew by %d events, want none", n-events)
	}
}
