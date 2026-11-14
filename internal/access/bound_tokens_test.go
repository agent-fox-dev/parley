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

// mintedToken is the answer of POST /workspaces/:slug/tokens.
type mintedToken struct {
	TokenID     string   `json:"token_id"`
	UserID      string   `json:"user_id"`
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
	Paths       []string `json:"paths"`
	Workspace   *string  `json:"workspace"`
	ExpiresAt   *string  `json:"expires_at"`
	RevokedAt   *string  `json:"revoked_at"`
	CreatedAt   string   `json:"created_at"`
	Token       string   `json:"token"`
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

// minted decodes a's body as a minted token.
func minted(t *testing.T, a answer) mintedToken {
	t.Helper()
	var m mintedToken
	if err := json.Unmarshal(a.body, &m); err != nil {
		t.Fatalf("body %s is not a token: %v", a.body, err)
	}
	return m
}

var tokenValue = regexp.MustCompile(`^parley_pat_([a-z0-9]{8})_[a-z0-9]{32}$`)

const validBody = `{"name":"agent","permissions":["git:read"]}`

func TestCreateBoundToken_MintsATokenOfTheCallersUserBoundToTheWorkspace(t *testing.T) {
	h := newTokenHub(t)
	unbound, _ := h.pat(h.alice, "", "tokens:manage", "git:read")
	for _, caller := range []struct{ name, credential string }{{"API key", h.aliceKey}, {"unbound PAT", unbound}} {
		for _, tc := range []struct {
			name, body string
			days       int
			paths      []string
		}{
			{"default expiry", validBody, 90, nil},
			{"never expires", `{"name":"agent","permissions":["git:read"],"expires":0}`, 0, nil},
			{"30 days with paths", `{"name":"agent","permissions":["git:read"],"expires":30,"paths":["docs/**"]}`, 30, []string{"docs/**"}},
		} {
			t.Run(caller.name+" "+tc.name, func(t *testing.T) {
				a := h.post("/workspaces/w1/tokens", caller.credential, tc.body)
				if a.status != http.StatusCreated {
					t.Fatalf("POST = %d %s, want 201", a.status, a.body)
				}
				m := minted(t, a)
				if m.Workspace == nil || *m.Workspace != "w1" || m.UserID != h.alice || m.RevokedAt != nil || !reflect.DeepEqual(m.Paths, tc.paths) {
					t.Errorf("token %s, want alice's live token bound to w1 with paths %v", a.body, tc.paths)
				}
				if v := tokenValue.FindStringSubmatch(m.Token); v == nil || v[1] != m.TokenID {
					t.Errorf("token %q, want parley_pat_%s_<secret>", m.Token, m.TokenID)
				}
				created, _ := time.Parse(time.RFC3339, m.CreatedAt)
				want := created.Add(time.Duration(tc.days) * 24 * time.Hour).UTC().Format(time.RFC3339)
				if tc.days == 0 && m.ExpiresAt != nil || tc.days != 0 && (m.ExpiresAt == nil || *m.ExpiresAt != want) {
					t.Errorf("expires_at %v, want %s days after %s", m.ExpiresAt, map[bool]string{true: "never", false: want}[tc.days == 0], m.CreatedAt)
				}
				if n := h.pats("token_id = ? AND workspace = 'w1'", m.TokenID); n != 1 {
					t.Errorf("the store holds %d tokens %s bound to w1, want 1", n, m.TokenID)
				}
				r := httptest.NewRequest(http.MethodGet, "/api/v1/user", nil)
				r.Header.Set("Authorization", "Bearer "+m.Token)
				if c, err := h.ids.Authenticate(r); err != nil || c.CredentialID != m.TokenID {
					t.Errorf("authenticate the new token = %+v, %v, want the PAT %s", c, err, m.TokenID)
				}
			})
		}
	}
}

func TestCreateBoundToken_RecordsOneBindEventInTheTransactionOfTheToken(t *testing.T) {
	for _, caller := range []string{"api_key", "pat"} {
		t.Run(caller, func(t *testing.T) {
			h := newTokenHub(t)
			credential := h.aliceKey
			if caller == "pat" {
				credential, _ = h.pat(h.alice, "", "tokens:manage", "git:read")
			}
			// What the event's transaction holds when the event is emitted.
			var sawWorkspace string
			h.events.seen = func(tx *sql.Tx, ev access.Event) {
				if err := tx.QueryRow("SELECT workspace FROM pats WHERE token_id = ?", ev.ResourceID).Scan(&sawWorkspace); err != nil {
					t.Errorf("the event's transaction does not hold token %s: %v", ev.ResourceID, err)
				}
			}
			a := h.post("/workspaces/w1/tokens", credential, validBody)
			if a.status != http.StatusCreated {
				t.Fatalf("POST = %d %s, want 201", a.status, a.body)
			}
			m := minted(t, a)
			var expires any
			if m.ExpiresAt != nil {
				expires = *m.ExpiresAt
			}
			want := access.Event{Type: "parley.token.bind", Workspace: "w1", ActorType: caller, ActorID: h.alice,
				ResourceType: "token", ResourceID: m.TokenID, Metadata: map[string]any{
					"token_id": m.TokenID, "workspace": "w1", "permissions": []string{"git:read"},
					"paths": []string(nil), "expires_at": expires}}
			evs := h.events.all()
			if len(evs) != 1 || evs[0].tx == nil || !reflect.DeepEqual(evs[0].event, want) {
				t.Fatalf("events = %+v, want one %+v in a transaction", evs, want)
			}
			if sawWorkspace != "w1" {
				t.Errorf("the event's transaction held the token bound to %q, want w1", sawWorkspace)
			}
			raw, _ := json.Marshal(evs[0].event)
			secret := m.Token[strings.LastIndex(m.Token, "_")+1:]
			if strings.Contains(string(raw), secret) {
				t.Errorf("the event %s holds the token's secret", raw)
			}
		})
	}
}

func TestCreateBoundToken_RollsBackTheWholeMintWhenAStepFails(t *testing.T) {
	for _, tc := range []struct {
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
		t.Run(tc.name, func(t *testing.T) {
			h := newTokenHub(t)
			unbound, _ := h.pat(h.alice, "", "git:read")
			// Whether the event's transaction held the token it reports.
			var heldToken bool
			h.events.seen = func(tx *sql.Tx, ev access.Event) {
				var n int
				heldToken = tx.QueryRow("SELECT count(*) FROM pats WHERE token_id = ?", ev.ResourceID).Scan(&n) == nil && n == 1
			}
			tc.fail(h)

			a := h.post("/workspaces/w1/tokens", h.aliceKey, validBody)
			e := decode(t, a)
			if a.status != http.StatusInternalServerError || e.Error.ErrorType != "internal" || strings.Contains(string(a.body), tc.cause) {
				t.Errorf("POST = %d %s, want 500 internal without the cause", a.status, a.body)
			}
			if n := h.pats("1"); n != 1 {
				t.Errorf("the store holds %d tokens, want only the unbound one (%s)", n, unbound)
			}
			// An event emitted in the transaction of the token, which the
			// store shows rolled back, was rolled back with it.
			if evs := h.events.all(); len(evs) > 1 || len(evs) == 1 && !heldToken {
				t.Errorf("events = %+v, want none outside the rolled-back mint", evs)
			}
		})
	}
}

func TestCreateBoundToken_RefusesAndMintsNothing(t *testing.T) {
	h := newTokenHub(t)
	admin := h.admin()
	bobKey, carolKey := h.key(h.bob), h.key(h.carol)
	noManage, _ := h.pat(h.alice, "", "git:read")
	writeOnly, _ := h.pat(h.alice, "", "tokens:manage", "git:write")
	srcOnly, _ := h.storePAT(identity.PATSpec{UserID: h.alice, Permissions: []string{"tokens:manage", "git:read"}, Paths: []string{"src/**"}})
	before := h.pats("1")
	globs := `"` + strings.Repeat(`a/**","`, 50) + `a/**"`
	for _, tc := range []struct {
		name, credential, slug, body string
		status                       int
		errorType, message           string
	}{
		{"no name", h.aliceKey, "w1", `{"permissions":["git:read"]}`, 400, "invalid_request", "name"},
		{"a name of 256 characters", h.aliceKey, "w1", `{"name":"` + strings.Repeat("a", 256) + `","permissions":["git:read"]}`, 400, "invalid_request", "name"},
		{"no permissions", h.aliceKey, "w1", `{"name":"agent","permissions":[]}`, 400, "invalid_request", "permissions"},
		{"an unknown permission", h.aliceKey, "w1", `{"name":"agent","permissions":["nope:read"]}`, 400, "invalid_request", "nope:read"},
		{"45 days", h.aliceKey, "w1", `{"name":"agent","permissions":["git:read"],"expires":45}`, 400, "invalid_request", "expires"},
		{"no paths", h.aliceKey, "w1", `{"name":"agent","permissions":["git:read"],"paths":[]}`, 400, "invalid_request", "paths"},
		{"51 paths", h.aliceKey, "w1", `{"name":"agent","permissions":["git:read"],"paths":[` + globs + `]}`, 400, "invalid_request", "51"},
		{"an absolute path", h.aliceKey, "w1", `{"name":"agent","permissions":["git:read"],"paths":["/abs/**"]}`, 400, "invalid_request", "/abs/**"},
		{"an array body", h.aliceKey, "w1", `[` + validBody + `]`, 400, "invalid_request", "object"},
		{"an invalid body on an archived workspace", h.aliceKey, "w2", `{"permissions":["git:read"]}`, 400, "invalid_request", "name"},
		{"a PAT without tokens:manage", noManage, "w1", validBody, 403, "insufficient_scope", "tokens:manage"},
		{"a PAT without tokens:manage on an unknown slug", noManage, "nosuch", validBody, 403, "insufficient_scope", "tokens:manage"},
		{"a PAT asking a permission it lacks", writeOnly, "w1", validBody, 403, "insufficient_scope", "git:read"},
		{"a PAT asking a permission it lacks on an unknown slug", writeOnly, "nosuch", validBody, 403, "insufficient_scope", "git:read"},
		{"a PAT asking paths it does not cover", srcOnly, "w1", `{"name":"agent","permissions":["git:read"],"paths":["docs/**"]}`, 403, "insufficient_scope", "docs/**"},
		{"a PAT with paths asking none", srcOnly, "nosuch", validBody, 403, "insufficient_scope", "paths is required"},
		{"the admin token", admin, "w1", validBody, 403, "forbidden", "this route needs a user"},
		{"the admin token with an invalid body", admin, "w1", `{"permissions":["git:read"]}`, 403, "forbidden", "this route needs a user"},
		{"the admin token on an archived workspace", admin, "w2", validBody, 403, "forbidden", "this route needs a user"},
		{"a stranger", bobKey, "w1", validBody, 404, "not_found", "workspace not found"},
		{"a stranger on an unknown slug", bobKey, "nosuch", validBody, 404, "not_found", "workspace not found"},
		{"an org member", carolKey, "w1", validBody, 403, "forbidden", "read-only"},
		{"an org member with an invalid body", carolKey, "w1", `{"permissions":["git:read"]}`, 403, "forbidden", "read-only"},
		{"an org member on an archived workspace", carolKey, "w2", validBody, 403, "forbidden", "read-only"},
		{"the owner on an archived workspace", h.aliceKey, "w2", validBody, 409, "workspace_archived", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.post("/workspaces/"+tc.slug+"/tokens", tc.credential, tc.body)
			e := decode(t, a)
			if a.status != tc.status || e.Error.ErrorType != tc.errorType || !strings.Contains(e.Error.Message, tc.message) {
				t.Errorf("POST = %d %s, want %d %s naming %q", a.status, a.body, tc.status, tc.errorType, tc.message)
			}
		})
	}
	stranger := h.post("/workspaces/w1/tokens", bobKey, validBody)
	unknown := h.post("/workspaces/nosuch/tokens", bobKey, validBody)
	if string(stranger.body) != string(unknown.body) {
		t.Errorf("a stranger on w1 got %s and on an unknown slug %s, want the same bytes", stranger.body, unknown.body)
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
	if first.status != http.StatusCreated || replay.status != http.StatusOK {
		t.Fatalf("POST = %d %s, then %d %s, want 201 then 200", first.status, first.body, replay.status, replay.body)
	}
	var want, got map[string]any
	if json.Unmarshal(first.body, &want) != nil || json.Unmarshal(replay.body, &got) != nil {
		t.Fatalf("bodies %s and %s are not JSON objects", first.body, replay.body)
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
	a := h.post("/workspaces/w1/tokens", h.aliceKey, `{"name":"agent","permissions":["git:read","git:write"],"paths":["docs/**"]}`)
	if a.status != http.StatusCreated {
		t.Fatalf("POST = %d %s, want 201", a.status, a.body)
	}
	own := "/user/tokens/" + minted(t, a).TokenID
	keeps := func(t *testing.T, tok mintedToken) {
		t.Helper()
		if tok.Workspace == nil || *tok.Workspace != "w1" || !reflect.DeepEqual(tok.Paths, []string{"docs/**"}) {
			t.Errorf("token %+v, want it bound to w1 with paths [docs/**]", tok)
		}
	}
	for _, step := range []struct{ name, method, path, body string }{
		{"replace the permissions, with a workspace and paths", http.MethodPut, own + "/permissions", `{"permissions":["git:read"],"workspace":"w9","paths":["**"]}`},
		{"add a permission", http.MethodPatch, own + "/permissions", `{"permissions":["changes:read"]}`},
		{"remove a permission", http.MethodDelete, own + "/permissions", `{"permissions":["changes:read"]}`},
		{"revoke", http.MethodDelete, own, ""},
	} {
		t.Run(step.name, func(t *testing.T) {
			if a := h.send(step.method, step.path, h.aliceKey, step.body, nil); a.status/100 != 2 {
				t.Fatalf("%s %s = %d %s, want success", step.method, step.path, a.status, a.body)
			}
			keeps(t, minted(t, h.do(http.MethodGet, own, h.aliceKey)))
		})
	}
	l := listed(t, h.do(http.MethodGet, "/workspaces/w1/tokens", h.aliceKey))
	if len(l.Items) != 1 || l.Items[0].RevokedAt == nil {
		t.Fatalf("GET /workspaces/w1/tokens = %+v, want the one token, revoked", l.Items)
	}
	keeps(t, l.Items[0])
}

// tokenList is the answer of GET /workspaces/:slug/tokens.
type tokenList struct {
	Items      []mintedToken `json:"items"`
	NextCursor *string       `json:"next_cursor"`
	raw        []map[string]any
}

// listed decodes a's body as a list of tokens, failing the test unless a
// is a 200.
func listed(t *testing.T, a answer) tokenList {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("GET = %d %s, want 200", a.status, a.body)
	}
	var l tokenList
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if json.Unmarshal(a.body, &l) != nil || json.Unmarshal(a.body, &raw) != nil || l.Items == nil {
		t.Fatalf("body %s is not a list of tokens", a.body)
	}
	l.raw = raw.Items
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
	if _, err := h.st.Main().Exec("UPDATE pats SET revoked_at = ? WHERE token_id = ?", at.Add(time.Minute).UnixNano(), t2); err != nil {
		t.Fatal(err)
	}
	_, onW2 := h.storePAT(spec("w2", at, nil))
	unbound, unboundID := h.pat(h.alice, "", "tokens:read")

	for _, caller := range []struct{ name, credential string }{{"API key", h.aliceKey}, {"unbound PAT", unbound}} {
		for _, tc := range []struct {
			slug string
			want []string
		}{{"w1", []string{t1, t2, t3}}, {"w2", []string{onW2}}, {"w3", []string{}}} {
			t.Run(caller.name+" "+tc.slug, func(t *testing.T) {
				a := h.do(http.MethodGet, "/workspaces/"+tc.slug+"/tokens", caller.credential)
				l := listed(t, a)
				if !reflect.DeepEqual(l.ids(), tc.want) || l.NextCursor != nil {
					t.Fatalf("items %v, next_cursor %v, want %v and null", l.ids(), l.NextCursor, tc.want)
				}
				for i, item := range l.raw {
					if _, ok := item["token"]; ok || l.Items[i].Workspace == nil || *l.Items[i].Workspace != tc.slug {
						t.Errorf("item %v, want a Token object bound to %s without token", item, tc.slug)
					}
				}
				if tc.slug != "w1" {
					return
				}
				if l.Items[0].RevokedAt != nil || l.Items[1].RevokedAt == nil {
					t.Errorf("revoked_at %v and %v, want t1 live and t2 revoked", l.Items[0].RevokedAt, l.Items[1].RevokedAt)
				}
				if e, err := time.Parse(time.RFC3339, deref(l.Items[2].ExpiresAt)); err != nil || !e.Before(time.Now()) {
					t.Errorf("t3 expires_at %v, want an instant in the past", l.Items[2].ExpiresAt)
				}
				if strings.Contains(string(a.body), onW2) || strings.Contains(string(a.body), unboundID) {
					t.Errorf("w1's list %s holds the token of w2 or the unbound token", a.body)
				}
			})
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestListBoundTokens_AnswersTheAdminTokenAsTheOwner(t *testing.T) {
	h := newTokenHub(t)
	admin := h.admin()
	for _, slug := range []string{"w1", "w1", "w2"} {
		h.pat(h.alice, slug, "git:read")
	}
	for _, slug := range []string{"w1", "w2"} {
		t.Run(slug, func(t *testing.T) {
			a := h.do(http.MethodGet, "/workspaces/"+slug+"/tokens", admin)
			o := h.do(http.MethodGet, "/workspaces/"+slug+"/tokens", h.aliceKey)
			if a.status != http.StatusOK || len(listed(t, o).Items) == 0 || string(a.body) != string(o.body) {
				t.Errorf("admin: %d %s, owner: %d %s, want 200 with the owner's items", a.status, a.body, o.status, o.body)
			}
		})
	}
}

func TestListBoundTokens_RefusesAndListsNothing(t *testing.T) {
	h := newTokenHub(t)
	_, t1 := h.pat(h.alice, "w1", "git:read")
	noRead, _ := h.pat(h.alice, "", "git:read")
	bobKey, carolKey := h.key(h.bob), h.key(h.carol)
	for _, tc := range []struct {
		name, credential, slug string
		status                 int
		errorType              string
	}{
		{"a PAT without tokens:read", noRead, "w1", 403, "insufficient_scope"},
		{"a PAT without tokens:read on an unknown slug", noRead, "nosuch", 403, "insufficient_scope"},
		{"a stranger", bobKey, "w1", 404, "not_found"},
		{"a stranger on an unknown slug", bobKey, "nosuch", 404, "not_found"},
		{"an org member", carolKey, "w1", 403, "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.do(http.MethodGet, "/workspaces/"+tc.slug+"/tokens", tc.credential)
			if e := decode(t, a); a.status != tc.status || e.Error.ErrorType != tc.errorType || strings.Contains(string(a.body), t1) {
				t.Errorf("GET = %d %s, want %d %s without a token", a.status, a.body, tc.status, tc.errorType)
			}
		})
	}
	stranger := h.do(http.MethodGet, "/workspaces/w1/tokens", bobKey)
	unknown := h.do(http.MethodGet, "/workspaces/nosuch/tokens", bobKey)
	if string(stranger.body) != string(unknown.body) || decode(t, stranger).Error.Message != "workspace not found" {
		t.Errorf("a stranger on w1 got %s and on an unknown slug %s, want the same workspace not found", stranger.body, unknown.body)
	}
}

func TestTokenRoutes_RefuseABoundTokenOnEveryWorkspaceItsBindingReaches(t *testing.T) {
	h := newTokenHub(t)
	seed(t, h.st, repo("w5", h.alice, "acme", access.StatusReady))
	all := []string{"tokens:manage", "tokens:read", "git:read"}
	full, fullID := h.pat(h.alice, "w1", all...)
	narrow, _ := h.pat(h.alice, "w1", "git:read")
	seed(t, h.st, createdBy(fork("f1", h.alice, "acme", "w1"), fullID), fork("f2", h.alice, "acme", "w1"))
	onFork, _ := h.pat(h.alice, "f2", all...)
	before := h.pats("1")

	for _, tc := range []struct {
		name, token, slug string
		status            int
		errorType         string
	}{
		{"every scope, the bound workspace", full, "w1", 403, "token_bound"},
		{"no scope of the routes, the bound workspace", narrow, "w1", 403, "token_bound"},
		{"a fork the token created", full, "f1", 403, "token_bound"},
		{"the bound fork", onFork, "f2", 403, "token_bound"},
		{"the parent of the bound fork", onFork, "w1", 403, "token_bound"},
		{"every scope, another workspace", full, "w5", 404, "not_found"},
		{"no scope of the routes, another workspace", narrow, "w5", 404, "not_found"},
		{"a fork token on another workspace", onFork, "w5", 404, "not_found"},
	} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			t.Run(tc.name+" "+method, func(t *testing.T) {
				reported := len(h.refusals.all())
				body := ""
				if method == http.MethodPost {
					body = validBody
				}
				a := h.send(method, "/workspaces/"+tc.slug+"/tokens", tc.token, body, nil)
				if e := decode(t, a); a.status != tc.status || e.Error.ErrorType != tc.errorType {
					t.Errorf("%s = %d %s, want %d %s", method, a.status, a.body, tc.status, tc.errorType)
				}
				slug := tc.slug
				want := access.Refusal{Method: method, Route: "/api/v1/workspaces/:slug/tokens", Status: tc.status, RequestedWorkspace: &slug}
				got := h.refusals.all()[reported:]
				if len(got) != 1 || got[0].Method != want.Method || got[0].Route != want.Route || got[0].Status != want.Status ||
					got[0].RequestedWorkspace == nil || *got[0].RequestedWorkspace != slug {
					t.Errorf("refusals = %+v, want one like %+v", got, want)
				}
			})
		}
	}
	if n := h.pats("1"); n != before || len(h.events.all()) != 0 {
		t.Errorf("the store holds %d tokens and %d events were emitted, want %d and none", n, len(h.events.all()), before)
	}
}
