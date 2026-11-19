package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/session"
)

// sessionHub is a hub holding the ready workspace demo and its task fork
// demo--t1, both owned by alice in the organization acme, of which bob is a
// member. Each of them has an API key. Its clock runs ahead of the wall
// clock by ahead.
type sessionHub struct {
	*hubtest.Hub
	comp                         *hub.Composition
	alice, bob, aliceKey, bobKey string
	ahead                        atomic.Int64
}

// startSessionHub starts the hub and stores its users and workspaces.
func startSessionHub(t *testing.T) *sessionHub {
	t.Helper()
	h := &sessionHub{}
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Now:     func() time.Time { return time.Now().Add(time.Duration(h.ahead.Load())) },
		Compose: func(c *hub.Composition) { h.comp = c },
	})
	acme := h.CreateOrg(t, "acme")
	h.alice, h.bob = h.CreateUser(t, "alice"), h.CreateUser(t, "bob")
	h.aliceKey, h.bobKey = h.IssueKey(t, h.alice), h.IssueKey(t, h.bob)
	h.AddMember(t, acme, h.bob)
	h.AddWorkspace(t, access.Record{Slug: "demo", OwnerID: h.alice, OrgID: acme, Kind: access.KindRepository, Status: access.StatusReady})
	h.AddWorkspace(t, access.Record{Slug: "demo--t1", OwnerID: h.alice, OrgID: acme, Kind: access.KindFork, ParentSlug: "demo", Status: access.StatusReady})
	return h
}

// parley runs parley against the hub as alice.
func (h *sessionHub) parley(t *testing.T, args ...string) ran {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--endpoint-url", h.URL(""), "--api-key", h.aliceKey}, args...), &stdout, &stderr)
	return ran{code, stdout.String(), stderr.String()}
}

// answer sends method path with credential and body, fails the test unless
// the hub answers want, and returns the answer's body.
func (h *sessionHub) answer(t *testing.T, credential, method, path string, body any, want int) []byte {
	t.Helper()
	status, got := h.Call(t, method, path, credential, body)
	if status != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, status, got, want)
	}
	return got
}

// open opens a session of alice on demo and returns its id.
func (h *sessionHub) open(t *testing.T) string {
	t.Helper()
	var s sessionView
	decodeInto(t, h.answer(t, h.aliceKey, http.MethodPost, "/api/v1/sessions", map[string]any{"workspace_slug": "demo"}, http.StatusCreated), &s)
	return s.ID
}

// read is the session id as bob reads it, with the body it came in.
func (h *sessionHub) read(t *testing.T, id string) (sessionView, []byte) {
	t.Helper()
	body := h.answer(t, h.bobKey, http.MethodGet, "/api/v1/sessions/"+id, nil, http.StatusOK)
	var s sessionView
	decodeInto(t, body, &s)
	return s, body
}

// sessionView is the part of a session object the flows read.
type sessionView struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	EndedAt      *string `json:"ended_at"`
	DurationMS   *int64  `json:"duration_ms"`
	ErrorMessage *string `json:"error_message"`
	TokenSummary struct {
		TotalInputTokens int64    `json:"total_input_tokens"`
		ModelsUsed       []string `json:"models_used"`
	} `json:"token_summary"`
	Forks   []string            `json:"forks"`
	Changes []session.ChangeRef `json:"changes"`
	Counts  session.Counts      `json:"counts"`
}

// decodeInto decodes the JSON document body into v.
func decodeInto(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("body is not the JSON document expected (%v): %s", err, body)
	}
}

// userPrincipal is a credential of the user userID as the session-linking
// rule takes it.
func userPrincipal(userID string) identity.Principal {
	return identity.Principal{Type: identity.TypeAPIKey, User: &identity.User{ID: userID}}
}

