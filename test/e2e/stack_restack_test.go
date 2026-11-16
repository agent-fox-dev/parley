package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/cli"
)

// restackDeadline is how long a test waits for the hub to restack or flag
// a change in the background.
const restackDeadline = 10 * time.Second

// summaries are the numbers and the needs_restack of the changes of a
// stack as the stack route answers it.
func summaries(stack map[string]any) (numbers []float64, flagged []any) {
	list, _ := stack["changes"].([]any)
	for _, item := range list {
		s, _ := item.(map[string]any)
		numbers, flagged = append(numbers, s["number"].(float64)), append(flagged, s["needs_restack"])
	}
	return numbers, flagged
}

// version is version v of the change number of demo, read by the owner.
func (h *lifecycleHub) version(t *testing.T, number, v string) map[string]any {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/"+number+"/versions/"+v, h.key, nil)
	if status != http.StatusOK {
		t.Fatalf("GET changes/%s/versions/%s = %d %s, want 200", number, v, status, body)
	}
	return decoded(t, body)
}

// A developer amends the bottom of a stack of three changes and
// force-pushes it; the hub restacks the two changes above in the
// background, bottom first, and the stack reads as built on the amended
// commit.
func TestStackRestack_AnAmendedBottomRestacksTheChangesAboveIt(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	owner := cloneAs(t, h.forkHub, h.key, "demo")
	branches := []string{"low", "mid", "top"}
	for _, b := range branches {
		owner("checkout", "--quiet", "-b", b)
		commitFile(t, owner, b+".txt", b+"\n", "Add "+b)
		owner("push", "--quiet", "origin", b)
	}
	for _, b := range branches {
		h.open(t, h.key, "demo", b, map[string]any{"intent": "why"})
	}
	if c := h.read(t, h.key, "3"); c["depends_on"] != float64(2) {
		t.Fatalf("demo#3 depends on %v, want 2", c["depends_on"])
	}

	owner("checkout", "--quiet", "low")
	amended := commitFile(t, owner, "low.txt", "low, amended\n", "Add low, amended", "--amend")
	owner("push", "--quiet", "--force", "origin", "low")
	h.within(t, restackDeadline, "3", "restacked", func(c map[string]any) bool { return c["version"] == float64(2) })

	for _, n := range []string{"2", "3"} {
		if v := h.version(t, n, "2"); v["kind"] != change.KindRestack || str(v, "created_by", "actor_type") != change.ActorSystem {
			t.Errorf("version 2 of demo#%s = %v, want a restack created by the system", n, v)
		}
	}
	owner("fetch", "--quiet", "origin")
	owner("merge-base", "--is-ancestor", amended, "origin/top")

	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/3/stack", h.key, nil)
	numbers, flagged := summaries(decoded(t, body))
	if status != http.StatusOK || !reflect.DeepEqual(numbers, []float64{1, 2, 3}) || !reflect.DeepEqual(flagged, []any{false, false, false}) {
		t.Errorf("GET changes/3/stack = %d %s, want changes 1, 2, 3, none needing a restack", status, body)
	}

	status, body = h.Call(t, http.MethodGet, "/api/v1/events/log?after=0&types="+change.EventRestack, h.Token, nil)
	var page struct {
		Items []struct {
			ResourceID string `json:"resource_id"`
			Metadata   struct {
				Trigger string `json:"trigger"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET events/log = %d %s, want 200 with a page", status, body)
	}
	var restacked []string
	for _, e := range page.Items {
		restacked = append(restacked, e.ResourceID+" "+e.Metadata.Trigger)
	}
	if want := []string{"2 version", "3 version"}; !reflect.DeepEqual(restacked, want) {
		t.Errorf("the restack events are %v, want %v", restacked, want)
	}
}

// A developer's new commit below makes the restack of the change above
// conflict, which the hub reports on the change and the CLI shows as data;
// the developer rebases the change by hand and pushes it, which clears the
// flag.
func TestStackRestack_AConflictingRestackIsReportedAndAPushResolvesIt(t *testing.T) {
	h := startLifecycleHub(t)
	owner := cloneAs(t, h.forkHub, h.key, "demo")
	owner("checkout", "--quiet", "-b", "low")
	commitFile(t, owner, "f.txt", "low\n", "Add f")
	owner("push", "--quiet", "origin", "low")
	owner("checkout", "--quiet", "-b", "top")
	commitFile(t, owner, "f.txt", "top\n", "Change f")
	owner("push", "--quiet", "origin", "top")
	h.open(t, h.key, "demo", "low", map[string]any{"intent": "why"})
	h.open(t, h.key, "demo", "top", map[string]any{"intent": "why"})

	owner("checkout", "--quiet", "low")
	commitFile(t, owner, "f.txt", "low again\n", "Change f below")
	owner("push", "--quiet", "origin", "low")
	c := h.within(t, restackDeadline, "2", "flagged", func(c map[string]any) bool { return c["needs_restack"] == true })
	conflict, _ := c["conflict"].(map[string]any)
	files, _ := conflict["files"].([]any)
	if c["restack_reason"] != change.RestackConflict || c["version"] != float64(1) || len(files) == 0 || str(files[0].(map[string]any), "path") != "f.txt" {
		t.Fatalf("after the push below demo#2 is %v, want version 1 in conflict on f.txt", c)
	}

	// The agent's shell holds the hub's address and its API key.
	t.Setenv("PARLEY_ENDPOINT_URL", h.URL(""))
	t.Setenv("PARLEY_API_KEY", h.key)
	parley := func(args ...string) ran {
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), args, &stdout, &stderr)
		return ran{code, stdout.String(), stderr.String()}
	}
	r := parley("change", "restack", "demo", "2")
	if r.code != 1 || r.stdout != "" || r.errorType(t) != "rebase_conflict" {
		t.Errorf("change restack exited %d with stdout %q and stderr %s, want 1, nothing and rebase_conflict", r.code, r.stdout, r.stderr)
	}

	owner("checkout", "--quiet", "top")
	owner("reset", "--quiet", "--hard", "low")
	commitFile(t, owner, "f.txt", "low again\ntop\n", "Change f on top of low")
	owner("push", "--quiet", "--force", "origin", "top")
	c = h.within(t, restackDeadline, "2", "unflagged at version 2", func(c map[string]any) bool { return c["version"] == float64(2) })
	if c["needs_restack"] != false || c["restack_reason"] != nil || h.version(t, "2", "2")["kind"] != change.KindPush {
		t.Errorf("after the fixing push demo#2 is %v, want version 2 of kind push, needs_restack false and no reason", c)
	}

	r = parley("change", "stack", "demo", "2")
	if r.code != 0 {
		t.Fatalf("change stack exited %d: %s", r.code, r.stderr)
	}
	if numbers, flagged := summaries(r.json(t)); !reflect.DeepEqual(numbers, []float64{1, 2}) || !reflect.DeepEqual(flagged, []any{false, false}) {
		t.Errorf("change stack printed %s, want changes 1 and 2, neither needing a restack", r.stdout)
	}
}
