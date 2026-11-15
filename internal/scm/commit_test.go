package scm_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

// commitStream is the history of the workspace the commits are made on:
// main is one commit m1 holding README.md, api/old.go, docs/guide.md, the
// executable bin/run and the submodule entry vendor/lib, then m2, changing
// README.md.
const commitStream = "commit refs/heads/main\nmark :1\n" + testCommitter + "data 2\nm1\n" +
	"M 100644 inline README.md\ndata 7\nreadme\n\nM 100644 inline api/old.go\ndata 12\npackage api\n\n" +
	"M 100644 inline docs/guide.md\ndata 6\nguide\n\nM 100755 inline bin/run\ndata 4\nrun\n\n" +
	"M 160000 5555555555555555555555555555555555555555 vendor/lib\n\n" +
	"commit refs/heads/main\nmark :2\n" + testCommitter + "data 2\nm2\nfrom :1\n" +
	"M 100644 inline README.md\ndata 8\nreadme2\n\n"

// commitWorld is a writeWorld with the workspace cm of the owner, its
// repository made by commitStream, whose git directory is dir and whose
// main is m2 on top of m1.
type commitWorld struct {
	*writeWorld
	dir, m1, m2 string
	path        string
}

func newCommitWorld(t *testing.T, toml string) *commitWorld {
	t.Helper()
	w := &commitWorld{writeWorld: newWriteWorld(t, toml), path: scmPath("cm", "/commits")}
	w.dir = w.addWorkspace(t, "cm", w.org, commitStream)
	ids := strings.Fields(gitrepotest.Git(t, w.dir, "rev-parse", "main~1", "main"))
	w.m1, w.m2 = ids[0], ids[1]
	return w
}

// branch makes the branch name at main's head and returns that head.
func (w *commitWorld) branch(t *testing.T, name string) string {
	t.Helper()
	gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/"+name, w.m2)
	return w.m2
}

// head is the commit the branch name is at, "" when it does not exist.
func (w *commitWorld) head(t *testing.T, name string) string {
	t.Helper()
	return gitrepotest.Refs(t, w.dir, "refs/heads/")[name]
}

// put is a put operation writing content to path.
func put(path, content string) map[string]any {
	return map[string]any{"op": "put", "path": path, "content": content}
}

// answerCommit is the commit member of a commit answer.
func answerCommit(r reply) map[string]any {
	c, _ := r.json["commit"].(map[string]any)
	return c
}

