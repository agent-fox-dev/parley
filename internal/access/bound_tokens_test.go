package access_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// mintedToken is a Token object as the bound-token routes answer it.
type mintedToken struct {
	TokenID     string   `json:"token_id"`
	UserID      string   `json:"user_id"`
	Permissions []string `json:"permissions"`
	Paths       []string `json:"paths"`
	Workspace   *string  `json:"workspace"`
	ExpiresAt   *string  `json:"expires_at"`
	RevokedAt   *string  `json:"revoked_at"`
	CreatedAt   string   `json:"created_at"`
	Token       *string  `json:"token"`
}

// tokenHub is a guard hub with the bound-token routes, the user alice with
// an API key and the org acme, which she and carol are members of, alice's
// repository w1 and her archived repository w2, both of acme, and bob, who
// has nothing to do with them.
type tokenHub struct {
	*guardHub
	alice, aliceKey, bob, carol string
}

func newTokenHub(t *testing.T) tokenHub {
	h := newGuardHub(t)
	h.tokenRoutes()
	alice, _ := h.user("alice")
	bob, _ := h.user("bob")
	carol, _ := h.user("carol")
	h.orgs[alice], h.orgs[carol] = []string{"acme"}, []string{"acme"}
	seed(t, h.st, repo("w1", alice, "acme", access.StatusReady), repo("w2", alice, "acme", access.StatusArchived))
	return tokenHub{h, alice, h.key(alice), bob, carol}
}

// minted decodes a's body as a Token object, failing the test unless a has
// status.
func minted(t *testing.T, a answer, status int) mintedToken {
	t.Helper()
	var m mintedToken
	if a.status != status || json.Unmarshal(a.body, &m) != nil {
		t.Fatalf("answer %d %s, want %d with a token", a.status, a.body, status)
	}
	return m
}

var tokenValue = regexp.MustCompile(`^parley_pat_([a-z0-9]{8})_[a-z0-9]{32}$`)

const validBody = `{"name":"agent","permissions":["git:read"]}`

func TestCreateBoundToken_MintsATokenOfTheCallersUserAndItsEventInOneTransaction(t *testing.T) {
	h := newTokenHub(t)
	unbound, _ := h.pat(h.alice, "", "tokens:manage", "git:read")
	// What the event's transaction holds when the event is emitted.
	var sawWorkspace string
	h.events.seen = func(tx *sql.Tx, ev access.Event) {
		sawWorkspace = ""
		_ = tx.QueryRow("SELECT workspace FROM pats WHERE token_id = ?", ev.ResourceID).Scan(&sawWorkspace)
	}
	for _, caller := range []struct{ actor, credential string }{{"api_key", h.aliceKey}, {"pat", unbound}} {
		for _, c := range []struct {
			body  string
			days  int
			paths []string
		}{
			{validBody, 90, nil},
			{`{"name":"agent","permissions":["git:read"],"expires":0}`, 0, nil},
			{`{"name":"agent","permissions":["git:read"],"expires":30,"paths":["docs/**"]}`, 30, []string{"docs/**"}},
		} {
			before := len(h.events.all())
			m := minted(t, h.send(http.MethodPost, "/workspaces/w1/tokens", caller.credential, c.body, nil), http.StatusCreated)
			what := caller.actor + " " + c.body
			if m.Workspace == nil || *m.Workspace != "w1" || m.UserID != h.alice || m.RevokedAt != nil || !reflect.DeepEqual(m.Paths, c.paths) {
				t.Errorf("%s: token %+v, want alice's live token bound to w1 with paths %v", what, m, c.paths)
			}
			if v := tokenValue.FindStringSubmatch(*m.Token); v == nil || v[1] != m.TokenID {
				t.Errorf("%s: token %q, want parley_pat_%s_<secret>", what, *m.Token, m.TokenID)
			}
			created, _ := time.Parse(time.RFC3339, m.CreatedAt)
			var expires any
			if c.days != 0 {
				expires = created.Add(time.Duration(c.days) * 24 * time.Hour).UTC().Format(time.RFC3339)
			}
			if m.ExpiresAt == nil && expires != nil || m.ExpiresAt != nil && *m.ExpiresAt != expires {
				t.Errorf("%s: expires_at %v, want %v", what, m.ExpiresAt, expires)
			}
			r := httptest.NewRequest(http.MethodGet, "/api/v1/user", nil)
			r.Header.Set("Authorization", "Bearer "+*m.Token)
			if got, err := h.ids.Authenticate(r); err != nil || got.CredentialID != m.TokenID {
				t.Errorf("%s: authenticate the new token = %+v, %v, want the PAT %s", what, got, err, m.TokenID)
			}
			want := bindEvent(m.TokenID, h.alice, caller.actor, []string{"git:read"}, c.paths, expires)
			evs := h.events.all()[before:]
			if len(evs) != 1 || evs[0].tx == nil || !reflect.DeepEqual(evs[0].event, want) || sawWorkspace != "w1" {
				t.Errorf("%s: events %+v, its transaction holding the token bound to %q; want one %+v in the token's", what, evs, sawWorkspace, want)
			}
			if raw, _ := json.Marshal(evs); strings.Contains(string(raw), (*m.Token)[20:]) {
				t.Errorf("%s: the event %s holds the token's secret", what, raw)
			}
		}
	}
}

