package change_test

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// conflicting opens, in demo, a change from the branch p whose commits set
// the files of each of commits in turn, made on a commit of main that sets
// base, and then moves main past that commit to one that sets target; it
// returns the change and the commit main is at.
func (w *world) conflicting(t *testing.T, p string, base, target map[string]string, commits ...map[string]string) (change.Change, string) {
	t.Helper()
	from := commit(t, w.demo, "main", w.head(t, "main"), p+" base\n", base)
	head := from
	for i, files := range commits {
		head = commit(t, w.demo, p, head, fmt.Sprintf("%s %d\n", p, i+1), files)
	}
	ch := w.openBranch(t, p, nil)
	return ch, commit(t, w.demo, "main", from, p+" target\n", target)
}

// resolve sends the manual restack of the change ch with the resolutions
// as the owner, and fails the test unless it answers 200 restacked with
// the change at its next version, whose head the source branch is at and
// is built on onto; it returns that head.
func (w *world) resolve(t *testing.T, ch change.Change, onto string, resolutions ...map[string]any) string {
	t.Helper()
	status, out := w.restack(t, ch.Number, w.ownerKey, map[string]any{"resolutions": resolutions})
	got, _ := out["change"].(map[string]any)
	head, _ := got["head_sha"].(string)
	if status != http.StatusOK || out["status"] != "restacked" || got["version"] != float64(ch.Version+1) {
		t.Fatalf("restack demo#%d with %v = %d %v, want 200 restacked at version %d", ch.Number, resolutions, status, out, ch.Version+1)
	}
	if at := w.head(t, ch.Source.Branch); at != head {
		t.Errorf("the source branch is at %s, want the change's head %s", at, head)
	}
	if _, err := gitrepotest.Run(w.demo, "", "merge-base", "--is-ancestor", onto, head); err != nil {
		t.Errorf("the head %s is not built on %s: %v", head, onto, err)
	}
	return head
}

func TestRestackRoute_ReplaysTheChangeWithTheFilesItsCallerGives(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i, c := range []struct {
		name                  string
		path                  string
		base, target, changed string
		resolution            map[string]any
		want                  []byte
	}{
		{"a content conflict, given as text", "app.txt", "one\ntwo\nthree\n", "one\nTARGET\nthree\n", "one\nCHANGE\nthree\n",
			map[string]any{"content": "one\nMERGED\nthree\n"}, []byte("one\nMERGED\nthree\n")},
		{"a binary conflict, given in base64", "logo.bin", "\x00\x01base", "\x00\x01target", "\x00\x01change",
			map[string]any{"content_base64": "AAEC"}, []byte{0, 1, 2}},
		{"a file the target edits and the change deletes, deleted", "old.txt", "old\n", "target\n", gone,
			map[string]any{"delete": true}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("v%d", i)
			path := p + "-" + c.path
			ch, onto := w.conflicting(t, p, map[string]string{path: c.base}, map[string]string{path: c.target}, map[string]string{path: c.changed})
			entry := maps.Clone(c.resolution)
			entry["path"] = path

			head := w.resolve(t, ch, onto, entry)

			vs := w.versions(t, "demo", ch.Number)
			v := vs[len(vs)-1]
			by, _ := v["created_by"].(map[string]any)
			if v["number"] != float64(ch.Version+1) || v["kind"] != change.KindRestack || v["head_sha"] != head ||
				by["actor_type"] != change.CredentialAPIKey || deref(stringOf(by["actor_id"])) != w.owner {
				t.Errorf("the last version is %v, want version %d of kind restack at %s by the owner %s with their API key", v, ch.Version+1, head, w.owner)
			}
			if c.want == nil {
				if _, err := gitrepotest.Run(w.demo, "", "cat-file", "-e", head+":"+path); err == nil {
					t.Errorf("%s holds %s, want no such file", head, path)
				}
				return
			}
			if got := gitrepotest.Blob(t, w.demo, head+":"+path); !bytes.Equal(got, c.want) {
				t.Errorf("%s holds %s = %q, want %q", head, path, got, c.want)
			}
		})
	}
}