func TestSession_AnAgentOpensReportsAndCompletesASessionFromTheCLI(t *testing.T) {
	parallel(t)
	h := startSessionHub(t)

	started := h.parley(t, "session", "start", "demo", "--model", "m1")
	if started.code != 0 {
		t.Fatalf("session start exited %d, stderr %q; want 0", started.code, started.stderr)
	}
	id := str(started.json(t), "id")
	if status := str(started.json(t), "status"); id == "" || status != "active" {
		t.Fatalf("session start printed %s, want an active session with its id", started.stdout)
	}
	for _, args := range [][]string{
		{"session", "usage", id, "--model", "m1", "--input", "10", "--output", "5"},
		{"session", "complete", id},
	} {
		if r := h.parley(t, args...); r.code != 0 {
			t.Fatalf("%v exited %d, stderr %q; want 0", args, r.code, r.stderr)
		}
	}

	got := h.parley(t, "session", "get", id)
	var s sessionView
	decodeInto(t, []byte(got.stdout), &s)
	if got.code != 0 || s.Status != "completed" || s.EndedAt == nil || s.DurationMS == nil ||
		s.TokenSummary.TotalInputTokens != 10 || !slices.Equal(s.TokenSummary.ModelsUsed, []string{"m1"}) {
		t.Errorf("session get exited %d and printed %s; want a completed session with ended_at, duration_ms, 10 input tokens and models_used [m1]", got.code, got.stdout)
	}
	again := h.parley(t, "session", "complete", id, "--status", "failed")
	if again.code != 0 || str(again.json(t), "status") != "completed" {
		t.Errorf("a second session complete --status failed exited %d and printed %s; want 0 and the session still completed", again.code, again.stdout)
	}
	late := h.parley(t, "session", "usage", id, "--model", "m1")
	if late.code != 1 || late.stdout != "" || errorType([]byte(late.stderr)) != "session_not_active" {
		t.Errorf("session usage after complete exited %d, stdout %q, stderr %q; want 1, nothing and session_not_active", late.code, late.stdout, late.stderr)
	}
}

func TestSession_ADeveloperFollowsTheWorkLinkedToASessionAndItsCost(t *testing.T) {
	parallel(t)
	h := startSessionHub(t)
	ctx := context.Background()
	id := h.open(t)
	var usage struct {
		ReportedAt string `json:"reported_at"`
	}
	decodeInto(t, h.answer(t, h.aliceKey, http.MethodPost, "/api/v1/sessions/"+id+"/usage",
		map[string]any{"model": "m1", "input_tokens": 10}, http.StatusCreated), &usage)
	sessions := h.comp.Sessions()

	if got, err := sessions.Check(ctx, userPrincipal(h.alice), id, "demo--t1"); err != nil || got != id {
		t.Errorf("the linking check for alice on demo--t1 = %q, %v; want %s accepted", got, err, id)
	}
	if _, err := sessions.Check(ctx, userPrincipal(h.bob), id, "demo--t1"); !errors.Is(err, session.ErrUnknownSession) {
		t.Errorf("the linking check for bob on demo--t1 = %v, want the unknown-session error", err)
	}
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := sessions.Record(ctx, tx, session.ForkLink(id, "demo", "demo--t1")); err != nil {
			return err
		}
		return sessions.Record(ctx, tx, session.VersionLink(id, "demo", 1, 1))
	})
	if err != nil {
		t.Fatalf("record the fork and version links: %v", err)
	}
	// The cost report's window ends, exclusive, at the clock's whole
	// second, which must be past the one the usage was reported in.
	h.ahead.Store(int64(2 * time.Second))

	s, body := h.read(t, id)
	if !slices.Equal(s.Forks, []string{"demo--t1"}) || !slices.Equal(s.Changes, []session.ChangeRef{{Workspace: "demo", Number: 1}}) ||
		s.Counts.Forks != 1 || s.Counts.Changes != 1 || s.Counts.Versions != 1 {
		t.Errorf("bob read %s; want forks [demo--t1], changes [demo 1] and counts forks, changes and versions 1", body)
	}
	var cost struct {
		Totals struct {
			InputTokens int64 `json:"input_tokens"`
			Sessions    int64 `json:"sessions"`
		} `json:"totals"`
		Items []struct {
			Key         string `json:"key"`
			InputTokens int64  `json:"input_tokens"`
		} `json:"items"`
	}
	costBody := h.answer(t, h.bobKey, http.MethodGet, "/api/v1/workspaces/demo/cost", nil, http.StatusOK)
	decodeInto(t, costBody, &cost)
	if cost.Totals.InputTokens != 10 || cost.Totals.Sessions != 1 || len(cost.Items) != 1 ||
		len(usage.ReportedAt) < 10 || cost.Items[0].Key != usage.ReportedAt[:10] || cost.Items[0].InputTokens != 10 {
		t.Errorf("bob's cost of demo = %s; want totals of 10 input tokens and 1 session, in one item for the UTC date of %s", costBody, usage.ReportedAt)
	}
	var list struct {
		Items []sessionView `json:"items"`
	}
	listBody := h.answer(t, h.bobKey, http.MethodGet, "/api/v1/sessions?workspace=demo", nil, http.StatusOK)
	decodeInto(t, listBody, &list)
	if !slices.ContainsFunc(list.Items, func(v sessionView) bool { return v.ID == id }) {
		t.Errorf("bob's sessions of demo = %s, want %s among them", listBody, id)
	}
}