func TestCreateBoundToken_RollsBackTheWholeMintWhenAStepFails(t *testing.T) {
	for _, c := range []struct {
		name, cause string
		fail        func(h tokenHub)
	}{
		{"the event emitter fails", "emitter down", func(h tokenHub) { h.events.err = errors.New("emitter down") }},
		{"the token insert fails", "token insert blocked", func(h tokenHub) {
			if _, err := h.st.Main().Exec(`CREATE TRIGGER block BEFORE INSERT ON pats BEGIN SELECT RAISE(ABORT, 'token insert blocked'); END`); err != nil {
				h.t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTokenHub(t)
			// Whether the event's transaction held the token it reports.
			var heldToken bool
			h.events.seen = func(tx *sql.Tx, ev access.Event) {
				var n int
				heldToken = tx.QueryRow("SELECT count(*) FROM pats WHERE token_id = ?", ev.ResourceID).Scan(&n) == nil && n == 1
			}
			c.fail(h)
			a := h.send(http.MethodPost, "/workspaces/w1/tokens", h.aliceKey, validBody, nil)
			if e := decode(t, a); a.status != http.StatusInternalServerError || e.Error.ErrorType != "internal" || strings.Contains(string(a.body), c.cause) {
				t.Errorf("POST = %d %s, want 500 internal without the cause", a.status, a.body)
			}
			// An event emitted in the transaction of the token, which the
			// store shows rolled back, was rolled back with it.
			if n, evs := h.pats("1"), h.events.all(); n != 0 || len(evs) > 1 || len(evs) == 1 && !heldToken {
				t.Errorf("the store holds %d tokens, events %+v; want none outside the rolled-back mint", n, evs)
			}
		})
	}
}

func TestTokenRoutes_RefuseAndChangeNothing(t *testing.T) {
	h := newTokenHub(t)
	admin, bobKey, carolKey := h.admin(), h.key(h.bob), h.key(h.carol)
	noScope, _ := h.pat(h.alice, "", "git:read")
	writeOnly, _ := h.pat(h.alice, "", "tokens:manage", "git:write")
	srcOnly, _ := h.storePAT(identity.PATSpec{UserID: h.alice, Permissions: []string{"tokens:manage", "git:read"}, Paths: []string{"src/**"}})
	seed(t, h.st, repo("w5", h.alice, "acme", access.StatusReady))
	every := []string{"tokens:manage", "tokens:read", "git:read"}
	full, fullID := h.pat(h.alice, "w1", every...)
	narrow, _ := h.pat(h.alice, "w1", "git:read")
	seed(t, h.st, createdBy(fork("f1", h.alice, "acme", "w1"), fullID), fork("f2", h.alice, "acme", "w1"))
	onFork, _ := h.pat(h.alice, "f2", every...)
	before := h.pats("1")
	globs := `"` + strings.Repeat(`a/**","`, 50) + `a/**"`
	post, get := http.MethodPost, http.MethodGet
	type tokenCase struct {
		method, credential, slug, body string
		status                         int
		errorType, message             string
	}
	cases := []tokenCase{
		{post, h.aliceKey, "w1", `{"permissions":["git:read"]}`, 400, "invalid_request", "name"},
		{post, h.aliceKey, "w1", `{"name":"` + strings.Repeat("a", 256) + `","permissions":["git:read"]}`, 400, "invalid_request", "name"},
		{post, h.aliceKey, "w1", `{"name":"agent","permissions":[]}`, 400, "invalid_request", "permissions"},
		{post, h.aliceKey, "w1", `{"name":"agent","permissions":["nope:read"]}`, 400, "invalid_request", "nope:read"},
		{post, h.aliceKey, "w1", `{"name":"agent","permissions":["git:read"],"expires":45}`, 400, "invalid_request", "expires"},
		{post, h.aliceKey, "w1", `{"name":"agent","permissions":["git:read"],"paths":[]}`, 400, "invalid_request", "paths"},
		{post, h.aliceKey, "w1", `{"name":"agent","permissions":["git:read"],"paths":[` + globs + `]}`, 400, "invalid_request", "51"},
		{post, h.aliceKey, "w1", `{"name":"agent","permissions":["git:read"],"paths":["/abs/**"]}`, 400, "invalid_request", "/abs/**"},
		{post, h.aliceKey, "w1", `[` + validBody + `]`, 400, "invalid_request", "object"},
		{post, h.aliceKey, "w2", `{"permissions":["git:read"]}`, 400, "invalid_request", "name"},
		{post, noScope, "w1", validBody, 403, "insufficient_scope", "tokens:manage"},
		{post, noScope, "nosuch", validBody, 403, "insufficient_scope", "tokens:manage"},
		{post, writeOnly, "w1", validBody, 403, "insufficient_scope", "git:read"},
		{post, writeOnly, "nosuch", validBody, 403, "insufficient_scope", "git:read"},
		{post, srcOnly, "w1", `{"name":"agent","permissions":["git:read"],"paths":["docs/**"]}`, 403, "insufficient_scope", "docs/**"},
		{post, srcOnly, "nosuch", validBody, 403, "insufficient_scope", "paths is required"},
		{post, admin, "w1", validBody, 403, "forbidden", "this route needs a user"},
		{post, admin, "w1", `{"permissions":["git:read"]}`, 403, "forbidden", "this route needs a user"},
		{post, admin, "w2", validBody, 403, "forbidden", "this route needs a user"},
		{post, bobKey, "w1", validBody, 404, "not_found", "workspace not found"},
		{post, bobKey, "nosuch", validBody, 404, "not_found", "workspace not found"},
		{post, carolKey, "w1", validBody, 403, "forbidden", "read-only"},
		{post, carolKey, "w1", `{"permissions":["git:read"]}`, 403, "forbidden", "read-only"},
		{post, carolKey, "w2", validBody, 403, "forbidden", "read-only"},
		{post, h.aliceKey, "w2", validBody, 409, "workspace_archived", ""},
		{get, noScope, "w1", "", 403, "insufficient_scope", "tokens:read"},
		{get, noScope, "nosuch", "", 403, "insufficient_scope", "tokens:read"},
		{get, bobKey, "w1", "", 404, "not_found", "workspace not found"},
		{get, bobKey, "nosuch", "", 404, "not_found", "workspace not found"},
		{get, carolKey, "w1", "", 403, "forbidden", "read-only"},
	}
	// A bound token is refused on every workspace its binding reaches, and a
	// workspace outside it is not found, whatever scopes it holds.
	for _, method := range []string{post, get} {
		body := map[bool]string{true: validBody}[method == post]
		for _, b := range []struct {
			token, slug string
			status      int
		}{{full, "w1", 403}, {narrow, "w1", 403}, {full, "f1", 403}, {onFork, "f2", 403}, {onFork, "w1", 403},
			{full, "w5", 404}, {narrow, "w5", 404}, {onFork, "w5", 404}} {
			cases = append(cases, tokenCase{method, b.token, b.slug, body, b.status, map[int]string{403: "token_bound", 404: "not_found"}[b.status], ""})
		}
	}
	answers := map[string]string{}
	for _, c := range cases {
		a := h.send(c.method, "/workspaces/"+c.slug+"/tokens", c.credential, c.body, nil)
		if e := decode(t, a); a.status != c.status || e.Error.ErrorType != c.errorType || !strings.Contains(e.Error.Message, c.message) || strings.Contains(string(a.body), fullID) {
			t.Errorf("%s %s with %s = %d %s, want %d %s naming %q", c.method, c.slug, c.body, a.status, a.body, c.status, c.errorType, c.message)
		}
		if c.credential == bobKey {
			answers[c.method+c.slug] = string(a.body)
		}
	}
	for _, method := range []string{post, get} {
		if answers[method+"w1"] != answers[method+"nosuch"] {
			t.Errorf("%s: a stranger on w1 got %s and on an unknown slug %s, want the same bytes", method, answers[method+"w1"], answers[method+"nosuch"])
		}
	}
	if n := h.pats("1"); n != before || len(h.events.all()) != 0 {
		t.Errorf("the store holds %d tokens and %d events were emitted, want %d and none", n, len(h.events.all()), before)
	}
}

func TestCreateBoundToken_ReplaysTheFirstAnswerWithoutTheTokenForTheSameIdempotencyKey(t *testing.T) {
	h := newTokenHub(t)
	key := http.Header{"Idempotency-Key": {"k1"}}
	first := h.send(http.MethodPost, "/workspaces/w1/tokens", h.aliceKey, validBody, key)
	replay := h.send(http.MethodPost, "/workspaces/w1/tokens", h.aliceKey, validBody, key)
	var want, got map[string]any
	if first.status != http.StatusCreated || replay.status != http.StatusOK || json.Unmarshal(first.body, &want) != nil || json.Unmarshal(replay.body, &got) != nil {
		t.Fatalf("POST = %d %s, then %d %s, want 201 then 200 with a JSON object", first.status, first.body, replay.status, replay.body)
	}
	delete(want, "token")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replay = %s, want the first answer %s without token", replay.body, first.body)
	}
	if n, evs := h.pats("workspace = 'w1'"), h.events.all(); n != 1 || len(evs) != 1 {
		t.Errorf("the store holds %d tokens bound to w1 and %d events were emitted, want 1 and 1", n, len(evs))
	}
}

