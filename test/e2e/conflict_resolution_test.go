package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/hub"
)

// conflictingChanges serves a hub whose workspace demo holds app.txt, and
// has the agent of the task fork demo--t1, with a token bound to it, push
// two changes that set line 2 of app.txt differently; the first is landed,
// and the second is in conflict. It returns the hub, the owner's clone of
// demo, the agent's clone of the fork, the agent's token and the head of
// main after the land.
func conflictingChanges(t *testing.T) (h *lifecycleHub, owner, pusher func(...string) string, agent, main0 string) {
	t.Helper()
	h = &lifecycleHub{forkHub: startForkHubWith(t, nil, func(c *hub.Composition) {
		c.AdjustChange(func(d *change.Deps) { d.MergeableDebounce = 0 })
	})}
	owner = cloneAs(t, h.forkHub, h.key, "demo")
	commitFile(t, owner, "app.txt", "one\ntwo\nthree\n", "Add app")
	owner("push", "--quiet", "origin", "main")

	// The agent works in a task fork of demo with a token bound to it.
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.memberKey, map[string]any{
		"name": "t1", "purpose": "work on app", "auto_change": false,
		"token": map[string]any{"permissions": []string{"git:write", "changes:read", "changes:write"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create demo--t1 = %d %s, want 201", status, body)
	}
	agent = str(decoded(t, body), "token", "token")
	pusher = cloneAs(t, h.forkHub, agent, "demo--t1")
	for _, line := range []string{"TARGET", "CHANGE"} {
		branch := strings.ToLower(line)
		pusher("checkout", "--quiet", "-b", branch, "origin/main")
		commitFile(t, pusher, "app.txt", "one\n"+line+"\nthree\n", "Set line 2 to "+line)
		pusher("push", "--quiet", "-o", "parley.change", "-o", "parley.intent=why", "origin", branch)
	}
	if c := h.read(t, agent, "2"); str(c, "source", "branch") != "change" || str(c, "author", "username") != "member" {
		t.Fatalf("demo#2 is %v, want the agent's change of the branch change", c)
	}
	if status, a := h.land(t, h.key, "1", ""); status != http.StatusOK {
		t.Fatalf("the land of demo#1 = %d %v, want 200", status, a)
	}
	main0 = fetchMain(owner)
	h.until(t, "2", "in conflict after the land of demo#1", func(c map[string]any) bool { return c["mergeable"] == "conflict" })
	return h, owner, pusher, agent, main0
}

// Two changes pushed by an agent set line 2 of app.txt differently; the
// first lands. The agent, which never clones, reads the second's conflict
// from the command line, prints the conflicted file with markers, resolves
// it in a local file and hands it back with parley change resolve: the hub
// rebuilds the change on main with the resolution, as a restack version of
// the agent's, and the change no longer conflicts, while main stays where
// the land left it.
func TestConflictResolution_AnAgentWithoutACloneReadsAConflictAndResolvesIt(t *testing.T) {
	parallel(t)
	h, owner, pusher, agent, main0 := conflictingChanges(t)

	c := h.runAs(t, agent, "change", "conflict", "demo", "2", "--fail-on-conflict")
	if c.code != 3 || c.errorType(t) != "condition_not_met" {
		t.Errorf("change conflict --fail-on-conflict exited %d with stderr %s, want 3 and condition_not_met", c.code, c.stderr)
	}
	answer := c.json(t)
	report, _ := answer["conflict"].(map[string]any)
	files, _ := report["files"].([]any)
	landed, _ := report["landed_changes"].([]any)
	if answer["mergeable"] != "conflict" || len(files) != 1 || str(files[0].(map[string]any), "path") != "app.txt" ||
		str(files[0].(map[string]any), "kind") != "content" || fmt.Sprint(files[0].(map[string]any)["landed_by"]) != "[1]" {
		t.Errorf("change conflict printed %s, want one content conflict on app.txt landed by demo#1", c.stdout)
	}
	if len(landed) != 1 || landed[0].(map[string]any)["number"] != float64(1) {
		t.Errorf("the report's landed_changes are %v, want demo#1", landed)
	}

	f := h.runAs(t, agent, "change", "conflict", "demo", "2", "--file", "app.txt")
	if want := "one\n<<<<<<< target\nTARGET\n=======\nCHANGE\n>>>>>>> change\nthree\n"; f.code != 0 || f.stdout != want {
		t.Errorf("change conflict --file exited %d and printed %q (stderr %q), want 0 and %q", f.code, f.stdout, f.stderr, want)
	}

	resolved := "one\nBOTH\nthree\n"
	local := filepath.Join(t.TempDir(), "app.txt")
	if err := os.WriteFile(local, []byte(resolved), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.runAs(t, agent, "change", "resolve", "demo", "2", "--file", "app.txt="+local)
	if r.code != 0 {
		t.Fatalf("change resolve exited %d: %s", r.code, r.stderr)
	}
	outcome := r.json(t)
	head := str(outcome, "change", "head_sha")
	if outcome["status"] != "restacked" || head == "" {
		t.Fatalf("change resolve printed %s, want status restacked with the change", r.stdout)
	}
	// The source branch is the fork's; its clone fetches it to look.
	pusher("fetch", "--quiet", "origin", "change")
	if got := strings.TrimSpace(pusher("rev-parse", "FETCH_HEAD")); got != head {
		t.Errorf("the source branch change is at %s, want the restacked head %s", got, head)
	}
	if parent := strings.TrimSpace(pusher("rev-parse", head+"^")); parent != main0 {
		t.Errorf("the restacked head %s sits on %s, want the head of main %s", head, parent, main0)
	}
	if got := pusher("cat-file", "blob", head+":app.txt"); got != resolved {
		t.Errorf("app.txt at the restacked head is %q, want the resolution %q", got, resolved)
	}

	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/2/versions", agent, nil)
	items, _ := decoded(t, body)["items"].([]any)
	if status != http.StatusOK || len(items) != 2 {
		t.Fatalf("GET changes/2/versions = %d %s, want two versions", status, body)
	}
	last := items[1].(map[string]any)
	if last["kind"] != change.KindRestack || str(last, "created_by", "username") != "member" || last["head_sha"] != head {
		t.Errorf("the last version of demo#2 is %v, want a restack to %s created by member", last, head)
	}
	// The check of the restacked version runs after it.
	read := h.until(t, "2", "clean after the resolve", func(c map[string]any) bool { return c["mergeable"] == change.MergeableClean })
	if read["head_sha"] != head || read["version"] != last["number"] || read["conflict"] != nil || read["needs_restack"] != false ||
		str(read, "landing", "status") != change.LandingReady {
		t.Errorf("after the resolve demo#2 is at %v version %v, conflict %v, needs_restack %v, landing %v; want %s, version %v, null, false and ready",
			read["head_sha"], read["version"], read["conflict"], read["needs_restack"], read["landing"], head, last["number"])
	}

	c = h.runAs(t, agent, "change", "conflict", "demo", "2", "--fail-on-conflict")
	if want := map[string]any{"mergeable": "clean", "conflict": nil}; c.code != 0 || !reflect.DeepEqual(c.json(t), want) {
		t.Errorf("change conflict --fail-on-conflict after the resolve exited %d and printed %s, want 0 and %v", c.code, c.stdout, want)
	}
	if got := fetchMain(owner); got != main0 {
		t.Errorf("main is at %s, want it where the land of demo#1 left it, %s", got, main0)
	}
}
