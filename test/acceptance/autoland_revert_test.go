package acceptance

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

func TestAutoLand_LandsOnceTheRequiredCheckPasses(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	hub.SetPolicy(key, demo.Slug, `{"rules": [{"name": "code", "paths": ["src/**"], "require": {"checks": ["test"]}}]}`)
	hub.Call(http.MethodPost, scm+"/commits", key, map[string]any{
		"branch": "feat", "base_ref": "main", "message": "Edit src/a.txt", "operations": []any{put("src/a.txt", "edited\n")},
	}).Want(t, http.StatusCreated)
	opened := hub.Call(http.MethodPost, changes, key, map[string]any{
		"source": map[string]any{"branch": "feat"}, "title": "Edit src/a.txt", "intent": "edit a", "auto_land": true,
	}).Want(t, http.StatusCreated)
	n := harness.Num(opened.Body["number"])

	if c := hub.Get(key, changePath(n, "")).Body; c["state"] != "open" || c["auto_land"] != true {
		t.Fatalf("the change opened %v with auto_land %v, want open with auto_land true", c["state"], c["auto_land"])
	}
	// Nothing may land before the check is reported: the lander looks
	// every second, so three seconds give it every chance to.
	time.Sleep(3 * time.Second)
	if state := hub.Get(key, changePath(n, "")).Body["state"]; state != "open" {
		t.Fatalf("the change is %v before its check was reported, want open", state)
	}
	if lands := hub.Get(key, changePath(n, "/lands")).Items(); len(lands) != 0 {
		t.Fatalf("the change has the land attempts %v before its check was reported, want none", lands)
	}

	hub.Call(http.MethodPut, changePath(n, "/checks/test"), key, map[string]any{"status": "passed"}).Want(t, http.StatusOK)
	harness.Eventually(t, "the change to land by itself", func() bool {
		return hub.Get(key, changePath(n, "")).Body["state"] == "landed"
	})
	lands := hub.Get(key, changePath(n, "/lands")).Items()
	if len(lands) != 1 || lands[0]["trigger"] != "auto_land" || lands[0]["status"] != "landed" {
		t.Errorf("the change has the land attempts %v, want one, triggered by auto_land, landed", lands)
	}
	var events int
	for _, e := range eventLog(t, hub, key, "workspace=demo&types=parley.change.land", 0) {
		if e["resource_id"] == strconv.Itoa(n) {
			events++
		}
	}
	if events != 1 {
		t.Errorf("the log holds %d parley.change.land events of the change, want 1", events)
	}
}

func TestRevert_OneCallAndLandRestoresPreviousContent(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	seedFiveLines(t, hub, key)
	before := mainHead(t, hub, key)
	n := openChange(t, hub, key, "edit", map[string]string{"src/a.txt": lineThree("line three"), "n.txt": "new\n"})
	hub.Land(key, demo.Slug, n)

	revert := hub.CLI(key, "change", "revert", demo.Slug, strconv.Itoa(n))
	if revert.Code != 0 {
		t.Fatalf("parley change revert exited %d:\n%s", revert.Code, revert.Stderr)
	}
	m := harness.Num(revert.JSON()["number"])
	if state := hub.Get(key, changePath(m, "")).Body["state"]; m < 0 || state != "open" {
		t.Fatalf("parley change revert printed the change %d, which is %v, want an open change:\n%s", m, state, revert.Stdout)
	}
	hub.Land(key, demo.Slug, m)

	clone := hub.Clone(demo.Remote, key)
	tree := func(rev string) string {
		return strings.TrimSpace(hub.Git(clone, "", "rev-parse", rev+"^{tree}").Must(t).Stdout)
	}
	if got, want := tree("origin/main"), tree(before); got != want {
		t.Errorf("main's tree after the revert landed is %s, want %s, its tree before the change landed", got, want)
	}
	if r := hub.Call(http.MethodGet, scm+"/file?path=n.txt&ref=main", key, nil); r.Status != http.StatusNotFound {
		t.Errorf("reading n.txt on main answered %d, want 404: %s", r.Status, r.Raw)
	}
	again := hub.Call(http.MethodPost, changePath(n, "/revert"), key, nil)
	if again.Status != http.StatusConflict || again.ErrorType() != "already_reverted" {
		t.Errorf("a second revert answered %d %s, want 409 already_reverted", again.Status, again.Raw)
	}
}
