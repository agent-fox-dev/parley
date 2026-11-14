package workspace_test

import (
	"maps"
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
