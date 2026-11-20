package acceptance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// methods are the methods a wrong-method request is chosen from.
var methods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// runID is the run the telemetry routes are given: the form a run id must
// have.
const runID = "20261120_120000_abcdef"

// seeded is a hub with the resources the routes' placeholders name, and
// the identifier that fills each placeholder.
type seeded struct {
	t     *testing.T
	hub   *harness.Hub
	alice *harness.User
	// ids maps a placeholder, written as the segment before it, a slash
	// and the placeholder (workspaces/:slug), to its identifier. An entry
	// prefixed with a seed's name and a space applies to the rows of that
	// seed alone. The paging and idempotency scenarios keep the
	// identifiers of their parents in it by a name of their own.
	ids map[string]string
	// origin is the origin and upstream of the carry-patch workspace.
	origin *harness.Origin
	// forge is the fake forge the workspace of forgedHub reads and writes
	// as the repository acme/<its slug>.
	forge *harness.Forge
}

// forgedHub starts a hub that trusts a fake forge, with alice signed in
// and her workspace slug registered from an origin the forge knows as
// acme/<slug>, whose forge it is.
func forgedHub(t *testing.T, slug string) *seeded {
	t.Helper()
	forge := harness.NewForge(t)
	hub := harness.Start(t, harness.Options{TOML: "[webhooks]\nallow_insecure = true\n", Env: forge.Env()})
	alice := hub.User("alice")
	s := &seeded{t: t, hub: hub, alice: alice, ids: map[string]string{}, forge: forge}
	hub.Register(alice, slug, hub.Origin(harness.WithRepoPath("acme/"+slug+".git")), nil)
	s.call(http.MethodPatch, wsPath(slug, ""), alice.Key, map[string]any{"settings": map[string]any{"forge_api_url": forge.URL}}, http.StatusOK)
	return s
}

// path is the row's path with every placeholder replaced by its
// identifier, or by "none", which names nothing, when the seeding gave it
// none.
func (s *seeded) path(r route) string {
	segs := strings.Split(r.path, "/")
	for i, seg := range segs {
		if seg == "" || (seg[0] != ':' && seg[0] != '*') {
			continue
		}
		key := segs[i-1] + "/" + seg
		v, ok := s.ids[r.seed+" "+key]
		if !ok {
			v, ok = s.ids[key]
		}
		if !ok {
			v = "none"
		}
		segs[i] = v
	}
	return strings.Join(segs, "/")
}

// call sends method path with credential and body, failing the test unless
// the answer has status, and returns it.
func (s *seeded) call(method, path, credential string, body any, status int) harness.Response {
	s.t.Helper()
	return s.hub.Call(method, path, credential, body).Want(s.t, status)
}

// wsPath is the path of the workspace slug followed by rest.
func wsPath(slug, rest string) string { return "/api/v1/workspaces/" + slug + rest }

// entries is the body of a create of secrets or variables: one entry per
// key, each with a value of its own.
func entries(keys ...string) map[string]any {
	var list []any
	for _, k := range keys {
		list = append(list, map[string]any{"key": k, "value": "value of " + k})
	}
	return map[string]any{"entries": list}
}

// commitOn commits a file named after the branch on top of base in the
// workspace slug through the source control API, opening a change of the
// branch when title is not "", and returns the answer.
func (s *seeded) commitOn(credential, slug, branch, base, title string) harness.Response {
	s.t.Helper()
	body := map[string]any{
		"branch": branch, "base_ref": base, "message": "Add " + branch,
		"operations": []any{put(branch+".txt", branch+"\n")},
	}
	if title != "" {
		body["change"] = map[string]any{"title": title, "intent": "seed " + branch}
	}
	return s.call(http.MethodPost, wsPath(slug, "/scm/commits"), credential, body, http.StatusCreated)
}

// carryPatch registers the carry-patch workspace slug, owned by alice,
// whose origin and upstream are one origin holding the patch branches, by
// name, each a commit on main adding its files, and returns the origin.
func (s *seeded) carryPatch(slug string, patches map[string]map[string]string) *harness.Origin {
	s.t.Helper()
	o := s.hub.Origin()
	for name, files := range patches {
		o.PatchBranch(name, files)
	}
	s.hub.Register(s.alice, slug, o, map[string]any{"workspace_mode": "carry_patch", "upstream_url": o.URL()})
	return o
}

