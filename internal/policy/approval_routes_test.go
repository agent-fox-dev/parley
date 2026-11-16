package policy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The types of the events a verdict records.
const (
	eventApprove = "parley.change.approve"
	eventReject  = "parley.change.reject"
)

// wholeSecondsUTC is a time as the routes write it.
var wholeSecondsUTC = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// verdictEvents are the approve and reject events after seq.
func (w *world) verdictEvents(t *testing.T, seq int64) []event.Record {
	t.Helper()
	return append(w.eventsAfter(t, seq, eventApprove), w.eventsAfter(t, seq, eventReject)...)
}

func TestPostApproval_StoresTheVerdictOnTheNamedOrCurrentVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	melPAT := w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:approve"}})
	changes := []struct {
		state  string
		number int64
	}{
		{"open", w.openPushed(t, "open").number},
		{"draft", w.openPushed(t, "draft", func(in *change.OpenInput) { in.Draft = true }).number},
	}
	callers := []struct {
		name, credential, actorType, id, username string
	}{
		{"dana's API key", w.danaKey, "api_key", w.dana, "dana"},
		{"mel's API key", w.melKey, "api_key", w.mel, "mel"},
		{"mel's PAT with changes:approve", melPAT, "pat", w.mel, "mel"},
	}
	long := strings.Repeat("é", 16000)
	bodies := []struct {
		name string
		body map[string]any
	}{
		{"approve", map[string]any{"verdict": "approve"}},
		{"reject with a comment", map[string]any{"verdict": "reject", "comment": "no tests"}},
		{"approve on version 1", map[string]any{"verdict": "approve", "version": 1}},
		{"approve with a comment of 16000 characters", map[string]any{"verdict": "approve", "comment": long}},
	}
	for _, ch := range changes {
		for _, c := range callers {
			for _, b := range bodies {
				t.Run(ch.state+", "+c.name+", "+b.name, func(t *testing.T) {
					w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
						_, err := tx.ExecContext(ctx, `DELETE FROM change_approvals`)
						return err
					})
					r := w.postVerdict(t, ch.number, c.credential, b.body)
					if r.status != http.StatusCreated {
						t.Fatalf("= %d %s, want 201", r.status, r.body)
					}
					a := r.json
					id, _ := a["id"].(string)
					if _, err := uuid.Parse(id); err != nil {
						t.Errorf("id %q is not a UUID: %v", id, err)
					}
					comment, _ := b.body["comment"].(string)
					want := map[string]any{
						"change": float64(ch.number), "version": 1.0, "verdict": b.body["verdict"], "comment": comment,
						"by":           map[string]any{"actor_type": c.actorType, "actor_id": c.id, "username": c.username},
						"carried_from": nil,
					}
					for k, v := range want {
						if !reflect.DeepEqual(a[k], v) {
							t.Errorf("%s = %v, want %v", k, a[k], v)
						}
					}
					if _, ok := a["counts_for"].([]any); !ok {
						t.Errorf("counts_for = %v, want an array", a["counts_for"])
					}
					if at, _ := a["created_at"].(string); !wholeSecondsUTC.MatchString(at) {
						t.Errorf("created_at = %v, want RFC 3339 in UTC with whole seconds", a["created_at"])
					}
					if ids := w.verdictIDs(t, ch.number, 1, c.id); !reflect.DeepEqual(ids, []string{id}) {
						t.Errorf("verdicts of %s on version 1 = %v, want [%s]", c.username, ids, id)
					}
				})
			}
		}
	}
}

