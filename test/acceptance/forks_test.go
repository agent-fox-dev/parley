package acceptance

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// forkPermissions are what an agent's fork token asks for to clone, push
// and open and land changes.
var forkPermissions = []string{"git:read", "git:write", "changes:read", "changes:write"}

// forkSlugs are the slugs of the forks GET …/workspaces/<parent>/forks
// lists for credential.
func forkSlugs(hub *harness.Hub, credential, parent string) []string {
	var slugs []string
	for _, f := range hub.Get(credential, "/api/v1/workspaces/"+parent+"/forks").Items() {
		slugs = append(slugs, harness.Str(f["slug"]))
	}
	return slugs
}

// lsRemoteHead is the commit ref points at in what git ls-remote prints.
func lsRemoteHead(out, ref string) string {
	for _, line := range strings.Split(out, "\n") {
		if sha, name, ok := strings.Cut(line, "\t"); ok && name == ref {
			return sha
		}
	}
	return ""
}

func TestFork_OneCallGivesForkRemoteAndToken(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	// A token minted with the fork holds only permissions its maker holds.
	pat := hub.PAT(demo.Owner, append([]string{"workspaces:write", "tokens:manage"}, forkPermissions...))

	fork := hub.Fork(pat, demo.Slug, forkPermissions, nil)
	ws, _ := fork.Body["workspace"].(map[string]any)
	if ws["kind"] != "fork" || ws["status"] != "ready" {
		t.Errorf("the fork is %v %v, want a ready fork", ws["kind"], ws["status"])
	}
	if fork.Remote == "" || !strings.HasPrefix(fork.Token, "parley_pat_") {
		t.Errorf("the answer has the remote %q and the token %q, want a remote and a parley_pat_ token", fork.Remote, fork.Token)
	}
	if got := harness.Field(fork.Body, "token.workspace"); got != fork.Slug {
		t.Errorf("the token is bound to %v, want the fork %s", got, fork.Slug)
	}

	// No wait between the answer and the clone.
	clone := hub.Clone(fork.Remote, fork.Token)
	main := harness.Str(hub.Get(demo.Owner.Key, "/api/v1/workspaces/demo/scm/refs/main").Body["sha"])
	if head := hub.Rev(clone, "HEAD"); head != main {
		t.Errorf("the clone's HEAD is %s, want demo's main %s", head, main)
	}
	hub.Git(clone, "", "checkout", "-q", "-b", "t").Must(t)
	hub.CommitFiles(clone, "Add the task's file", map[string]string{"task.txt": "task\n"})
	hub.Git(clone, "", "push", "-q", "origin", "t").Must(t)

	listed := hub.CLI(pat, "fork", "list", demo.Slug).Must(t).JSON()
	items, _ := listed["items"].([]any)
	var slugs []string
	for _, it := range items {
		f, _ := it.(map[string]any)
		slugs = append(slugs, harness.Str(f["slug"]))
	}
	if !slices.Contains(slugs, fork.Slug) {
		t.Errorf("parley fork list lists %v, want %s among them", slugs, fork.Slug)
	}
}

func TestFork_TokenIsRefusedOutsideTheFork(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	other := hub.Workspace(demo.Owner, "other")
	fork := hub.Fork(key, demo.Slug, []string{
		"git:read", "git:write", "changes:read", "changes:write", "workspaces:read", "workspaces:write", "audit:read",
	}, nil)
	refsBefore := hub.Get(key, "/api/v1/workspaces/demo/scm/refs").Raw
	revisionBefore := hub.Get(key, "/api/v1/workspaces/demo/policy").Body["revision"]

	commit := map[string]any{
		"branch": "intruder", "base_ref": "main", "message": "Intrude",
		"operations": []any{map[string]any{"op": "put", "path": "intruder.txt", "content": "x\n"}},
	}
	bodies := map[string][]byte{}
	for _, tc := range []struct {
		method, path string
		body         any
		status       int
		errorType    string
	}{
		{http.MethodGet, "/api/v1/workspaces/other", nil, http.StatusNotFound, "not_found"},
		{http.MethodGet, "/api/v1/workspaces/no-such", nil, http.StatusNotFound, "not_found"},
		{http.MethodGet, "/api/v1/workspaces/other/scm/refs", nil, http.StatusNotFound, "not_found"},
		{http.MethodGet, "/api/v1/workspaces/demo/scm/refs", nil, http.StatusForbidden, "token_bound"},
		{http.MethodPost, "/api/v1/workspaces/demo/scm/commits", commit, http.StatusForbidden, "token_bound"},
		{http.MethodPut, "/api/v1/workspaces/demo/policy", map[string]any{"rules": []any{}}, http.StatusForbidden, "token_bound"},
		{http.MethodPost, "/api/v1/workspaces/demo/forks", map[string]any{}, http.StatusForbidden, "token_bound"},
	} {
		r := hub.Call(tc.method, tc.path, fork.Token, tc.body)
		if r.Status != tc.status || r.ErrorType() != tc.errorType {
			t.Errorf("%s %s answered %d %s, want %d %s", tc.method, tc.path, r.Status, r.Raw, tc.status, tc.errorType)
		}
		bodies[tc.path] = r.Raw
	}
	if a, b := bodies["/api/v1/workspaces/other"], bodies["/api/v1/workspaces/no-such"]; string(a) != string(b) {
		t.Errorf("the answer for an existing workspace outside the fork is %s, for one that does not exist %s: want the same", a, b)
	}

	for _, remote := range []string{demo.Remote, other.Remote} {
		r := hub.Git("", "", "ls-remote", harness.AuthURL(remote, fork.Token))
		if r.Code == 0 || !strings.Contains(r.Stderr, "not found") {
			t.Errorf("git ls-remote %s with the fork's token exited %d (%s), want the repository not found", remote, r.Code, r.Stderr)
		}
	}

	// Every refusal is audited by design, so what is unwritten is the
	// parent's refs and policy.
	if after := hub.Get(key, "/api/v1/workspaces/demo/scm/refs").Raw; string(after) != string(refsBefore) {
		t.Errorf("demo's refs changed from %s to %s", refsBefore, after)
	}
	if after := hub.Get(key, "/api/v1/workspaces/demo/policy").Body["revision"]; after != revisionBefore {
		t.Errorf("demo's policy revision moved from %v to %v", revisionBefore, after)
	}
}