// seedIdentifiers starts a hub and gives it one resource of every kind a
// placeholder of the routes names, except those a hub without an OpenShell
// gateway or a forge cannot have.
func seedIdentifiers(t *testing.T) *seeded {
	t.Helper()
	hub := harness.Start(t, harness.Options{TOML: "[webhooks]\nallow_insecure = true\n"})
	alice := hub.User("alice")
	s := &seeded{t: t, hub: hub, alice: alice, ids: map[string]string{}}
	key := alice.Key
	hub.Workspace(alice, "demo")
	s.ids["workspaces/:slug"] = "demo"

	s.ids["orgs/:id"] = hub.CreateOrg("acme")
	s.ids["orgs/:slug"] = "acme"
	hub.AddMember("acme", alice)
	s.ids["members/:user_id"] = alice.ID
	s.ids["users/:id"] = alice.ID
	s.ids["keys/:key_id"] = harness.Str(hub.Get(key, "/api/v1/user/keys").Items()[0]["key_id"])
	s.ids["tokens/:token_id"] = harness.Str(s.call(http.MethodPost, "/api/v1/user/tokens", key,
		map[string]any{"name": "seeded", "permissions": []string{"git:read"}}, http.StatusCreated).Body["token_id"])
	s.ids["sessions/:id"] = harness.Str(s.call(http.MethodPost, "/api/v1/sessions", key,
		map[string]any{"workspace_slug": "demo"}, http.StatusCreated).Body["id"])

	c := s.commitOn(key, "demo", "seeded", "main", "Seeded change")
	number := fmt.Sprint(harness.Num(harness.Field(c.Body, "change.number")))
	s.ids["changes/:number"], s.ids["versions/:version"] = number, "1"
	s.ids["by-commit/:sha"] = harness.Str(harness.Field(c.Body, "commit.sha"))
	s.ids["commits/:sha"] = s.ids["by-commit/:sha"]
	change := wsPath("demo", "/changes/"+number)
	s.call(http.MethodPut, change+"/checks/ci", key, map[string]any{"status": "passed"}, http.StatusOK)
	s.ids["checks/:name"] = "ci"
	s.ids["approvals/:id"] = harness.Str(s.call(http.MethodPost, change+"/approvals", key,
		map[string]any{"verdict": "approve"}, http.StatusCreated).Body["id"])
	s.ids["lands/:id"] = harness.Str(s.call(http.MethodPost, change+"/land", key, nil, http.StatusOK).Body["id"])
	s.ids["branches/:name"], s.ids["refs/*name"] = "main", "main"
	s.ids["claims/:id"] = harness.Str(harness.Field(s.call(http.MethodPost, wsPath("demo", "/claims"), key,
		map[string]any{"paths": []string{"a.txt"}, "intent": "seed"}, http.StatusCreated).Body, "claim.id"))

	for scope, credential := range map[string]string{wsPath("demo", ""): key, "/api/v1/user": key, "/api/v1/orgs/acme": hub.AdminToken} {
		s.call(http.MethodPost, scope+"/secrets", credential, entries("SEEDED"), http.StatusCreated)
		s.call(http.MethodPost, scope+"/vars", credential, entries("SEEDED"), http.StatusCreated)
	}
	s.ids["secrets/:key"], s.ids["vars/:key"] = "SEEDED", "SEEDED"

	receiver := harness.NewReceiver(t)
	webhook := harness.Str(s.call(http.MethodPost, wsPath("demo", "/webhooks"), key,
		map[string]any{"url": receiver.URL, "types": []string{"parley.webhook.ping"}}, http.StatusCreated).Body["id"])
	s.ids["webhooks/:id"] = webhook
	s.ids["deliveries/:delivery_id"] = harness.Str(s.call(http.MethodPost, wsPath("demo", "/webhooks/"+webhook+"/ping"), key,
		nil, http.StatusOK).Body["id"])

	s.ids["runs/:run_id"] = runID
	s.call(http.MethodPost, wsPath("demo", "/runs/"+runID+"/events"), key, map[string]any{"event_type": "seeded"}, http.StatusCreated)

	s.carryPatch("carry", map[string]map[string]string{"patch-1": {"patch-1.txt": "one\n"}})
	s.ids["carryPatch workspaces/:slug"] = "carry"
	s.ids["patches/:id"] = harness.Str(s.call(http.MethodPost, wsPath("carry", "/patches"), key,
		map[string]any{"branch_name": "patch-1"}, http.StatusCreated).Body["id"])
	s.ids["rebuilds/:id"] = harness.Str(s.call(http.MethodPost, wsPath("carry", "/rebuild"), key, nil, http.StatusAccepted).Body["id"])
	s.ids["rerere/*pathspec"] = "patch-1.txt"
	return s
}

// assertEnvelope checks that r is the error envelope with status and
// errorType, a non-empty message and the JSON content type.
func assertEnvelope(t *testing.T, what string, r harness.Response, status int, errorType string) {
	t.Helper()
	e, _ := r.Body["error"].(map[string]any)
	switch {
	case r.Status != status:
		t.Errorf("%s: answer %d, want %d: %s", what, r.Status, status, r.Raw)
	case r.Header.Get("Content-Type") != "application/json; charset=utf-8":
		t.Errorf("%s: Content-Type %q, want application/json; charset=utf-8", what, r.Header.Get("Content-Type"))
	case harness.Num(e["code"]) != status || e["error_type"] != errorType || harness.Str(e["message"]) == "":
		t.Errorf("%s: body %s, want the envelope with code %d, error_type %s and a message", what, r.Raw, status, errorType)
	}
}

// matches reports whether the concrete path is one the pattern serves: a
// :name segment takes any one segment, a last *name segment one or more.
func matches(pattern, path string) bool {
	ps, cs := strings.Split(pattern, "/"), strings.Split(path, "/")
	for i, p := range ps {
		switch {
		case strings.HasPrefix(p, "*"):
			return len(cs) > i
		case i >= len(cs):
			return false
		case strings.HasPrefix(p, ":"):
			if cs[i] == "" {
				return false
			}
		case p != cs[i]:
			return false
		}
	}
	return len(ps) == len(cs)
}

// wrongMethod is a method no route has for the concrete path, "" when every
// method has one: a route of the method whose pattern matches the path
// serves it, as the router does, even when another pattern was meant.
func wrongMethod(path string) string {
	for _, m := range methods {
		if !slices.ContainsFunc(routes, func(r route) bool { return r.method == m && matches(r.path, path) }) {
			return m
		}
	}
	return ""
}

func TestConventions_EveryErrorIsTheEnvelopeAndWrongMethodIs405(t *testing.T) {
	t.Parallel()
	s := seedIdentifiers(t)
	hub := s.hub
	wrong := 0
	for _, r := range routes {
		path := s.path(r)
		what := r.method + " " + path
		switch {
		case r.method == http.MethodPost && r.path == "/api/v1/auth/callback":
			assertEnvelope(t, what+" with a malformed body", hub.Call(r.method, path, "", "{"), http.StatusBadRequest, "invalid_request")
		case !r.public:
			assertEnvelope(t, what+" without a credential", hub.Call(r.method, path, "", nil), http.StatusUnauthorized, "unauthenticated")
		}
		if m := wrongMethod(path); m != "" {
			wrong++
			assertEnvelope(t, m+" "+path, hub.Call(m, path, hub.AdminToken, nil), http.StatusMethodNotAllowed, "method_not_allowed")
		}
	}
	if wrong == 0 {
		t.Error("no route of the table lacks a method, so no wrong method was sent")
	}
	t.Logf("%d routes, %d of them sent a method their path lacks", len(routes), wrong)
	assertEnvelope(t, "GET /api/v1/no/such/path", hub.Call(http.MethodGet, "/api/v1/no/such/path", hub.AdminToken, nil),
		http.StatusNotFound, "not_found")
}

// withQuery is path with the query parameters q added.
func withQuery(path, q string) string {
	if strings.Contains(path, "?") {
		return path + "&" + q
	}
	return path + "?" + q
}

