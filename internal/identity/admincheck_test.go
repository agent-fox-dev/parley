package identity_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// adminRoute is a route behind the admin check. In its path, ":user"
// stands for a user selector and ":org" for an org selector; body is a body
// the route accepts, nil for a route that takes none.
type adminRoute struct {
	method, path string
	body         any
}

// adminRoutes are every admin route of the identity service.
var adminRoutes = []adminRoute{
	{http.MethodPost, "/api/v1/users", newUserBody("zed")},
}

// newUserBody is a POST /users body for a user no team hub has.
func newUserBody(username string) map[string]any {
	return map[string]any{"username": username, "email": username + "@example.com", "provider": "github",
		"provider_id": username + "-id"}
}

// adminTargets name what an admin route's selectors point at: alice and
// her personal org, which exist, or a user and an org that do not.
var adminTargets = map[string]*strings.Replacer{
	"existing target": strings.NewReplacer(":user", "alice", ":org", "alice"),
	"missing target":  strings.NewReplacer(":user", "nobody", ":org", "nope"),
}

func TestAdminCheck_RefusesEveryCallerButAnAdminBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	callers := map[string]string{"API key of a user": h.ku, "PAT of a user": h.pu, "PAT of an admin": h.pa}
	for _, rt := range adminRoutes {
		bodies := map[string]any{"no body": nil}
		if rt.method != http.MethodGet && rt.method != http.MethodDelete {
			bodies = map[string]any{"empty object": map[string]any{}}
			if rt.body != nil {
				bodies["valid body"] = rt.body
			}
		}
		for target, names := range adminTargets {
			path := names.Replace(rt.path)
			for bodyName, body := range bodies {
				for callerName, caller := range callers {
					t.Run(fmt.Sprintf("%s %s/%s/%s/%s", rt.method, rt.path, target, bodyName, callerName), func(t *testing.T) {
						before := h.snapshot()
						r := h.call(rt.method, path, caller, body)
						checkRefusal(t, "answer", r, http.StatusForbidden, "forbidden", "this route needs an admin")
						if h.snapshot() != before {
							t.Errorf("the store changed")
						}
					})
				}
			}
		}
	}
}

func TestAdminCheck_PassesTheAdminTokenAndAnAdminsAPIKeyByTheRoleOfTheMoment(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	n := 0
	create := func(caller string) response {
		n++
		return h.call(http.MethodPost, "/api/v1/users", caller, newUserBody(fmt.Sprintf("probe%d", n)))
	}
	for name, caller := range map[string]string{"admin token": h.admin, "API key of an admin": h.ka} {
		if r := create(caller); r.status != http.StatusCreated {
			t.Errorf("%s: POST /users = %d %s, want 201", name, r.status, r.body)
		}
	}
	h.setRole(h.root.ID, identity.RoleUser)
	checkRefusal(t, "API key of a demoted admin", create(h.ka), http.StatusForbidden, "forbidden", "this route needs an admin")
	if r := create(h.admin); r.status != http.StatusCreated {
		t.Errorf("admin token after the demotion: POST /users = %d %s, want 201", r.status, r.body)
	}
	h.setRole(h.alice.ID, identity.RoleAdmin)
	if r := create(h.ku); r.status != http.StatusCreated {
		t.Errorf("API key of a promoted user: POST /users = %d %s, want 201", r.status, r.body)
	}
}