func TestPostApproval_ReplacesTheUsersVerdictOnTheVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	n := w.openPushed(t, "work").number
	melPAT := w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:approve"}})
	first := w.postVerdict(t, n, w.melKey, map[string]any{"verdict": "approve"})
	if first.status != http.StatusCreated {
		t.Fatalf("first verdict = %d %s, want 201", first.status, first.body)
	}
	second := w.postVerdict(t, n, melPAT, map[string]any{"verdict": "reject", "comment": "broke the build"})
	if second.status != http.StatusOK {
		t.Fatalf("second verdict = %d %s, want 200", second.status, second.body)
	}
	if second.json["id"] == first.json["id"] || second.json["verdict"] != "reject" ||
		second.json["comment"] != "broke the build" || second.json["carried_from"] != nil {
		t.Errorf("second verdict = %s, want a new id, reject, its comment and carried_from null (first %s)", second.body, first.body)
	}
	if ids := w.listedIDs(t, n, w.danaKey); !reflect.DeepEqual(ids, []string{second.json["id"].(string)}) {
		t.Errorf("listed verdicts = %v, want only %v", ids, second.json["id"])
	}
	if r := w.withdraw(t, n, first.json["id"].(string), w.melKey); r.status != http.StatusNotFound || r.errorType() != "not_found" {
		t.Errorf("withdraw the replaced verdict = %d %s, want 404 not_found", r.status, r.body)
	}
}

func TestPostApproval_RefusesAndStoresNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	n := w.openPushed(t, "work").number
	forked := w.openTouching(t, "forked", "f.txt", func(in *change.OpenInput) { in.SourceWorkspace = "api--f1" })
	melNoScope := w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:read", "changes:write"}})
	boundToFork := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"changes:approve"}, Workspace: "api--f1"})
	approve := map[string]any{"verdict": "approve"}
	path := func(slug, number string) string {
		return "/api/v1/workspaces/" + slug + "/changes/" + number + "/approvals"
	}
	at1 := path("api", "1")
	cases := []struct {
		name, path, credential string
		body                   any
		status                 int
		errorType              string
	}{
		{"admin token, valid body", at1, w.Token, approve, http.StatusForbidden, "forbidden"},
		{"admin token, invalid body", at1, w.Token, map[string]any{"verdict": "nope"}, http.StatusForbidden, "forbidden"},

		{"PAT without changes:approve", at1, melNoScope, approve, http.StatusForbidden, "insufficient_scope"},
		{"PAT without changes:approve, unknown workspace", path("nosuch", "1"), melNoScope, approve, http.StatusForbidden, "insufficient_scope"},
		{"outsider", at1, w.ozKey, approve, http.StatusNotFound, "not_found"},
		{"outsider, unknown workspace", path("nosuch", "1"), w.ozKey, approve, http.StatusNotFound, "not_found"},
		{"token bound to a fork, on its parent, for the fork's change", approvalsPath(forked), boundToFork, approve, http.StatusForbidden, "token_bound"},

		{"no verdict", at1, w.danaKey, map[string]any{}, http.StatusBadRequest, "invalid_request"},
		{"unknown verdict", at1, w.danaKey, map[string]any{"verdict": "maybe"}, http.StatusBadRequest, "invalid_request"},
		{"comment of 16001 characters", at1, w.danaKey, map[string]any{"verdict": "approve", "comment": strings.Repeat("é", 16001)}, http.StatusBadRequest, "invalid_request"},
		{"comment not a string", at1, w.danaKey, map[string]any{"verdict": "approve", "comment": nil}, http.StatusBadRequest, "invalid_request"},
		{"version 0", at1, w.danaKey, map[string]any{"verdict": "approve", "version": 0}, http.StatusBadRequest, "invalid_request"},
		{"version -1", at1, w.danaKey, map[string]any{"verdict": "approve", "version": -1}, http.StatusBadRequest, "invalid_request"},
		{"version not a number", at1, w.danaKey, map[string]any{"verdict": "approve", "version": "two"}, http.StatusBadRequest, "invalid_request"},
		{"version null", at1, w.danaKey, map[string]any{"verdict": "approve", "version": nil}, http.StatusBadRequest, "invalid_request"},
		{"version not an integer", at1, w.danaKey, map[string]any{"verdict": "approve", "version": 1.5}, http.StatusBadRequest, "invalid_request"},
		{"unknown member", at1, w.danaKey, map[string]any{"verdict": "approve", "extra": 1}, http.StatusBadRequest, "invalid_request"},
		{"not an object", at1, w.danaKey, []any{"approve"}, http.StatusBadRequest, "invalid_request"},

		{"unknown change", path("api", "99"), w.danaKey, approve, http.StatusNotFound, "change_not_found"},
		{"number not a number", path("api", "abc"), w.danaKey, approve, http.StatusNotFound, "change_not_found"},
		{"number 0", path("api", "0"), w.danaKey, approve, http.StatusNotFound, "change_not_found"},
		{"unknown change with an invalid body", path("api", "99"), w.danaKey, map[string]any{"verdict": "maybe"}, http.StatusNotFound, "change_not_found"},
		{"version the change does not have", at1, w.danaKey, map[string]any{"verdict": "approve", "version": 7}, http.StatusNotFound, "not_found"},
	}
	if n != 1 {
		t.Fatalf("the change is number %d, want 1", n)
	}
	seq := w.seq(t)
	bodies := map[string][]byte{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := w.send(t, http.MethodPost, tc.path, tc.credential, tc.body, nil)
			if r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
			}
			bodies[tc.name] = r.body
			if got := w.count(t, "change_approvals"); got != 0 {
				t.Errorf("%d verdicts stored, want none", got)
			}
			if evs := w.verdictEvents(t, seq); len(evs) != 0 {
				t.Errorf("%d verdict events written, want none", len(evs))
			}
		})
	}
	if a, b := bodies["outsider"], bodies["outsider, unknown workspace"]; string(a) != string(b) {
		t.Errorf("the outsider's refusal %s differs from that of an unknown workspace %s", a, b)
	}
}