// sweep are the rows of the table that keep holds and that no reason puts
// outside the paging and idempotency scenarios. It fails the test when
// there is none, so that a sweep never passes on an empty table.
func sweep(t *testing.T, keep func(route) bool) []route {
	t.Helper()
	var rows []route
	for _, r := range routes {
		if keep(r) && r.outside == "" {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		t.Fatal("the route table has no row to sweep")
	}
	return rows
}

// listing is how the paging scenario reads a list route.
type listing struct {
	// path is the list on the parent the seeding gives three items, with
	// its query; empty is the list on a parent that has none, "" where no
	// parent can be without items.
	path, empty string
	// credential reads the lists, emptyCredential the empty one when it
	// differs; id is the member that tells items apart.
	credential, emptyCredential, id string
}

// pageParents starts a hub and creates the parents of every list route,
// none of which has an item yet.
func pageParents(t *testing.T) *seeded {
	t.Helper()
	s := forgedHub(t, "pages")
	hub, alice, key := s.hub, s.alice, s.alice.Key
	hub.User("erin")
	hub.User("kate")
	hub.Workspace(alice, "third")
	// The forge's issue 2 and pull request 5 never get an item of a list.
	s.forge.SetIssues("acme", "pages", harness.Issue{Number: 1, Title: "First"}, harness.Issue{Number: 2, Title: "Second"},
		harness.Issue{Number: 3, Title: "Third"})
	s.forge.SetPulls("acme", "pages", harness.Pull{Number: 4, Title: "Fourth"}, harness.Pull{Number: 5, Title: "Fifth"})
	hub.CreateOrg("acme")
	s.ids["zed"] = harness.Str(s.call(http.MethodPost, "/api/v1/users", hub.AdminToken, map[string]any{
		"username": "zed", "email": "zed@example.test", "provider": "github", "provider_id": "999999",
	}, http.StatusCreated).Body["id"])
	s.ids["session"] = harness.Str(s.call(http.MethodPost, "/api/v1/sessions", key,
		map[string]any{"workspace_slug": "pages", "model": "m1"}, http.StatusCreated).Body["id"])
	s.ids["e"] = fmt.Sprint(harness.Num(harness.Field(s.commitOn(key, "pages", "e", "main", "Change e").Body, "change.number")))
	s.ids["o"] = fmt.Sprint(harness.Num(harness.Field(s.commitOn(key, "pages", "shared", "main", "Change o").Body, "change.number")))
	s.ids["webhook"] = harness.Str(s.call(http.MethodPost, wsPath("pages", "/webhooks"), key,
		map[string]any{"url": harness.NewReceiver(t).URL, "types": []string{"parley.webhook.ping"}}, http.StatusCreated).Body["id"])
	s.origin = s.carryPatch("carry", map[string]map[string]string{
		"patch-1": {"patch-1.txt": "one\n"},
		"patch-2": {"patch-2.txt": "two\n"},
		"patch-3": {"patch-3.txt": "three\n"},
	})
	return s
}

// listings are the list routes of the table, by path, read on the parents
// of pageParents with the credentials the users hold now.
func (s *seeded) listings() map[string]listing {
	key, admin := s.alice.Key, s.hub.AdminToken
	erin := s.hub.User("erin").Key
	pages, carry := wsPath("pages", ""), wsPath("carry", "")
	e, o := pages+"/changes/"+s.ids["e"], pages+"/changes/"+s.ids["o"]
	run := pages + "/runs/" + runID
	user := func(path, id string) listing { return listing{path: path, empty: path, credential: key, id: id} }
	ws := func(rest, id string) listing {
		return listing{path: pages + rest, empty: pages + rest, credential: key, id: id}
	}
	claimsOnly := "?workspace=pages&types=parley.claim.create"
	return map[string]listing{
		"/api/v1/audit":      {path: "/api/v1/audit" + claimsOnly + "&source=hub", empty: "/api/v1/audit" + claimsOnly + "&source=hub", credential: key, id: "id"},
		"/api/v1/events/log": {path: "/api/v1/events/log" + claimsOnly, empty: "/api/v1/events/log" + claimsOnly, credential: key, id: "seq"},
		// Every user has an organization of their own from the first
		// sign-in, so neither list of organizations is ever empty.
		"/api/v1/orgs":               {path: "/api/v1/orgs", credential: admin, id: "id"},
		"/api/v1/user/orgs":          {path: "/api/v1/user/orgs", credential: key, id: "id"},
		"/api/v1/orgs/:id/members":   {path: "/api/v1/orgs/acme/members", empty: "/api/v1/orgs/acme/members", credential: admin, id: "user_id"},
		"/api/v1/orgs/:slug/secrets": {path: "/api/v1/orgs/acme/secrets", empty: "/api/v1/orgs/acme/secrets", credential: admin, id: "key"},
		"/api/v1/orgs/:slug/vars":    {path: "/api/v1/orgs/acme/vars", empty: "/api/v1/orgs/acme/vars", credential: admin, id: "key"},
		// A fresh user is the parent with no sessions, secrets, tokens,
		// variables or workspaces.
		"/api/v1/sessions":           {path: "/api/v1/sessions", empty: "/api/v1/sessions", credential: key, emptyCredential: erin, id: "id"},
		"/api/v1/user/secrets":       {path: "/api/v1/user/secrets", empty: "/api/v1/user/secrets", credential: key, emptyCredential: erin, id: "key"},
		"/api/v1/user/tokens":        {path: "/api/v1/user/tokens", empty: "/api/v1/user/tokens", credential: key, emptyCredential: erin, id: "token_id"},
		"/api/v1/user/vars":          {path: "/api/v1/user/vars", empty: "/api/v1/user/vars", credential: key, emptyCredential: erin, id: "key"},
		"/api/v1/workspaces":         {path: "/api/v1/workspaces", empty: "/api/v1/workspaces", credential: key, emptyCredential: erin, id: "slug"},
		"/api/v1/sessions/:id/usage": user("/api/v1/sessions/"+s.ids["session"]+"/usage", "id"),
		// The key that reads the list is one of its items; a hub's users
		// are those the scenario signed in.
		"/api/v1/user/keys":                                  {path: "/api/v1/user/keys", credential: s.hub.User("kate").Key, id: "key_id"},
		"/api/v1/users":                                      {path: "/api/v1/users", credential: admin, id: "id"},
		"/api/v1/users/:id/keys":                             {path: "/api/v1/users/" + s.hub.User("kate").ID + "/keys", empty: "/api/v1/users/" + s.ids["zed"] + "/keys", credential: admin, id: "key_id"},
		"/api/v1/users/:id/tokens":                           {path: "/api/v1/users/" + s.alice.ID + "/tokens", empty: "/api/v1/users/" + s.ids["zed"] + "/tokens", credential: admin, id: "token_id"},
		"/api/v1/workspaces/:slug/changes":                   {path: pages + "/changes", empty: wsPath("carry", "/changes"), credential: key, id: "number"},
		"/api/v1/workspaces/:slug/changes/:number/approvals": user(e+"/approvals", "id"),
		"/api/v1/workspaces/:slug/changes/:number/checks":    user(e+"/checks", "name"),
		"/api/v1/workspaces/:slug/changes/:number/lands":     user(e+"/lands", "id"),
		"/api/v1/workspaces/:slug/changes/:number/overlaps":  user(o+"/overlaps", "change.number"),
		// A change has its first version from its opening.
		"/api/v1/workspaces/:slug/changes/:number/versions":       {path: e + "/versions", credential: key, id: "number"},
		"/api/v1/workspaces/:slug/claims":                         ws("/claims", "id"),
		"/api/v1/workspaces/:slug/cost":                           ws("/cost?group_by=model", "key"),
		"/api/v1/workspaces/:slug/forge/deliveries":               ws("/forge/deliveries", "delivery_id"),
		"/api/v1/workspaces/:slug/forks":                          ws("/forks", "slug"),
		"/api/v1/workspaces/:slug/issues":                         {path: pages + "/issues", empty: pages + "/issues?state=closed", credential: key, id: "number"},
		"/api/v1/workspaces/:slug/issues/:number/comments":        {path: pages + "/issues/1/comments", empty: pages + "/issues/2/comments", credential: key, id: "id"},
		"/api/v1/workspaces/:slug/pulls/:number/files":            {path: pages + "/pulls/4/files", empty: pages + "/pulls/5/files", credential: key, id: "filename"},
		"/api/v1/workspaces/:slug/pulls/:number/checks":           {path: pages + "/pulls/4/checks", empty: pages + "/pulls/5/checks", credential: key, id: "name"},
		"/api/v1/workspaces/:slug/pulls/:number/reviews":          {path: pages + "/pulls/4/reviews", empty: pages + "/pulls/5/reviews", credential: key, id: "body"},
		"/api/v1/workspaces/:slug/lands":                          ws("/lands", "id"),
		"/api/v1/workspaces/:slug/patches":                        {path: carry + "/patches", empty: carry + "/patches", credential: key, id: "id"},
		"/api/v1/workspaces/:slug/rebuilds":                       {path: carry + "/rebuilds", empty: carry + "/rebuilds", credential: key, id: "id"},
		"/api/v1/workspaces/:slug/rerere":                         {path: carry + "/rerere", empty: carry + "/rerere", credential: key, id: "id"},
		"/api/v1/workspaces/:slug/runs/:run_id/events":            user(run+"/events", "id"),
		"/api/v1/workspaces/:slug/runs/:run_id/sessions/outcomes": user(run+"/sessions/outcomes", "id"),
		"/api/v1/workspaces/:slug/runs/:run_id/tools/calls":       user(run+"/tools/calls", "id"),
		"/api/v1/workspaces/:slug/runs/:run_id/tools/errors":      user(run+"/tools/errors", "id"),
		"/api/v1/workspaces/:slug/runs/:run_id/traces":            user(run+"/traces", "id"),
		"/api/v1/workspaces/:slug/runs/:run_id/transcript":        user(run+"/transcript?node_id=n1", "content"),
		// A workspace always has a history, a default branch and a tree.
		"/api/v1/workspaces/:slug/scm/commits":             {path: pages + "/scm/commits?ref=main", credential: key, id: "sha"},
		"/api/v1/workspaces/:slug/scm/refs":                {path: pages + "/scm/refs", credential: key, id: "name"},
		"/api/v1/workspaces/:slug/scm/tree":                {path: pages + "/scm/tree?ref=main", credential: key, id: "path"},
		"/api/v1/workspaces/:slug/scm/search":              ws("/scm/search?q=needle", "path"),
		"/api/v1/workspaces/:slug/secrets":                 ws("/secrets", "key"),
		"/api/v1/workspaces/:slug/tokens":                  ws("/tokens", "token_id"),
		"/api/v1/workspaces/:slug/vars":                    ws("/vars", "key"),
		"/api/v1/workspaces/:slug/vars/resolved":           ws("/vars/resolved", "key"),
		"/api/v1/workspaces/:slug/webhooks":                {path: pages + "/webhooks", empty: carry + "/webhooks", credential: key, id: "id"},
		"/api/v1/workspaces/:slug/webhooks/:id/deliveries": user(pages+"/webhooks/"+s.ids["webhook"]+"/deliveries", "id"),
	}
}

// waitRebuild waits until the rebuild id of the workspace slug has ended
// and returns its status.
func (s *seeded) waitRebuild(slug, id string) string {
	s.t.Helper()
	var status string
	harness.Eventually(s.t, "rebuild "+id+" to end", func() bool {
		status = harness.Str(s.hub.Get(s.alice.Key, wsPath(slug, "/rebuilds/"+id)).Body["status"])
		return status != "queued" && status != "running"
	})
	return status
}

// forgeDelivery sends the forge delivery id of event to the workspace slug,
// signed with secret as GitHub signs it, and fails the test unless the hub
// takes it.
func (s *seeded) forgeDelivery(slug, secret, id string) {
	s.t.Helper()
	body := `{"zen":"Keep it logically awesome.","hook_id":1}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r := s.hub.Call(http.MethodPost, "/hooks/forge/"+slug, "", body, "X-GitHub-Event", "ping", "X-GitHub-Delivery", id,
		"X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if r.Status/100 != 2 {
		s.t.Fatalf("forge delivery %s: answer %d %s", id, r.Status, r.Raw)
	}
}

// seedThree gives every parent of pageParents at least three items of each
// list.
func (s *seeded) seedThree() {
	s.t.Helper()
	s.seedThreeOwned()
	s.seedThreeOfChanges()
	s.seedThreeFromOutside()
}

// seedThreeOwned gives the organizations, users and workspaces three
// members, secrets, variables, tokens, sessions, claims, forks, webhooks,
// deliveries and telemetry records each, and kate three API keys.
func (s *seeded) seedThreeOwned() {
	s.t.Helper()
	hub, key, admin := s.hub, s.alice.Key, s.hub.AdminToken
	pages := wsPath("pages", "")
	three := []string{"1", "2", "3"}

	hub.AddMember("acme", s.alice)
	hub.AddMember("acme", hub.User("erin"))
	hub.AddMember("acme", hub.User("kate"))
	hub.CreateOrg("acme2")
	hub.AddMember("acme2", s.alice)
	for _, scope := range []string{"/api/v1/orgs/acme", "/api/v1/user", pages} {
		credential := key
		if scope == "/api/v1/orgs/acme" {
			credential = admin
		}
		s.call(http.MethodPost, scope+"/secrets", credential, entries("K1", "K2", "K3"), http.StatusCreated)
		s.call(http.MethodPost, scope+"/vars", credential, entries("K1", "K2", "K3"), http.StatusCreated)
	}
	for _, n := range three {
		s.call(http.MethodPost, "/api/v1/user/tokens", key, map[string]any{"name": "user-" + n, "permissions": []string{"git:read"}}, http.StatusCreated)
		s.call(http.MethodPost, pages+"/tokens", key, map[string]any{"name": "bound-" + n, "permissions": []string{"git:read"}}, http.StatusCreated)
		s.call(http.MethodPost, "/api/v1/sessions", key, map[string]any{"workspace_slug": "pages"}, http.StatusCreated)
		s.call(http.MethodPost, "/api/v1/sessions/"+s.ids["session"]+"/usage", key,
			map[string]any{"model": "model-" + n, "input_tokens": 10, "output_tokens": 5}, http.StatusCreated)
		s.call(http.MethodPost, pages+"/claims", key, map[string]any{"paths": []string{"c" + n + ".txt"}, "intent": "claim " + n}, http.StatusCreated)
		hub.Fork(key, "pages", nil, nil)
		s.call(http.MethodPost, pages+"/webhooks", key, map[string]any{"url": harness.NewReceiver(s.t).URL}, http.StatusCreated)
		s.call(http.MethodPost, pages+"/webhooks/"+s.ids["webhook"]+"/ping", key, nil, http.StatusOK)
		run := pages + "/runs/" + runID
		s.call(http.MethodPost, run+"/events", key, map[string]any{"event_type": "step " + n}, http.StatusCreated)
		s.call(http.MethodPost, run+"/sessions/outcomes", key,
			map[string]any{"session_id": s.ids["session"], "status": "completed"}, http.StatusCreated)
		s.call(http.MethodPost, run+"/tools/calls", key, map[string]any{"tool_name": "tool-" + n}, http.StatusCreated)
		s.call(http.MethodPost, run+"/tools/errors", key, map[string]any{"tool_name": "tool-" + n, "error_msg": "failed " + n}, http.StatusCreated)
		s.call(http.MethodPost, run+"/traces", key, map[string]any{"event_type": "assistant.message", "node_id": "n1",
			"sequence": len(n), "data": map[string]any{"content": "message " + n}}, http.StatusCreated)
	}
	hub.SignIn("kate")
	hub.SignIn("kate")
}

// seedThreeOfChanges gives pages three files to find, change e three
// versions, approvals, checks and land attempts, and change o three
// overlapping changes.
func (s *seeded) seedThreeOfChanges() {
	s.t.Helper()
	hub, key := s.hub, s.alice.Key
	pages := wsPath("pages", "")
	three := []string{"1", "2", "3"}

	// Three files with the word the search looks for, committed to main by
	// its owner.
	main := mainHeadOf(s.t, hub, key, "pages")
	s.call(http.MethodPost, pages+"/scm/commits", key, map[string]any{
		"branch": "main", "expected_head_sha": main, "message": "Add the needles",
		"operations": []any{put("n1.txt", "needle 1\n"), put("n2.txt", "needle 2\n"), put("n3.txt", "needle 3\n")},
	}, http.StatusCreated)

	// Change e gets two more versions, an approval of each, three checks
	// and three land attempts: two refused while the policy waits for the
	// check gate, the third landing once it passed.
	e := pages + "/changes/" + s.ids["e"]
	for v := 1; v <= 3; v++ {
		if v > 1 {
			head := harness.Str(hub.Get(key, pages+"/scm/refs/e").Body["sha"])
			s.call(http.MethodPost, pages+"/scm/commits", key, map[string]any{
				"branch": "e", "expected_head_sha": head, "message": fmt.Sprint("Version ", v),
				"operations": []any{put("e.txt", fmt.Sprint("version ", v, "\n"))},
			}, http.StatusCreated)
		}
		s.call(http.MethodPost, e+"/approvals", key, map[string]any{"verdict": "approve", "version": v}, http.StatusCreated)
	}
	// The checks of a change are those of its latest version.
	for _, n := range three {
		s.call(http.MethodPut, e+"/checks/check-"+n, key, map[string]any{"status": "passed"}, http.StatusOK)
	}
	s.call(http.MethodPut, pages+"/policy", key, map[string]any{"default": map[string]any{"checks": []string{"gate"}}, "rules": []any{}}, http.StatusOK)
	for range 2 {
		s.call(http.MethodPost, e+"/land", key, nil, http.StatusConflict)
	}
	s.call(http.MethodPut, e+"/checks/gate", key, map[string]any{"status": "passed"}, http.StatusOK)
	s.call(http.MethodPost, e+"/land", key, nil, http.StatusOK)

	// Three changes touching the file of change o overlap it.
	for _, n := range three {
		s.call(http.MethodPost, pages+"/scm/commits", key, map[string]any{
			"branch": "overlap-" + n, "base_ref": "main", "message": "Overlap " + n,
			"operations": []any{put("shared.txt", "overlap "+n+"\n")},
			"change":     map[string]any{"title": "Overlap " + n, "intent": "overlap " + n},
		}, http.StatusCreated)
	}
	harness.Eventually(s.t, "change o to overlap three changes", func() bool {
		return len(hub.Get(key, pages+"/changes/"+s.ids["o"]+"/overlaps").Items()) >= 3
	})

}

// seedThreeFromOutside gives pages three deliveries from its forge, and its
// forge's issue 1 three comments, written through the hub, and pull
// request 4 three files, check runs and reviews; and carry three patches
// and rebuilds, and the rerere records of three conflicts.
func (s *seeded) seedThreeFromOutside() {
	s.t.Helper()
	key := s.alice.Key
	pages := wsPath("pages", "")
	three := []string{"1", "2", "3"}

	secret := harness.Str(s.call(http.MethodPut, pages+"/forge/webhook", key, nil, http.StatusOK).Body["secret"])
	for _, n := range three {
		s.forgeDelivery("pages", secret, "delivery-"+n)
	}
	// A write reaches the forge with the workspace's forge credential.
	s.call(http.MethodPost, pages+"/secrets", key, entries("GIT_PAT"), http.StatusCreated)
	for _, n := range three {
		s.call(http.MethodPost, pages+"/issues/1/comments", key, map[string]any{"body": "comment " + n}, http.StatusCreated)
	}
	s.forge.SetPulls("acme", "pages", harness.Pull{Number: 4, Title: "Fourth", Files: []string{"f1.txt", "f2.txt", "f3.txt"},
		Checks: []string{"build", "lint", "test"}, Reviews: []string{"review 1", "review 2", "review 3"}},
		harness.Pull{Number: 5, Title: "Fifth"})

	// Three patches and three rebuilds of carry; the last rebuild, which
	// goes on past a conflict, meets one in each patch, as the upstream
	// added the patches' files too, and remembers the three.
	carry := wsPath("carry", "")
	for _, n := range three {
		s.call(http.MethodPost, carry+"/patches", key, map[string]any{"branch_name": "patch-" + n}, http.StatusCreated)
	}
	for range 2 {
		id := harness.Str(s.call(http.MethodPost, carry+"/rebuild", key, nil, http.StatusAccepted).Body["id"])
		s.waitRebuild("carry", id)
	}
	s.origin.CommitFiles("main", "Upstream adds what the patches add", map[string]string{
		"patch-1.txt": "upstream\n", "patch-2.txt": "upstream\n", "patch-3.txt": "upstream\n",
	})
	s.call(http.MethodPatch, carry, key, map[string]any{"settings": map[string]any{"rebuild_fail_mode": "continue"}}, http.StatusOK)
	id := harness.Str(s.call(http.MethodPost, carry+"/rebuild", key, nil, http.StatusAccepted).Body["id"])
	s.waitRebuild("carry", id)
}

func TestConventions_EveryListPagesTheSameWay(t *testing.T) {
	t.Parallel()
	s := pageParents(t)
	hub := s.hub
	rows := sweep(t, func(r route) bool { return r.list })
	t.Logf("%d list routes", len(rows))
	before := s.listings()
	for _, r := range rows {
		l, ok := before[r.path]
		if !ok {
			t.Errorf("GET %s: the scenario has no listing for this list route", r.path)
			continue
		}
		if l.empty == "" {
			continue
		}
		credential := l.credential
		if l.emptyCredential != "" {
			credential = l.emptyCredential
		}
		empty := hub.Call(http.MethodGet, l.empty, credential, nil)
		cursor, hasCursor := empty.Body["next_cursor"]
		if empty.Status != http.StatusOK || len(empty.Items()) != 0 || !hasCursor || cursor != nil || empty.Body["items"] == nil {
			t.Errorf("GET %s on a parent without items: %d %s, want items [] and next_cursor null", l.empty, empty.Status, empty.Raw)
		}
	}
	s.seedThree()
	after := s.listings()
	// Cursors of two other lists, each sent to every list but its own.
	foreign := map[string]string{}
	for _, p := range []string{"/api/v1/user/tokens", "/api/v1/workspaces/:slug/claims"} {
		l := after[p]
		foreign[p] = harness.Str(hub.Get(l.credential, withQuery(l.path, "limit=1")).Body["next_cursor"])
		if foreign[p] == "" {
			t.Fatalf("GET %s?limit=1 gave no cursor to send to the other lists", l.path)
		}
	}
	for _, r := range rows {
		l, ok := after[r.path]
		if !ok {
			continue
		}
		all := hub.Call(http.MethodGet, l.path, l.credential, nil)
		want := members(all.Body, "items", l.id)
		if all.Status != http.StatusOK || len(want) < 3 {
			t.Errorf("GET %s: %d with %d items, want 200 with at least three: %s", l.path, all.Status, len(want), all.Raw)
			continue
		}
		if slices.Contains(want, "") || len(slices.Compact(sorted(want))) != len(want) {
			t.Errorf("GET %s: the items' %s are %v, want each one set and distinct", l.path, l.id, want)
			continue
		}
		var seen []string
		cursor := ""
		for page := 0; page <= len(want); page++ {
			p := hub.Call(http.MethodGet, withQuery(l.path, "limit=1"+cursor), l.credential, nil)
			if _, ok := p.Body["next_cursor"]; !ok {
				t.Errorf("GET %s page %d with limit=1: %d %s, want a next_cursor member", l.path, page+1, p.Status, p.Raw)
				break
			}
			if r.feed && p.Status == http.StatusOK && len(p.Items()) == 0 && p.Body["next_cursor"] == nil && len(seen) == len(want) {
				break // the empty page that ends a walk of the event log
			}
			if p.Status != http.StatusOK || len(p.Items()) != 1 {
				t.Errorf("GET %s page %d with limit=1: %d %s, want one item", l.path, page+1, p.Status, p.Raw)
				break
			}
			seen = append(seen, members(p.Body, "items", l.id)...)
			next, isString := p.Body["next_cursor"].(string)
			if !isString {
				break
			}
			cursor = "&cursor=" + next
		}
		if !slices.Equal(seen, want) {
			t.Errorf("GET %s with limit=1 to the end gave %v, want each item once in the list's order %v", l.path, seen, want)
		}
		maxLimit := 200
		if r.maxLimit != 0 {
			maxLimit = r.maxLimit
		}
		for _, bad := range []string{"limit=0", fmt.Sprint("limit=", maxLimit+1)} {
			assertEnvelope(t, "GET "+l.path+" with "+bad, hub.Call(http.MethodGet, withQuery(l.path, bad), l.credential, nil),
				http.StatusBadRequest, "invalid_request")
		}
		other := foreign["/api/v1/user/tokens"]
		if r.path == "/api/v1/user/tokens" {
			other = foreign["/api/v1/workspaces/:slug/claims"]
		}
		assertEnvelope(t, "GET "+l.path+" with another list's cursor", hub.Call(http.MethodGet, withQuery(l.path, "cursor="+other), l.credential, nil),
			http.StatusBadRequest, "invalid_request")
	}
}

// creation is how the idempotency scenario sends a create route.
type creation struct {
	path, credential string
	body             any
	// count is the number of resources of the kind the route creates.
	count func() int
}

// createParents starts a hub and creates what every create route of the
// table needs to exist first.
func createParents(t *testing.T) *seeded {
	t.Helper()
	s := forgedHub(t, "idem")
	hub, key := s.hub, s.alice.Key
	hub.CreateOrg("acme")
	s.call(http.MethodPost, wsPath("idem", "/secrets"), key, entries("GIT_PAT"), http.StatusCreated)
	s.forge.SetIssues("acme", "idem", harness.Issue{Number: 1, Title: "To comment on"})
	s.forge.SetPulls("acme", "idem", harness.Pull{Number: 2, Title: "To comment on"})
	s.ids["session"] = harness.Str(s.call(http.MethodPost, "/api/v1/sessions", key,
		map[string]any{"workspace_slug": "idem"}, http.StatusCreated).Body["id"])
	number := func(r harness.Response) string {
		return fmt.Sprint(harness.Num(harness.Field(r.Body, "change.number")))
	}
	s.ids["approve"] = number(s.commitOn(key, "idem", "approve", "main", "Change to approve"))
	s.ids["land"] = number(s.commitOn(key, "idem", "land", "main", "Change to land"))
	s.ids["revert"] = number(s.commitOn(key, "idem", "revert", "main", "Change to revert"))
	s.ids["behind"] = number(s.commitOn(key, "idem", "behind", "main", "Change to rebase"))
	s.commitOn(key, "idem", "plain", "main", "")
	s.call(http.MethodPost, wsPath("idem", "/changes/"+s.ids["revert"]+"/land"), key, map[string]any{}, http.StatusOK)
	s.ids["main"] = mainHeadOf(t, hub, key, "idem")
	s.ids["receiver"] = harness.NewReceiver(t).URL
	s.carryPatch("carry", map[string]map[string]string{"patch-2": {"patch-2.txt": "two\n"}})
	s.ids["origin"] = hub.Origin().URL()
	return s
}

// creations are the create routes of the table, by path, on the parents
// of createParents.
func (s *seeded) creations() map[string]creation {
	key, admin := s.alice.Key, s.hub.AdminToken
	idem, carry := wsPath("idem", ""), wsPath("carry", "")
	run := idem + "/runs/" + runID
	counted := func(credential, path string) func() int {
		return func() int { return len(walk(s.t, s.hub, credential, path)) }
	}
	in := func(path string, body any, list string) creation {
		return creation{path: path, credential: key, body: body, count: counted(key, list)}
	}
	change := func(id, rest string) string { return idem + "/changes/" + s.ids[id] + rest }
	session := "/api/v1/sessions/" + s.ids["session"]
	return map[string]creation{
		"/api/v1/orgs": {path: "/api/v1/orgs", credential: admin, body: map[string]any{"name": "Idem", "slug": "idem-org"},
			count: counted(admin, "/api/v1/orgs")},
		"/api/v1/orgs/:slug/secrets": {path: "/api/v1/orgs/acme/secrets", credential: admin, body: entries("IDEM"),
			count: counted(admin, "/api/v1/orgs/acme/secrets")},
		"/api/v1/orgs/:slug/vars": {path: "/api/v1/orgs/acme/vars", credential: admin, body: entries("IDEM"),
			count: counted(admin, "/api/v1/orgs/acme/vars")},
		"/api/v1/users": {path: "/api/v1/users", credential: admin, body: map[string]any{
			"username": "yan", "email": "yan@example.test", "provider": "github", "provider_id": "888888",
		}, count: counted(admin, "/api/v1/users")},
		"/api/v1/sessions":           in("/api/v1/sessions", map[string]any{"workspace_slug": "idem"}, "/api/v1/sessions"),
		"/api/v1/sessions/:id/usage": in(session+"/usage", map[string]any{"model": "m", "input_tokens": 1}, session+"/usage"),
		"/api/v1/user/secrets":       in("/api/v1/user/secrets", entries("IDEM"), "/api/v1/user/secrets"),
		"/api/v1/user/vars":          in("/api/v1/user/vars", entries("IDEM"), "/api/v1/user/vars"),
		"/api/v1/user/tokens": in("/api/v1/user/tokens", map[string]any{"name": "idem", "permissions": []string{"git:read"}},
			"/api/v1/user/tokens"),
		"/api/v1/workspaces": in("/api/v1/workspaces", map[string]any{"slug": "fresh", "git_url": s.ids["origin"]}, "/api/v1/workspaces"),
		"/api/v1/workspaces/:slug/changes": in(idem+"/changes", map[string]any{
			"source": map[string]any{"branch": "plain"}, "target_branch": "main", "title": "Plain", "intent": "open plain",
		}, idem+"/changes"),
		"/api/v1/workspaces/:slug/changes/:number/approvals": in(change("approve", "/approvals"), map[string]any{"verdict": "approve"},
			change("approve", "/approvals")),
		"/api/v1/workspaces/:slug/changes/:number/land":   in(change("land", "/land"), map[string]any{}, idem+"/lands"),
		"/api/v1/workspaces/:slug/changes/:number/revert": in(change("revert", "/revert"), map[string]any{}, idem+"/changes"),
		"/api/v1/workspaces/:slug/claims": in(idem+"/claims", map[string]any{"paths": []string{"a.txt"}, "intent": "idem"},
			idem+"/claims"),
		"/api/v1/workspaces/:slug/issues": {path: idem + "/issues", credential: key, body: map[string]any{"title": "Idem"},
			count: func() int { return len(s.forge.Issues("acme", "idem")) }},
		"/api/v1/workspaces/:slug/issues/:number/comments": {path: idem + "/issues/1/comments", credential: key,
			body: map[string]any{"body": "idem"}, count: func() int { return len(s.forge.Issues("acme", "idem")[0].Comments) }},
		"/api/v1/workspaces/:slug/labels": {path: idem + "/labels", credential: key, body: map[string]any{"name": "idem"},
			count: func() int { return len(s.forge.Labels("acme", "idem")) }},
		"/api/v1/workspaces/:slug/pulls": {path: idem + "/pulls", credential: key,
			body:  map[string]any{"title": "Idem", "head": "plain", "base": "main"},
			count: func() int { return len(s.forge.Pulls("acme", "idem")) }},
		"/api/v1/workspaces/:slug/pulls/:number/comments": {path: idem + "/pulls/2/comments", credential: key,
			body: map[string]any{"body": "idem"}, count: func() int { return len(s.forge.Pulls("acme", "idem")[0].Comments) }},
		"/api/v1/workspaces/:slug/forks": in(idem+"/forks", map[string]any{"token": map[string]any{"permissions": []string{"git:read"}}},
			idem+"/forks"),
		"/api/v1/workspaces/:slug/patches":             in(carry+"/patches", map[string]any{"branch_name": "patch-2"}, carry+"/patches"),
		"/api/v1/workspaces/:slug/rebuild":             in(carry+"/rebuild", map[string]any{}, carry+"/rebuilds"),
		"/api/v1/workspaces/:slug/runs/:run_id/events": in(run+"/events", map[string]any{"event_type": "idem"}, run+"/events"),
		"/api/v1/workspaces/:slug/runs/:run_id/sessions/outcomes": in(run+"/sessions/outcomes",
			map[string]any{"session_id": s.ids["session"], "status": "completed"}, run+"/sessions/outcomes"),
		"/api/v1/workspaces/:slug/runs/:run_id/tools/calls": in(run+"/tools/calls", map[string]any{"tool_name": "tool"}, run+"/tools/calls"),
		"/api/v1/workspaces/:slug/runs/:run_id/tools/errors": in(run+"/tools/errors", map[string]any{"tool_name": "tool", "error_msg": "failed"},
			run+"/tools/errors"),
		"/api/v1/workspaces/:slug/runs/:run_id/traces": in(run+"/traces", map[string]any{"event_type": "session.init"}, run+"/traces"),
		"/api/v1/workspaces/:slug/runs/:run_id/postmortem": {path: run + "/postmortem", credential: key, body: map[string]any{
			"run_status": "stalled", "started_at": "2026-11-20T10:00:00Z", "completed_at": "2026-11-20T11:00:00Z",
			"task_summary": map[string]any{"total": 1}, "cost_summary": map[string]any{},
		}, count: func() int {
			if s.hub.Call(http.MethodGet, run+"/postmortem", key, nil).Status == http.StatusOK {
				return 1
			}
			return 0
		}},
		"/api/v1/workspaces/:slug/scm/commits": in(idem+"/scm/commits", map[string]any{
			"branch": "committed", "base_ref": "main", "message": "Add committed", "operations": []any{put("committed.txt", "c\n")},
		}, idem+"/scm/refs"),
		"/api/v1/workspaces/:slug/scm/rebase": in(idem+"/scm/rebase", map[string]any{"branch": "behind", "onto": "main"},
			change("behind", "/versions")),
		"/api/v1/workspaces/:slug/scm/refs": in(idem+"/scm/refs", map[string]any{"name": "refs/heads/made", "sha": s.ids["main"]},
			idem+"/scm/refs"),
		"/api/v1/workspaces/:slug/secrets": in(idem+"/secrets", entries("IDEM"), idem+"/secrets"),
		"/api/v1/workspaces/:slug/vars":    in(idem+"/vars", entries("IDEM"), idem+"/vars"),
		"/api/v1/workspaces/:slug/tokens": in(idem+"/tokens", map[string]any{"name": "idem", "permissions": []string{"git:read"}},
			idem+"/tokens"),
		"/api/v1/workspaces/:slug/webhooks": in(idem+"/webhooks", map[string]any{"url": s.ids["receiver"]}, idem+"/webhooks"),
	}
}

// without is a copy of m without the members at the dotted paths.
func without(m map[string]any, paths []string) map[string]any {
	raw, _ := json.Marshal(m)
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	for _, p := range paths {
		names := strings.Split(p, ".")
		parent, _ := harness.Field(c, strings.Join(names[:len(names)-1], ".")).(map[string]any)
		if len(names) == 1 {
			parent = c
		}
		delete(parent, names[len(names)-1])
	}
	return c
}

func TestConventions_EveryCreatingPostHonoursIdempotencyKey(t *testing.T) {
	t.Parallel()
	s := createParents(t)
	hub := s.hub
	cs := s.creations()
	rows := sweep(t, func(r route) bool { return r.create && !r.public })
	t.Logf("%d create routes", len(rows))
	for _, r := range rows {
		c, ok := cs[r.path]
		if !ok {
			t.Errorf("POST %s: the scenario has no body for this create route", r.path)
			continue
		}
		post := func(body any, key string) harness.Response {
			return hub.Call(http.MethodPost, c.path, c.credential, body, "Idempotency-Key", key)
		}
		what := "POST " + c.path
		key := "idem-" + strings.ReplaceAll(r.path, "/", "-")
		first := http.StatusCreated
		if r.status != 0 {
			first = r.status
		}
		n0 := c.count()
		r1 := post(c.body, key)
		if r1.Status != first {
			t.Errorf("%s: answer %d, want %d: %s", what, r1.Status, first, r1.Raw)
			continue
		}
		r2 := post(c.body, key)
		if r2.Status != http.StatusOK {
			t.Errorf("%s again with the same key and body: answer %d, want 200: %s", what, r2.Status, r2.Raw)
		} else if want := without(r1.Body, r.showOnce); !reflect.DeepEqual(r2.Body, want) {
			t.Errorf("%s again with the same key and body answered\n%s\nwant the first answer without %v:\n%s", what, r2.Raw, r.showOnce, r1.Raw)
		}
		for _, p := range r.showOnce {
			if harness.Field(r1.Body, p) == nil {
				t.Errorf("%s: the first answer has no %s to show once: %s", what, p, r1.Raw)
			}
		}
		if n := c.count(); n != n0+1 {
			t.Errorf("%s twice with one key: %d resources, want %d", what, n, n0+1)
		}
		assertEnvelope(t, what+" with the same key and another body", post(map[string]any{"different": true}, key),
			http.StatusConflict, "conflict")
		for _, bad := range []string{"", strings.Repeat("k", 256)} {
			assertEnvelope(t, fmt.Sprintf("%s with a key of %d characters", what, len(bad)), post(c.body, bad),
				http.StatusBadRequest, "invalid_request")
		}
		if n := c.count(); n != n0+1 {
			t.Errorf("%s with refused keys: %d resources, want %d", what, n, n0+1)
		}
	}
}
