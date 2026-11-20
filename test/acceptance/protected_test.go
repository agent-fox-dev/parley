package acceptance

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// mainRef is the source control path of demo's main.
const mainRef = scm + "/refs/main"

// The texts of a refused update of a protected main over git, and of an
// org member's refused push, which is refused for the member's read-only
// access before any rule looks at the branch.
const (
	viaChangeText = "main is protected; push another branch and open a change"
	rewriteText   = "main is protected and cannot be force-pushed or deleted"
	readOnlyText  = "org members have read-only access to this workspace; create a task fork to contribute"
)

// attempt is how the hub took one attempt to move demo's main: the result
// of a git push or the answer of an API call, and the commit the attempt
// asked main to move to ("" for a delete).
type attempt struct {
	git *harness.Result
	api *harness.Response
	sha string
}

// mainUpdates attempts updates of demo's main of the cast, each with the
// credential it is given, from one clone that each push first brings to
// main's head.
type mainUpdates struct {
	t     *testing.T
	hub   *harness.Hub
	cast  *harness.Cast
	clone string
	n     int
}

func newMainUpdates(t *testing.T, hub *harness.Hub, cast *harness.Cast) *mainUpdates {
	t.Helper()
	return &mainUpdates{t: t, hub: hub, cast: cast, clone: hub.Clone(cast.Demo.Remote, cast.OwnerKey)}
}

// next numbers the commits the updates make, whose files must differ.
func (u *mainUpdates) next() int {
	u.n++
	return u.n
}

// push pushes with credential a new commit on top of main to main.
func (u *mainUpdates) push(credential string) attempt {
	u.t.Helper()
	hub, n := u.hub, u.next()
	hub.Git(u.clone, "", "fetch", "-q", "origin", "main").Must(u.t)
	hub.Git(u.clone, "", "checkout", "-q", "-B", "main", "FETCH_HEAD").Must(u.t)
	hub.CommitFiles(u.clone, fmt.Sprintf("Update main directly, %d", n), map[string]string{fmt.Sprintf("direct-%d.txt", n): "direct\n"})
	r := hub.Git(u.clone, "", "push", "-q", harness.AuthURL(u.cast.Demo.Remote, credential), "HEAD:refs/heads/main")
	return attempt{git: &r, sha: hub.Rev(u.clone, "HEAD")}
}

// commit commits with credential a new file on main through the source
// control API.
func (u *mainUpdates) commit(credential string) attempt {
	u.t.Helper()
	n := u.next()
	r := u.hub.Call(http.MethodPost, scm+"/commits", credential, map[string]any{
		"branch": "main", "expected_head_sha": mainHead(u.t, u.hub, u.cast.OwnerKey),
		"message": fmt.Sprintf("Update main through the API, %d", n), "operations": []any{put(fmt.Sprintf("api-%d.txt", n), "api\n")},
	})
	return attempt{api: &r, sha: harness.Str(harness.Field(r.Body, "commit.sha"))}
}

// move moves main with credential through the source control API to a
// descendant that demo's owner commits on a side branch.
func (u *mainUpdates) move(credential string) attempt {
	u.t.Helper()
	n, head := u.next(), mainHead(u.t, u.hub, u.cast.OwnerKey)
	side := u.hub.Call(http.MethodPost, scm+"/commits", u.cast.OwnerKey, map[string]any{
		"branch": fmt.Sprintf("side-%d", n), "base_ref": "main",
		"message": fmt.Sprintf("Prepare a move of main, %d", n), "operations": []any{put(fmt.Sprintf("side-%d.txt", n), "side\n")},
	}).Want(u.t, http.StatusCreated)
	sha := harness.Str(harness.Field(side.Body, "commit.sha"))
	r := u.hub.Call(http.MethodPatch, mainRef, credential, map[string]any{"sha": sha, "expected_sha": head})
	return attempt{api: &r, sha: sha}
}

// forcePush pushes with credential main's parent, which main descends
// from, to main with --force.
func (u *mainUpdates) forcePush(credential string) attempt {
	u.t.Helper()
	parent := u.hub.Rev(u.clone, "origin/main~1")
	r := u.hub.Git(u.clone, "", "push", "-q", "--force", harness.AuthURL(u.cast.Demo.Remote, credential), parent+":refs/heads/main")
	return attempt{git: &r, sha: parent}
}

// deletePush deletes main with credential over git.
func (u *mainUpdates) deletePush(credential string) attempt {
	u.t.Helper()
	r := u.hub.Git(u.clone, "", "push", "-q", harness.AuthURL(u.cast.Demo.Remote, credential), "--delete", "main")
	return attempt{git: &r}
}