// errorDetails are the details of an error envelope.
func errorDetails(r reply) map[string]any {
	e, _ := r.json["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

// TestCommit checks POST …/scm/commits on one workspace, each behaviour on
// its own branch.
func TestCommit(t *testing.T) {
	t.Parallel()
	w := newCommitWorld(t, "")

	t.Run("commits onto an existing branch at the expected head", func(t *testing.T) {
		w.branch(t, "work")
		for _, tc := range []struct {
			name string
			more map[string]any
		}{
			{"without base_ref", nil},
			{"with a base_ref naming nothing, which is ignored", map[string]any{"base_ref": "nope"}},
		} {
			head := w.head(t, "work")
			body := map[string]any{"branch": "work", "expected_head_sha": head, "message": "m",
				"operations": []any{put("a.txt", "hi "+tc.name)}}
			for k, v := range tc.more {
				body[k] = v
			}
			w.observed.reset()
			r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, body)
			c := answerCommit(r)
			if r.status != http.StatusCreated || c == nil {
				t.Fatalf("%s: %d %s, want 201 with a commit", tc.name, r.status, r.body)
			}
			sha, _ := c["sha"].(string)
			branch, _ := r.json["branch"].(map[string]any)
			if len(sha) != 40 || fmt.Sprint(c["parents"]) != fmt.Sprint([]any{head}) || c["message"] != "m\n" {
				t.Errorf("%s: commit %v, want a 40-character sha, parents [%s] and message %q", tc.name, c, head, "m\n")
			}
			if branch["name"] != "refs/heads/work" || branch["sha"] != sha {
				t.Errorf("%s: branch %v, want refs/heads/work at %s", tc.name, branch, sha)
			}
			if _, ok := c["stats"].(map[string]any); !ok {
				t.Errorf("%s: commit %v has no stats", tc.name, c)
			}
			for _, k := range []string{"change", "change_error"} {
				if v, ok := r.json[k]; !ok || v != nil {
					t.Errorf("%s: %s is %v (present %v), want null", tc.name, k, v, ok)
				}
			}
			if got := w.head(t, "work"); got != sha {
				t.Errorf("%s: work is at %s, want %s", tc.name, got, sha)
			}
			if got := gitrepotest.Git(t, w.dir, "show", "work:a.txt"); got != "hi "+tc.name {
				t.Errorf("%s: work:a.txt is %q", tc.name, got)
			}
			if seen := w.observed.reset(); !slices.Equal(seen, []string{"commit " + "ok"}) {
				t.Errorf("%s: the recorder saw %q, want [commit ok]", tc.name, seen)
			}
		}
	})

	t.Run("creates a branch on the commit base_ref names", func(t *testing.T) {
		for i, base := range []string{"main", "refs/heads/main", w.m2, w.m2[:7]} {
			name := "new-" + strconv.Itoa(i)
			seq := w.lastSeq(t)
			r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, map[string]any{"branch": name, "base_ref": base,
				"message": "m", "operations": []any{put("x", "y")}})
			c := answerCommit(r)
			if r.status != http.StatusCreated || fmt.Sprint(c["parents"]) != fmt.Sprint([]any{w.m2}) {
				t.Errorf("base_ref %s: %d %s, want 201 with parents [%s]", base, r.status, r.body, w.m2)
				continue
			}
			if got := w.head(t, name); got != c["sha"] {
				t.Errorf("base_ref %s: %s is at %q, want %v", base, name, got, c["sha"])
			}
			_, pushes := w.eventsAfter(t, seq)
			if len(pushes) != 1 || !pushes[0].Created || pushes[0].Via != pushrule.ViaAPI {
				t.Errorf("base_ref %s: push events %+v, want one creating %s via api", base, pushes, name)
			}
		}
	})

	t.Run("records the author and the hub as committer", func(t *testing.T) {
		w.branch(t, "authors")
		ctx := context.Background()
		if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE users SET full_name = 'Kim', email = 'kim@x' WHERE id = ?`, w.owner)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, credential string
			author           map[string]any
			wantName, wantAt string
		}{
			{"the request's author", w.ownerKey, map[string]any{"name": "A", "email": "a@x"}, "A", "a@x"},
			{"the caller", w.ownerKey, nil, "Kim", "kim@x"},
			{"the hub for the admin token", w.Token, nil, gitrepo.HubName, gitrepo.HubEmail},
		} {
			body := map[string]any{"branch": "authors", "expected_head_sha": w.head(t, "authors"), "message": "m",
				"operations": []any{put("who", tc.name)}}
			if tc.author != nil {
				body["author"] = tc.author
			}
			r := w.sendJSON(t, http.MethodPost, w.path, tc.credential, body)
			c := answerCommit(r)
			author, _ := c["author"].(map[string]any)
			committer, _ := c["committer"].(map[string]any)
			if r.status != http.StatusCreated || author["name"] != tc.wantName || author["email"] != tc.wantAt ||
				committer["name"] != gitrepo.HubName || committer["email"] != gitrepo.HubEmail {
				t.Errorf("%s: %d %s, want 201 by %s <%s> committed by the hub", tc.name, r.status, r.body, tc.wantName, tc.wantAt)
			}
			stored := strings.TrimSpace(gitrepotest.Git(t, w.dir, "log", "-1", "--format=%an|%ae|%cn|%ce", "authors"))
			if want := tc.wantName + "|" + tc.wantAt + "|" + gitrepo.HubName + "|" + gitrepo.HubEmail; stored != want {
				t.Errorf("%s: the stored commit says %q, want %q", tc.name, stored, want)
			}
		}
	})

	t.Run("refuses a head the caller has not seen", func(t *testing.T) {
		head := w.branch(t, "stale")
		for _, tc := range []struct {
			name     string
			expected any
		}{
			{"absent", nil},
			{"main's parent", w.m1},
			{"40 zeros", strings.Repeat("0", 40)},
		} {
			body := map[string]any{"branch": "stale", "message": "m", "operations": []any{put("a", "b")}}
			if tc.expected != nil {
				body["expected_head_sha"] = tc.expected
			}
			seq := w.lastSeq(t)
			w.observed.reset()
			r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, body)
			if r.status != http.StatusConflict || r.errorType() != "ref_moved" || errorDetails(r)["current_sha"] != head {
				t.Errorf("%s: %d %s, want 409 ref_moved with current_sha %s", tc.name, r.status, r.body, head)
			}
			if got := w.head(t, "stale"); got != head {
				t.Errorf("%s: stale moved to %s", tc.name, got)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
			if seen := w.observed.reset(); !slices.Equal(seen, []string{"commit ref_moved"}) {
				t.Errorf("%s: the recorder saw %q, want [commit ref_moved]", tc.name, seen)
			}
		}
	})

	t.Run("refuses a malformed request and moves no ref", func(t *testing.T) {
		head := w.branch(t, "shape")
		stale := w.m1
		puts := func(n int) []any {
			ops := make([]any, n)
			for i := range ops {
				ops[i] = put("many/f"+strconv.Itoa(i), "x")
			}
			return ops
		}
		on := func(ops ...any) map[string]any {
			return map[string]any{"branch": "shape", "expected_head_sha": stale, "message": "m", "operations": ops}
		}
		op := func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		}
		for _, tc := range []struct {
			name      string
			body      map[string]any
			status    int
			errorType string
		}{
			{"no branch", map[string]any{"base_ref": "main", "message": "m"}, 400, "invalid_request"},
			{"empty branch", map[string]any{"branch": "", "base_ref": "main", "message": "m"}, 400, "invalid_request"},
			{"branch with ..", map[string]any{"branch": "a..b", "base_ref": "main", "message": "m"}, 400, "invalid_request"},
			{"no message", map[string]any{"branch": "shape", "expected_head_sha": head}, 400, "invalid_request"},
			{"empty message", map[string]any{"branch": "shape", "expected_head_sha": head, "message": ""}, 400, "invalid_request"},
			{"operations not an array", map[string]any{"branch": "shape", "expected_head_sha": head, "message": "m", "operations": "put"}, 400, "invalid_request"},
			{"1001 operations", map[string]any{"branch": "shape", "expected_head_sha": head, "message": "m", "operations": puts(1001)}, 400, "invalid_request"},
			{"author without email", map[string]any{"branch": "shape", "expected_head_sha": head, "message": "m",
				"operations": []any{put("a", "b")}, "author": map[string]any{"name": "A"}}, 400, "invalid_request"},
			{"author without name", map[string]any{"branch": "shape", "expected_head_sha": head, "message": "m",
				"operations": []any{put("a", "b")}, "author": map[string]any{"email": "a@x"}}, 400, "invalid_request"},
			{"expected_head_sha not a full id", map[string]any{"branch": "shape", "expected_head_sha": head[:7], "message": "m",
				"operations": []any{put("a", "b")}}, 400, "invalid_request"},
			{"new branch without base_ref", map[string]any{"branch": "nobase", "message": "m", "operations": []any{put("a", "b")}}, 400, "invalid_request"},
			{"new branch whose base_ref names nothing", map[string]any{"branch": "nobase", "base_ref": "no-such-ref", "message": "m",
				"operations": []any{put("a", "b")}}, 404, "ref_not_found"},
			{"new branch below an existing branch", map[string]any{"branch": "shape/x", "base_ref": "main", "message": "m",
				"operations": []any{put("a", "b")}}, 409, "ref_exists"},
			// Malformed operations, each sent with a stale head, which is
			// never reached.
			{"op copy", on(op("op", "copy", "path", "a")), 400, "invalid_request"},
			{"no op", on(op("path", "a", "content", "x")), 400, "invalid_request"},
			{"put with an empty path", on(put("", "x")), 400, "invalid_request"},
			{"absolute path", on(put("/abs", "x")), 400, "invalid_request"},
			{"path with a . segment", on(put("a/./b", "x")), 400, "invalid_request"},
			{"path with a .. segment", on(put("a/../b", "x")), 400, "invalid_request"},
			{"path in .git", on(put(".git/config", "x")), 400, "invalid_request"},
			{"path with a deeper .git segment", on(put("x/.git/hooks/h", "x")), 400, "invalid_request"},
			{"path with a .git segment in another case", on(put("x/.GiT/h", "x")), 400, "invalid_request"},
			{"path with a .git segment HFS+ reads past an ignorable code point", on(put(".g\u200cit/config", "x")), 400, "invalid_request"},
			{"path with a .git segment HFS+ reads past a byte order mark", on(put("x/\ufeff.git/h", "x")), 400, "invalid_request"},
			{"path with the NTFS short name of .git", on(put("GIT~1/config", "x")), 400, "invalid_request"},
			{"path with a .git segment and a trailing dot", on(put("x/.git./h", "x")), 400, "invalid_request"},
			{"path with a .git segment and trailing spaces and dots", on(put(".git . /h", "x")), 400, "invalid_request"},
			{"path with a .git segment and an NTFS stream name", on(put(".git::$INDEX_ALLOCATION/h", "x")), 400, "invalid_request"},
			{"move to a .git lookalike", on(op("op", "move", "from", "README.md", "to", "git~1. /h")), 400, "invalid_request"},
			{"put with content and content_base64", on(op("op", "put", "path", "a", "content", "x", "content_base64", "eA==")), 400, "invalid_request"},
			{"put with neither content", on(op("op", "put", "path", "a")), 400, "invalid_request"},
			{"put with invalid base64", on(op("op", "put", "path", "a", "content_base64", "!!!")), 400, "invalid_request"},
			{"put mode 100600", on(op("op", "put", "path", "a", "content", "x", "mode", "100600")), 400, "invalid_request"},
			{"put mode 160000", on(op("op", "put", "path", "a", "content", "x", "mode", "160000")), 400, "invalid_request"},
			{"chmod without mode", on(op("op", "chmod", "path", "README.md")), 400, "invalid_request"},
			{"chmod mode 120000", on(op("op", "chmod", "path", "README.md", "mode", "120000")), 400, "invalid_request"},
			{"move without to", on(op("op", "move", "from", "README.md")), 400, "invalid_request"},
			{"delete without path", on(op("op", "delete")), 400, "invalid_request"},
		} {
			refs, seq := gitrepotest.Git(t, w.dir, "for-each-ref"), w.lastSeq(t)
			r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, tc.body)
			if r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("%s: %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.errorType)
			}
			if after := gitrepotest.Git(t, w.dir, "for-each-ref"); after != refs {
				t.Errorf("%s: refs are\n%s\nwant them unchanged\n%s", tc.name, after, refs)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
		}
		r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, map[string]any{"branch": "shape",
			"expected_head_sha": head, "message": "m", "operations": puts(1000)})
		if r.status != http.StatusCreated {
			t.Errorf("1000 operations: %d %s, want 201", r.status, r.body)
		}
	})

	t.Run("refuses an operation that does not apply and names it", func(t *testing.T) {
		head := w.branch(t, "ops")
		for _, tc := range []struct {
			name string
			op   map[string]any
		}{
			{"delete of a missing path", map[string]any{"op": "delete", "path": "missing.txt"}},
			{"move of a missing path", map[string]any{"op": "move", "from": "missing.txt", "to": "x"}},
			{"move onto an existing file", map[string]any{"op": "move", "from": "a.txt", "to": "bin/run"}},
			{"move onto a directory", map[string]any{"op": "move", "from": "a.txt", "to": "docs"}},
			{"chmod of a missing path", map[string]any{"op": "chmod", "path": "missing.txt", "mode": "100755"}},
			{"put under a file", put("a.txt/child", "x")},
			{"put onto a directory", put("docs", "x")},
			{"put onto a submodule", put("vendor/lib", "x")},
			{"delete of a submodule", map[string]any{"op": "delete", "path": "vendor/lib"}},
			{"put under a submodule", put("vendor/lib/x", "x")},
		} {
			seq := w.lastSeq(t)
			r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, map[string]any{"branch": "ops",
				"expected_head_sha": head, "message": "m", "operations": []any{put("a.txt", "a"), tc.op}})
			if index, ok := errorDetails(r)["index"].(float64); r.status != http.StatusBadRequest || r.errorType() != "invalid_operation" || !ok || index != 1 {
				t.Errorf("%s: %d %s, want 400 invalid_operation with details.index 1", tc.name, r.status, r.body)
			}
			if got := w.head(t, "ops"); got != head {
				t.Errorf("%s: ops moved to %s", tc.name, got)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
		}
	})

	t.Run("refuses a commit that changes nothing", func(t *testing.T) {
		head := w.branch(t, "empty")
		for _, tc := range []struct {
			name string
			ops  any
		}{
			{"no operations", []any{}},
			{"operations absent", nil},
			{"a put of the current content", []any{put("README.md", "readme2\n")}},
			{"a put undone by a delete", []any{put("t", "x"), map[string]any{"op": "delete", "path": "t"}}},
		} {
			body := map[string]any{"branch": "empty", "expected_head_sha": head, "message": "m"}
			if tc.ops != nil {
				body["operations"] = tc.ops
			}
			r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, body)
			if r.status != http.StatusBadRequest || r.errorType() != "empty_commit" {
				t.Errorf("%s: %d %s, want 400 empty_commit", tc.name, r.status, r.body)
			}
			if got := w.head(t, "empty"); got != head {
				t.Errorf("%s: empty moved to %s", tc.name, got)
			}
		}
		r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, map[string]any{"branch": "empty", "expected_head_sha": head,
			"message": "m", "operations": []any{put("README.md", "readme2\n"), put("new.txt", "x")}})
		if r.status != http.StatusCreated {
			t.Errorf("one operation that changes the tree: %d %s, want 201", r.status, r.body)
		}
	})

	t.Run("makes an empty commit when allowed", func(t *testing.T) {
		head := w.branch(t, "marker")
		tree := strings.TrimSpace(gitrepotest.Git(t, w.dir, "rev-parse", head+"^{tree}"))
		r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, map[string]any{"branch": "marker", "expected_head_sha": head,
			"message": "marker", "operations": []any{}, "allow_empty": true})
		c := answerCommit(r)
		if r.status != http.StatusCreated || c["tree_sha"] != tree || fmt.Sprint(c["parents"]) != fmt.Sprint([]any{head}) {
			t.Errorf("%d %s, want 201 with tree_sha %s and parents [%s]", r.status, r.body, tree, head)
		}
	})

	t.Run("reads a body over the server's own limit", func(t *testing.T) {
		head := w.branch(t, "big")
		content := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 3<<19)))
		body := map[string]any{"branch": "big", "expected_head_sha": head, "message": "m",
			"operations": []any{map[string]any{"op": "put", "path": "big.bin", "content_base64": content}}}
		if b, _ := json.Marshal(body); len(b) < 2<<20 {
			t.Fatalf("the body is %d bytes, want at least 2 MiB", len(b))
		}
		if r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, body); r.status != http.StatusCreated {
			t.Errorf("a 2 MiB body: %d %.200s, want 201", r.status, r.body)
		}
	})

	t.Run("lets exactly one of concurrent commits move the branch", func(t *testing.T) {
		head := w.branch(t, "race")
		seq := w.lastSeq(t)
		replies := make([]reply, 8)
		var wg sync.WaitGroup
		for i := range replies {
			wg.Go(func() {
				replies[i] = w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, map[string]any{"branch": "race",
					"expected_head_sha": head, "message": "m", "operations": []any{put("r"+strconv.Itoa(i), "x")}})
			})
		}
		wg.Wait()
		var winner string
		for _, r := range replies {
			if r.status == http.StatusCreated {
				if winner != "" {
					t.Fatalf("two commits answered 201")
				}
				winner, _ = answerCommit(r)["sha"].(string)
			}
		}
		if winner == "" {
			for _, r := range replies {
				t.Errorf("%d %s", r.status, r.body)
			}
			t.Fatal("no commit answered 201")
		}
		for _, r := range replies {
			if r.status == http.StatusCreated {
				continue
			}
			if cur := errorDetails(r)["current_sha"]; r.status != http.StatusConflict || r.errorType() != "ref_moved" || (cur != winner && cur != head) {
				t.Errorf("a loser answered %d %s, want 409 ref_moved with current_sha %s or %s", r.status, r.body, winner, head)
			}
		}
		if n := strings.TrimSpace(gitrepotest.Git(t, w.dir, "rev-list", "--count", head+"..race")); n != "1" {
			t.Errorf("race is %s commits past the old head, want 1", n)
		}
		if _, pushes := w.eventsAfter(t, seq); len(pushes) != 1 || pushes[0].After == nil || *pushes[0].After != winner {
			t.Errorf("push events %+v, want one, for %s", pushes, winner)
		}
	})
}

// TestCommit_RefusesABodyOverTheCommitLimit checks that the commit route's
// body limit is [scm].max_commit_size: a larger body is refused before it
// is read, one within it is taken.
func TestCommit_RefusesABodyOverTheCommitLimit(t *testing.T) {
	t.Parallel()
	w := newCommitWorld(t, "[scm]\nmax_commit_size = \"1KB\"\n")
	head := w.branch(t, "small")
	sized := func(size int) json.RawMessage {
		body := map[string]any{"branch": "small", "expected_head_sha": head, "operations": []any{put("s", "x")}}
		body["message"] = ""
		b, _ := json.Marshal(body)
		body["message"] = "m" + strings.Repeat("x", size-len(b)-1)
		b, _ = json.Marshal(body)
		if len(b) != size {
			t.Fatalf("the body is %d bytes, want %d", len(b), size)
		}
		return b
	}
	if r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, sized(2000)); r.status != http.StatusRequestEntityTooLarge || r.errorType() != "payload_too_large" {
		t.Errorf("2000 bytes: %d %s, want 413 payload_too_large", r.status, r.body)
	}
	if got := w.head(t, "small"); got != head {
		t.Errorf("small moved to %s", got)
	}
	if r := w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, sized(900)); r.status != http.StatusCreated {
		t.Errorf("900 bytes: %d %s, want 201", r.status, r.body)
	}
}