func TestPostApproval_RefusesAClosedChangeOrAnArchivedWorkspace(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	landed, abandoned, open := w.openPushed(t, "landed").number, w.openPushed(t, "abandoned").number, w.openPushed(t, "open").number
	if r := w.postVerdict(t, landed, w.melKey, map[string]any{"verdict": "approve"}); r.status != http.StatusCreated {
		t.Fatalf("mel's verdict = %d %s, want 201", r.status, r.body)
	}
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetLanded(ctx, tx, "api", landed, change.Landed{SHA: w.base, At: time.Now(),
			By: change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(w.dana), Username: new("dana")}, LandID: "l1"}); err != nil {
			return err
		}
		return w.changes.Abandon(ctx, tx, "api", abandoned, "undone", w.danaCaller())
	})
	approve := map[string]any{"verdict": "approve"}
	archive := func() { w.setStatus(t, "api", workspace.StatusArchived) }
	cases := []struct {
		name      string
		before    func()
		number    int64
		body      any
		status    int
		errorType string
	}{
		{"landed change", nil, landed, approve, http.StatusConflict, "change_closed"},
		{"abandoned change", nil, abandoned, approve, http.StatusConflict, "change_closed"},
		{"landed change with an invalid body", nil, landed, map[string]any{"verdict": "maybe"}, http.StatusBadRequest, "invalid_request"},
		{"open change in an archived workspace", archive, open, approve, http.StatusConflict, "workspace_archived"},
		{"landed change in an archived workspace", nil, landed, approve, http.StatusConflict, "workspace_archived"},
	}
	seq := w.seq(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before()
			}
			if r := w.postVerdict(t, tc.number, w.danaKey, tc.body); r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
			}
			if got := w.count(t, "change_approvals"); got != 1 {
				t.Errorf("%d verdicts stored, want mel's one as before", got)
			}
			if evs := w.verdictEvents(t, seq); len(evs) != 0 {
				t.Errorf("%d verdict events written, want none", len(evs))
			}
		})
	}
}

// txProbe passes the policy component's events on and records, for each
// verdict event, whether the verdict it names is visible through the
// transaction the event is written in, and for each check event the status
// and updated_at, in seconds, of the result it names as that transaction
// reads them.
type txProbe struct {
	policy.EventWriter
	mu      sync.Mutex
	visible map[string]bool
	results [][2]any
}

