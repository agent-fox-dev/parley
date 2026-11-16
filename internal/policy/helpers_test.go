package policy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// world is a hub with the policy composed: the org o1, whose members are
// dana and mel, and the outsider oz, each with an API key; dana's ready
// workspaces api and web of o1, whose default branch is main; dana's task
// fork api--f1 and mel's task fork api--m1 of api, each with a PAT of its
// owner bound to it with changes:read. The workspaces are records only: no
// repository is on disk.
type world struct {
	*hubtest.Hub
	events                 *event.Component
	org                    string
	dana, mel, oz          string
	danaKey, melKey, ozKey string
	// danaF1 and melM1 are the PATs bound to api--f1 and api--m1.
	danaF1, melM1 string
}

// newWorld starts the world's hub with adjust run on the policy
// component's dependencies.
func newWorld(t *testing.T, adjust ...func(*policy.Deps)) *world {
	t.Helper()
	w := &world{}
	w.Hub = hubtest.StartWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
		w.events = c.Events()
		for _, fn := range adjust {
			c.AdjustPolicy(fn)
		}
	}})
	w.org = w.CreateOrg(t, "o1")
	for _, u := range []struct {
		name    string
		id, key *string
		member  bool
	}{{"dana", &w.dana, &w.danaKey, true}, {"mel", &w.mel, &w.melKey, true}, {"oz", &w.oz, &w.ozKey, false}} {
		*u.id = w.CreateUser(t, u.name)
		*u.key = w.IssueKey(t, *u.id)
		if u.member {
			w.AddMember(t, w.org, *u.id)
		}
	}
	w.addWorkspace(t, "api", "")
	w.addWorkspace(t, "web", "")
	w.addWorkspace(t, "api--f1", w.dana)
	w.addWorkspace(t, "api--m1", w.mel)
	w.danaF1 = w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"changes:read"}, Workspace: "api--f1"})
	w.melM1 = w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:read"}, Workspace: "api--m1"})
	return w
}

// addWorkspace stores the ready record of slug in o1: dana's repository
// workspace for owner "", else a task fork of api owned by owner.
func (w *world) addWorkspace(t *testing.T, slug, owner string) {
	t.Helper()
	ctx, now := context.Background(), time.Now().UTC()
	rec := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
		OwnerID: w.dana, OrgID: w.org, Status: workspace.StatusReady, DisplayName: slug,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		if owner == "" {
			rec.GitURL = new("https://origin.test/" + slug + ".git")
			return workspace.Insert(ctx, tx, rec)
		}
		rec.Kind, rec.OwnerID, rec.ParentSlug = access.KindFork, owner, new("api")
		rec.ForkPoint = &workspace.ForkPoint{Ref: "main", SHA: "1111111111111111111111111111111111111111"}
		rec.ExpiresAt, rec.AutoChange = new(now.Add(7*24*time.Hour)), new(false)
		return fork.Insert(ctx, tx, fork.Fork{Workspace: rec})
	})
	if err != nil {
		t.Fatalf("store the workspace %s: %v", slug, err)
	}
}

// setStatus sets the status of slug, which no clone worker acts on once the
// hub has started.
func (w *world) setStatus(t *testing.T, slug string, status workspace.Status) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ? WHERE slug = ?`, status, slug)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// byDana and byAdmin are the writers of a revision dana's API key and the
// admin token write.
func (w *world) byDana() policy.Writer {
	return policy.Writer{ActorType: "api_key", ActorID: new(w.dana), Username: new("dana")}
}

var byAdmin = policy.Writer{ActorType: "admin_token"}

// setPolicy stores raw, validated, as the next revision of slug, written by
// by at at, through the store function the replacement writes with.
func (w *world) setPolicy(t *testing.T, slug, raw string, by policy.Writer, at time.Time) {
	t.Helper()
	doc, err := validate(t, raw)
	if err != nil {
		t.Fatalf("validate %s: %v", raw, err)
	}
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := policy.Replace(ctx, tx, slug, *doc, nil, by, at)
		return err
	}); err != nil {
		t.Fatalf("store a revision of %s: %v", slug, err)
	}
}

// answer is a hub's answer: its status, its headers, its body as sent and
// as decoded JSON.
type answer struct {
	status int
	header http.Header
	body   []byte
	json   map[string]any
}

// errorType is the error_type of an error envelope, "" for any other body.
func (a answer) errorType() string {
	e, _ := a.json["error"].(map[string]any)
	s, _ := e["error_type"].(string)
	return s
}

// get sends GET path to the hub with credential.
func (w *world) get(t *testing.T, path, credential string) answer {
	t.Helper()
	return w.send(t, http.MethodGet, path, credential, nil, nil)
}

// put sends PUT path to the hub with credential and the document doc,
// with If-Match set to ifMatch unless it is "".
func (w *world) put(t *testing.T, path, credential, doc, ifMatch string) answer {
	t.Helper()
	var header http.Header
	if ifMatch != "" {
		header = http.Header{"If-Match": {ifMatch}}
	}
	return w.send(t, http.MethodPut, path, credential, doc, header)
}

// send sends the request to the hub; t.Errorf, not t.Fatalf, reports an
// answer that is not JSON, so that it may run outside the test's goroutine.
func (w *world) send(t *testing.T, method, path, credential string, body any, header http.Header) answer {
	t.Helper()
	status, h, raw := w.Exchange(t, method, path, credential, body, header)
	a := answer{status: status, header: h, body: raw}
	if err := json.Unmarshal(raw, &a.json); err != nil {
		t.Errorf("%s %s = %d %s: not a JSON object: %v", method, path, status, raw, err)
	}
	return a
}

// updates are the parley.policy.update events of the workspace slug, in
// log order.
func (w *world) updates(t *testing.T, slug string) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Record
	for _, r := range recs {
		if r.Type == "parley.policy.update" && r.Workspace != nil && *r.Workspace == slug {
			out = append(out, r)
		}
	}
	return out
}

// asJSON is v encoded and decoded as JSON, for comparing with a decoded
// body.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}