func TestRestackRoute_UsesAResolutionAtEveryCommitWhereItsPathConflicts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ch, onto := w.conflicting(t, "m",
		map[string]string{"run.sh": executable + "base\nrest\n"},
		map[string]string{"run.sh": executable + "target\nrest\n"},
		map[string]string{"run.sh": executable + "one\nrest\n"},
		map[string]string{"run.sh": executable + "two\nrest\n", "two.txt": "two\n"})

	head := w.resolve(t, ch, onto, map[string]any{"path": "run.sh", "content": "final\n"})

	commits := strings.Fields(gitrepotest.Git(t, w.demo, "rev-list", "--reverse", onto+".."+head))
	if len(commits) != 2 {
		t.Fatalf("the restack made the commits %q, want two", commits)
	}
	for _, c := range commits {
		if got := gitrepotest.Blob(t, w.demo, c+":run.sh"); string(got) != "final\n" {
			t.Errorf("commit %s holds run.sh = %q, want %q", c, got, "final\n")
		}
	}
	if mode := gitrepotest.Git(t, w.demo, "ls-tree", "--format=%(objectmode)", head, "run.sh"); mode != "100755" {
		t.Errorf("run.sh has the mode %s, want 100755, its mode in the restacked version", mode)
	}
}

func TestRestackRoute_IgnoresTheResolutionOfAPathThatDoesNotConflict(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	t.Run("a path the commit changes without a conflict, or does not change", func(t *testing.T) {
		ch, onto := w.conflicting(t, "i",
			map[string]string{"i-app.txt": "one\ntwo\n"},
			map[string]string{"i-app.txt": "one\nTARGET\n"},
			map[string]string{"i-app.txt": "one\nCHANGE\n", "i-notes.txt": "mine\n"})

		head := w.resolve(t, ch, onto,
			map[string]any{"path": "i-app.txt", "content": "ok\n"},
			map[string]any{"path": "i-notes.txt", "content": "IGNORED\n"},
			map[string]any{"path": "i-ghost.txt", "delete": true})

		if got := gitrepotest.Blob(t, w.demo, head+":i-notes.txt"); string(got) != "mine\n" {
			t.Errorf("i-notes.txt is %q, want the change's own %q", got, "mine\n")
		}
		if got := gitrepotest.Blob(t, w.demo, head+":i-app.txt"); string(got) != "ok\n" {
			t.Errorf("i-app.txt is %q, want the resolution %q", got, "ok\n")
		}
	})
	t.Run("a change already on its target head", func(t *testing.T) {
		commit(t, w.demo, "u", w.head(t, "main"), "U\n", map[string]string{"app.txt": "u\n"})
		ch := w.openBranch(t, "u", nil)

		status, out := w.restack(t, ch.Number, w.ownerKey, map[string]any{"resolutions": []any{map[string]any{"path": "app.txt", "content": "x"}}})

		got, _ := out["change"].(map[string]any)
		if status != http.StatusOK || out["status"] != "up_to_date" || got["version"] != float64(ch.Version) || w.head(t, "u") != ch.HeadSHA {
			t.Errorf("restack demo#%d = %d %v, branch at %s; want 200 up_to_date at version %d, the branch at %s",
				ch.Number, status, out, w.head(t, "u"), ch.Version, ch.HeadSHA)
		}
	})
}