func (p *txProbe) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	if in.Type == eventCheck {
		raw, err := json.Marshal(in.Metadata)
		if err != nil {
			return event.Record{}, err
		}
		var meta struct {
			Number, Version int64
			Name            string
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return event.Record{}, err
		}
		var (
			status  string
			updated int64
		)
		if err := tx.QueryRowContext(ctx, `SELECT status, updated_at FROM change_checks
WHERE workspace_slug = ? AND number = ? AND version = ? AND name = ?`, *in.Workspace, meta.Number, meta.Version, meta.Name).
			Scan(&status, &updated); err != nil {
			return event.Record{}, err
		}
		p.mu.Lock()
		p.results = append(p.results, [2]any{status, updated / 1e9})
		p.mu.Unlock()
	}
	if in.Type == eventApprove || in.Type == eventReject {
		raw, err := json.Marshal(in.Metadata)
		if err != nil {
			return event.Record{}, err
		}
		var meta struct {
			ApprovalID string `json:"approval_id"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return event.Record{}, err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM change_approvals WHERE id = ?`, meta.ApprovalID).Scan(&n); err != nil {
			return event.Record{}, err
		}
		p.mu.Lock()
		p.visible[meta.ApprovalID] = n == 1
		p.mu.Unlock()
	}
	return p.EventWriter.Emit(ctx, tx, in)
}

func TestPostApproval_WritesOneEventInTheVerdictsTransaction(t *testing.T) {
	t.Parallel()
	probe := &txProbe{visible: map[string]bool{}}
	w := newWorld(t, func(d *policy.Deps) {
		probe.EventWriter = d.Events
		d.Events = probe
	})
	n := w.openPushed(t, "work").number
	melPAT := w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:approve"}})
	cases := []struct {
		name, credential, verdict, eventType, actorType, actorID string
	}{
		{"approve by dana's API key", w.danaKey, "approve", eventApprove, "api_key", w.dana},
		{"reject by mel's PAT", melPAT, "reject", eventReject, "pat", w.mel},
		{"replacement by dana", w.danaKey, "reject", eventReject, "api_key", w.dana},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := w.seq(t)
			r := w.postVerdict(t, n, tc.credential, map[string]any{"verdict": tc.verdict})
			if r.status != http.StatusCreated && r.status != http.StatusOK {
				t.Fatalf("= %d %s, want the approval", r.status, r.body)
			}
			evs := w.verdictEvents(t, seq)
			if len(evs) != 1 {
				t.Fatalf("%d verdict events, want 1", len(evs))
			}
			e := evs[0]
			if e.Type != tc.eventType || e.Workspace == nil || *e.Workspace != "api" || e.ResourceType != "change" ||
				e.ResourceID != "1" || e.Action != tc.verdict || e.ActorType != tc.actorType ||
				e.ActorID == nil || *e.ActorID != tc.actorID || e.SessionID != nil {
				t.Errorf("event = %+v, want %s of api change 1 by %s %s with no session", e, tc.eventType, tc.actorType, tc.actorID)
			}
			var meta map[string]any
			if err := json.Unmarshal(e.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"version": 1.0, "approval_id": r.json["id"], "counts_for": r.json["counts_for"],
				"number": 1.0, "ref": change.Ref("api", 1)}
			if !reflect.DeepEqual(meta, want) {
				t.Errorf("metadata = %v, want %v", meta, want)
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if id, _ := r.json["id"].(string); !probe.visible[id] {
				t.Errorf("the event of %s was not written in the transaction that stored the verdict", id)
			}
		})
	}
}