func TestSession_ArchivingAWorkspaceTerminatesItsActiveSessions(t *testing.T) {
	parallel(t)
	h := startSessionHub(t)
	ctx := context.Background()
	active, done := h.open(t), h.open(t)
	h.answer(t, h.aliceKey, http.MethodPost, "/api/v1/sessions/"+done+"/complete", map[string]any{"status": "completed"}, http.StatusOK)
	_, doneBefore := h.read(t, done)

	var closed int
	err := h.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		closed, err = h.comp.Sessions().CloseActive(ctx, tx, "demo", "workspace archived", time.Now())
		return err
	})
	if err != nil || closed != 1 {
		t.Fatalf("close the active sessions of demo = %d, %v; want 1", closed, err)
	}

	s, body := h.read(t, active)
	if s.Status != "terminated" || s.ErrorMessage == nil || *s.ErrorMessage != "workspace archived" || s.EndedAt == nil {
		t.Errorf("bob read the active session as %s; want it terminated with workspace archived and ended_at", body)
	}
	if _, doneAfter := h.read(t, done); !bytes.Equal(doneAfter, doneBefore) {
		t.Errorf("the completed session read %s after the close, want it unchanged: %s", doneAfter, doneBefore)
	}
	var log struct {
		Items []struct {
			Type       string `json:"type"`
			ResourceID string `json:"resource_id"`
			Metadata   struct {
				Reason string `json:"reason"`
			} `json:"metadata"`
		} `json:"items"`
	}
	decodeInto(t, h.answer(t, h.Token, http.MethodGet, "/api/v1/events/log?after=0", nil, http.StatusOK), &log)
	var forced []string
	for _, ev := range log.Items {
		if ev.Type == "parley.session.force_closed" {
			forced = append(forced, ev.ResourceID+" "+ev.Metadata.Reason)
		}
	}
	if want := []string{active + " workspace archived"}; !slices.Equal(forced, want) {
		t.Errorf("force_closed events (resource_id reason) = %q, want %q", forced, want)
	}

	if status, body := h.Call(t, http.MethodPost, "/api/v1/sessions/"+active+"/usage", h.aliceKey, map[string]any{"model": "m1"}); status != http.StatusConflict || errorType(body) != "session_not_active" {
		t.Errorf("usage on the terminated session = %d %s, want 409 session_not_active", status, body)
	}
	var completed sessionView
	decodeInto(t, h.answer(t, h.aliceKey, http.MethodPost, "/api/v1/sessions/"+active+"/complete", map[string]any{"status": "failed"}, http.StatusOK), &completed)
	if completed.Status != "terminated" {
		t.Errorf("complete on the terminated session answered status %q, want terminated", completed.Status)
	}
}
