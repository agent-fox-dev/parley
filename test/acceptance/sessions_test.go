package acceptance

import (
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// sessionThread is what an agent session of alice did on demo: the fork it
// was named on, the two changes it opened by pushing from the fork, and
// the first of them, which it landed.
type sessionThread struct {
	hub     *harness.Hub
	key     string
	session string
	fork    *harness.Fork
	c1, c2  int
}

// newSessionThread starts a session with parley session start, creates a
// fork naming it, pushes two branches from the fork naming it, and lands
// the first change with parley change land --session.
func newSessionThread(t *testing.T) *sessionThread {
	t.Helper()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	session := harness.Str(hub.CLI(key, "session", "start", demo.Slug).Must(t).JSON()["id"])
	if session == "" {
		t.Fatal("parley session start printed no id")
	}
	fork := hub.Fork(key, demo.Slug, forkPermissions, map[string]any{"session_id": session})

	clone := hub.Clone(fork.Remote, fork.Token)
	hub.Git(clone, "", "checkout", "-q", "-b", "one").Must(t)
	hub.CommitFiles(clone, "Add one", map[string]string{"one.txt": "one\n"})
	c1 := changeOfPush(t, hub.PushChange(clone, "", "one", "add one", "parley.session="+session).Must(t))
	hub.Git(clone, "", "checkout", "-q", "-b", "two", "main").Must(t)
	hub.CommitFiles(clone, "Add two", map[string]string{"two.txt": "two\n"})
	c2 := changeOfPush(t, hub.PushChange(clone, "", "two", "add two", "parley.session="+session).Must(t))

	hub.CLI(fork.Token, "change", "land", demo.Slug, strconv.Itoa(c1), "--session", session).Must(t)
	harness.Eventually(t, "change "+strconv.Itoa(c1)+" to be landed", func() bool {
		return hub.Get(key, changePath(c1, "")).Body["state"] == "landed"
	})
	return &sessionThread{hub: hub, key: key, session: session, fork: fork, c1: c1, c2: c2}
}

func TestSession_ReachesForkChangesVersionsAndLands(t *testing.T) {
	t.Parallel()
	s := newSessionThread(t)
	hub, key := s.hub, s.key
	want := sorted2(s.c1, s.c2)

	got := hub.Get(key, "/api/v1/sessions/"+s.session).Body
	if forks := members(got, "forks", ""); !slices.Contains(forks, s.fork.Slug) {
		t.Errorf("the session lists the forks %v, want %s among them", forks, s.fork.Slug)
	}
	if numbers := numbersAt(objectsOf(got, "changes"), "number"); !slices.Equal(numbers, want) {
		t.Errorf("the session lists the changes %v, want %v", numbers, want)
	}
	listed := hub.Get(key, changes+"?session_id="+s.session+"&state=open&state=landed").Items()
	if numbers := numbersAt(listed, "number"); !slices.Equal(numbers, want) {
		t.Errorf("the changes of the session are %v, want %v", numbers, want)
	}
	for _, c := range want {
		versions := hub.Get(key, changePath(c, "/versions")).Items()
		if len(versions) == 0 {
			t.Errorf("change %d lists no version", c)
		}
		for _, v := range versions {
			if v["session_id"] != s.session {
				t.Errorf("version %v of change %d has the session %v, want %s", v["number"], c, v["session_id"], s.session)
			}
		}
	}
	lands := hub.Get(key, changePath(s.c1, "/lands")).Items()
	if len(lands) != 1 || lands[0]["session_id"] != s.session || lands[0]["status"] != "landed" {
		t.Errorf("change %d lists the land attempts %v, want one landed with the session %s", s.c1, lands, s.session)
	}
	if id := hub.CLI(key, "session", "get", s.session).Must(t).JSON()["id"]; id != s.session {
		t.Errorf("parley session get prints the id %v, want %s", id, s.session)
	}
}

func TestSession_IsReachableFromALandedCommit(t *testing.T) {
	t.Parallel()
	s := newSessionThread(t)
	hub, key := s.hub, s.key

	lands := hub.Get(key, changePath(s.c1, "/lands")).Items()
	if len(lands) == 0 {
		t.Fatalf("change %d lists no land attempt", s.c1)
	}
	sha := harness.Str(lands[0]["landed_sha"])
	commit := hub.Get(key, "/api/v1/workspaces/demo/scm/commits/"+sha).Body
	if commit["session_id"] != s.session {
		t.Errorf("the landed commit %s names the session %v, want %s", sha, commit["session_id"], s.session)
	}
	session := hub.Call(http.MethodGet, "/api/v1/sessions/"+harness.Str(commit["session_id"]), key, nil)
	if session.Status != http.StatusOK || session.Body["id"] != s.session {
		t.Errorf("the commit's session reads %d %s, want 200 with the id %s", session.Status, session.Raw, s.session)
	}
	if n := harness.Num(harness.Field(hub.Get(key, changes+"/by-commit/"+sha).Body, "change.number")); n != s.c1 {
		t.Errorf("by-commit of %s names change %d, want %d", sha, n, s.c1)
	}
}