func TestListApprovals_ListsTheVerdictsNewestFirst(t *testing.T) {
	t.Parallel()
	clock := &stepClock{at: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	w := newWorld(t, func(d *policy.Deps) { d.Now = clock.now })
	n := w.openPushed(t, "work").number
	quiet := w.openPushed(t, "quiet").number
	melRead := w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:read"}})
	give := func(credential, verdict string) any {
		t.Helper()
		clock.tick()
		r := w.postVerdict(t, n, credential, map[string]any{"verdict": verdict})
		if r.status != http.StatusCreated {
			t.Fatalf("%s = %d %s, want 201", verdict, r.status, r.body)
		}
		return r.json
	}
	melOn1 := give(w.melKey, "approve")
	// Version 2 edits the file version 1 added, so it is not patch-equivalent.
	head := commit(t, w.api, "work", "work", map[string]string{"work.txt": "changed\n"})
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := w.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: "api", Branch: "work", Head: head,
			Kind: change.KindCommit, Caller: w.danaCaller()})
		return err
	})
	danaOn2 := give(w.danaKey, "reject")
	melOn2 := give(w.melKey, "approve")
	if cf := danaOn2.(map[string]any)["counts_for"]; !reflect.DeepEqual(cf, []any{"default"}) {
		t.Fatalf("dana's verdict counts for %v, want [default]", cf)
	}

	r := w.get(t, approvalsPath(n), melRead)
	items, _ := r.json["items"].([]any)
	if r.status != http.StatusOK || !reflect.DeepEqual(items, []any{melOn2, danaOn2, melOn1}) || r.json["next_cursor"] != nil {
		t.Fatalf("list = %d %s, want mel's verdict on 2, dana's on 2, mel's on 1 as recorded, no next cursor", r.status, r.body)
	}
	for _, item := range items {
		if len(item.(map[string]any)) != 9 {
			t.Errorf("item %v has %d members, want the 9 of an approval", item, len(item.(map[string]any)))
		}
	}
	for _, tc := range []struct {
		name, query string
		want        []any
		more        bool
	}{
		{"version 1", "?version=1", []any{melOn1}, false},
		{"version 2", "?version=2", []any{melOn2, danaOn2}, false},
		{"a page of one", "?limit=1", []any{melOn2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := w.get(t, approvalsPath(n)+tc.query, melRead)
			if r.status != http.StatusOK || !reflect.DeepEqual(r.json["items"], tc.want) || (r.json["next_cursor"] != nil) != tc.more {
				t.Errorf("= %d %s, want %v with a next cursor %v", r.status, r.body, tc.want, tc.more)
			}
		})
	}
	first := w.get(t, approvalsPath(n)+"?limit=1", melRead)
	cursor, _ := first.json["next_cursor"].(string)
	next := w.get(t, approvalsPath(n)+"?limit=1&cursor="+url.QueryEscape(cursor), melRead)
	if items, _ := next.json["items"].([]any); next.status != http.StatusOK || len(items) != 1 || !reflect.DeepEqual(items[0], danaOn2) {
		t.Errorf("second page of one = %d %s, want dana's verdict", next.status, next.body)
	}
	if r := w.get(t, approvalsPath(quiet), melRead); r.status != http.StatusOK || !reflect.DeepEqual(r.json["items"], []any{}) {
		t.Errorf("list of a change with no verdict = %d %s, want items []", r.status, r.body)
	}
}

