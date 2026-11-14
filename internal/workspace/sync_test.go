package workspace_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
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
	origin := s.origin.Dir()
	base := s.origin.Head("main")
	// From base, same is equal on both sides, new-b (absent from the hub)
	// and ff have a commit on origin only, ahead-b one in the hub only and
	// div one of its own on each side; main and dev are as the clone left
	// them.
	gitrepotest.Git(t, origin, "branch", "same", base)
	gitrepotest.Git(t, origin, "branch", "ahead-b", base)
	for _, b := range []string{"new-b", "ff", "div"} {
		grow(t, origin, b, base, "origin")
	}
	gitrepotest.Git(t, s.copy, "branch", "same", base)
	gitrepotest.Git(t, s.copy, "branch", "ff", base)
	hubAhead := grow(t, s.copy, "ahead-b", base, "hub")
	hubDiv := grow(t, s.copy, "div", base, "hub")
	theirs := refsUnder(t, origin, "refs/heads/")
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
