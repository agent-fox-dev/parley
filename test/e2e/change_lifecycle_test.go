package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
)

// lifecycleHub is a fork hub whose main holds a.txt, pushed by the
// developer who owns demo, and where the member has the task fork demo--t1
// with agent, a token bound to it that reads and writes changes. Its
// mergeability checks are not spaced.
type lifecycleHub struct {
	*forkHub
	agent string
}

func startLifecycleHub(t *testing.T) *lifecycleHub {
	t.Helper()
	h := &lifecycleHub{forkHub: startForkHubWith(t, nil, func(c *hub.Composition) {
		c.AdjustChange(func(d *change.Deps) { d.MergeableDebounce = 0 })
	})}
	owner := cloneAs(t, h.forkHub, h.key, "demo")
	commitFile(t, owner, "a.txt", "l1\nl2\nl3\n", "Add a")
	owner("push", "--quiet", "origin", "main")
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.memberKey, map[string]any{
		"name": "t1", "purpose": "work on a", "auto_change": false,
		"token": map[string]any{"permissions": []string{"git:write", "changes:read", "changes:write"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create demo--t1 = %d %s, want 201", status, body)
	}
	h.agent = str(decoded(t, body), "token", "token")
	return h
}

// commitFile writes content to file in the clone that run works in and
// commits it, with the commit's other flags (--amend replaces the commit
// at the head), and returns the new head.
func commitFile(t *testing.T, run func(...string) string, file, content, message string, flags ...string) string {
	t.Helper()
	dir := strings.TrimSpace(run("rev-parse", "--show-toplevel"))
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", file)
	run(append([]string{"-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit", "--quiet", "-m", message}, flags...)...)
	return strings.TrimSpace(run("rev-parse", "HEAD"))
}

// open opens a change of demo from branch of the workspace source with the
// credential and body, and returns its number.
func (h *lifecycleHub) open(t *testing.T, credential, source, branch string, more map[string]any) string {
	t.Helper()
	body := map[string]any{"source": map[string]any{"workspace_slug": source, "branch": branch}}
	for k, v := range more {
		body[k] = v
	}
	status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes", credential, body)
	if status != http.StatusCreated {
		t.Fatalf("POST changes = %d %s, want 201", status, raw)
	}
	return fmt.Sprint(decoded(t, raw)["number"])
}

// read is the change number of demo as the credential reads it.
func (h *lifecycleHub) read(t *testing.T, credential, number string) map[string]any {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/"+number, credential, nil)
	if status != http.StatusOK {
		t.Fatalf("GET changes/%s = %d %s, want 200", number, status, body)
	}
	return decoded(t, body)
}

// until reads the change number of demo until done holds for it, for up to
// 20 s, and returns it.
func (h *lifecycleHub) until(t *testing.T, number, what string, done func(map[string]any) bool) map[string]any {
	t.Helper()
	return h.within(t, 20*time.Second, number, what, done)
}

// within is until for up to d.
func (h *lifecycleHub) within(t *testing.T, d time.Duration, number, what string, done func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		c := h.read(t, h.key, number)
		if done(c) {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("demo#%s never %s: %v", number, what, c)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// computed holds for a change whose mergeability is computed.
func computed(c map[string]any) bool { return c["mergeable"] != "unknown" }

// changeEvent is an event of a change in the hub's log.
type changeEvent struct {
	seq  int64
	typ  string
	meta map[string]any
}

// changeEvents are the events of the change number of demo after seq,
// oldest first, of the types given, or of every type when none is.
func (h *lifecycleHub) changeEvents(t *testing.T, number string, after int64, types ...string) []changeEvent {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT seq, type, metadata FROM hub_events
		WHERE resource_type = 'change' AND workspace = 'demo' AND resource_id = ? AND seq > ? ORDER BY seq`, number, after)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var evs []changeEvent
	for rows.Next() {
		var ev changeEvent
		var meta string
		if err := rows.Scan(&ev.seq, &ev.typ, &meta); err != nil {
			t.Fatal(err)
		}
		if len(types) > 0 && !slices.Contains(types, ev.typ) {
			continue
		}
		if err := json.Unmarshal([]byte(meta), &ev.meta); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func typesOf(evs []changeEvent) []string {
	types := []string{}
	for _, ev := range evs {
		types = append(types, ev.typ)
	}
	return types
}

// An agent opens a draft from its fork, states its intent and labels it
// from the shell, marks it ready, and waits until the hub found it
// landable in the background.
func TestChangeLifecycle_AnAgentFinishesADraftMarksItReadyAndWaitsUntilItIsLandable(t *testing.T) {
	h := startLifecycleHub(t)
	run := cloneAs(t, h.forkHub, h.agent, "demo--t1")
	run("checkout", "--quiet", "-b", "feat")
	commitFile(t, run, "b.txt", "b\n", "Add b")
	run("push", "--quiet", "origin", "feat")
	n := h.open(t, h.agent, "demo--t1", "feat", map[string]any{"draft": true})

	if r := h.runAs(t, h.agent, "change", "edit", "demo", n, "--intent", "why", "--label", "area:api"); r.code != 0 {
		t.Fatalf("change edit exited %d: %s", r.code, r.stderr)
	}
	ready := h.runAs(t, h.agent, "change", "ready", "demo", n)
	if ready.code != 0 || ready.json(t)["state"] != "open" {
		t.Fatalf("change ready exited %d with %s %s, want 0 and an open change", ready.code, ready.stdout, ready.stderr)
	}
	waited := h.runAs(t, h.agent, "change", "wait", "demo", n, "--for", "ready", "--timeout", "20s", "--poll-interval", "1s")
	if waited.code != 0 {
		t.Fatalf("change wait --for ready exited %d: %s", waited.code, waited.stderr)
	}
	c := waited.json(t)
	labels, _ := c["labels"].([]any)
	if c["state"] != "open" || c["intent"] != "why" || len(labels) != 1 || labels[0] != "area:api" ||
		str(c, "landing", "status") != "ready" || c["mergeable"] != "clean" {
		t.Errorf("change wait printed %s, want an open change with intent why, labels [area:api], landing ready and mergeable clean", waited.stdout)
	}

	evs := h.changeEvents(t, n, 0)
	if got := strings.Join(typesOf(evs), " "); got != "parley.change.version parley.change.open parley.change.ready" {
		t.Fatalf("the events of demo#%s = %s, want the version, the open and one ready event", n, got)
	}
	if evs[0].meta["version"] != float64(1) || evs[0].meta["draft"] != true || evs[1].meta["was_draft"] != true {
		t.Errorf("the version event = %v and the open event = %v, want version 1 of a draft and was_draft true", evs[0].meta, evs[1].meta)
	}
}

// A developer's push to main makes an agent's open change conflict, which
// the hub announces once; the agent's rebased version applies again and is
// announced landable again, and no check moved a ref.
func TestChangeLifecycle_ATargetMoveMakesAChangeConflictAndANewVersionClearsIt(t *testing.T) {
	h := startLifecycleHub(t)
	agent := cloneAs(t, h.forkHub, h.agent, "demo--t1")
	agent("checkout", "--quiet", "-b", "feat")
	commitFile(t, agent, "a.txt", "agent\nl2\nl3\n", "Change line 1")
	agent("push", "--quiet", "origin", "feat")
	n := h.open(t, h.agent, "demo--t1", "feat", map[string]any{"intent": "why"})
	h.until(t, n, "announced landable", func(map[string]any) bool {
		return len(h.changeEvents(t, n, 0, change.EventReady)) == 1
	})

	owner := cloneAs(t, h.forkHub, h.key, "demo")
	ownerHead := commitFile(t, owner, "a.txt", "owner\nl2\nl3\n", "Change line 1 too")
	owner("push", "--quiet", "origin", "main")
	c := h.until(t, n, "computed after the push to main", computed)
	files, _ := c["conflict"].(map[string]any)["files"].([]any)
	if c["state"] != "open" || c["mergeable"] != "conflict" || len(files) != 1 || str(files[0].(map[string]any), "path") != "a.txt" {
		t.Fatalf("after the push to main demo#%s is %v, want open, conflicting on a.txt", n, c)
	}
	conflicts := h.changeEvents(t, n, 0, change.EventConflict)
	if len(conflicts) != 1 {
		t.Errorf("the conflict events = %v, want one naming a.txt", conflicts)
	} else if gotFiles, _ := json.Marshal(conflicts[0].meta["files"]); string(gotFiles) != `["a.txt"]` {
		t.Errorf("the conflict event names %s, want a.txt", gotFiles)
	}

	dir := strings.TrimSpace(agent("rev-parse", "--show-toplevel"))
	if out, err := newGitClient(t, h.URL(""), h.memberKey).git(dir, "fetch", "--quiet", h.URL("/git/acme/demo.git"), "main"); err != nil {
		t.Fatalf("git fetch demo main = %v: %s", err, out)
	}
	agent("reset", "--quiet", "--hard", "FETCH_HEAD")
	agentHead := commitFile(t, agent, "a.txt", "owner and agent\nl2\nl3\n", "Change line 1 on top of the owner's")
	agent("push", "--quiet", "--force", "origin", "feat")
	c = h.until(t, n, "computed at version 2", func(c map[string]any) bool { return c["version"] == float64(2) && computed(c) })
	if c["mergeable"] != "clean" || c["conflict"] != nil {
		t.Errorf("at version 2 demo#%s has mergeable %v and conflict %v, want clean and none", n, c["mergeable"], c["conflict"])
	}
	readies := h.changeEvents(t, n, 0, change.EventReady)
	if len(readies) != 2 || readies[1].meta["head_sha"] != c["head_sha"] {
		t.Errorf("the ready events = %v, want two, the second for the head of version 2 %v", readies, c["head_sha"])
	}
	if main, feat := gitrepotest.Refs(t, h.repo("demo"), "refs/heads/")["main"], gitrepotest.Refs(t, h.repo("demo--t1"), "refs/heads/")["feat"]; main != ownerHead || feat != agentHead {
		t.Errorf("main is at %s and the fork's feat at %s, want the pushed %s and %s", main, feat, ownerHead, agentHead)
	}
}

// A developer abandons a change from the shell, deletes its branch with
// git, and restores it from the shell, which brings the branch back at the
// last version; a second restore is refused.
func TestChangeLifecycle_ADeveloperAbandonsAChangeDeletesItsBranchAndRestoresIt(t *testing.T) {
	h := startLifecycleHub(t)
	owner := cloneAs(t, h.forkHub, h.key, "demo")
	owner("checkout", "--quiet", "-b", "feat")
	commitFile(t, owner, "c.txt", "c\n", "Add c")
	owner("push", "--quiet", "origin", "feat")
	opened := h.runAs(t, h.key, "change", "open", "demo", "--source", "feat", "--intent", "why")
	if opened.code != 0 {
		t.Fatalf("change open exited %d: %s", opened.code, opened.stderr)
	}
	n := fmt.Sprint(opened.json(t)["number"])
	head := commitFile(t, owner, "c.txt", "c\nc\n", "Grow c")
	owner("push", "--quiet", "origin", "feat")
	h.until(t, n, "announced landable at version 2", func(c map[string]any) bool {
		readies := h.changeEvents(t, n, 0, change.EventReady)
		return c["version"] == float64(2) && len(readies) > 0 && readies[len(readies)-1].meta["head_sha"] == head
	})
	evs := h.changeEvents(t, n, 0)
	seq := evs[len(evs)-1].seq

	abandoned := h.runAs(t, h.key, "change", "abandon", "demo", n, "--reason", "parked")
	if abandoned.code != 0 || abandoned.json(t)["state"] != "abandoned" {
		t.Fatalf("change abandon exited %d with %s %s, want 0 and an abandoned change", abandoned.code, abandoned.stdout, abandoned.stderr)
	}
	refs := gitrepotest.Refs(t, h.repo("demo"), "refs/")
	if refs["heads/feat"] != head || refs["parley/changes/"+n+"/1"] == "" || refs["parley/changes/"+n+"/2"] != head {
		t.Errorf("after the abandon the refs of demo are %v, want feat and both version refs kept", refs)
	}
	owner("push", "--quiet", "origin", "--delete", "feat")
	if c := h.read(t, h.key, n); c["state"] != "abandoned" {
		t.Errorf("after the branch was deleted demo#%s is %v, want abandoned", n, c["state"])
	}

	restored := h.runAs(t, h.key, "change", "restore", "demo", n)
	if restored.code != 0 {
		t.Fatalf("change restore exited %d: %s", restored.code, restored.stderr)
	}
	if r := restored.json(t); r["state"] != "open" || r["version"] != float64(2) || r["head_sha"] != head {
		t.Errorf("change restore printed %s, want an open change at version 2, %s", restored.stdout, head)
	}
	if feat := gitrepotest.Refs(t, h.repo("demo"), "refs/heads/")["feat"]; feat != head {
		t.Errorf("after the restore feat is at %q, want %s", feat, head)
	}
	h.until(t, n, "announced landable again", func(c map[string]any) bool {
		return c["mergeable"] == "clean" && len(h.changeEvents(t, n, seq, change.EventReady)) == 1
	})
	after := h.changeEvents(t, n, seq)
	if got := strings.Join(typesOf(after), " "); got != "parley.change.abandon parley.change.restore parley.change.ready" {
		t.Errorf("the events after the abandon = %s, want the abandon, the restore and one ready event", got)
	}
	if after[0].meta["reason"] != "parked" {
		t.Errorf("the abandon event = %v, want reason parked", after[0].meta)
	}

	again := h.runAs(t, h.key, "change", "restore", "demo", n)
	if again.code != 1 || again.errorType(t) != "change_not_abandoned" {
		t.Errorf("a second change restore exited %d with %s, want 1 and change_not_abandoned", again.code, again.stderr)
	}
}

// An operator restarts the hub after a task fork's branch moved without its
// bookkeeping: the hub records the head as a version before it answers, and
// computes its mergeability without being asked.
func TestChangeLifecycle_ARestartRecordsAHeadWhoseBookkeepingWasLost(t *testing.T) {
	h := startLifecycleHub(t)
	agent := cloneAs(t, h.forkHub, h.agent, "demo--t1")
	agent("checkout", "--quiet", "-b", "feat")
	commitFile(t, agent, "b.txt", "b\n", "Add b")
	agent("push", "--quiet", "origin", "feat")
	n := h.open(t, h.agent, "demo--t1", "feat", map[string]any{"intent": "why"})
	h.until(t, n, "computed", computed)

	// The branch moves on disk, where no hook of the hub sees it, as a
	// crash between a ref update and its store commit leaves it.
	fork := h.repo("demo--t1")
	moved, err := gitrepotest.Run(fork, "", "commit-tree", "refs/heads/feat^{tree}", "-p", "refs/heads/feat", "-m", "Lost bookkeeping")
	if err != nil {
		t.Fatalf("git commit-tree = %v: %s", err, moved)
	}
	moved = strings.TrimSpace(moved)
	if out, err := gitrepotest.Run(fork, "", "update-ref", "refs/heads/feat", moved); err != nil {
		t.Fatalf("git update-ref = %v: %s", err, out)
	}
	h.Restart(t)

	if c := h.read(t, h.agent, n); c["version"] != float64(2) || c["head_sha"] != moved {
		t.Fatalf("the first GET after the restart = version %v at %v, want version 2 at %s", c["version"], c["head_sha"], moved)
	}
	if ref := gitrepotest.Refs(t, h.repo("demo"), "refs/parley/changes/"+n+"/")["2"]; ref != moved {
		t.Errorf("refs/parley/changes/%s/2 is at %q, want %s", n, ref, moved)
	}
	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/"+n+"/versions", h.agent, nil)
	items, _ := decoded(t, body)["items"].([]any)
	if status != http.StatusOK || len(items) != 2 || str(items[1].(map[string]any), "kind") != "ref" ||
		str(items[1].(map[string]any), "created_by", "actor_type") != "system" {
		t.Errorf("GET versions = %d %s, want two, the last of kind ref created by the system", status, body)
	}
	if c := h.until(t, n, "computed after the restart", computed); c["mergeable"] != "clean" {
		t.Errorf("after the restart demo#%s has mergeable %v, want clean", n, c["mergeable"])
	}

	h.Restart(t)
	if c := h.read(t, h.agent, n); c["version"] != float64(2) {
		t.Errorf("after a third start demo#%s is at version %v, want still 2", n, c["version"])
	}
}
