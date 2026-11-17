package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/land"
)

// pathRepo is a bare repository created with git init in a temporary
// directory, opened with the hub's git, and its git directory.
func pathRepo(t *testing.T) (*gitrepo.Repo, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	gitrepotest.InitBare(t, dir, "main")
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, dir
}

func TestCheckPaths_AllowsAVersionOnlyWhenEveryFileItChangesMatchesAGlob(t *testing.T) {
	t.Parallel()
	repo, dir := pathRepo(t)
	base := commit(t, dir, "main", "", "base", map[string]string{"docs/a.md": "a\nb\nc\nd\n", "lib/z": "z\n"})
	many := map[string]treeEntry{"zz/last": {"100644", "last\n"}}
	for i := range 2001 {
		many[fmt.Sprintf("docs/many/%04d.md", i)] = treeEntry{"100644", strconv.Itoa(i) + "\n"}
	}
	for i, c := range []struct {
		name  string
		globs []string
		// entries are what the version's one commit on base sets, a zero
		// entry deleting the path.
		entries map[string]treeEntry
		refused string
	}{
		{name: "only docs changed", globs: []string{"docs/**"},
			entries: map[string]treeEntry{"docs/a.md": {"100644", "changed\n"}}},
		{name: "the first outside in byte order", globs: []string{"docs/**"}, refused: "src/a.go",
			entries: map[string]treeEntry{"docs/a.md": {"100644", "changed\n"}, "src/b.go": {"100644", "b\n"}, "src/a.go": {"100644", "a\n"}}},
		{name: "a rename out of the globs by its new path", globs: []string{"docs/**"}, refused: "src/a.md",
			entries: map[string]treeEntry{"docs/a.md": {}, "src/a.md": {"100644", "a\nb\nc\nd\n"}}},
		{name: "a rename into the globs by its old path", globs: []string{"src/**"}, refused: "docs/a.md",
			entries: map[string]treeEntry{"docs/a.md": {}, "src/a.md": {"100644", "a\nb\nc\nd\n"}}},
		{name: "a star does not cross a slash", globs: []string{"src/*.go"}, refused: "src/x/y.go",
			entries: map[string]treeEntry{"src/x/y.go": {"100644", "y\n"}}},
		{name: "files under each of two globs", globs: []string{"docs/**", "src/**"},
			entries: map[string]treeEntry{"docs/b.md": {"100644", "b\n"}, "src/b.go": {"100644", "b\n"}}},
		{name: "a deleted file", globs: []string{"docs/**"}, refused: "lib/z",
			entries: map[string]treeEntry{"lib/z": {}}},
		{name: "a file beyond the paths a change stores", globs: []string{"docs/**"}, refused: "zz/last", entries: many},
		// A credential without paths, which the hub's own writes carry
		// too, is held to nothing: neither a repository nor a version is
		// there to read.
		{name: "a credential without paths"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var r *gitrepo.Repo
			head := "0123456789abcdef0123456789abcdef01234567"
			if c.globs != nil {
				r, head = repo, commitEntries(t, dir, "v"+strconv.Itoa(i), base, "", c.name, c.entries)
			}

			refused, err := land.CheckPaths(context.Background(), r, c.globs, base, head)

			if err != nil || refused != c.refused {
				t.Errorf("CheckPaths(%q) = %q, %v; want %q", c.globs, refused, err, c.refused)
			}
		})
	}
}

func TestCheckPaths_ReturnsAnErrorWhenGitFails(t *testing.T) {
	t.Parallel()
	repo, dir := pathRepo(t)
	base := commit(t, dir, "main", "", "base", map[string]string{"docs/a.md": "a\n"})
	missing := "0123456789abcdef0123456789abcdef01234567"

	refused, err := land.CheckPaths(context.Background(), repo, []string{"docs/**"}, base, missing)

	if err == nil || refused != "" {
		t.Errorf("CheckPaths of a missing head = %q, %v; want an error and no path", refused, err)
	}
}

// docsPAT is a PAT of the owner that may write git and changes, limited to
// the paths under docs/, or to none for paths nil.
func (w *world) docsPAT(t *testing.T, paths []string) string {
	t.Helper()
	return w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:read", "git:write", "changes:read", "changes:write"},
		Paths: paths})
}

// pathCaller is the owner with a PAT limited to globs, as a change names
// them.
func (w *world) pathCaller(globs []string) change.Caller {
	return change.Caller{UserID: w.owner, Username: "owner", Credential: change.Credential{Type: change.CredentialPAT, ID: "p1"}, Paths: globs}
}

