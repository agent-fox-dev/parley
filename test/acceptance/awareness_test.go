package acceptance

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// activityOfA is the activity route of demo for the file src/a.txt.
const activityOfA = "/api/v1/workspaces/demo/activity?paths=src/a.txt"

// overlapEvents are the parley.change.overlap events of the event log.
func overlapEvents(t *testing.T, hub *harness.Hub, credential string) []map[string]any {
	t.Helper()
	return eventLog(t, hub, credential, "types=parley.change.overlap", 0)
}

// numbersAt are the numbers at path, such as "change.number", of the
// objects, sorted.
func numbersAt(objects []map[string]any, path string) []int {
	numbers := []int{}
	for _, o := range objects {
		numbers = append(numbers, harness.Num(harness.Field(o, path)))
	}
	slices.Sort(numbers)
	return numbers
}

// objectsOf are the objects of the list at member of m.
func objectsOf(m map[string]any, member string) []map[string]any {
	raw, _ := m[member].([]any)
	objects := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		o, _ := it.(map[string]any)
		objects = append(objects, o)
	}
	return objects
}

// claimIDs are the ids of an activity answer's claims.
func claimIDs(activity map[string]any) []string {
	ids := []string{}
	for _, c := range objectsOf(activity, "claims") {
		ids = append(ids, harness.Str(c["id"]))
	}
	return ids
}

// activityWithBoth waits until the activity of src/a.txt lists the changes
// x and y, which the awareness worker indexes in the background, and
// returns it.
func activityWithBoth(t *testing.T, hub *harness.Hub, credential string, x, y int) map[string]any {
	t.Helper()
	var a harness.Response
	harness.Eventually(t, "the activity of src/a.txt to list both changes", func() bool {
		a = hub.Get(credential, activityOfA)
		return slices.Equal(numbersAt(objectsOf(a.Body, "changes"), "change.number"), sorted2(x, y))
	})
	return a.Body
}

// sorted2 is x and y in ascending order.
func sorted2(x, y int) []int { return []int{min(x, y), max(x, y)} }

func TestAwareness_OverlapIsAnnouncedOncePerChange(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	x := openChange(t, hub, key, "x", map[string]string{"src/a.txt": "from x\n"})
	y := openChange(t, hub, key, "y", map[string]string{"docs/b.md": "# From y\n"})

	clone := hub.Clone(demo.Remote, key)
	hub.Git(clone, "", "checkout", "-q", "y").Must(t)
	hub.CommitFiles(clone, "Edit src/a.txt in y", map[string]string{"src/a.txt": "from y\n"})
	hub.Git(clone, "", "push", "-q", "origin", "y").Must(t)

	var events []map[string]any
	harness.Eventually(t, "two parley.change.overlap events", func() bool {
		events = overlapEvents(t, hub, key)
		return len(events) >= 2
	})
	if len(events) != 2 {
		t.Fatalf("the log holds %d parley.change.overlap events, want 2: %v", len(events), events)
	}
	named := map[string]int{}
	for _, e := range events {
		named[harness.Str(e["resource_id"])] = harness.Num(harness.Field(e, "metadata.with.number"))
	}
	if want := map[string]int{strconv.Itoa(x): y, strconv.Itoa(y): x}; !maps.Equal(named, want) {
		t.Errorf("the overlap events name %v (change: the other change), want %v", named, want)
	}
	if got := numbersAt(hub.Get(key, changePath(x, "/overlaps")).Items(), "change.number"); !slices.Equal(got, []int{y}) {
		t.Errorf("change %d overlaps %v, want [%d]", x, got, y)
	}

	// A further version that keeps the overlap announces nothing: the
	// awareness worker runs every second, so five seconds give it five
	// passes over the new version.
	hub.CommitFiles(clone, "Edit src/a.txt in y again", map[string]string{"src/a.txt": "from y, again\n"})
	hub.Git(clone, "", "push", "-q", "origin", "y").Must(t)
	time.Sleep(5 * time.Second)
	if events := overlapEvents(t, hub, key); len(events) != 2 {
		t.Errorf("after a further overlapping version the log holds %d parley.change.overlap events, want 2: %v", len(events), events)
	}
}

func TestAwareness_ActivityReturnsChangesAndLiveClaim(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	x := openChange(t, hub, key, "x", map[string]string{"src/a.txt": "from x\n"})
	y := openChange(t, hub, key, "y", map[string]string{"src/a.txt": "from y\n"})
	created := hub.CLI(key, "claim", "create", demo.Slug, "--paths", "src/**", "--intent", "refactor", "--ttl", "1h").Must(t)
	claim := harness.Str(harness.Field(created.JSON(), "claim.id"))
	if claim == "" {
		t.Fatalf("parley claim create printed no claim id:\n%s", created.Stdout)
	}

	a := activityWithBoth(t, hub, key, x, y)
	if ids := claimIDs(a); !slices.Equal(ids, []string{claim}) {
		t.Errorf("the activity lists the claims %v, want [%s]", ids, claim)
	} else if state := objectsOf(a, "claims")[0]["state"]; state != "active" {
		t.Errorf("the claim's state is %v, want active", state)
	}
	out := hub.CLI(key, "activity", demo.Slug, "--paths", "src/a.txt").Must(t).JSON()
	if ids := claimIDs(out); !slices.Equal(ids, []string{claim}) {
		t.Errorf("parley activity lists the claims %v, want [%s]", ids, claim)
	}
	if got := numbersAt(objectsOf(out, "changes"), "change.number"); !slices.Equal(got, sorted2(x, y)) {
		t.Errorf("parley activity lists the changes %v, want %v", got, sorted2(x, y))
	}
}

