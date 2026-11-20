package acceptance

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// openChange commits the files on top of main on the new branch of demo
// through the source control API with credential, asking the commit to
// open a change of the branch, and returns the change's number.
func openChange(t *testing.T, hub *harness.Hub, credential, branch string, files map[string]string) int {
	t.Helper()
	var ops []any
	for path, content := range files {
		ops = append(ops, put(path, content))
	}
	r := hub.Call(http.MethodPost, scm+"/commits", credential, map[string]any{
		"branch": branch, "base_ref": "main", "message": "Change " + branch, "operations": ops,
		"change": map[string]any{"title": "Change " + branch, "intent": "change " + branch},
	}).Want(t, http.StatusCreated)
	number := harness.Num(harness.Field(r.Body, "change.number"))
	if number < 0 {
		t.Fatalf("the commit on %s opened no change: %s", branch, r.Raw)
	}
	return number
}

// mainHead is the commit demo's main points at.
func mainHead(t *testing.T, hub *harness.Hub, credential string) string {
	t.Helper()
	return mainHeadOf(t, hub, credential, "demo")
}

// mainHeadOf is the commit main of the workspace slug points at.
func mainHeadOf(t *testing.T, hub *harness.Hub, credential, slug string) string {
	t.Helper()
	return harness.Str(hub.Get(credential, "/api/v1/workspaces/"+slug+"/scm/refs/main").Body["sha"])
}

// fiveLines is src/a.txt as seedFiveLines puts it on main; lineThree is
// it with its third line replaced by line.
const fiveLines = "line 1\nline 2\nline 3\nline 4\nline 5\n"

func lineThree(line string) string { return strings.Replace(fiveLines, "line 3\n", line+"\n", 1) }

// seedFiveLines commits src/a.txt with five lines on demo's main, as its
// owner pushes directly to it.
func seedFiveLines(t *testing.T, hub *harness.Hub, owner string) {
	t.Helper()
	hub.Call(http.MethodPost, scm+"/commits", owner, map[string]any{
		"branch": "main", "expected_head_sha": mainHead(t, hub, owner), "message": "Add src/a.txt",
		"operations": []any{put("src/a.txt", fiveLines)},
	}).Want(t, http.StatusCreated)
}

// landPath is the land route of change number of demo, waiting up to 60 s
// for the attempt to end.
func landPath(number int) string { return changePath(number, "/land?wait=60") }

// race is how two land requests sent at the same moment were answered:
// the one answered 200 won, the other lost.
type race struct {
	winner, loser int
	win, lose     harness.Response
}

// raceToLand sends the land requests of the changes p and q of demo at
// the same moment, each with its own credential, and fails the test
// unless one is answered 200 and the other 409.
func raceToLand(t *testing.T, hub *harness.Hub, p int, credP string, q int, credQ string) race {
	t.Helper()
	numbers, creds := []int{p, q}, []string{credP, credQ}
	answers := make([]harness.Response, 2)
	harness.Release(2, func(i int) {
		answers[i] = hub.Call(http.MethodPost, landPath(numbers[i]), creds[i], nil)
	})()
	r := race{winner: p, loser: q, win: answers[0], lose: answers[1]}
	if answers[1].Status == http.StatusOK {
		r = race{winner: q, loser: p, win: answers[1], lose: answers[0]}
	}
	if r.win.Status != http.StatusOK || r.lose.Status != http.StatusConflict {
		t.Fatalf("the two lands answered %d %s and %d %s, want one 200 and one 409",
			answers[0].Status, answers[0].Raw, answers[1].Status, answers[1].Raw)
	}
	return r
}

// spacedMergeability gives a hub the default spacing of the mergeability
// checks of one change, for a scenario that lands many changes at once and
// reads no mergeability: without it every land checks every other open
// change again at once, work the suite pays for in CPU and the scenario
// does not look at.
const spacedMergeability = "[changes]\nmergeable_debounce = \"5s\"\n"