func TestDeleteApproval_WithdrawsOnlyThatVerdict(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	n := w.openPushed(t, "work").number
	melApprove := w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:approve"}})
	dana := w.postVerdict(t, n, w.danaKey, map[string]any{"verdict": "approve"})
	if dana.status != http.StatusCreated {
		t.Fatalf("dana's verdict = %d %s, want 201", dana.status, dana.body)
	}
	for _, tc := range []struct{ name, credential string }{
		{"mel's API key, which gave it", w.melKey},
		{"mel's PAT, which did not give it", melApprove},
		{"dana's API key, the owner's", w.danaKey},
		{"the admin token", w.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := w.postVerdict(t, n, w.melKey, map[string]any{"verdict": "approve"})
			if v.status != http.StatusCreated {
				t.Fatalf("mel's verdict = %d %s, want 201", v.status, v.body)
			}
			id, seq := v.json["id"].(string), w.seq(t)
			if r := w.withdraw(t, n, id, tc.credential); r.status != http.StatusNoContent || len(r.body) != 0 {
				t.Fatalf("withdraw = %d %q, want 204 with no body", r.status, r.body)
			}
			if ids := w.listedIDs(t, n, w.danaKey); !reflect.DeepEqual(ids, []string{dana.json["id"].(string)}) {
				t.Errorf("listed verdicts = %v, want dana's %v alone", ids, dana.json["id"])
			}
			if evs := w.verdictEvents(t, seq); len(evs) != 0 {
				t.Errorf("%d verdict events written, want none", len(evs))
			}
			if r := w.withdraw(t, n, id, tc.credential); r.status != http.StatusNotFound || r.errorType() != "not_found" {
				t.Errorf("withdraw again = %d %s, want 404 not_found", r.status, r.body)
			}
		})
	}
}

func TestDeleteApproval_RefusesACallerWhoMayNotWithdraw(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	n := w.openPushed(t, "work").number
	dave := w.CreateUser(t, "dave")
	w.AddMember(t, w.org, dave)
	v := w.postVerdict(t, n, w.danaKey, map[string]any{"verdict": "approve"})
	if v.status != http.StatusCreated {
		t.Fatalf("dana's verdict = %d %s, want 201", v.status, v.body)
	}
	id := v.json["id"].(string)
	for _, tc := range []struct {
		name, credential string
		status           int
		errorType        string
	}{
		{"mel, a member", w.melKey, http.StatusForbidden, "forbidden"},
		{"dave, another member", w.IssueKey(t, dave), http.StatusForbidden, "forbidden"},
		{"dana's PAT without changes:approve", w.IssuePAT(t, identity.PATSpec{UserID: w.dana,
			Permissions: []string{"changes:read", "changes:write"}}), http.StatusForbidden, "insufficient_scope"},
		{"oz, an outsider", w.ozKey, http.StatusNotFound, "not_found"},
		{"dana's PAT bound to her fork", w.IssuePAT(t, identity.PATSpec{UserID: w.dana,
			Permissions: []string{"changes:approve"}, Workspace: "api--f1"}), http.StatusForbidden, "token_bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := w.withdraw(t, n, id, tc.credential); r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
			}
			if ids := w.listedIDs(t, n, w.danaKey); !slices.Contains(ids, id) {
				t.Errorf("listed verdicts = %v, want dana's %s still among them", ids, id)
			}
		})
	}
}

func TestDeleteApproval_RefusesAndRemovesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ids := map[int64]string{}
	numbers := map[string]int64{}
	for _, b := range []string{"one", "two", "landed", "abandoned"} {
		numbers[b] = w.openPushed(t, b).number
		r := w.postVerdict(t, numbers[b], w.melKey, map[string]any{"verdict": "approve"})
		if r.status != http.StatusCreated {
			t.Fatalf("mel's verdict on %s = %d %s, want 201", b, r.status, r.body)
		}
		ids[numbers[b]] = r.json["id"].(string)
	}
	one, landed, abandoned := numbers["one"], numbers["landed"], numbers["abandoned"]
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetLanded(ctx, tx, "api", landed, change.Landed{SHA: w.base, At: time.Now(),
			By: change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(w.dana), Username: new("dana")}, LandID: "l1"}); err != nil {
			return err
		}
		return w.changes.Abandon(ctx, tx, "api", abandoned, "undone", w.danaCaller())
	})
	cases := []struct {
		name      string
		before    func()
		number    int64
		id        string
		status    int
		errorType string
	}{
		{"a random UUID", nil, one, uuid.NewV4().String(), http.StatusNotFound, "not_found"},
		{"not a UUID", nil, one, "not-a-uuid", http.StatusNotFound, "not_found"},
		{"a verdict of another change", nil, one, ids[numbers["two"]], http.StatusNotFound, "not_found"},
		{"a landed change", nil, landed, ids[landed], http.StatusConflict, "change_closed"},
		{"an abandoned change", nil, abandoned, ids[abandoned], http.StatusConflict, "change_closed"},
		{"an unknown change", nil, 99, ids[one], http.StatusNotFound, "change_not_found"},
		{"an archived workspace", func() { w.setStatus(t, "api", workspace.StatusArchived) }, one, ids[one],
			http.StatusConflict, "workspace_archived"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before()
			}
			if r := w.withdraw(t, tc.number, tc.id, w.danaKey); r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
			}
			if got := w.count(t, "change_approvals"); got != len(ids) {
				t.Errorf("%d verdicts stored, want the %d as before", got, len(ids))
			}
		})
	}
}

