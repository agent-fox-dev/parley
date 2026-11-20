package harness

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// provider is the fake GitHub identity provider of the test process, which
// every hub's [[oauth.providers]] entry names. It knows the codes the
// harness issued, each for a user name, and gives every name its own stable
// numeric id, since the id is what identifies a user to the hub.
var provider struct {
	once  sync.Once
	srv   *httptest.Server
	mu    sync.Mutex
	codes map[string]string
	ids   map[string]int
}

// identityProvider returns the provider, started the first time.
func identityProvider(tb testing.TB) *httptest.Server {
	tb.Helper()
	provider.once.Do(func() {
		provider.codes, provider.ids = map[string]string{}, map[string]int{}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
			code := r.PostFormValue("code")
			provider.mu.Lock()
			_, ok := provider.codes[code]
			provider.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if !ok {
				fmt.Fprint(w, `{"error":"bad_verification_code"}`)
				return
			}
			fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer"}`, "token-"+code)
		})
		mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
			code := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer token-")
			provider.mu.Lock()
			name, ok := provider.codes[code]
			id := provider.ids[name]
			provider.mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%d,"login":%q,"name":%q,"email":%q}`, id, name, name, email(name))
		})
		provider.srv = httptest.NewServer(mux)
	})
	return provider.srv
}

// issueCode returns a new code the provider accepts for the user name.
func issueCode(name string) string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if _, ok := provider.ids[name]; !ok {
		provider.ids[name] = 1000 + len(provider.ids)
	}
	code := fmt.Sprintf("code-%d", len(provider.codes)+1)
	provider.codes[code] = name
	return code
}

// email is the e-mail address the provider gives the user name; the admin
// is the user the bootstrap named.
func email(name string) string {
	if name == "admin" {
		return AdminEmail
	}
	return name + "@example.test"
}

// User is a user signed in to a hub through the identity provider.
type User struct {
	// ID is the hub's id of the user; Name is its username.
	ID, Name, Email string
	// Key is the API key the sign-in returned.
	Key string
}

// User returns the user name signed in to the hub, signing in the first
// time: every sign-in revokes the user's earlier API keys, so a hub signs a
// name in once. User("admin") is the hub's admin.
func (h *Hub) User(name string) *User {
	h.t.Helper()
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if u, ok := h.users[name]; ok {
		return u
	}
	r := h.Call(http.MethodPost, "/api/v1/auth/callback", "", map[string]any{
		"provider": "github", "code": issueCode(name), "redirect_uri": "http://127.0.0.1/callback",
	}).Want(h.t, http.StatusOK)
	user, _ := r.Body["user"].(map[string]any)
	key, _ := r.Body["api_key"].(map[string]any)
	u := &User{ID: str(user["id"]), Name: name, Email: email(name), Key: str(key["key"])}
	h.users[name] = u
	return u
}

// CreateOrg creates the organization slug with the admin token and returns
// its id.
func (h *Hub) CreateOrg(slug string) string {
	h.t.Helper()
	r := h.Call(http.MethodPost, "/api/v1/orgs", h.AdminToken, map[string]any{"name": slug, "slug": slug}).
		Want(h.t, http.StatusCreated)
	return str(r.Body["id"])
}

// AddMember makes u a member of the organization org, its id or slug.
func (h *Hub) AddMember(org string, u *User) {
	h.t.Helper()
	h.Call(http.MethodPut, "/api/v1/orgs/"+org+"/members/"+u.ID, h.AdminToken, nil).Want(h.t, http.StatusNoContent)
}

// tokenNames numbers the tokens the harness mints, whose names must differ.
var tokenNames atomic.Int64

// PAT mints a personal access token of u with the permissions given, its
// writes limited to the path globs when there are any, and returns it.
func (h *Hub) PAT(u *User, permissions []string, paths ...string) string {
	h.t.Helper()
	return h.mint(u, "/api/v1/user/tokens", permissions, paths)
}

// BoundPAT mints a token of u bound to the workspace, otherwise as PAT.
func (h *Hub) BoundPAT(u *User, workspace string, permissions []string, paths ...string) string {
	h.t.Helper()
	return h.mint(u, "/api/v1/workspaces/"+workspace+"/tokens", permissions, paths)
}

func (h *Hub) mint(u *User, path string, permissions, paths []string) string {
	h.t.Helper()
	body := map[string]any{"name": fmt.Sprintf("acceptance-%d", tokenNames.Add(1)), "permissions": permissions}
	if len(paths) > 0 {
		body["paths"] = paths
	}
	return str(h.Call(http.MethodPost, path, u.Key, body).Want(h.t, http.StatusCreated).Body["token"])
}

// AgentScopes are the permissions an agent's token needs to clone, push,
// open and land changes in a fork.
var AgentScopes = []string{"git:read", "git:write", "changes:read", "changes:write", "workspaces:read"}

// str is v when it is a string, "" otherwise.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// SetPolicy replaces the landing policy of workspace with document, a JSON
// policy, through parley policy set run with credential, and returns the
// policy's new revision.
func (h *Hub) SetPolicy(credential, workspace, document string) int {
	h.t.Helper()
	file := filepath.Join(h.t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(document), 0o644); err != nil {
		h.t.Fatal(err)
	}
	set := h.CLI(credential, "policy", "set", workspace, "--file", file).Must(h.t)
	rev := Num(set.JSON()["revision"])
	if rev < 1 {
		h.t.Fatalf("parley policy set printed no revision:\n%s", set.Stdout)
	}
	return rev
}

// Cast is the organization acme, owned by alice, with bob as a member who
// may only read its workspaces; the workspace demo of acme, which alice
// owns, registered from a seeded origin; and five credentials that reach
// demo.
type Cast struct {
	Demo       *Workspace
	Alice, Bob *User
	// OwnerKey is alice's API key; BoundPAT is a token of alice bound to
	// demo; PathPAT is a token of alice bound to no workspace whose writes
	// are limited to the paths **; MemberKey is bob's API key; Admin is
	// the admin token. Both tokens may read and write over git.
	OwnerKey, BoundPAT, PathPAT, MemberKey, Admin string
}

// Cast signs alice and bob in and builds the cast around them.
func (h *Hub) Cast() *Cast {
	h.t.Helper()
	alice, bob := h.User("alice"), h.User("bob")
	h.Call(http.MethodPost, "/api/v1/orgs", h.AdminToken, map[string]any{"name": "acme", "slug": "acme", "owner_id": alice.ID}).
		Want(h.t, http.StatusCreated)
	// Owning an organization is not being a member of it, and only a
	// member may create a workspace in it.
	h.AddMember("acme", alice)
	h.AddMember("acme", bob)
	demo := h.Register(alice, "demo", h.Origin(), map[string]any{"org_id": "acme"})
	git := []string{"git:read", "git:write"}
	return &Cast{
		Demo: demo, Alice: alice, Bob: bob,
		OwnerKey: alice.Key, BoundPAT: h.BoundPAT(alice, demo.Slug, git), PathPAT: h.PAT(alice, git, "**"),
		MemberKey: bob.Key, Admin: h.AdminToken,
	}
}