func TestBoundToken_KeepsItsWorkspaceAndPathsThroughEveryTokenRoute(t *testing.T) {
	h := newTokenHub(t)
	h.identityRoutes()
	m := minted(t, h.send(http.MethodPost, "/workspaces/w1/tokens", h.aliceKey, `{"name":"agent","permissions":["git:read","git:write"],"paths":["docs/**"]}`, nil), http.StatusCreated)
	own := "/user/tokens/" + m.TokenID
	keeps := func(what string, tok mintedToken) {
		if tok.Workspace == nil || *tok.Workspace != "w1" || !reflect.DeepEqual(tok.Paths, []string{"docs/**"}) {
			t.Errorf("after %s: token %+v, want it bound to w1 with paths [docs/**]", what, tok)
		}
	}
	for _, step := range []struct{ method, path, body string }{
		{http.MethodPut, own + "/permissions", `{"permissions":["git:read"],"workspace":"w9","paths":["**"]}`},
		{http.MethodPatch, own + "/permissions", `{"permissions":["changes:read"]}`},
		{http.MethodDelete, own + "/permissions", `{"permissions":["changes:read"]}`},
		{http.MethodDelete, own, ""},
	} {
		if a := h.send(step.method, step.path, h.aliceKey, step.body, nil); a.status/100 != 2 {
			t.Fatalf("%s %s = %d %s, want success", step.method, step.path, a.status, a.body)
		}
		keeps(step.method+" "+step.path, minted(t, h.do(http.MethodGet, own, h.aliceKey), http.StatusOK))
	}
	l := listed(t, h.do(http.MethodGet, "/workspaces/w1/tokens", h.aliceKey))
	if len(l.Items) != 1 || l.Items[0].RevokedAt == nil {
		t.Fatalf("GET /workspaces/w1/tokens = %+v, want the one token, revoked", l.Items)
	}
	keeps("the list", l.Items[0])
}

