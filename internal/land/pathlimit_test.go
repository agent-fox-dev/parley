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
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
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
	} {
		t.Run(c.name, func(t *testing.T) {
			head := commitEntries(t, dir, "v"+strconv.Itoa(i), base, "", c.name, c.entries)

			refused, err := land.CheckPaths(context.Background(), repo, c.globs, base, head)

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

// A credential without paths, which the hub's own writes carry too, is
// held to nothing, and the repository is not read for it.
func TestCheckPaths_AllowsEveryVersionOfACredentialWithoutPaths(t *testing.T) {
	t.Parallel()

	refused, err := land.CheckPaths(context.Background(), nil, nil, "", "0123456789abcdef0123456789abcdef01234567")

	if err != nil || refused != "" {
		t.Errorf("CheckPaths without globs = %q, %v; want allowed", refused, err)
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

		deadline := time.Now().Add(30 * time.Second)
		for w.readChange(t, upper.Number).Version == upper.Version {
			if time.Now().After(deadline) {
				t.Fatalf("%s was not restacked after %s landed", upper.Ref, lower.Ref)
			}
			time.Sleep(20 * time.Millisecond)
		}
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