func TestAwareness_ExpiredClaimLeavesActivity(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	x := openChange(t, hub, key, "x", map[string]string{"src/a.txt": "from x\n"})
	y := openChange(t, hub, key, "y", map[string]string{"src/a.txt": "from y\n"})
	// The changes are indexed first, so that the read right after the
	// claim falls within its two seconds.
	activityWithBoth(t, hub, key, x, y)

	claim, _ := hub.Call(http.MethodPost, "/api/v1/workspaces/demo/claims", key, map[string]any{
		"paths": []string{"src/**"}, "intent": "i", "ttl": "2s",
	}).Want(t, http.StatusCreated).Body["claim"].(map[string]any)
	id := harness.Str(claim["id"])
	if ids := claimIDs(hub.Get(key, activityOfA).Body); !slices.Contains(ids, id) {
		t.Fatalf("the activity right after the claim lists the claims %v, want %s among them", ids, id)
	}

	createdAt, err := time.Parse(time.RFC3339, harness.Str(claim["created_at"]))
	if err != nil {
		t.Fatalf("the claim's created_at %v: %v", claim["created_at"], err)
	}
	time.Sleep(time.Until(createdAt.Add(4 * time.Second)))
	a := hub.Get(key, activityOfA).Body
	if ids := claimIDs(a); len(ids) != 0 {
		t.Errorf("4 s after its creation the activity lists the claims %v, want none", ids)
	}
	if got := numbersAt(objectsOf(a, "changes"), "change.number"); !slices.Equal(got, sorted2(x, y)) {
		t.Errorf("the activity lists the changes %v, want %v", got, sorted2(x, y))
	}
	if state := hub.Get(key, "/api/v1/workspaces/demo/claims/"+id).Body["state"]; state != "expired" {
		t.Errorf("the claim reads back with the state %v, want expired", state)
	}
}

func TestAwareness_NeverRefusesAPushOrALand(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	cast := hub.Cast()
	key := cast.OwnerKey
	seedFiveLines(t, hub, key)
	x := openChange(t, hub, key, "x", map[string]string{"src/a.txt": strings.Replace(fiveLines, "line 1\n", "line 1 by x\n", 1)})
	y := openChange(t, hub, key, "y", map[string]string{"src/a.txt": strings.Replace(fiveLines, "line 5\n", "line 5 by y\n", 1)})
	harness.Eventually(t, "the two changes to be announced as overlapping", func() bool {
		return len(hub.Get(key, changePath(x, "/overlaps")).Items()) == 1
	})

	// bob, another user, claims the same files under a session of his own.
	// A member who may only read demo cannot start a session on it, so he
	// starts it on his fork of demo, which lets him name it on demo.
	bobFork := hub.Fork(cast.MemberKey, cast.Demo.Slug, nil, nil)
	session := harness.Str(hub.CLI(cast.MemberKey, "session", "start", bobFork.Slug).Must(t).JSON()["id"])
	if session == "" {
		t.Fatal("parley session start printed no id for bob")
	}
	hub.Call(http.MethodPost, "/api/v1/workspaces/demo/claims", cast.MemberKey, map[string]any{
		"paths": []string{"src/**"}, "intent": "refactor src", "ttl": "1h", "session_id": session,
	}).Want(t, http.StatusCreated)
	if claims := claimIDs(hub.Get(key, activityOfA).Body); len(claims) != 1 {
		t.Fatalf("the activity of src/a.txt lists the claims %v, want bob's alone", claims)
	}

	clone := hub.Clone(cast.Demo.Remote, key)
	hub.Git(clone, "", "checkout", "-q", "x").Must(t)
	hub.CommitFiles(clone, "Add src/x.txt", map[string]string{"src/x.txt": "x\n"})
	if push := hub.Git(clone, "", "push", "origin", "x"); push.Code != 0 || strings.Contains(push.Stderr, "error") {
		t.Errorf("the push to x exited %d, want 0 without an error:\n%s", push.Code, push.Stderr)
	}

	yHead := harness.Str(hub.Get(key, scm+"/refs/y").Body["sha"])
	commit := hub.Call(http.MethodPost, scm+"/commits", key, map[string]any{
		"branch": "y", "expected_head_sha": yHead, "message": "Add src/y.txt", "operations": []any{put("src/y.txt", "y\n")},
	})
	if commit.Status != http.StatusCreated {
		t.Errorf("the API commit on y answered %d %s, want 201", commit.Status, commit.Raw)
	}

	if l := hub.Land(key, cast.Demo.Slug, x); l.Body["status"] != "landed" {
		t.Errorf("the land of %d answered %s, want the status landed", x, l.Raw)
	}
	harness.Eventually(t, "y to be mergeable clean", func() bool {
		return hub.Get(key, changePath(y, "")).Body["mergeable"] == "clean"
	})
	if l := hub.Land(key, cast.Demo.Slug, y); l.Body["status"] != "landed" {
		t.Errorf("the land of %d answered %s, want the status landed", y, l.Raw)
	}
}