func TestLand_TwentyDisjointChangesLandConcurrently(t *testing.T) {
	t.Parallel()
	// Twenty lands racing for one ref need more compare-and-swap tries
	// than the default allows.
	hub := harness.Start(t, harness.Options{TOML: "[land]\nmax_attempts = 20\n\n" + spacedMergeability})
	demo := hub.Workspace(hub.User("alice"), "demo")
	key := demo.Owner.Key
	const n = 20
	numbers := make([]int, n)
	for i := range n {
		numbers[i] = openChange(t, hub, key, fmt.Sprintf("c%d", i+1), map[string]string{fmt.Sprintf("f/%d.txt", i+1): "file\n"})
	}
	base := mainHead(t, hub, key)

	answers := make([]harness.Response, n)
	harness.Release(n, func(i int) {
		answers[i] = hub.Call(http.MethodPost, landPath(numbers[i]), key, nil)
	})()
	for i, a := range answers {
		if a.Status != http.StatusOK || a.Body["status"] != "landed" {
			t.Errorf("the land of c%d answered %d %s, want 200 with the status landed", i+1, a.Status, a.Raw)
		}
	}

	clone := hub.Clone(demo.Remote, key)
	if count := strings.TrimSpace(hub.Git(clone, "", "rev-list", "--count", base+"..origin/main").Must(t).Stdout); count != strconv.Itoa(n) {
		t.Errorf("main has %s new commits, want %d", count, n)
	}
	if merges := hub.Git(clone, "", "rev-list", "--min-parents=2", base+"..origin/main").Must(t).Lines(); len(merges) != 0 {
		t.Errorf("main's new history holds the merge commits %v, want none", merges)
	}
	for i, number := range numbers {
		path := fmt.Sprintf("f/%d.txt", i+1)
		if adds := hub.Git(clone, "", "log", "--format=%H", base+"..origin/main", "--", path).Must(t).Lines(); len(adds) != 1 {
			t.Errorf("%s is touched by %d of main's new commits, want 1", path, len(adds))
		}
		if state := hub.Get(key, changePath(number, "")).Body["state"]; state != "landed" {
			t.Errorf("change %d is %v, want landed", number, state)
		}
	}
}

func TestLand_SameLinesExactlyOneLands(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	seedFiveLines(t, hub, key)
	p := openChange(t, hub, key, "p", map[string]string{"src/a.txt": lineThree("line 3 from p")})
	q := openChange(t, hub, key, "q", map[string]string{"src/a.txt": lineThree("line 3 from q")})

	r := raceToLand(t, hub, p, key, q, key)
	if r.win.Body["status"] != "landed" {
		t.Errorf("the winning land answered %s, want the status landed", r.win.Raw)
	}
	if r.lose.ErrorType() != "land_conflict" {
		t.Errorf("the losing land answered %s, want land_conflict", r.lose.Raw)
	}
	attempt, _ := harness.Field(r.lose.Body, "error.details").(map[string]any)
	if attempt["status"] != "conflict" {
		t.Errorf("the losing attempt has the status %v, want conflict", attempt["status"])
	}
	if files := members(attempt, "conflict.files", "path"); !slices.Contains(files, "src/a.txt") {
		t.Errorf("the conflict report names the files %v, want src/a.txt among them", files)
	}
	if landed := landedNumbers(attempt); !slices.Contains(landed, r.winner) {
		t.Errorf("the conflict report names the landed changes %v, want %d among them", landed, r.winner)
	}

	body := string(hub.Get(key, scm+"/file?path=src/a.txt&ref=main").Raw)
	winLine, loseLine := fmt.Sprintf("line 3 from %s", branchOf(r.winner, p)), fmt.Sprintf("line 3 from %s", branchOf(r.loser, p))
	if !strings.Contains(body, winLine) || strings.Contains(body, loseLine) || strings.Contains(body, "<<<<<<<") {
		t.Errorf("src/a.txt on main holds %q, want %q without %q or a conflict marker", body, winLine, loseLine)
	}
	if state := hub.Get(key, changePath(r.loser, "")).Body["state"]; state != "open" {
		t.Errorf("the losing change is %v, want open", state)
	}
	var ids []string
	for _, a := range hub.Get(key, "/api/v1/workspaces/demo/lands?status=conflict").Items() {
		ids = append(ids, harness.Str(a["id"]))
	}
	if !slices.Contains(ids, harness.Str(attempt["id"])) {
		t.Errorf("the conflict attempts are %v, want the losing attempt %v among them", ids, attempt["id"])
	}
}

