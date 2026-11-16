package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// changeEventTypes are the types of the change events of the workspace slug
// in the hub's event log, oldest first.
func (h *workspaceHub) changeEventTypes(t *testing.T, slug string) []string {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT type FROM hub_events WHERE resource_type = 'change' AND workspace = ? ORDER BY seq`, slug)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
	}
	return types
}

// A developer pushes a branch with stock git, opens a change of it from the
// shell, reads it back by number and by commit, and fetches its first
// version's ref with stock git.
func TestChange_ADeveloperOpensAChangeAndReadsItBackByNumberCommitAndRef(t *testing.T) {
	parallel(t)
	h := startForkHub(t)
	g := newGitClient(t, h.URL(""), h.key)
	dir := filepath.Join(t.TempDir(), "demo")
	if out, err := g.git("", "clone", "--quiet", h.URL("/git/acme/demo.git"), dir); err != nil {
		t.Fatalf("git clone = %v: %s", err, out)
	}
	commit := func(file, content, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := g.git(dir, "add", file); err != nil {
			t.Fatalf("git add = %v: %s", err, out)
		}
		if out, err := g.git(dir, "-c", "user.name=Dev", "-c", "user.email=dev@example.invalid", "commit", "--quiet", "-m", message); err != nil {
			t.Fatalf("git commit = %v: %s", err, out)
		}
	}
	if out, err := g.git(dir, "checkout", "--quiet", "-b", "feat"); err != nil {
		t.Fatalf("git checkout = %v: %s", err, out)
	}
	commit("one.txt", "one\n", "Add one")
	commit("two.txt", "two\n", "Add two")
	head, err := g.git(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse = %v: %s", err, head)
	}
	head = strings.TrimSpace(head)
	if out, err := g.git(dir, "push", "--quiet", "origin", "feat"); err != nil {
		t.Fatalf("git push origin feat = %v: %s", err, out)
	}

	opened := h.runAs(t, h.key, "change", "open", "demo", "--source", "feat", "--intent", "why")
	if opened.code != 0 {
		t.Fatalf("change open exited %d: %s", opened.code, opened.stderr)
	}
	c := opened.json(t)
	if c["number"] != float64(1) || c["state"] != "open" || c["version"] != float64(1) || c["intent"] != "why" {
		t.Errorf("change open printed number %v, state %v, version %v, intent %v; want 1, open, 1, why",
			c["number"], c["state"], c["version"], c["intent"])
	}

	shown := h.runAs(t, h.key, "change", "show", "demo", "1", "--include", "versions")
	if shown.code != 0 {
		t.Fatalf("change show exited %d: %s", shown.code, shown.stderr)
	}
	versions, _ := shown.json(t)["versions"].([]any)
	if len(versions) != 1 || str(versions[0].(map[string]any), "head_sha") != head {
		t.Errorf("change show printed versions %v, want one whose head_sha is the pushed head %s", versions, head)
	}

	found := h.runAs(t, h.key, "change", "find", "demo", head)
	if found.code != 0 {
		t.Fatalf("change find exited %d: %s", found.code, found.stderr)
	}
	f := found.json(t)
	if ch, _ := f["change"].(map[string]any); ch["number"] != float64(1) || f["version"] != float64(1) || f["landed"] != false {
		t.Errorf("change find printed %s, want change 1 with version 1 and landed false", found.stdout)
	}

	if out, err := g.git(dir, "fetch", "--quiet", "origin", "refs/parley/changes/1/1"); err != nil {
		t.Fatalf("git fetch origin refs/parley/changes/1/1 = %v: %s", err, out)
	}
	if fetched, _ := g.git(dir, "rev-parse", "FETCH_HEAD"); strings.TrimSpace(fetched) != head {
		t.Errorf("FETCH_HEAD = %s, want the pushed head %s", fetched, head)
	}

	// The mergeability check announces the clean change in the background.
	types := slices.DeleteFunc(h.changeEventTypes(t, "demo"), func(typ string) bool { return typ == "parley.change.ready" })
	if strings.Join(types, " ") != "parley.change.version parley.change.open" {
		t.Errorf("the change events of demo = %v, want parley.change.version and then parley.change.open", types)
	}
}

// An agent opens a change, commits to its source branch through the source
// control API, which records the second version with the ref update, and
// reads the versions and the interdiff of the two from the shell.
func TestChange_AnAgentCommitsToTheSourceAndReadsWhatMovedBetweenTwoVersions(t *testing.T) {
	parallel(t)
	h := startForkHub(t)
	agent := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"git:write", "changes:write", "changes:read"}, CreatedAt: time.Now()})
	commits := "/api/v1/workspaces/demo/scm/commits"
	first := scmWrite(t, h.workspaceHub, http.MethodPost, commits, agent, map[string]any{
		"branch": "feat", "base_ref": "main", "message": "Change a\n\nBecause a must change.",
		"operations": []map[string]any{{"op": "put", "path": "a.txt", "content": "changed\n"}},
	})
	if first.status != http.StatusCreated {
		t.Fatalf("the commit that makes feat = %d %s, want 201", first.status, first.body)
	}
	head := str(first.json, "commit", "sha")

	opened := scmWrite(t, h.workspaceHub, http.MethodPost, "/api/v1/workspaces/demo/changes", agent,
		map[string]any{"source": map[string]any{"branch": "feat"}})
	if opened.status != http.StatusCreated || opened.json["number"] != float64(1) {
		t.Fatalf("POST changes = %d %s, want 201 with change 1", opened.status, opened.body)
	}

	second := scmWrite(t, h.workspaceHub, http.MethodPost, commits, agent, map[string]any{
		"branch": "feat", "expected_head_sha": head, "message": "more",
		"operations": []map[string]any{{"op": "put", "path": "b.txt", "content": "b\n"}},
	})
	if second.status != http.StatusCreated {
		t.Fatalf("the commit on feat = %d %s, want 201", second.status, second.body)
	}
	moved := str(second.json, "commit", "sha")

	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/1", agent, nil)
	var ch struct {
		Version int    `json:"version"`
		HeadSHA string `json:"head_sha"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &ch) != nil || ch.Version != 2 || ch.HeadSHA != moved {
		t.Errorf("GET changes/1 = %d %s, want version 2 at the commit the API returned, %s", status, body, moved)
	}
	status, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/1/versions/2", agent, nil)
	var v struct {
		Kind string `json:"kind"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &v) != nil || v.Kind != "commit" {
		t.Errorf("GET changes/1/versions/2 = %d %s, want kind commit", status, body)
	}
	refs := gitrepotest.Refs(t, h.repo("demo"), "refs/parley/changes/1/")
	if refs["1"] != head || refs["2"] != moved {
		t.Errorf("version refs of change 1 in demo's repository = %v, want 1 at %s and 2 at %s", refs, head, moved)
	}

	listed := h.runAs(t, agent, "change", "versions", "demo", "1")
	if listed.code != 0 {
		t.Fatalf("change versions exited %d: %s", listed.code, listed.stderr)
	}
	items, _ := listed.json(t)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["number"] != float64(1) || items[1].(map[string]any)["number"] != float64(2) ||
		items[1].(map[string]any)["kind"] != "commit" {
		t.Errorf("change versions printed %s, want versions 1 and 2, the second of kind commit", listed.stdout)
	}

	diffed := h.runAs(t, agent, "change", "diff", "demo", "1", "--against", "1")
	if diffed.code != 0 {
		t.Fatalf("change diff exited %d: %s", diffed.code, diffed.stderr)
	}
	d := diffed.json(t)
	changed := map[string]any{}
	files, _ := d["files"].([]any)
	for _, f := range files {
		file := f.(map[string]any)
		changed[file["path"].(string)] = file["changed"]
	}
	if d["version"] != float64(2) || d["against"] != float64(1) || changed["b.txt"] != true || changed["a.txt"] != false {
		t.Errorf("change diff printed version %v, against %v, changed %v; want 2, 1, b.txt changed and a.txt not",
			d["version"], d["against"], changed)
	}
}