func TestPathLimits_LeaveACredentialWithoutPathsAndTheHubsOwnVersionsUnchecked(t *testing.T) {
	t.Parallel()
	t.Run("an unrestricted token restacks and lands", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		token := w.docsPAT(t, nil)
		commit(t, w.api, "feature", w.base, "src", map[string]string{"src/a.go": "package a\n"})
		ch := w.openBranch(t, "feature", w.caller())
		commit(t, w.api, "main", w.base, "moved", map[string]string{"m.txt": "m\n"})

		status, raw := w.Call(t, http.MethodPost, fmt.Sprintf("/api/v1/workspaces/api/changes/%d/restack", ch.Number), token, map[string]any{})
		if status != http.StatusOK {
			t.Fatalf("restack = %d %s, want 200", status, raw)
		}
		status, a := w.landCall(t, token, ch.Number, "", nil)
		if status != http.StatusOK || a["status"] != land.StatusLanded {
			t.Fatalf("land = %d %v, want 200 landed", status, a)
		}
	})
	t.Run("the hub restacks a change of a path-scoped token over files outside its paths", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.configure(t, `{"changes.auto_restack":true}`)
		low := commit(t, w.api, "low", w.base, "low", map[string]string{"src/l.go": "package l\n"})
		lower := w.openBranch(t, "low", w.caller())
		commit(t, w.api, "up", low, "up", map[string]string{"docs/u.md": "u\n"})
		// Opened through the component, since its first version holds the
		// change below's src/l.go, which the open's own path check refuses.
		upper := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: "up", Intent: new("why"), Kind: change.KindRef,
			Caller: w.pathCaller([]string{"docs/**"}), DependsOn: &lower.Number})
		commit(t, w.api, "main", w.base, "src moved", map[string]string{"src/m.go": "package m\n"})

		if status, a := w.landCall(t, w.ownerKey, lower.Number, "", nil); status != http.StatusOK {
			t.Fatalf("land of %s = %d %v, want 200", lower.Ref, status, a)
		}

		until(t, upper.Ref+" is restacked after "+lower.Ref+" landed", func() bool {
			return w.readChange(t, upper.Number).Version != upper.Version
		})
		versions, err := w.changes.ReadVersions(context.Background(), nil, "api", upper.Number)
		if err != nil {
			t.Fatal(err)
		}
		last := versions[len(versions)-1]
		if last.Kind != change.KindRestack || last.CreatedBy.ActorType != change.ActorSystem {
			t.Errorf("%s's last version is of kind %s by %s, want a restack by system", upper.Ref, last.Kind, last.CreatedBy.ActorType)
		}
	})
}

func TestPathLimits_HoldARefMoveOntoExistingCommitsToTheTokensPaths(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	docs := w.docsPAT(t, []string{"docs/**"})
	feature := commit(t, w.api, "feature", w.base, "docs", map[string]string{"docs/a.md": "a\n"})
	ch := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: "feature", Intent: new("why"), Kind: change.KindRef,
		Caller: w.pathCaller([]string{"docs/**"}), CheckPaths: true})
	// Both commits are in the repository before the move, on other
	// branches, so that the move adds no commit of its own.
	outside := commit(t, w.api, "other", feature, "src", map[string]string{"src/a.go": "package a\n"})
	inside := commit(t, w.api, "more", feature, "more docs", map[string]string{"docs/b.md": "b\n"})

	for _, c := range []struct {
		name, to string
		status   int
		path     string
	}{
		{name: "onto a commit touching src/a.go", to: outside, status: http.StatusForbidden, path: "src/a.go"},
		{name: "onto a commit touching docs only", to: inside, status: http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			at, version := head(t, w.api, "feature"), w.readChange(t, ch.Number).Version

			status, raw := w.Call(t, http.MethodPatch, "/api/v1/workspaces/api/scm/refs/feature", docs,
				map[string]any{"sha": c.to, "expected_sha": at})

			var answer map[string]any
			_ = json.Unmarshal(raw, &answer)
			errObj, _ := answer["error"].(map[string]any)
			if status != c.status || (c.path != "" && (errorType(answer) != "path_not_allowed" || errObj["path"] != c.path)) {
				t.Fatalf("move of feature = %d %s, want %d with error.path %q", status, raw, c.status, c.path)
			}
			want, wantVersion := c.to, version+1
			if c.path != "" {
				want, wantVersion = at, version
			}
			if got := head(t, w.api, "feature"); got != want {
				t.Errorf("feature is at %s, want %s", got, want)
			}
			if v := w.readChange(t, ch.Number).Version; v != wantVersion {
				t.Errorf("%s is at version %d, want %d", ch.Ref, v, wantVersion)
			}
		})
	}
}