// landedNumbers are the numbers of the landed changes of an attempt's
// conflict report.
func landedNumbers(attempt map[string]any) []int {
	raw, _ := harness.Field(attempt, "conflict.landed_changes").([]any)
	var out []int
	for _, it := range raw {
		c, _ := it.(map[string]any)
		out = append(out, harness.Num(c["number"]))
	}
	return out
}

// branchOf is the name of the branch of change number of the same-lines
// race, whose change p is on branch p.
func branchOf(number, p int) string {
	if number == p {
		return "p"
	}
	return "q"
}

func TestLand_KilledHubLeavesNoHalfLandedChange(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t, harness.Options{TOML: spacedMergeability})
	demo := hub.Workspace(hub.User("alice"), "demo")
	key := demo.Owner.Key
	landed, leftOpen := 0, 0
	for _, delay := range []time.Duration{0, 20 * time.Millisecond, 60 * time.Millisecond, 150 * time.Millisecond} {
		const n = 10
		numbers := make([]int, n)
		file := func(i int) string { return fmt.Sprintf("k%d/%d.txt", delay.Milliseconds(), i) }
		for i := range n {
			numbers[i] = openChange(t, hub, key, fmt.Sprintf("k%d-%d", delay.Milliseconds(), i), map[string]string{file(i): "file\n"})
		}

		wait := harness.Release(n, func(i int) { hub.Call(http.MethodPost, landPath(numbers[i]), key, nil) })
		// The scenario is about the moment of the kill: real time, not a
		// condition to wait for.
		time.Sleep(delay)
		hub.Kill()
		// The requests not yet answered fail with the connection; none may
		// reach the hub started again.
		wait()
		hub.Start()

		// A land in flight at the kill is run again after the start, and
		// ends.
		inFlight := members(hub.Get(key, "/api/v1/workspaces/demo/lands?status=running").Body, "items", "id")
		harness.Eventually(t, "no land attempt to be running", func() bool {
			return len(hub.Get(key, "/api/v1/workspaces/demo/lands?status=running").Items()) == 0
		})
		for _, id := range inFlight {
			if status := hub.Get(key, "/api/v1/workspaces/demo/lands/"+id).Body["status"]; status == "running" || status == nil {
				t.Errorf("kill after %v: the land attempt %s, running at the start, is %v, want it ended", delay, id, status)
			}
		}
		landEvents := map[string]int{}
		for _, e := range eventLog(t, hub, key, "workspace=demo&types=parley.change.land", 0) {
			landEvents[harness.Str(e["resource_id"])]++
		}
		clone := hub.Clone(demo.Remote, key)
		files := hub.Git(clone, "", "ls-tree", "-r", "--name-only", "origin/main").Must(t).Lines()
		var open []int
		for i, number := range numbers {
			state := hub.Get(key, changePath(number, "")).Body["state"]
			held, events := slices.Contains(files, file(i)), landEvents[strconv.Itoa(number)]
			switch {
			case state == "landed" && held && events == 1:
				landed++
			case state == "open" && !held && events == 0:
				open = append(open, number)
			default:
				t.Errorf("kill after %v: change %d is %v, main holds its file: %v, %d parley.change.land events; want landed, true, 1 or open, false, 0",
					delay, number, state, held, events)
			}
		}
		leftOpen += len(open)
		t.Logf("kill after %v: %d of %d changes left open, %d land attempts running at the start", delay, len(open), n, len(inFlight))
		if len(open) > 0 {
			if r := hub.Call(http.MethodPost, landPath(open[0]), key, nil); r.Status != http.StatusOK || r.Body["status"] != "landed" {
				t.Errorf("kill after %v: the land of the open change %d answered %d %s, want 200 landed", delay, open[0], r.Status, r.Raw)
			}
		}
	}
	// Without a landed change and one left open no kill came while lands
	// were under way, and the scenario would have shown nothing.
	if landed == 0 || leftOpen == 0 {
		t.Errorf("%d changes landed and %d were left open over the rounds, want at least one of each: the kills to come while lands were under way", landed, leftOpen)
	}
}
