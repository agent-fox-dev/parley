package acceptance

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// pushedChange is the change number a push's remote lines name.
var pushedChange = regexp.MustCompile(`change \S+#(\d+) version`)

// changeOfPush is the number of the change the push answered with, failing
// the test when its remote lines name none.
func changeOfPush(t *testing.T, push harness.Result) int {
	t.Helper()
	m := pushedChange.FindStringSubmatch(push.Stderr)
	if m == nil {
		t.Fatalf("the push names no change:\n%s", push.Stderr)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func TestJourney_AgentForksPushesAndLands(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	alice := hub.User("alice")
	// A token minted with a fork holds no permission its minter lacks.
	pat := hub.PAT(alice, append([]string{"workspaces:write", "tokens:manage"}, harness.AgentScopes...))
	origin := hub.Origin()

	created := hub.CLI(alice.Key, "workspace", "create", "--slug", "demo", "--git-url", origin.URL(), "--wait").Must(t)
	if status := created.JSON()["status"]; status != "ready" {
		t.Fatalf("parley workspace create printed the status %v, want ready:\n%s", status, created.Stdout)
	}
	fork := hub.Fork(pat, "demo", []string{"git:read", "git:write", "changes:read", "changes:write"}, nil)

	clone := hub.Clone(fork.Remote, fork.Token)
	hub.Git(clone, "", "checkout", "-q", "-b", "task").Must(t)
	hub.CommitFiles(clone, "Add the agent's file", map[string]string{"agent.txt": "from the agent\n"})
	n := changeOfPush(t, hub.PushChange(clone, "", "task", "add the agent's file").Must(t))
	if open := hub.Get(alice.Key, changes+"?state=open").Items(); len(open) != 1 || harness.Num(open[0]["number"]) != n {
		t.Fatalf("demo's open changes are %v, want change %d alone", open, n)
	}

	l := hub.CLI(fork.Token, "change", "land", "demo", strconv.Itoa(n))
	if l.Code != 0 || l.JSON()["status"] != "landed" {
		t.Fatalf("parley change land exited %d with %s%s, want 0 and the status landed", l.Code, l.Stdout, l.Stderr)
	}

	if state := hub.Get(alice.Key, changePath(n, "")).Body["state"]; state != "landed" {
		t.Errorf("change %d is %v, want landed", n, state)
	}
	if lands := hub.Get(alice.Key, changePath(n, "/lands")).Items(); len(lands) != 1 || lands[0]["status"] != "landed" {
		t.Errorf("the change's land attempts are %v, want one, landed", lands)
	}
	fresh := hub.Clone(harness.Str(hub.WaitReady(alice.Key, "demo")["hub_url"]), alice.Key)
	if got := hub.Git(fresh, "", "show", "origin/main:agent.txt").Must(t).Stdout; got != "from the agent\n" {
		t.Errorf("agent.txt at the head of main holds %q, want the agent's file", got)
	}
	if merges := hub.Git(fresh, "", "rev-list", "--min-parents=2", "origin/main").Must(t).Lines(); len(merges) != 0 {
		t.Errorf("main's history holds the merge commits %v, want none", merges)
	}

	// The fork and the push to it are events of the fork, which the owner
	// of its parent sees; the change and its land are demo's. A push maps
	// itself to changes before its own event is written, so the change's
	// open comes before the push that opened it.
	seqOf := map[string]int{}
	var events []string
	for _, e := range eventLog(t, hub, alice.Key, "", 0) {
		key := harness.Str(e["type"]) + " " + harness.Str(e["workspace"])
		if _, ok := seqOf[key]; !ok {
			seqOf[key] = harness.Num(e["seq"])
		}
		events = append(events, key)
	}
	forked, push, open, land := seqOf["parley.workspace.fork "+fork.Slug], seqOf["parley.git.push "+fork.Slug],
		seqOf["parley.change.open demo"], seqOf["parley.change.land demo"]
	if forked == 0 || push == 0 || open == 0 || land == 0 || forked > min(push, open) || max(push, open) > land {
		t.Errorf("the event log %v does not hold the fork, then the push and the change's open, then its land", events)
	}

	// The fork's token stays confined to the fork and its parent.
	hub.Register(alice, "second", origin, nil)
	if r := hub.Call(http.MethodGet, "/api/v1/workspaces/second", fork.Token, nil); r.Status != http.StatusNotFound {
		t.Errorf("the fork's token on a second workspace answered %d %s, want 404", r.Status, r.Raw)
	}
}

func TestJourney_TwoAgentsRaceAndTheLoserResolves(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	seedFiveLines(t, hub, key)

	var numbers []int
	tokens := map[int]string{}
	for i, line := range []string{"line 3 from the first agent", "line 3 from the second agent"} {
		fork := hub.Fork(key, demo.Slug, harness.AgentScopes, nil)
		clone := hub.Clone(fork.Remote, fork.Token)
		branch := "agent-" + strconv.Itoa(i+1)
		hub.Git(clone, "", "checkout", "-q", "-b", branch).Must(t)
		hub.CommitFiles(clone, "Rewrite line 3", map[string]string{"src/a.txt": lineThree(line)})
		n := changeOfPush(t, hub.PushChange(clone, "", branch, "rewrite line 3").Must(t))
		numbers = append(numbers, n)
		tokens[n] = fork.Token
	}
	for _, n := range numbers {
		harness.Eventually(t, "change "+strconv.Itoa(n)+" to be mergeable clean", func() bool {
			return hub.Get(key, changePath(n, "")).Body["mergeable"] == "clean"
		})
	}

	r := raceToLand(t, hub, numbers[0], tokens[numbers[0]], numbers[1], tokens[numbers[1]])
	if r.lose.ErrorType() != "land_conflict" {
		t.Fatalf("the losing land answered %s, want land_conflict", r.lose.Raw)
	}
	lostID := harness.Str(harness.Field(r.lose.Body, "error.details.id"))
	loserToken := tokens[r.loser]
	if body := hub.Get(loserToken, changePath(r.loser, "/conflict/file?path=src/a.txt")).Raw; !strings.Contains(string(body), "<<<<<<<") {
		t.Errorf("the conflicted file %q has no conflict marker", body)
	}
	resolved := lineThree("line 3 from both agents")
	resolve(t, hub, loserToken, r.loser, resolved)
	landWithCLI(t, hub, loserToken, r.loser)

	for _, n := range numbers {
		if state := hub.Get(key, changePath(n, "")).Body["state"]; state != "landed" {
			t.Errorf("change %d is %v, want landed", n, state)
		}
	}
	if body := string(hub.Get(key, scm+"/file?path=src/a.txt&ref=main").Raw); body != resolved {
		t.Errorf("src/a.txt on main holds %q, want the resolution %q", body, resolved)
	}
	// Attempts are listed newest first, those of one second in no fixed
	// order: each change's are compared as a set.
	statuses := func(path string) []string {
		var out []string
		for _, a := range hub.Get(key, path).Items() {
			out = append(out, harness.Str(a["status"]))
		}
		return sorted(out)
	}
	if got := statuses(changePath(r.winner, "/lands")); !slices.Equal(got, []string{"landed"}) {
		t.Errorf("the winner's land attempts are %v, want one, landed", got)
	}
	if got := statuses(changePath(r.loser, "/lands")); !slices.Equal(got, []string{"conflict", "landed"}) {
		t.Errorf("the loser's land attempts are %v, want a conflict and a landed one", got)
	}
	if got := statuses("/api/v1/workspaces/demo/lands"); !slices.Equal(got, []string{"conflict", "landed", "landed"}) {
		t.Errorf("demo's land attempts are %v, want two landed and one conflict", got)
	}

	failed, landed := false, map[string]int{}
	for _, e := range eventLog(t, hub, key, "workspace=demo", 0) {
		switch e["type"] {
		case "parley.change.land_fail":
			failed = failed || (harness.Str(e["resource_id"]) == strconv.Itoa(r.loser) && harness.Field(e, "metadata.land_id") == lostID)
		case "parley.change.land":
			landed[harness.Str(e["resource_id"])]++
		}
	}
	if !failed {
		t.Errorf("demo's event log holds no parley.change.land_fail for the attempt %s of change %d", lostID, r.loser)
	}
	for _, n := range numbers {
		if landed[strconv.Itoa(n)] != 1 {
			t.Errorf("demo's event log holds %d parley.change.land for change %d, want 1", landed[strconv.Itoa(n)], n)
		}
	}
}

func TestJourney_ConsumerResumesTheEventStream(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	// A runner reporting checks holds a token of the owner: write access
	// to a workspace is its owner's.
	ci := hub.PAT(demo.Owner, []string{"audit:read", "changes:read", "checks:write"})
	claim := func(intent string) {
		t.Helper()
		hub.Call(http.MethodPost, "/api/v1/workspaces/demo/claims", key, map[string]any{
			"paths": []string{"docs/**"}, "intent": intent,
		}).Want(t, http.StatusCreated)
	}

	r := hub.Events(ci, "workspace=demo")
	claim("watch the stream")
	s0 := r.Next(1, 10*time.Second)[0].Seq
	r.Close()

	clone := hub.Clone(demo.Remote, key)
	hub.Git(clone, "", "checkout", "-q", "-b", "one").Must(t)
	hub.CommitFiles(clone, "Add one", map[string]string{"one.txt": "one\n"})
	hub.Land(key, demo.Slug, changeOfPush(t, hub.PushChange(clone, "", "one", "add one").Must(t)))
	hub.Git(clone, "", "checkout", "-q", "-b", "two", "main").Must(t)
	hub.CommitFiles(clone, "Add two", map[string]string{"two.txt": "two\n"})
	n2 := changeOfPush(t, hub.PushChange(clone, "", "two", "add two").Must(t))

	want := eventLog(t, hub, key, "workspace=demo", s0)
	var wantSeqs []int64
	for _, e := range want {
		wantSeqs = append(wantSeqs, int64(harness.Num(e["seq"])))
	}
	r2 := hub.Events(ci, "workspace=demo", "Last-Event-ID", strconv.FormatInt(s0, 10))
	got := r2.Next(len(want), 10*time.Second)
	var gotSeqs []int64
	for _, e := range got {
		gotSeqs = append(gotSeqs, e.Seq)
	}
	if !slices.Equal(gotSeqs, wantSeqs) {
		t.Errorf("the resumed stream replayed the seqs %v, want the log's after %d: %v", gotSeqs, s0, wantSeqs)
	}
	claim("after the replay")
	// The hub keeps writing events of its own (a change turning ready, an
	// overlap), so the live claim may come after some of them: each event
	// read on must still be newer than the last one replayed.
	last := gotSeqs[len(gotSeqs)-1]
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := r2.Next(1, time.Until(deadline))[0]
		if live.Seq <= last {
			t.Errorf("the live event %s has seq %d, want above %d", live.Type, live.Seq, last)
		}
		last = live.Seq
		if live.Type == "parley.claim.create" {
			break
		}
	}
	seen := map[int64]bool{}
	for _, e := range r2.All() {
		if seen[e.Seq] || e.Seq <= s0 {
			t.Errorf("the resumed stream sent the seq %d, which is a repeat or at most %d", e.Seq, s0)
		}
		seen[e.Seq] = true
	}

	// parley events tail replays the same events from the same position.
	var tailSeqs []int64
	for _, line := range hub.Tail(ci, len(wantSeqs), 30*time.Second, "--workspace", "demo", "--after", strconv.FormatInt(s0, 10)) {
		var rec struct{ Seq int64 }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("parley events tail printed %q, want an event record: %v", line, err)
		}
		tailSeqs = append(tailSeqs, rec.Seq)
	}
	if !slices.Equal(tailSeqs, wantSeqs) {
		t.Errorf("parley events tail --after %d printed the seqs %v first, want the log's %v", s0, tailSeqs, wantSeqs)
	}

	// The review agent learned of the second change from its replayed
	// parley.change.open.
	n := -1
	for _, e := range got {
		if e.Type == "parley.change.open" {
			n, _ = strconv.Atoi(harness.Str(e.Record["resource_id"]))
		}
	}
	if n != n2 {
		t.Fatalf("the last replayed parley.change.open names change %d, want %d", n, n2)
	}
	hub.Call(http.MethodPut, changePath(n, "/checks/test"), ci, map[string]any{"status": "passed"}).Want(t, http.StatusOK)
	if checks := hub.Get(ci, changePath(n, "/checks")).Items(); len(checks) == 0 || checks[0]["name"] != "test" {
		t.Errorf("the change's checks are %v, want the check test", checks)
	}
}