// rewind moves main with credential through the source control API back
// to its parent, which is no descendant of it; the clone's main is main's
// head, which these updates leave where it is.
func (u *mainUpdates) rewind(credential string) attempt {
	u.t.Helper()
	parent := u.hub.Rev(u.clone, "origin/main~1")
	r := u.hub.Call(http.MethodPatch, mainRef, credential, map[string]any{"sha": parent, "expected_sha": mainHead(u.t, u.hub, u.cast.OwnerKey)})
	return attempt{api: &r, sha: parent}
}

// remove deletes main with credential through the source control API.
func (u *mainUpdates) remove(credential string) attempt {
	u.t.Helper()
	r := u.hub.Call(http.MethodDelete, mainRef+"?expected_sha="+mainHead(u.t, u.hub, u.cast.OwnerKey), credential, nil)
	return attempt{api: &r}
}

// accepted fails the test unless a was taken: the push exited 0 or the
// call was answered status.
func accepted(t *testing.T, what string, a attempt, status int) {
	t.Helper()
	switch {
	case a.git != nil && a.git.Code != 0:
		t.Fatalf("%s: git push exited %d:\n%s", what, a.git.Code, a.git.Stderr)
	case a.api != nil && a.api.Status != status:
		t.Fatalf("%s: answered %d, want %d: %s", what, a.api.Status, status, a.api.Raw)
	}
}

// refused fails the test unless a was refused: a push that exited non-zero
// with text on its standard error, or a call answered 403 with errorType.
func refused(t *testing.T, what string, a attempt, text, errorType string) {
	t.Helper()
	switch {
	case a.git != nil && (a.git.Code == 0 || !strings.Contains(a.git.Stderr, text)):
		t.Errorf("%s: git push exited %d with\n%s\nwant a refusal saying %q", what, a.git.Code, a.git.Stderr, text)
	case a.api != nil && (a.api.Status != http.StatusForbidden || a.api.ErrorType() != errorType):
		t.Errorf("%s: answered %d %s, want 403 %s", what, a.api.Status, a.api.Raw, errorType)
	}
}

// mainPushes are the parley.git.push events of demo's main that credential
// sees, oldest first.
func mainPushes(t *testing.T, hub *harness.Hub, credential string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range eventLog(t, hub, credential, "workspace=demo&types=parley.git.push", 0) {
		if harness.Field(e, "metadata.ref") == "refs/heads/main" {
			out = append(out, e)
		}
	}
	return out
}

// pushesTo are the events of pushes whose metadata.after is sha.
func pushesTo(events []map[string]any, sha string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if harness.Field(e, "metadata.after") == sha {
			out = append(out, e)
		}
	}
	return out
}

func TestProtected_MovesByLandingAndBySync(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	// The origin takes pushes, so that it can be brought to the landed
	// commit before a commit of its own is added on top of it.
	demo := hub.Register(hub.User("alice"), "demo", hub.PushableOrigin(), nil)
	key := demo.Owner.Key
	n := openChange(t, hub, key, "feat", map[string]string{"feat.txt": "feature\n"})

	landed := harness.Str(hub.Land(key, demo.Slug, n).Body["landed_sha"])
	if head := mainHead(t, hub, key); landed == "" || head != landed {
		t.Fatalf("after the land main is at %s, want the attempt's landed commit %q", head, landed)
	}
	if at := hub.PushToOrigin(demo, "main"); at != landed {
		t.Fatalf("the origin's main is at %q after the push, want the landed commit %s", at, landed)
	}
	o1 := demo.Origin.CommitFiles("main", "Fix upstream", map[string]string{"upstream.txt": "fixed\n"})

	sync := hub.CLI(key, "workspace", "sync", demo.Slug)
	if sync.Code != 0 {
		t.Fatalf("parley workspace sync exited %d:\n%s", sync.Code, sync.Stderr)
	}
	var result any
	branches, _ := sync.JSON()["branches"].([]any)
	for _, b := range branches {
		if m, _ := b.(map[string]any); m["name"] == "main" {
			result = m["result"]
		}
	}
	if result != "fast_forward" {
		t.Errorf("the sync's result for main is %v, want fast_forward:\n%s", result, sync.Stdout)
	}
	if head := mainHead(t, hub, key); head != o1 {
		t.Errorf("after the sync main is at %s, want the origin's commit %s", head, o1)
	}
}