// tokenList is the answer of GET /workspaces/:slug/tokens.
type tokenList struct {
	Items      []mintedToken `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

// listed decodes a's body as a list of tokens, failing the test unless a
// is a 200.
func listed(t *testing.T, a answer) tokenList {
	t.Helper()
	var l tokenList
	if a.status != http.StatusOK || json.Unmarshal(a.body, &l) != nil || l.Items == nil {
		t.Fatalf("GET = %d %s, want 200 with a list of tokens", a.status, a.body)
	}
	return l
}

// ids returns the token ids of l's items, in order.
func (l tokenList) ids() []string {
	ids := []string{}
	for _, tok := range l.Items {
		ids = append(ids, tok.TokenID)
	}
	return ids
}

func TestListBoundTokens_ListsEveryTokenOfTheWorkspaceInAnyStateOldestFirst(t *testing.T) {
	h := newTokenHub(t)
	seed(t, h.st, repo("w3", h.alice, "acme", access.StatusReady))
	at := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	expired := at.Add(time.Hour)
	spec := func(workspace string, created time.Time, expires *time.Time) identity.PATSpec {
		return identity.PATSpec{UserID: h.alice, Permissions: []string{"git:read"}, Workspace: workspace, CreatedAt: created, ExpiresAt: expires}
	}
	// Stored newest first, so the order is not the order of storing.
	_, t3 := h.storePAT(spec("w1", at.Add(time.Second), &expired))
	_, a := h.storePAT(spec("w1", at, nil))
	_, b := h.storePAT(spec("w1", at, nil))
	t1, t2 := min(a, b), max(a, b)
	// In the same whole second, the one created_at is shown in, but the
	// greater token_id is the earlier instant: token_id decides.
	for _, q := range []struct {
		sql string
		at  time.Time
		id  string
	}{
		{"UPDATE pats SET created_at = ? WHERE token_id = ?", at.Add(900 * time.Millisecond), t1},
		{"UPDATE pats SET created_at = ? WHERE token_id = ?", at.Add(100 * time.Millisecond), t2},
		{"UPDATE pats SET revoked_at = ? WHERE token_id = ?", at.Add(time.Minute), t2},
	} {
		if _, err := h.st.Main().Exec(q.sql, q.at.UnixNano(), q.id); err != nil {
			t.Fatal(err)
		}
	}
	_, onW2 := h.storePAT(spec("w2", at, nil))
	unbound, unboundID := h.pat(h.alice, "", "tokens:read")

	// The admin token is answered as the owner.
	for _, credential := range []string{h.aliceKey, unbound, h.admin()} {
		for _, c := range []struct {
			slug string
			want []string
		}{{"w1", []string{t1, t2, t3}}, {"w2", []string{onW2}}, {"w3", []string{}}} {
			a := h.do(http.MethodGet, "/workspaces/"+c.slug+"/tokens", credential)
			l := listed(t, a)
			if !reflect.DeepEqual(l.ids(), c.want) || l.NextCursor != nil {
				t.Fatalf("%s: items %v, next_cursor %v, want %v and null", c.slug, l.ids(), l.NextCursor, c.want)
			}
			for _, item := range l.Items {
				if item.Token != nil || item.Workspace == nil || *item.Workspace != c.slug {
					t.Errorf("%s: item %+v, want a Token object bound to %s without token", c.slug, item, c.slug)
				}
			}
			if c.slug != "w1" {
				continue
			}
			if l.Items[0].RevokedAt != nil || l.Items[1].RevokedAt == nil {
				t.Errorf("revoked_at %v and %v, want t1 live and t2 revoked", l.Items[0].RevokedAt, l.Items[1].RevokedAt)
			}
			if e := l.Items[2].ExpiresAt; e == nil || *e >= time.Now().UTC().Format(time.RFC3339) {
				t.Errorf("t3 expires_at %v, want an instant in the past", e)
			}
			if strings.Contains(string(a.body), onW2) || strings.Contains(string(a.body), unboundID) {
				t.Errorf("w1's list %s holds the token of w2 or the unbound token", a.body)
			}
		}
	}

	var paged []string
	path := "/workspaces/w1/tokens?limit=1"
	for range 4 {
		l := listed(t, h.do(http.MethodGet, path, h.aliceKey))
		paged = append(paged, l.ids()...)
		if l.NextCursor == nil {
			break
		}
		path = "/workspaces/w1/tokens?limit=1&cursor=" + url.QueryEscape(*l.NextCursor)
	}
	if want := []string{t1, t2, t3}; !reflect.DeepEqual(paged, want) {
		t.Errorf("pages of one held %v, want %v", paged, want)
	}
}