func TestVersionFiles_AnswersTheFilesOfTheVersionAnUpdateMakes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	forkDir := w.fork(t, "api--f", w.owner)
	// main moves on after the fork was made, so that a version's files are
	// read from the merge base with the target's head, and a fork's from
	// its parent's.
	moved := commit(t, w.api, "main", w.base, "moved", map[string]string{"m.txt": "m\n"})
	// next is a new head on top of from on the branch of the repository at
	// dir, which adds docs/x.md.
	next := func(dir, branch, from string) string {
		return commit(t, dir, branch, from, "next", map[string]string{"docs/x.md": "x\n"})
	}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		// arrange returns the workspace, the branch and the new head the
		// lookup is asked about.
		arrange     func(t *testing.T) (slug, branch, head string)
		opensChange bool
		files       []string
	}{
		{name: "a branch of no change", arrange: func(t *testing.T) (string, string, string) {
			return "api", "plain", next(w.api, "plain", w.base)
		}},
		{name: "the source of an abandoned change", arrange: func(t *testing.T) (string, string, string) {
			ch := w.open(t, "abandoned")
			w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.Abandon(ctx, tx, "api", ch.Number, "", w.caller())
			})
			return "api", "abandoned", next(w.api, "abandoned", ch.HeadSHA)
		}},
		{name: "the source of a landed change", arrange: func(t *testing.T) (string, string, string) {
			ch := w.open(t, "landed")
			w.landNow(t, ch, w.caller(), nil)
			return "api", "landed", next(w.api, "landed", ch.HeadSHA)
		}},
		{name: "the source of a draft change", files: []string{"docs/x.md", "draft.txt"},
			arrange: func(t *testing.T) (string, string, string) {
				ch := w.openDraft(t, "draft")
				return "api", "draft", next(w.api, "draft", ch.HeadSHA)
			}},
		{name: "the source of an open change", files: []string{"docs/x.md", "opened.txt"},
			arrange: func(t *testing.T) (string, string, string) {
				ch := w.open(t, "opened")
				return "api", "opened", next(w.api, "opened", ch.HeadSHA)
			}},
		{name: "a branch of no change that opens one", opensChange: true, files: []string{"docs/x.md"},
			arrange: func(t *testing.T) (string, string, string) {
				return "api", "opening", next(w.api, "opening", moved)
			}},
		{name: "the source of a change from a fork", files: []string{"docs/x.md", "forked.txt"},
			arrange: func(t *testing.T) (string, string, string) {
				ch := w.openFrom(t, "api--f", forkDir, "forked", w.caller())
				return "api--f", "forked", next(forkDir, "forked", ch.HeadSHA)
			}},
		{name: "a branch of a fork that opens a change", opensChange: true, files: []string{"docs/x.md"},
			arrange: func(t *testing.T) (string, string, string) {
				return "api--f", "fork-opening", next(forkDir, "fork-opening", moved)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug, branch, head := c.arrange(t)

			files, found, err := w.land.VersionFiles().Files(ctx, slug, branch, head, c.opensChange)

			if err != nil || found != (c.files != nil) || !slices.Equal(files, c.files) {
				t.Errorf("Files(%s, %s, %t) = %q, %t, %v; want %q, %t", slug, branch, c.opensChange, files, found, err, c.files, c.files != nil)
			}
		})
	}
}