// evidenceLists are the routes that list the evidence on a change: its
// verdicts and its check results.
var evidenceLists = []string{"approvals", "checks"}

// listPath is the path of the list route of the change number of api.
func listPath(number, route string) string {
	return "/api/v1/workspaces/api/changes/" + number + "/" + route
}

func TestEvidenceLists_RefuseABadVersionAnUnknownChangeAndACallerWithoutAccess(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	if n := w.openPushed(t, "work").number; n != 1 {
		t.Fatalf("the change is number %d, want 1", n)
	}
	noRead := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"changes:approve", "checks:write"}})
	for _, route := range evidenceLists {
		for _, tc := range []struct {
			name, number, query, credential string
			status                          int
			errorType                       string
		}{
			{"version 0", "1", "?version=0", w.melKey, http.StatusBadRequest, "invalid_request"},
			{"version not a number", "1", "?version=abc", w.melKey, http.StatusBadRequest, "invalid_request"},
			{"version the change does not have", "1", "?version=9", w.melKey, http.StatusNotFound, "not_found"},
			{"unknown change", "99", "", w.melKey, http.StatusNotFound, "change_not_found"},
			{"PAT without changes:read", "1", "", noRead, http.StatusForbidden, "insufficient_scope"},
			{"outsider", "1", "", w.ozKey, http.StatusNotFound, "not_found"},
		} {
			t.Run(route+", "+tc.name, func(t *testing.T) {
				if r := w.get(t, listPath(tc.number, route)+tc.query, tc.credential); r.status != tc.status || r.errorType() != tc.errorType {
					t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
				}
			})
		}
	}
}

func TestEvidenceLists_ShowATokenBoundToAForkOnlyTheChangesFromTheFork(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	own := w.openPushed(t, "work").number
	fromFork := w.openTouching(t, "forked", "f.txt", func(in *change.OpenInput) { in.SourceWorkspace = "api--m1" })
	if own != 1 || fromFork != 2 {
		t.Fatalf("the changes are numbers %d and %d, want 1 and 2", own, fromFork)
	}
	w.give(t, fromFork, w.danaKey, approves)
	w.report(t, fromFork, "test", map[string]any{"status": "passed"})
	for _, route := range evidenceLists {
		t.Run(route, func(t *testing.T) {
			want := w.get(t, listPath("2", route), w.danaKey).json["items"]
			if items, _ := want.([]any); len(items) != 1 {
				t.Fatalf("dana's list of change 2 = %v, want one item", want)
			}
			if r := w.get(t, listPath("2", route), w.melM1); r.status != http.StatusOK || !reflect.DeepEqual(r.json["items"], want) {
				t.Errorf("change from the fork = %d %s, want 200 with %v", r.status, r.body, want)
			}
			hidden, unknown := w.get(t, listPath("1", route), w.melM1), w.get(t, listPath("99", route), w.melM1)
			if hidden.status != http.StatusNotFound || hidden.errorType() != "change_not_found" {
				t.Errorf("change from api = %d %s, want 404 change_not_found", hidden.status, hidden.body)
			}
			if string(hidden.body) != string(unknown.body) {
				t.Errorf("change from api answers %s, an unknown change %s; want the same body", hidden.body, unknown.body)
			}
		})
	}
}
