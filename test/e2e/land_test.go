package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/land"
)

// land sends POST …/changes/number/land of demo with credential and the
// query, and returns the answer's status and body.
func (h *lifecycleHub) land(t *testing.T, credential, number, query string) (int, map[string]any) {
	t.Helper()
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes/"+number+"/land"+query, credential, map[string]any{})
	return status, decoded(t, body)
}

// get answers GET path of the API as the developer, which must be 200.
func (h *lifecycleHub) get(t *testing.T, path string) map[string]any {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1"+path, h.key, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", path, status, body)
	}
	return decoded(t, body)
}

// forkChange creates the fork name of demo as the developer, pushes the
// branch of the developer's clone to it as feat, and opens a change from
// it into main, whose number it returns.
func (h *lifecycleHub) forkChange(t *testing.T, owner func(...string) string, name, branch string) string {
	t.Helper()
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.key, map[string]any{"name": name, "auto_change": false})
	if status != http.StatusCreated {
		t.Fatalf("create the fork %s = %d %s, want 201", name, status, body)
	}
	slug := str(decoded(t, body), "workspace", "slug")
	owner("push", "--quiet", h.URL("/git/acme/"+slug+".git"), branch+":refs/heads/feat")
	return h.open(t, h.key, slug, "feat", map[string]any{"intent": "why"})
}

// loggedLand is an event of the hub's log as GET /events/log answers it.
type loggedLand struct {
	Seq        int64          `json:"seq"`
	Type       string         `json:"type"`
	ResourceID string         `json:"resource_id"`
	Metadata   map[string]any `json:"metadata"`
}