func TestFork_TokenReachesOwnChangeAndParentReads(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	// openFromFork pushes branch from a new fork of demo, which opens a
	// change on demo, and returns the fork and the change's number.
	openFromFork := func(branch string) (*harness.Fork, int) {
		t.Helper()
		fork := hub.Fork(key, demo.Slug, forkPermissions, nil)
		clone := hub.Clone(fork.Remote, fork.Token)
		hub.CommitFiles(clone, "Add "+branch, map[string]string{branch + ".txt": branch + "\n"})
		hub.PushChange(clone, "", branch, "add "+branch).Must(t)
		return fork, harness.Num(hub.ChangeOn(key, demo.Slug, branch)["number"])
	}
	f, numF := openFromFork("f-work")
	_, numG := openFromFork("g-work")

	for _, tc := range []struct {
		path   string
		status int
	}{
		{changePath(numF, ""), http.StatusOK},
		{"/api/v1/workspaces/demo/activity?paths=**", http.StatusOK},
		{"/api/v1/workspaces/demo/policy", http.StatusOK},
		{changePath(numG, ""), http.StatusNotFound},
	} {
		if r := hub.Call(http.MethodGet, tc.path, f.Token, nil); r.Status != tc.status {
			t.Errorf("GET %s with the fork's token answered %d %s, want %d", tc.path, r.Status, r.Raw, tc.status)
		}
	}
	hub.Land(f.Token, demo.Slug, numF)
}

func TestFork_PushToTrackingBranchIsRefused(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	fork := hub.Fork(key, demo.Slug, forkPermissions, nil)
	clone := hub.Clone(fork.Remote, fork.Token)
	const tracking = "tracks the parent workspace and is read-only in this fork"

	for _, tc := range []struct {
		name, branch, credential, refusal string
	}{
		// dev was in demo at fork time and is not protected.
		{"an unprotected tracking branch with the fork's token", "dev", fork.Token, tracking},
		{"the default branch with the owner's key", "main", key, tracking},
		// main is protected as well, and the protected-branch rule runs
		// before the tracking rule: a token bound to a workspace reaches a
		// protected branch only through a change.
		{"the default branch with the fork's token", "main", fork.Token, "main is protected"},
	} {
		hub.Git(clone, "", "checkout", "-q", "-B", tc.branch, "origin/"+tc.branch).Must(t)
		hub.CommitFiles(clone, "Change "+tc.branch+" in the fork", map[string]string{tc.branch + ".txt": tc.name + "\n"})
		r := hub.Git(clone, "", "push", harness.AuthURL(fork.Remote, tc.credential), tc.branch)
		if r.Code == 0 || !strings.Contains(r.Stderr, tc.refusal) {
			t.Errorf("%s: the push exited %d:\n%s\nwant a refusal holding %q", tc.name, r.Code, r.Stderr, tc.refusal)
		}
		ref := "refs/heads/" + tc.branch
		refs := hub.Git(clone, "", "ls-remote", "origin", ref).Must(t).Stdout
		parent := harness.Str(hub.Get(key, "/api/v1/workspaces/demo/scm/refs/"+tc.branch).Body["sha"])
		if got := lsRemoteHead(refs, ref); got != parent {
			t.Errorf("%s: the fork's %s is at %q, want demo's %s", tc.name, tc.branch, got, parent)
		}
	}
}