// refusal is the error_type and the member path of the error answer raw,
// "" for those it does not have.
func refusal(raw []byte) (errorType, path string) {
	var answer struct {
		Error struct {
			ErrorType string `json:"error_type"`
			Path      string `json:"path"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &answer)
	return answer.Error.ErrorType, answer.Error.Path
}

func TestRestack_HoldsTheVersionItWouldRecordToTheTokensPaths(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		globs []string
		// conflicting moves main over docs/a.md too, so that the restack
		// needs the resolution it is sent.
		conflicting bool
		refused     string
	}{
		{name: "a docs token without resolutions", globs: []string{"docs/**"}, refused: "src/b.go"},
		{name: "a docs token with a resolution for docs/a.md", globs: []string{"docs/**"}, conflicting: true, refused: "src/b.go"},
		{name: "a token whose globs cover every file", globs: []string{"docs/**", "src/**"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			commit(t, w.api, "feature", w.base, "change", map[string]string{"docs/a.md": "change\n", "src/b.go": "package b\n"})
			ch := w.openBranch(t, "feature", w.caller())
			moved := map[string]string{"m.txt": "m\n"}
			body := map[string]any{}
			if c.conflicting {
				moved["docs/a.md"] = "main\n"
				body["resolutions"] = []any{map[string]any{"path": "docs/a.md", "content": "resolved\n"}}
			}
			commit(t, w.api, "main", w.base, "moved", moved)
			at := head(t, w.api, "feature")

			status, raw := w.Call(t, http.MethodPost, fmt.Sprintf("/api/v1/workspaces/api/changes/%d/restack", ch.Number),
				w.docsPAT(t, c.globs), body)

			if c.refused != "" {
				if typ, path := refusal(raw); status != http.StatusForbidden || typ != "path_not_allowed" || path != c.refused {
					t.Fatalf("restack = %d %s, want 403 path_not_allowed with error.path %q", status, raw, c.refused)
				}
				if got := head(t, w.api, "feature"); got != at {
					t.Errorf("feature moved to %s, want it at %s", got, at)
				}
				if v := w.readChange(t, ch.Number).Version; v != ch.Version {
					t.Errorf("%s is at version %d, want %d", ch.Ref, v, ch.Version)
				}
				return
			}
			if status != http.StatusOK {
				t.Fatalf("restack = %d %s, want 200", status, raw)
			}
			versions, err := w.changes.ReadVersions(context.Background(), nil, "api", ch.Number)
			if err != nil {
				t.Fatal(err)
			}
			if last := versions[len(versions)-1]; len(versions) != 2 || last.Kind != change.KindRestack {
				t.Errorf("%s has %d versions, the last of kind %s; want 2, the last a restack", ch.Ref, len(versions), last.Kind)
			}
		})
	}
}

func TestLand_RefusesAChangeOutsideTheTokensPathsBeforeItsPreconditions(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	docs := w.docsPAT(t, []string{"docs/**"})
	src := map[string]string{"src/main.go": "package main\n"}
	for _, c := range []struct {
		name string
		// arrange returns the change to land, the land's body and the
		// changes the land would cover.
		arrange func(t *testing.T) (number int64, body any, covered []change.Change)
	}{
		{name: "a change touching src/main.go", arrange: func(t *testing.T) (int64, any, []change.Change) {
			commit(t, w.api, "solo", w.base, "solo", src)
			ch := w.openBranch(t, "solo", w.caller())
			return ch.Number, nil, []change.Change{ch}
		}},
		{name: "a stack whose lower change touches src/main.go", arrange: func(t *testing.T) (int64, any, []change.Change) {
			s := w.stack(t, "s", false, src, map[string]string{"docs/a.md": "a\n"})
			return s[1].Number, stacked, s
		}},
		{name: "a change touching src/main.go with another attempt running", arrange: func(t *testing.T) (int64, any, []change.Change) {
			commit(t, w.api, "busy", w.base, "busy", src)
			ch := w.openBranch(t, "busy", w.caller())
			w.insert(t, running(attemptID(1), "api", "main", time.Now().UTC(), ch.Number))
			return ch.Number, nil, []change.Change{ch}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			number, body, covered := c.arrange(t)
			for _, ch := range covered {
				w.mergeable(t, ch.Number, change.MergeableClean)
			}
			attempts, events, target := w.attempts(t), w.eventCount(t), head(t, w.api, "main")

			status, raw := w.post(t, docs, "api", number, "", body)

			if typ, path := refusal(raw); status != http.StatusForbidden || typ != "path_not_allowed" || path != "src/main.go" {
				t.Fatalf("land = %d %s, want 403 path_not_allowed with error.path src/main.go", status, raw)
			}
			if n := w.attempts(t); n != attempts {
				t.Errorf("%d land attempts are recorded, want %d", n, attempts)
			}
			if got := head(t, w.api, "main"); got != target {
				t.Errorf("main moved to %s, want it at %s", got, target)
			}
			if n := w.eventCount(t); n != events {
				t.Errorf("%d events are written, want %d", n, events)
			}
		})
	}
}

// A path-scoped token whose globs cover the change is answered by the
// policy as a credential of the same user without paths is.
func TestLand_AnswersATokenWithinItsPathsAsTheSameUserWithoutPaths(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.land.RegisterGate(w.policyGate)
	policy := map[string]any{"default": map[string]any{"approvals": 0},
		"rules": []any{map[string]any{"name": "docs", "paths": []string{"docs/**"}, "require": map[string]any{"approvals": 0, "checks": []string{"test"}}}}}
	if status, raw := w.Call(t, http.MethodPut, "/api/v1/workspaces/api/policy", w.ownerKey, policy); status != http.StatusOK {
		t.Fatalf("PUT api's policy = %d %s, want 200", status, raw)
	}
	for i, c := range []struct {
		name  string
		paths []string
	}{
		{name: "a token limited to docs", paths: []string{"docs/**"}},
		{name: "a token of the same user without paths"},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := "docs" + strconv.Itoa(i)
			commit(t, w.api, branch, w.base, branch, map[string]string{"docs/" + branch + ".md": "a\n"})
			ch := w.openBranch(t, branch, w.caller())
			token := w.docsPAT(t, c.paths)

			status, answer := w.landCall(t, token, ch.Number, "", nil)

			errObj, _ := answer["error"].(map[string]any)
			details, _ := errObj["details"].(map[string]any)
			blocked, _ := details["blocked"].(map[string]any)
			reqs, _ := blocked["requirements"].([]any)
			if status != http.StatusConflict || errorType(answer) != "policy_blocked" || details["status"] != land.StatusBlocked || len(reqs) == 0 {
				t.Fatalf("land before the check = %d %v, want 409 policy_blocked with the blocked attempt and its requirements", status, answer)
			}
			check := fmt.Sprintf("/api/v1/workspaces/api/changes/%d/checks/test", ch.Number)
			if status, raw := w.Call(t, http.MethodPut, check, w.ownerKey, map[string]any{"status": "passed"}); status != http.StatusOK && status != http.StatusCreated {
				t.Fatalf("PUT check test = %d %s, want it reported", status, raw)
			}

			status, answer = w.landCall(t, token, ch.Number, "", nil)

			if status != http.StatusOK || answer["status"] != land.StatusLanded {
				t.Errorf("land after the check = %d %v, want 200 landed", status, answer)
			}
		})
	}
}

func TestAutoLand_HoldsTheFlagATokenSetsToItsPaths(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	docs := w.docsPAT(t, []string{"docs/**"})
	for i, c := range []struct {
		name string
		// files are what the change, opened by the owner's key, sets; on
		// is its auto_land before the edit.
		files map[string]string
		on    bool
		body  map[string]any
		// refused is the path of a refusal, "" for an accepted edit.
		refused string
	}{
		{name: "the flag on over a file outside the globs", files: map[string]string{"src/main.go": "package main\n"},
			body: map[string]any{"auto_land": true, "title": "x"}, refused: "src/main.go"},
		{name: "the flag off over a file outside the globs", files: map[string]string{"src/main.go": "package main\n"}, on: true,
			body: map[string]any{"auto_land": false}},
		{name: "the flag on over files within the globs", files: map[string]string{"docs/a.md": "a\n"},
			body: map[string]any{"auto_land": true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := "flag" + strconv.Itoa(i)
			commit(t, w.api, branch, w.base, branch, c.files)
			ch := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: branch, Intent: new("why"), Kind: change.KindRef,
				AutoLand: new(c.on), Caller: w.caller()})

			status, answer := w.patchChange(t, docs, ch.Number, c.body)

			after := w.readChange(t, ch.Number)
			if c.refused != "" {
				errObj, _ := answer["error"].(map[string]any)
				if status != http.StatusForbidden || errorType(answer) != "path_not_allowed" || errObj["path"] != c.refused {
					t.Fatalf("PATCH %v = %d %v, want 403 path_not_allowed with error.path %s", c.body, status, answer, c.refused)
				}
				if after.AutoLand != c.on || after.Title != ch.Title {
					t.Errorf("the refused edit left auto_land %t and title %q, want %t and %q", after.AutoLand, after.Title, c.on, ch.Title)
				}
				return
			}
			want := c.body["auto_land"].(bool)
			if status != http.StatusOK || answer["auto_land"] != want || after.AutoLand != want {
				t.Fatalf("PATCH %v = %d %v, want 200 with auto_land %t", c.body, status, answer, want)
			}
			s, found, err := w.changes.AutoLandSetter(context.Background(), nil, "api", ch.Number)
			if err != nil {
				t.Fatal(err)
			}
			if want && (!found || s.CredentialType != change.CredentialPAT || !slices.Equal(s.Paths, []string{"docs/**"})) {
				t.Errorf("the flag's setter is %+v (found %t), want the PAT with its globs docs/**", s, found)
			}
		})
	}
}