func TestProtected_OwnerAndAdminDirectPushSucceedAndAreRecorded(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	cast := hub.Cast()
	u := newMainUpdates(t, hub, cast)

	for _, tc := range []struct {
		name       string
		credential string
		update     func(string) attempt
		status     int
	}{
		{"owner key, git push", cast.OwnerKey, u.push, 0},
		{"owner key, API commit", cast.OwnerKey, u.commit, http.StatusCreated},
		{"owner key, API ref move", cast.OwnerKey, u.move, http.StatusOK},
		{"admin token, git push", cast.Admin, u.push, 0},
	} {
		a := tc.update(tc.credential)
		accepted(t, tc.name, a, tc.status)
		if head := mainHead(t, hub, cast.OwnerKey); a.sha == "" || head != a.sha {
			t.Fatalf("%s: main is at %s, want the new commit %q", tc.name, head, a.sha)
		}
		var events []map[string]any
		harness.Eventually(t, "the push of "+a.sha+" in the event log", func() bool {
			events = pushesTo(mainPushes(t, hub, cast.OwnerKey), a.sha)
			return len(events) > 0
		})
		if len(events) != 1 || harness.Field(events[0], "metadata.direct") != true {
			t.Errorf("%s: the log holds the pushes %v of main to %s, want one, direct", tc.name, events, a.sha)
		}
	}
}

func TestProtected_BoundTokenPathTokenAndMemberAreRefused(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	cast := hub.Cast()
	u := newMainUpdates(t, hub, cast)
	before := mainHead(t, hub, cast.OwnerKey)
	pushes := len(mainPushes(t, hub, cast.OwnerKey))

	for _, who := range []struct {
		name, credential string
		// gitText and errorType are how the refusal reads: an org member
		// is refused for read-only access before the protected rule.
		gitText, errorType string
	}{
		{"owner's bound token", cast.BoundPAT, viaChangeText, "protected_branch"},
		{"owner's path-scoped token", cast.PathPAT, viaChangeText, "protected_branch"},
		{"member's key", cast.MemberKey, readOnlyText, "forbidden"},
	} {
		for _, op := range []struct {
			name   string
			update func(string) attempt
		}{{"git push", u.push}, {"API commit", u.commit}, {"API ref move", u.move}} {
			refused(t, who.name+", "+op.name, op.update(who.credential), who.gitText, who.errorType)
		}
	}
	if head := mainHead(t, hub, cast.OwnerKey); head != before {
		t.Errorf("main is at %s after the refused updates, want %s", head, before)
	}
	if n := len(mainPushes(t, hub, cast.OwnerKey)); n != pushes {
		t.Errorf("the log holds %d pushes of main, want the %d it held before", n, pushes)
	}
}

func TestProtected_ForceAndDeleteRefusedForEveryCredential(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	cast := hub.Cast()
	u := newMainUpdates(t, hub, cast)
	before := mainHead(t, hub, cast.OwnerKey)

	for _, who := range []struct {
		name, credential, gitText, errorType string
	}{
		{"owner's key", cast.OwnerKey, rewriteText, "protected_branch"},
		{"owner's bound token", cast.BoundPAT, rewriteText, "protected_branch"},
		{"owner's path-scoped token", cast.PathPAT, rewriteText, "protected_branch"},
		{"member's key", cast.MemberKey, readOnlyText, "forbidden"},
		{"admin token", cast.Admin, rewriteText, "protected_branch"},
	} {
		for _, op := range []struct {
			name   string
			update func(string) attempt
		}{{"git push --force", u.forcePush}, {"git push --delete", u.deletePush}, {"API move back", u.rewind}, {"API delete", u.remove}} {
			refused(t, who.name+", "+op.name, op.update(who.credential), who.gitText, who.errorType)
		}
	}
	if head := mainHead(t, hub, cast.OwnerKey); head != before {
		t.Errorf("main is at %s after the refused rewrites, want %s", head, before)
	}
}

func TestProtected_OwnerPushOffRefusesEveryDirectUpdate(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	cast := hub.Cast()
	hub.CLI(cast.OwnerKey, "workspace", "update", cast.Demo.Slug, "--setting", "protect.owner_push=false").Must(t)
	settings, _ := hub.Get(cast.OwnerKey, "/api/v1/workspaces/demo").Body["settings"].(map[string]any)
	if settings["protect.owner_push"] != false {
		t.Fatalf("demo's settings are %v, want protect.owner_push false", settings)
	}
	u := newMainUpdates(t, hub, cast)
	before := mainHead(t, hub, cast.OwnerKey)

	for _, who := range []struct{ name, credential string }{{"owner's key", cast.OwnerKey}, {"admin token", cast.Admin}} {
		for _, op := range []struct {
			name   string
			update func(string) attempt
		}{{"git push", u.push}, {"API commit", u.commit}, {"API ref move", u.move}} {
			refused(t, who.name+", "+op.name, op.update(who.credential), "is protected", "protected_branch")
		}
	}
	if head := mainHead(t, hub, cast.OwnerKey); head != before {
		t.Fatalf("main is at %s after the refused updates, want %s", head, before)
	}
	n := openChange(t, hub, cast.OwnerKey, "feat", map[string]string{"feat.txt": "feature\n"})
	hub.Land(cast.OwnerKey, cast.Demo.Slug, n)
	if head := mainHead(t, hub, cast.OwnerKey); head == before {
		t.Errorf("main is still at %s after the land, want it moved", head)
	}
}