func TestRestackRoute_FailsWhenAConflictedPathHasNoResolution(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ch, _ := w.conflicting(t, "n",
		map[string]string{"a.txt": "a\n", "b.txt": "b\n"},
		map[string]string{"a.txt": "target a\n", "b.txt": "target b\n"},
		map[string]string{"a.txt": "change a\n", "b.txt": "change b\n"})

	status, out := w.restack(t, ch.Number, w.ownerKey, map[string]any{"resolutions": []any{map[string]any{"path": "a.txt", "content": "x\n"}}})

	e, _ := out["error"].(map[string]any)
	details, _ := e["details"].(map[string]any)
	var paths []any
	files, _ := details["files"].([]any)
	for _, f := range files {
		paths = append(paths, f.(map[string]any)["path"])
	}
	commitSHA, _ := details["commit_sha"].(string)
	if status != http.StatusConflict || e["error_type"] != "rebase_conflict" || !slices.Contains(paths, any("b.txt")) ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commitSHA) {
		t.Errorf("restack demo#%d = %d %v, want 409 rebase_conflict with b.txt among the files of the report", ch.Number, status, out)
	}
	if now := w.read(t, ch.Number); now.Version != ch.Version || now.HeadSHA != ch.HeadSHA || w.head(t, "n") != ch.HeadSHA {
		t.Errorf("demo#%d at version %d and %s, the branch at %s; want %d, %s, %s",
			ch.Number, now.Version, now.HeadSHA, w.head(t, "n"), ch.Version, ch.HeadSHA, ch.HeadSHA)
	}
}

func TestRestackRoute_RefusesResolutionsThatAreNotAListOfValidEntries(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ch, _ := w.conflicting(t, "r",
		map[string]string{"a.txt": "a\n"}, map[string]string{"a.txt": "target\n"}, map[string]string{"a.txt": "change\n"})
	for _, c := range []struct {
		name        string
		resolutions any
	}{
		{"not a list", "x"},
		{"null", nil},
		{"an entry that is no object", []any{"a.txt"}},
		{"no path", []any{map[string]any{"content": "a"}}},
		{"an empty path", []any{map[string]any{"path": "", "content": "a"}}},
		{"a path that is no string", []any{map[string]any{"path": 1, "content": "a"}}},
		{"an absolute path", []any{map[string]any{"path": "/a.txt", "content": "a"}}},
		{"a .. segment", []any{map[string]any{"path": "a/../b", "content": "a"}}},
		{"an empty segment", []any{map[string]any{"path": "a//b", "content": "a"}}},
		{"a trailing slash", []any{map[string]any{"path": "a/", "content": "a"}}},
		{"a . segment", []any{map[string]any{"path": "./a", "content": "a"}}},
		{"no content and no delete", []any{map[string]any{"path": "a.txt"}}},
		{"content and delete", []any{map[string]any{"path": "a.txt", "content": "a", "delete": true}}},
		{"content and content_base64", []any{map[string]any{"path": "a.txt", "content": "a", "content_base64": "YQ=="}}},
		{"delete false", []any{map[string]any{"path": "a.txt", "delete": false}}},
		{"content that is no string", []any{map[string]any{"path": "a.txt", "content": 1}}},
		{"content_base64 that is not standard base64", []any{map[string]any{"path": "a.txt", "content_base64": "***"}}},
		{"content_base64 in the URL alphabet", []any{map[string]any{"path": "a.txt", "content_base64": "-_8="}}},
		{"a path two entries name", []any{map[string]any{"path": "a.txt", "content": "a"}, map[string]any{"path": "a.txt", "delete": true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, out := w.restack(t, ch.Number, w.ownerKey, map[string]any{"resolutions": c.resolutions})
			if status != http.StatusBadRequest || errorType(out) != "invalid_request" {
				t.Errorf("restack with resolutions %v = %d %v, want 400 invalid_request", c.resolutions, status, out)
			}
			if now := w.read(t, ch.Number); now.Version != ch.Version || w.head(t, "r") != ch.HeadSHA || now.NeedsRestack {
				t.Errorf("demo#%d at version %d, needs_restack %v, the branch at %s; want %d, no restack, %s",
					ch.Number, now.Version, now.NeedsRestack, w.head(t, "r"), ch.Version, ch.HeadSHA)
			}
		})
	}

	status, out := w.restack(t, ch.Number, w.ownerKey, map[string]any{"resolutions": []any{map[string]any{"path": "a.txt", "content": ""}}})
	if status != http.StatusOK || out["status"] != "restacked" {
		t.Errorf("restack with an empty file for a.txt = %d %v, want 200 restacked", status, out)
	}
}