func TestFork_DefaultLifetimeIsSevenDays(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	fork := hub.Fork(key, demo.Slug, nil, nil)
	ws, _ := fork.Body["workspace"].(map[string]any)
	created, err1 := time.Parse(time.RFC3339, harness.Str(ws["created_at"]))
	expires, err2 := time.Parse(time.RFC3339, harness.Str(ws["expires_at"]))
	if err1 != nil || err2 != nil {
		t.Fatalf("the fork's times do not parse: %v %v (%v)", err1, err2, ws)
	}
	if got := expires.Sub(created); got != 168*time.Hour {
		t.Errorf("the fork expires %v after its creation, want 168h", got)
	}
	got := hub.CLI(key, "workspace", "get", fork.Slug).Must(t).JSON()["expires_at"]
	if got != ws["expires_at"] {
		t.Errorf("parley workspace get gives expires_at %v, want %v", got, ws["expires_at"])
	}
}

func TestLong_ForkDisappearsAtItsTTL(t *testing.T) {
	harness.Require(t, harness.Long)
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	created := time.Now()
	fork := hub.Fork(key, demo.Slug, forkPermissions, map[string]any{"ttl": "5m"})
	forkPath := "/api/v1/workspaces/" + fork.Slug
	// A live fork's token reaches no route outside its workspaces, so the
	// later 401 is its revocation and not its binding.
	if r := hub.Call(http.MethodGet, "/api/v1/user", fork.Token, nil); r.Status != http.StatusForbidden || r.ErrorType() != "token_bound" {
		t.Errorf("GET /user with the live fork's token answered %d %s, want 403 token_bound", r.Status, r.Raw)
	}

	// The scenario is about the passing of real time: nothing to poll for.
	time.Sleep(time.Until(created.Add(4 * time.Minute)))
	hub.Get(key, forkPath)

	harness.EventuallyWithin(t, time.Until(created.Add(7*time.Minute)), "the fork to be removed", func() bool {
		return hub.Call(http.MethodGet, forkPath, key, nil).Status == http.StatusNotFound
	})
	if r := hub.Call(http.MethodGet, forkPath, key, nil); r.ErrorType() != "not_found" {
		t.Errorf("GET of the removed fork answered %d %s, want 404 not_found", r.Status, r.Raw)
	}
	if r := hub.Call(http.MethodGet, "/api/v1/user", fork.Token, nil); r.Status != http.StatusUnauthorized || r.ErrorType() != "unauthenticated" {
		t.Errorf("GET /user with the removed fork's token answered %d %s, want 401 unauthenticated", r.Status, r.Raw)
	}

	// The event is filed under the fork, which its parent's owner sees.
	var expiries []map[string]any
	for _, e := range hub.Get(key, "/api/v1/events/log?types=parley.fork.expire").Items() {
		if e["resource_id"] == fork.Slug {
			expiries = append(expiries, e)
		}
	}
	if len(expiries) != 1 || harness.Field(expiries[0], "metadata.reason") != "ttl" {
		t.Errorf("the event log holds %v for the fork, want one parley.fork.expire with the reason ttl", expiries)
	}
	if slugs := forkSlugs(hub, key, demo.Slug); slices.Contains(slugs, fork.Slug) {
		t.Errorf("demo's forks %v still list the removed fork %s", slugs, fork.Slug)
	}
}

func TestLargeRepo_ForkCreationDoesNotGrowWithSize(t *testing.T) {
	harness.Require(t, harness.LargeRepo)
	t.Parallel()
	hub := harness.Start(t)
	alice := hub.User("alice")
	small := hub.Register(alice, "small", hub.SizedOrigin(1<<20), nil)
	// The hub clones the whole origin before the workspace is ready, and a
	// parent that is not ready refuses forks.
	big := hub.RegisterWithin(15*time.Minute, alice, "big", hub.SizedOrigin(1<<30), nil)

	median := map[string]time.Duration{}
	var last *harness.Fork
	for _, ws := range []*harness.Workspace{small, big} {
		var times []time.Duration
		for range 20 {
			start := time.Now()
			last = hub.Fork(alice.Key, ws.Slug, []string{"git:read"}, nil)
			times = append(times, time.Since(start))
		}
		slices.Sort(times)
		if slowest := times[len(times)-1]; slowest >= time.Second {
			t.Errorf("the slowest fork of %s took %v, want under 1s", ws.Slug, slowest)
		}
		median[ws.Slug] = (times[9] + times[10]) / 2
	}
	r := hub.Git("", "", "ls-remote", harness.AuthURL(last.Remote, last.Token)).Must(t)
	if lsRemoteHead(r.Stdout, "refs/heads/main") == "" {
		t.Errorf("git ls-remote of the last fork of big lists no main:\n%s", r.Stdout)
	}
	if d := median["big"] - median["small"]; d > 200*time.Millisecond {
		t.Errorf("the median fork of big took %v, of small %v: %v more, want at most 200ms", median["big"], median["small"], d)
	}
}
