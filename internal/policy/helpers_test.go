package policy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/fork"
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
	org                    string
	dana, mel, oz          string
	danaKey, melKey, ozKey string
	// danaF1 and melM1 are the PATs bound to api--f1 and api--m1.
	danaF1, melM1 string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{Hub: hubtest.StartWith(t, hubtest.Config{})}
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

// archive sets the status of slug to archived.
func (w *world) archive(t *testing.T, slug string) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ? WHERE slug = ?`, workspace.StatusArchived, slug)
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
		cur, err := policy.CurrentRevision(ctx, tx, slug)
		if err != nil {
			return err
		}
		return policy.InsertRevision(ctx, tx, slug, policy.Revision{Number: cur.Number + 1, Document: *doc, UpdatedAt: &at, UpdatedBy: &by})
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
	status, header, body := w.Exchange(t, http.MethodGet, path, credential, nil, nil)
	a := answer{status: status, header: header, body: body}
	if err := json.Unmarshal(body, &a.json); err != nil {
		t.Fatalf("GET %s = %d %s: not a JSON object: %v", path, status, body, err)
	}
	return a
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
