package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// identityHub is a hub bootstrapped with an admin token.
type identityHub struct{ *hubtest.Hub }

// startIdentityHub starts a hub bootstrapped with an admin token. The client
// runs with an empty configuration directory, so every command takes the
// hub URL and the credential from its flags.
func startIdentityHub(t *testing.T) *identityHub {
	t.Helper()
	h := &identityHub{hubtest.Start(t, nil)}
	clientEnv(t, t.TempDir())
	return h
}

// issueKey issues an API key for the user username, as a login does.
func (h *identityHub) issueKey(t *testing.T, username string) string {
	t.Helper()
	u, err := identity.UserBySelector(context.Background(), h.Store.Main(), username)
	if err != nil {
		t.Fatalf("read user %s: %v", username, err)
	}
	return h.IssueKey(t, u.ID)
}

// ran is what one run of parley did.
type ran struct {
	code           int
	stdout, stderr string
}

// json decodes stdout, which must be one JSON object.
func (r ran) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("stdout is not one JSON object (%v): %q; stderr = %q", err, r.stdout, r.stderr)
	}
	return v
}

// errorType is the error_type of the envelope on stderr.
func (r ran) errorType(t *testing.T) any {
	t.Helper()
	var v map[string]map[string]any
	if err := json.Unmarshal([]byte(r.stderr), &v); err != nil {
		t.Fatalf("stderr is not one error envelope (%v): %q", err, r.stderr)
	}
	return v["error"]["error_type"]
}

// cli runs parley against the hub with credential and args.
func (h *identityHub) cli(t *testing.T, credential string, args ...string) ran {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append([]string{"--endpoint-url", h.URL(""), "--api-key", credential}, args...)
	code := cli.Run(context.Background(), args, &stdout, &stderr)
	return ran{code, stdout.String(), stderr.String()}
}

// must runs parley and fails the test unless it exits 0.
func (h *identityHub) must(t *testing.T, credential string, args ...string) ran {
	t.Helper()
	r := h.cli(t, credential, args...)
	if r.code != 0 {
		t.Fatalf("parley %v exited %d; stderr = %q", args, r.code, r.stderr)
	}
	return r
}

// itemValues are the values of member in the items of the list on stdout.
func itemValues(t *testing.T, r ran, member string) []any {
	t.Helper()
	items, _ := r.json(t)["items"].([]any)
	values := []any{}
	for _, it := range items {
		item, _ := it.(map[string]any)
		values = append(values, item[member])
	}
	return values
}

func TestIdentity_AnOperatorSetsUpATeamAndAMemberSeesTheirOrg(t *testing.T) {
	h := startIdentityHub(t)
	admin := h.Token

	for _, u := range []string{"alice", "bob"} {
		h.must(t, admin, "admin", "users", "create", "--username", u, "--email", u+"@example.com",
			"--provider", "github", "--provider-id", u)
	}
	h.must(t, admin, "admin", "orgs", "create", "--name", "Acme Inc", "--slug", "acme")
	if r := h.must(t, admin, "admin", "orgs", "members", "add", "acme", "alice"); r.stdout != "{}\n" {
		t.Errorf("members add printed %q, want {}", r.stdout)
	}
	ka, kb := h.issueKey(t, "alice"), h.issueKey(t, "bob")

	if got := itemValues(t, h.must(t, ka, "orgs", "list"), "slug"); !slices.Equal(got, []any{"acme", "alice"}) {
		t.Errorf("alice's orgs = %v, want [acme alice]", got)
	}
	if got := itemValues(t, h.must(t, ka, "orgs", "members", "acme"), "username"); !slices.Equal(got, []any{"alice"}) {
		t.Errorf("acme's members as alice = %v, want [alice]", got)
	}
	if r := h.cli(t, kb, "orgs", "show", "acme"); r.code != 1 || r.stdout != "" || r.errorType(t) != "not_found" {
		t.Errorf("orgs show acme as bob = %+v, want exit 1 with not_found", r)
	}

	h.must(t, admin, "admin", "orgs", "block", "acme")
	if got := itemValues(t, h.must(t, ka, "orgs", "list"), "slug"); !slices.Equal(got, []any{"alice"}) {
		t.Errorf("alice's orgs after the block = %v, want [alice]", got)
	}

	if r := h.must(t, admin, "admin", "users", "delete", "alice"); r.stdout != "{}\n" {
		t.Errorf("users delete printed %q, want {}", r.stdout)
	}
	if r := h.cli(t, admin, "admin", "users", "show", "alice"); r.code != 1 || r.errorType(t) != "not_found" {
		t.Errorf("users show alice after the delete = %+v, want exit 1 with not_found", r)
	}
	if r := h.cli(t, ka, "user", "show"); r.code != 1 || r.errorType(t) != "unauthenticated" {
		t.Errorf("alice's key after the delete = %+v, want exit 1 with unauthenticated", r)
	}
	if got := itemValues(t, h.must(t, admin, "admin", "orgs", "members", "list", "acme"), "username"); len(got) != 0 {
		t.Errorf("acme's members after the delete = %v, want none", got)
	}
	if org := h.must(t, admin, "orgs", "show", "alice").json(t); org["owner_id"] != nil || org["slug"] != "alice" {
		t.Errorf("alice's personal org after the delete = %v, want it kept with owner_id null", org)
	}
}

func TestIdentity_TheLastActiveAdminCanBeNeitherDemotedNorDeleted(t *testing.T) {
	h := startIdentityHub(t)
	admin := h.Token
	h.CreateUser(t, "alice")
	h.CreateUser(t, "bob")

	if u := h.must(t, admin, "admin", "users", "promote", "alice").json(t); u["role"] != "admin" {
		t.Fatalf("promote alice printed %v, want role admin", u)
	}
	for _, verb := range []string{"demote", "delete"} {
		r := h.cli(t, admin, "admin", "users", verb, "alice")
		if r.code != 1 || r.stdout != "" || r.errorType(t) != "conflict" {
			t.Errorf("%s of the only active admin = %+v, want exit 1, no stdout and conflict", verb, r)
		}
	}

	h.must(t, admin, "admin", "users", "promote", "bob")
	if u := h.must(t, admin, "admin", "users", "demote", "alice").json(t); u["role"] != "user" || u["username"] != "alice" {
		t.Errorf("demote alice beside admin bob printed %v, want alice with role user", u)
	}

	list := h.must(t, admin, "admin", "users", "list")
	names, roles, statuses := itemValues(t, list, "username"), itemValues(t, list, "role"), itemValues(t, list, "status")
	got := map[any]string{}
	for i, name := range names {
		got[name] = fmt.Sprint(roles[i], " ", statuses[i])
	}
	if want := map[any]string{"alice": "user active", "bob": "admin active"}; !maps.Equal(got, want) {
		t.Errorf("admin users list = %v, want %v", got, want)
	}
}