// landLog reads the land events and the pushes of the target main through
// GET /events/log, oldest first: every parley.change.land, every
// parley.change.land_fail and every parley.git.push of refs/heads/main
// made by a land.
func (h *lifecycleHub) landLog(t *testing.T) (lands, fails, pushes []loggedLand) {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=0&limit=200&types="+
		land.EventLand+","+land.EventLandFail+",parley.git.push", h.Token, nil)
	var page struct {
		Items []loggedLand `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET events/log = %d %s, want 200 with a page", status, body)
	}
	for _, e := range page.Items {
		switch {
		case e.Type == land.EventLand:
			lands = append(lands, e)
		case e.Type == land.EventLandFail:
			fails = append(fails, e)
		case e.ResourceID == "refs/heads/main" && e.Metadata["via"] == "land":
			pushes = append(pushes, e)
		}
	}
	return lands, fails, pushes
}

// fetchMain fetches demo's main into the clone run works in and returns
// its head.
func fetchMain(run func(...string) string) string {
	run("fetch", "--quiet", "origin", "main")
	return strings.TrimSpace(run("rev-parse", "FETCH_HEAD"))
}

// An agent pushes two commits to its task fork and opens a change; a
// developer moves main meanwhile; the agent lands the change with parley
// change land --wait, and main gains the agent's commits, rebuilt by the
// landing identity on top of the developer's, while the fork's branch goes
// and origin is not touched.
func TestLand_AnAgentLandsAChangeFromTheCommandLine(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	originMain := h.origin.Head("main")
	agent := cloneAs(t, h.forkHub, h.agent, "demo--t1")
	agent("checkout", "--quiet", "-b", "feat")
	commitFile(t, agent, "b.txt", "b\n", "Add b")
	commitFile(t, agent, "c.txt", "c\n", "Add c")
	agent("push", "--quiet", "origin", "feat")
	n := h.open(t, h.agent, "demo--t1", "feat", map[string]any{"intent": "why"})

	owner := cloneAs(t, h.forkHub, h.key, "demo")
	developer := commitFile(t, owner, "d.txt", "d\n", "Add d", "--author=Dev <dev@example.invalid>")
	owner("push", "--quiet", "origin", "main")
	activity := func() int64 {
		var at int64
		if err := h.Store.Main().QueryRow(`SELECT last_activity_at FROM workspaces WHERE slug = 'demo--t1'`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	before := activity()

	r := h.runAs(t, h.agent, "change", "land", "demo", n, "--wait")
	if r.code != 0 {
		t.Fatalf("change land --wait exited %d: %s", r.code, r.stderr)
	}
	a := r.json(t)
	commits, _ := a["commits"].([]any)
	if a["status"] != land.StatusLanded || a["attempts"] != float64(1) || len(commits) != 2 {
		t.Fatalf("change land printed %s, want a landed attempt after 1 try with two commits", r.stdout)
	}

	head := fetchMain(owner)
	log := strings.Split(strings.TrimSpace(owner("log", "--format=%H|%s|%an <%ae>|%cn <%ce>", "-3", "FETCH_HEAD")), "\n")
	want := []string{
		str(commits[1].(map[string]any), "landed_sha") + "|Add c|Agent <agent@example.invalid>|Parley <parley@localhost>",
		str(commits[0].(map[string]any), "landed_sha") + "|Add b|Agent <agent@example.invalid>|Parley <parley@localhost>",
	}
	if !reflect.DeepEqual(log[:2], want) || !strings.HasPrefix(log[2], developer+"|Add d|") || a["landed_sha"] != head {
		t.Errorf("main reads\n%s\nwant the two landed commits %v on top of the developer's %s", strings.Join(log, "\n"), want, developer)
	}
	if c := h.read(t, h.key, n); c["state"] != "landed" || str(c, "landed", "land_id") != a["id"] {
		t.Errorf("demo#%s is %v with landed %v, want landed by %v", n, c["state"], c["landed"], a["id"])
	}
	if feat, ok := gitrepotest.Refs(t, h.repo("demo--t1"), "refs/heads/")["feat"]; ok {
		t.Errorf("the fork's feat is at %s, want it deleted", feat)
	}
	if after := activity(); after <= before {
		t.Errorf("the fork's last activity is %d, want it after %d, when the land began", after, before)
	}
	lands, _, pushes := h.landLog(t)
	if len(lands) != 1 || lands[0].ResourceID != n || len(pushes) != 1 || pushes[0].Metadata["after"] != head {
		t.Errorf("the log holds land events %v and land pushes of main %v, want one of each, to %s", lands, pushes, head)
	}
	if got := h.origin.Head("main"); got != originMain {
		t.Errorf("origin's main is at %s, want it where it was, %s", got, originMain)
	}
}

// Eight changes from eight forks, each adding a file of its own, are
// landed at the same moment: each call lands, main takes the eight in one
// linear history, and the moves of main chain from the old head to the
// new.
func TestLand_EightChangesLandOnOneTargetAtTheSameMoment(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	owner := cloneAs(t, h.forkHub, h.key, "demo")
	old := fetchMain(owner)
	var numbers []string
	for i := 1; i <= 8; i++ {
		branch := fmt.Sprintf("f%d", i)
		owner("checkout", "--quiet", "-b", branch, old)
		commitFile(t, owner, branch+".txt", branch+"\n", "Add "+branch)
		numbers = append(numbers, h.forkChange(t, owner, "t"+branch, branch))
	}

	attempts := make([]map[string]any, len(numbers))
	statuses := make([]int, len(numbers))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, n := range numbers {
		wg.Go(func() {
			<-start
			statuses[i], attempts[i] = h.land(t, h.key, n, "")
		})
	}
	close(start)
	wg.Wait()

	sum := 0
	for i, a := range attempts {
		tries, _ := a["attempts"].(float64)
		if statuses[i] != http.StatusOK || a["status"] != land.StatusLanded || tries < 1 || tries > 8 {
			t.Errorf("the land of demo#%s = %d %v, want 200 landed after 1 to 8 tries", numbers[i], statuses[i], a)
		}
		sum += int(tries)
		if c := h.read(t, h.key, numbers[i]); c["state"] != "landed" {
			t.Errorf("demo#%s is %v, want landed", numbers[i], c["state"])
		}
		commits, _ := a["commits"].([]any)
		for _, cm := range commits {
			sha := str(cm.(map[string]any), "landed_sha")
			if got := h.get(t, "/workspaces/demo/changes/by-commit/"+sha); fmt.Sprint(got["change"].(map[string]any)["number"]) != numbers[i] || got["landed"] != true {
				t.Errorf("the change of the landed commit %s = %v, want demo#%s, landed", sha, got, numbers[i])
			}
		}
	}
	if sum < 8 {
		t.Errorf("the attempts sum to %d tries, want at least 8", sum)
	}

	head := fetchMain(owner)
	files := strings.Fields(owner("ls-tree", "--name-only", "FETCH_HEAD"))
	for i := 1; i <= 8; i++ {
		if !slices.Contains(files, fmt.Sprintf("f%d.txt", i)) {
			t.Errorf("main holds %v, want f%d.txt among them", files, i)
		}
	}
	if merges := strings.TrimSpace(owner("rev-list", "--merges", "FETCH_HEAD")); merges != "" {
		t.Errorf("main holds the merge commits %s, want a linear history", merges)
	}
	if count := strings.TrimSpace(owner("rev-list", "--count", old+"..FETCH_HEAD")); count != "8" {
		t.Errorf("main gained %s commits, want 8", count)
	}

	// The moves follow one another; their events are written once each move
	// is recorded, which is not always in the order of the moves.
	lands, _, pushes := h.landLog(t)
	if len(lands) != 8 || len(pushes) != 8 {
		t.Fatalf("the log holds %d land events and %d land pushes of main, want 8 and 8", len(lands), len(pushes))
	}
	next := map[any]any{}
	for _, p := range pushes {
		next[p.Metadata["before"]] = p.Metadata["after"]
	}
	at := any(old)
	for range pushes {
		at = next[at]
	}
	if at != head || len(next) != 8 {
		t.Errorf("the pushes of main %v chain from %s to %v, want to %s", next, old, at, head)
	}
}

// Two changes edit line 3 of f.txt differently: the first lands, and the
// land of the second ends conflict, reported with the first change it
// collided with, and leaves main and the second change as they were but
// for the conflict it reads.
func TestLand_AConflictingLandComesBackAsAReportAndChangesNothing(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	owner := cloneAs(t, h.forkHub, h.key, "demo")
	commitFile(t, owner, "f.txt", "1\n2\n3\n4\n5\n", "Add f")
	owner("push", "--quiet", "origin", "main")
	var numbers []string
	for _, edit := range []string{"one", "two"} {
		owner("checkout", "--quiet", "-b", edit, "main")
		commitFile(t, owner, "f.txt", "1\n2\n"+edit+"\n4\n5\n", "Set line 3 to "+edit)
		numbers = append(numbers, h.forkChange(t, owner, edit, edit))
	}
	first, second := numbers[0], numbers[1]

	if status, a := h.land(t, h.key, first, ""); status != http.StatusOK {
		t.Fatalf("the land of demo#%s = %d %v, want 200", first, status, a)
	}
	head := fetchMain(owner)
	// The land of the first has the second's mergeability checked again; it
	// is waited for, so that the check's own report does not replace the
	// land's afterwards.
	h.until(t, second, "computed after the first land", computed)

	status, body := h.land(t, h.key, second, "")
	e, _ := body["error"].(map[string]any)
	a, _ := e["details"].(map[string]any)
	if status != http.StatusConflict || e["error_type"] != "land_conflict" || a["status"] != land.StatusConflict {
		t.Fatalf("the land of demo#%s = %d %v, want 409 land_conflict with the attempt", second, status, body)
	}
	report, _ := a["conflict"].(map[string]any)
	files, _ := report["files"].([]any)
	landedChanges, _ := report["landed_changes"].([]any)
	if len(files) != 1 || str(files[0].(map[string]any), "path") != "f.txt" || str(files[0].(map[string]any), "kind") != "content" ||
		fmt.Sprint(files[0].(map[string]any)["landed_by"]) != "["+first+"]" {
		t.Errorf("the report's files are %v, want f.txt, a content conflict landed by demo#%s", files, first)
	}
	if len(landedChanges) != 1 || fmt.Sprint(landedChanges[0].(map[string]any)["number"]) != first ||
		str(landedChanges[0].(map[string]any), "landed_sha") != head {
		t.Errorf("the report's landed_changes are %v, want demo#%s landed at %s", landedChanges, first, head)
	}
	if got := fetchMain(owner); got != head {
		t.Errorf("main is at %s, want it where the first land left it, %s", got, head)
	}
	if c := h.read(t, h.key, second); c["state"] != "open" || c["mergeable"] != "conflict" {
		t.Errorf("demo#%s is %v, mergeable %v, want open in conflict", second, c["state"], c["mergeable"])
	}
	if got := h.get(t, "/workspaces/demo/lands/"+str(a, "id")); !reflect.DeepEqual(got, a) {
		t.Errorf("GET lands/%s = %v, want the attempt of the answer %v", str(a, "id"), got, a)
	}
	if items, _ := h.get(t, "/workspaces/demo/changes/"+second+"/lands")["items"].([]any); len(items) != 1 || !reflect.DeepEqual(items[0], a) {
		t.Errorf("the lands of demo#%s are %v, want the attempt of the answer", second, items)
	}
	if _, fails, _ := h.landLog(t); len(fails) != 1 || fails[0].ResourceID != second || fails[0].Metadata["status"] != land.StatusConflict {
		t.Errorf("the land_fail events are %v, want one on demo#%s with status conflict", fails, second)
	}
}

// A land is asked for and answered still running when the hub stops
// before a worker took it up; the next start runs it to its end, once.
func TestLand_ALandInFlightDuringARestartFinishesAfterwards(t *testing.T) {
	parallel(t)
	paused := true
	h := &lifecycleHub{forkHub: startForkHubWith(t, nil, func(c *hub.Composition) {
		c.AdjustLand(func(d *land.Deps) { d.Paused = paused })
	})}
	owner := cloneAs(t, h.forkHub, h.key, "demo")
	old := fetchMain(owner)
	owner("checkout", "--quiet", "-b", "feat")
	commitFile(t, owner, "b.txt", "b\n", "Add b")
	commitFile(t, owner, "c.txt", "c\n", "Add c")
	owner("push", "--quiet", "origin", "feat")
	n := h.open(t, h.key, "demo", "feat", map[string]any{"intent": "why"})

	status, a := h.land(t, h.key, n, "?wait=0")
	if status != http.StatusAccepted || a["status"] != land.StatusRunning {
		t.Fatalf("the land of demo#%s = %d %v, want 202 running", n, status, a)
	}
	id := str(a, "id")
	paused = false
	h.Restart(t)

	deadline := time.Now().Add(20 * time.Second)
	for h.get(t, "/workspaces/demo/lands/"+id)["status"] != land.StatusLanded {
		if time.Now().After(deadline) {
			t.Fatalf("the attempt %s is %v 20 s after the start, want landed", id, h.get(t, "/workspaces/demo/lands/"+id))
		}
		time.Sleep(50 * time.Millisecond)
	}
	fetchMain(owner)
	if got := strings.Split(strings.TrimSpace(owner("log", "--format=%s", old+"..FETCH_HEAD")), "\n"); !reflect.DeepEqual(got, []string{"Add c", "Add b"}) {
		t.Errorf("main gained %v, want the change's two commits once", got)
	}
	if c := h.read(t, h.key, n); c["state"] != "landed" || str(c, "landed", "land_id") != id {
		t.Errorf("demo#%s is %v with landed %v, want landed by %s", n, c["state"], c["landed"], id)
	}
	if lands := h.changeEvents(t, n, 0, land.EventLand); len(lands) != 1 || lands[0].meta["land_id"] != id {
		t.Errorf("the land events of demo#%s are %v, want one, of %s", n, lands, id)
	}
	if items, _ := h.get(t, "/workspaces/demo/lands?status=running")["items"].([]any); len(items) != 0 {
		t.Errorf("the running attempts of demo are %v, want none", items)
	}
}
