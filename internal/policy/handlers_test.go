package policy_test

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

const policyPath = "/api/v1/workspaces/api/policy"

// eventUpdate is the type of the event a replacement records.
const eventUpdate = "parley.policy.update"

// oneRule and otherRule are two more documents, to tell revisions apart.
const (
	oneRule   = `{"rules": [{"name": "all", "paths": ["**"], "require": {"approvals": 2}}]}`
	otherRule = `{"default": {"checks": ["test"]}, "rules": [{"name": "src", "paths": ["src/**"], "exclude": ["src/gen/**"], "require": {}}]}`
)

func TestGetPolicy(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	w.setPolicy(t, "api", threeRules, w.byDana(), at.Add(1*time.Second+500*time.Millisecond))
	w.setPolicy(t, "api", otherRule, w.byDana(), at.Add(2*time.Second))
	w.setPolicy(t, "api", oneRule, byAdmin, at.Add(3*time.Second))

	t.Run("answers revision 0 while no policy is set and on a fork", func(t *testing.T) {
		want := map[string]any{
			"revision": 0.0,
			"default": map[string]any{
				"approvals": 0.0, "approvers": []any{"owner"}, "checks": []any{}, "fresh_checks": false, "block": false,
			},
			"rules": []any{}, "updated_at": nil, "updated_by": nil,
		}
		for _, path := range []string{
			"/api/v1/workspaces/web/policy",
			"/api/v1/workspaces/web/policy?revision=0",
			"/api/v1/workspaces/api--f1/policy",
			"/api/v1/workspaces/api--f1/policy?revision=0",
		} {
			r := w.get(t, path, w.danaKey)
			if r.status != http.StatusOK || !reflect.DeepEqual(r.json, want) || r.header.Get("ETag") != `"0"` {
				t.Errorf("GET %s = %d ETag %q %s, want 200 with the revision-0 document and ETag \"0\"", path, r.status, r.header.Get("ETag"), r.body)
			}
		}
	})

	t.Run("answers a stored revision as it was written", func(t *testing.T) {
		// Each revision answers its document as validated, its writer and
		// its time cut to whole seconds.
		docs := []string{threeRules, otherRule, oneRule}
		writers := []map[string]any{
			{"actor_type": "api_key", "actor_id": w.dana, "username": "dana"},
			{"actor_type": "api_key", "actor_id": w.dana, "username": "dana"},
			{"actor_type": "admin_token", "actor_id": nil, "username": nil},
		}
		for _, tc := range []struct {
			query string
			n     int
		}{{"", 3}, {"?revision=1", 1}, {"?revision=2", 2}, {"?revision=3", 3}} {
			doc, err := validate(t, docs[tc.n-1])
			if err != nil {
				t.Fatal(err)
			}
			want := asJSON(t, doc).(map[string]any)
			want["revision"] = float64(tc.n)
			want["updated_at"] = "2026-11-16T10:00:0" + strconv.Itoa(tc.n) + "Z"
			want["updated_by"] = writers[tc.n-1]
			r := w.get(t, policyPath+tc.query, w.danaKey)
			if r.status != http.StatusOK || !reflect.DeepEqual(r.json, want) {
				t.Errorf("GET %s = %d %s\nwant 200 %v", tc.query, r.status, r.body, want)
			}
			if got, want := r.header.Get("ETag"), strconv.Quote(strconv.Itoa(tc.n)); got != want {
				t.Errorf("GET %s: ETag = %q, want %q", tc.query, got, want)
			}
		}
	})

	t.Run("refuses a revision that is not kept or not a non-negative integer", func(t *testing.T) {
		for _, tc := range []struct {
			path      string
			status    int
			errorType string
		}{
			{policyPath + "?revision=4", http.StatusNotFound, "not_found"},
			{policyPath + "?revision=0", http.StatusNotFound, "not_found"},
			{"/api/v1/workspaces/api--f1/policy?revision=1", http.StatusNotFound, "not_found"},
			{policyPath + "?revision=99999999999999999999", http.StatusNotFound, "not_found"},
			{"/api/v1/workspaces/api--f1/policy?revision=99999999999999999999", http.StatusNotFound, "not_found"},
			{policyPath + "?revision=-1", http.StatusBadRequest, "invalid_request"},
			{policyPath + "?revision=abc", http.StatusBadRequest, "invalid_request"},
			{policyPath + "?revision=1.5", http.StatusBadRequest, "invalid_request"},
			{policyPath + "?revision=+1", http.StatusBadRequest, "invalid_request"},
			{policyPath + "?revision=", http.StatusBadRequest, "invalid_request"},
		} {
			if r := w.get(t, tc.path, w.danaKey); r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("GET %s = %d %s, want %d %s", tc.path, r.status, r.body, tc.status, tc.errorType)
			}
		}
	})

	t.Run("refuses without the scope or read access", func(t *testing.T) {
		gitRead := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"git:read"}})
		if r := w.get(t, policyPath, gitRead); r.status != http.StatusForbidden || r.errorType() != "insufficient_scope" {
			t.Errorf("PAT with git:read only = %d %s, want 403 insufficient_scope", r.status, r.body)
		}
		a := w.get(t, policyPath, w.ozKey)
		b := w.get(t, "/api/v1/workspaces/nope/policy", w.ozKey)
		if e, _ := a.json["error"].(map[string]any); a.status != http.StatusNotFound || a.errorType() != "not_found" || e["message"] != "workspace not found" {
			t.Errorf("outsider = %d %s, want 404 not_found 'workspace not found'", a.status, a.body)
		}
		if !bytes.Equal(a.body, b.body) {
			t.Errorf("outsider's answer %s differs from the answer for no workspace %s", a.body, b.body)
		}
	})

	// Last, since it archives api.
	t.Run("answers a member, a bound fork token and an archived workspace as the owner", func(t *testing.T) {
		owner := w.get(t, policyPath, w.danaKey)
		for _, tc := range []struct {
			name       string
			before     func()
			credential string
		}{
			{"org member", nil, w.melKey},
			{"token bound to the member's fork", nil, w.melM1},
			{"owner of the archived workspace", func() { w.setStatus(t, "api", workspace.StatusArchived) }, w.danaKey},
		} {
			if tc.before != nil {
				tc.before()
			}
			if r := w.get(t, policyPath, tc.credential); r.status != http.StatusOK || !bytes.Equal(r.body, owner.body) {
				t.Errorf("%s = %d %s, want 200 %s", tc.name, r.status, r.body, owner.body)
			}
		}
	})
}

// invalidDoc is a document the validator refuses: its rule asks for more
// approvals than the format allows.
const invalidDoc = `{"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"approvals": 11}}]}`

// revisionOf is the revision the current policy of path answers to the
// owner.
func (w *world) revisionOf(t *testing.T, path string) any {
	t.Helper()
	return w.get(t, path, w.danaKey).json["revision"]
}

func TestPutPolicy_StoresTheNormalFormAsTheNextRevision(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	r1 := w.put(t, policyPath, w.danaKey, threeRules, "")
	if r1.status != http.StatusOK || r1.json["revision"] != 1.0 || r1.header.Get("ETag") != `"1"` {
		t.Fatalf("first put = %d ETag %q %s, want 200 at revision 1 with ETag \"1\"", r1.status, r1.header.Get("ETag"), r1.body)
	}
	rules, _ := r1.json["rules"].([]any)
	if first, _ := rules[0].(map[string]any); !reflect.DeepEqual(first["require"].(map[string]any)["approvers"], []any{"owner"}) {
		t.Errorf("the first rule's require = %v, want the normal form with approvers [owner]", first["require"])
	}
	if by, _ := r1.json["updated_by"].(map[string]any); by["actor_id"] != w.dana || by["actor_type"] != "api_key" || by["username"] != "dana" {
		t.Errorf("updated_by = %v, want dana's API key with her username", r1.json["updated_by"])
	}
	if g := w.get(t, policyPath, w.danaKey); !bytes.Equal(g.body, r1.body) || g.header.Get("ETag") != `"1"` {
		t.Errorf("GET after the put = %s ETag %q, want %s", g.body, g.header.Get("ETag"), r1.body)
	}
	if r2 := w.put(t, policyPath, w.danaKey, oneRule, "1"); r2.status != http.StatusOK || r2.json["revision"] != 2.0 {
		t.Errorf("put with If-Match 1 = %d %s, want 200 at revision 2", r2.status, r2.body)
	}
	r3 := w.put(t, policyPath, w.Token, threeRules, `"2"`)
	if by, _ := r3.json["updated_by"].(map[string]any); r3.status != http.StatusOK || r3.json["revision"] != 3.0 || !reflect.DeepEqual(by,
		map[string]any{"actor_type": "admin_token", "actor_id": nil, "username": nil}) {
		t.Errorf(`admin's put with If-Match "2" = %d %s, want 200 at revision 3 by the admin token, with no user`, r3.status, r3.body)
	}
	for _, status := range []workspace.Status{workspace.StatusPending, workspace.StatusCloning, workspace.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			w.setStatus(t, "web", status)
			path := "/api/v1/workspaces/web/policy"
			before := w.revisionOf(t, path).(float64)
			if r := w.put(t, path, w.danaKey, oneRule, ""); r.status != http.StatusOK || r.json["revision"] != before+1 {
				t.Errorf("= %d %s, want 200 at revision %v", r.status, r.body, before+1)
			}
		})
	}
}

// failingEvents refuses every event, as a store that cannot write the log.
type failingEvents struct{}

func (failingEvents) Emit(context.Context, *sql.Tx, event.Input) (event.Record, error) {
	return event.Record{}, errors.New("the event log is full")
}

func TestPutPolicy_RecordsOneUpdateEventWithTheRevision(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	seq := w.seq(t)
	if r := w.put(t, policyPath, w.danaKey, threeRules, ""); r.status != http.StatusOK {
		t.Fatalf("put = %d %s, want 200", r.status, r.body)
	}
	after := w.eventsAfter(t, seq, eventUpdate)
	if len(after) != 1 {
		t.Fatalf("%d update events after the put, want 1", len(after))
	}
	e := after[0]
	var meta map[string]any
	if err := json.Unmarshal(e.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if e.ResourceType != "policy" || e.ResourceID != "api" || e.Action != "update" || e.ActorType != "api_key" ||
		e.ActorID == nil || *e.ActorID != w.dana || !reflect.DeepEqual(meta, map[string]any{"revision": 1.0, "rules": 3.0}) {
		t.Errorf("event = %+v metadata %s, want policy api update by dana with {revision: 1, rules: 3}", e, e.Metadata)
	}

	t.Run("an event that cannot be written", func(t *testing.T) {
		w := newWorld(t, func(d *policy.Deps) { d.Events = failingEvents{} })
		if r := w.put(t, policyPath, w.danaKey, threeRules, ""); r.status != http.StatusInternalServerError || r.errorType() != "internal" {
			t.Errorf("put = %d %s, want 500 internal", r.status, r.body)
		}
		if rev := w.revisionOf(t, policyPath); rev != 0.0 {
			t.Errorf("revision after the failed put = %v, want 0", rev)
		}
	})
}

func TestPutPolicy_RefusesAnIfMatchThatIsNotTheStoredRevision(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for range 2 {
		if r := w.put(t, policyPath, w.danaKey, threeRules, ""); r.status != http.StatusOK {
			t.Fatalf("put = %d %s, want 200", r.status, r.body)
		}
	}
	stored, seq := w.get(t, policyPath, w.danaKey), w.seq(t)
	for _, ifMatch := range []string{"1", `"1"`, "3", "0", "99999999999999999999"} {
		t.Run(ifMatch, func(t *testing.T) {
			if r := w.put(t, policyPath, w.danaKey, oneRule, ifMatch); r.status != http.StatusPreconditionFailed || r.errorType() != "revision_mismatch" {
				t.Errorf("= %d %s, want 412 revision_mismatch", r.status, r.body)
			}
			if g := w.get(t, policyPath, w.danaKey); !bytes.Equal(g.body, stored.body) {
				t.Errorf("policy after the refusal = %s, want %s", g.body, stored.body)
			}
			if n := len(w.eventsAfter(t, seq, eventUpdate)); n != 0 {
				t.Errorf("%d update events, want none", n)
			}
		})
	}
	if r := w.put(t, "/api/v1/workspaces/web/policy", w.danaKey, oneRule, "0"); r.status != http.StatusOK || r.json["revision"] != 1.0 {
		t.Errorf("If-Match 0 on a policy never set = %d %s, want 200 at revision 1", r.status, r.body)
	}
}

func TestPutPolicy_RefusesAnInvalidBodyDocumentOrIfMatch(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	seq := w.seq(t)
	tooLong := `{"rules": []` + strings.Repeat(" ", 65537-len(`{"rules": []}`)) + `}`
	cases := []struct {
		name, doc, ifMatch, errorType string
		details                       map[string]any
	}{
		{"too many approvals", invalidDoc, "", "invalid_policy",
			map[string]any{"rule": "billing", "index": 0.0, "field": "require.approvals"}},
		{"unknown user in the default", `{"default": {"approvers": ["user:nobody"]}}`, "", "invalid_policy",
			map[string]any{"rule": "default", "index": nil, "field": "approvers[0]"}},
		{"a document of 65 537 bytes", tooLong, "", "invalid_policy",
			map[string]any{"rule": nil, "index": nil, "field": "document"}},
		{"a user named by e-mail", `{"default": {"approvers": ["user:dana@example.com"]}}`, "", "invalid_policy",
			map[string]any{"rule": "default", "index": nil, "field": "approvers[0]"}},
		{"a user named by id", `{"default": {"approvers": ["user:` + w.dana + `"]}}`, "", "invalid_policy",
			map[string]any{"rule": "default", "index": nil, "field": "approvers[0]"}},
		{"a list", `[]`, "", "invalid_request", nil},
		{"not JSON", `nope`, "", "invalid_request", nil},
		{"If-Match abc", oneRule, "abc", "invalid_request", nil},
		{"If-Match -1", oneRule, "-1", "invalid_request", nil},
		{"weak If-Match", oneRule, `W/"1"`, "invalid_request", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := w.put(t, policyPath, w.danaKey, tc.doc, tc.ifMatch)
			if r.status != http.StatusBadRequest || r.errorType() != tc.errorType {
				t.Fatalf("= %d %s, want 400 %s", r.status, r.body, tc.errorType)
			}
			if details := r.json["error"].(map[string]any)["details"]; tc.details != nil && !reflect.DeepEqual(details, tc.details) {
				t.Errorf("details = %v, want %v", details, tc.details)
			}
			if rev := w.revisionOf(t, policyPath); rev != 1.0 {
				t.Errorf("revision after the refusal = %v, want 1", rev)
			}
			if n := len(w.eventsAfter(t, seq, eventUpdate)); n != 0 {
				t.Errorf("%d update events, want none", n)
			}
		})
	}
}

func TestPutPolicy_RefusesAForkBeforeTheBodyAndAnArchivedWorkspaceAfterIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	seq := w.seq(t)
	archived := func() { w.setStatus(t, "api", workspace.StatusArchived) }
	cases := []struct {
		name, path, doc, ifMatch string
		before                   func()
		status                   int
		errorType                string
	}{
		{"fork", "/api/v1/workspaces/api--f1/policy", oneRule, "", nil, http.StatusBadRequest, "not_supported_on_fork"},
		{"fork with an invalid document", "/api/v1/workspaces/api--f1/policy", invalidDoc, "", nil, http.StatusBadRequest, "not_supported_on_fork"},
		{"archived", policyPath, oneRule, "", archived, http.StatusConflict, "workspace_archived"},
		{"archived with another revision", policyPath, oneRule, "99", nil, http.StatusConflict, "workspace_archived"},
		{"archived with an invalid document", policyPath, invalidDoc, "", nil, http.StatusBadRequest, "invalid_policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before()
			}
			if r := w.put(t, tc.path, w.danaKey, tc.doc, tc.ifMatch); r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
			}
			// A fork answers revision 0 whatever is stored, so the parent's
			// revision and the log tell that nothing was written.
			if rev := w.revisionOf(t, policyPath); rev != 0.0 {
				t.Errorf("api's revision after the refusal = %v, want 0", rev)
			}
			if events := w.eventsAfter(t, seq, eventUpdate); len(events) != 0 {
				t.Errorf("%d update events, the first of %v, want none", len(events), *events[0].Workspace)
			}
		})
	}
}

func TestPutPolicy_RefusesACallerThatIsNotTheOwnerWithTheScope(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	readOnly := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"changes:read"}})
	boundToFork := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"workspaces:write"}, Workspace: "api--f1"})
	cases := []struct {
		name, credential string
		status           int
		errorType        string
	}{
		{"PAT without workspaces:write", readOnly, http.StatusForbidden, "insufficient_scope"},
		{"org member", w.melKey, http.StatusForbidden, "forbidden"},
		{"token bound to a fork, on its parent", boundToFork, http.StatusForbidden, "token_bound"},
		{"outsider", w.ozKey, http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		for kind, doc := range map[string]string{"valid": oneRule, "invalid": invalidDoc} {
			t.Run(tc.name+", "+kind+" document", func(t *testing.T) {
				if r := w.put(t, policyPath, tc.credential, doc, ""); r.status != tc.status || r.errorType() != tc.errorType {
					t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
				}
			})
		}
	}
	if rev := w.revisionOf(t, policyPath); rev != 0.0 {
		t.Errorf("revision after the refusals = %v, want 0", rev)
	}
	bound := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"workspaces:write"}, Workspace: "api"})
	if r := w.put(t, policyPath, bound, oneRule, ""); r.status != http.StatusOK || r.json["revision"] != 1.0 {
		t.Errorf("PAT bound to the workspace = %d %s, want 200 at revision 1", r.status, r.body)
	}
}

func TestPutPolicy_GivesConcurrentReplacementsConsecutiveRevisions(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// both sends two documents at once, each with ifMatch, and returns the
	// answers in the order of the documents.
	both := func(ifMatch string) [2]answer {
		var out [2]answer
		var wg sync.WaitGroup
		for i, doc := range []string{oneRule, otherRule} {
			wg.Go(func() { out[i] = w.put(t, policyPath, w.danaKey, doc, ifMatch) })
		}
		wg.Wait()
		return out
	}
	n := 0.0
	for round := range 20 {
		rs := both("")
		got := []any{rs[0].json["revision"], rs[1].json["revision"]}
		if !slices.Equal(got, []any{n + 1, n + 2}) {
			got[0], got[1] = got[1], got[0]
		}
		if rs[0].status != http.StatusOK || rs[1].status != http.StatusOK || !slices.Equal(got, []any{n + 1, n + 2}) {
			t.Fatalf("round %d without If-Match = %d %s and %d %s, want 200 at %v and %v",
				round, rs[0].status, rs[0].body, rs[1].status, rs[1].body, n+1, n+2)
		}
		n += 2
		last := rs[0]
		if rs[1].json["revision"] == n {
			last = rs[1]
		}
		if g := w.get(t, policyPath, w.danaKey); !bytes.Equal(g.body, last.body) {
			t.Errorf("round %d: policy = %s, want the one answered with revision %v, %s", round, g.body, n, last.body)
		}

		rs = both(strconv.Itoa(int(n)))
		statuses := []int{rs[0].status, rs[1].status}
		slices.Sort(statuses)
		if !slices.Equal(statuses, []int{http.StatusOK, http.StatusPreconditionFailed}) {
			t.Fatalf("round %d with If-Match %v = %d %s and %d %s, want one 200 and one 412",
				round, n, rs[0].status, rs[0].body, rs[1].status, rs[1].body)
		}
		n++
		if rev := w.revisionOf(t, policyPath); rev != n {
			t.Errorf("round %d: revision = %v, want %v", round, rev, n)
		}
	}
}

// evalWorld is the world with the threeRules document stored as api's
// revision 1, an evidence source that would count an approval and pass
// every check of every change, and api's changes: billing, open, touching
// billing/a.go, whose stored landing that evidence makes ready; draft, a
// draft touching docs/a.md; fromF1 and fromM1, open, from the forks api--f1
// and api--m1. web is pending with no default branch yet, and wsWrite is a
// PAT of dana with only workspaces:write.
type evalWorld struct {
	*world
	billing, draft, fromF1, fromM1 int64
	wsWrite                        string
}

func newEvalWorld(t *testing.T) *evalWorld {
	t.Helper()
	w := &evalWorld{world: newWorld(t)}
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	w.policy.RegisterEvidence(&evidence{approvals: 1, pass: true})
	w.billing = w.openBilling(t, "billing")
	w.draft = w.openTouching(t, "draft", "docs/a.md", func(in *change.OpenInput) { in.Draft = true })
	w.fromF1 = w.openTouching(t, "f1", "docs/b.md", func(in *change.OpenInput) { in.SourceWorkspace = "api--f1" })
	w.fromM1 = w.openTouching(t, "m1", "docs/c.md", func(in *change.OpenInput) { in.SourceWorkspace = "api--m1" })
	// Once its mergeability is computed, nothing writes a change again
	// until another trigger.
	for _, n := range []int64{w.billing, w.draft, w.fromF1, w.fromM1} {
		w.checked(t, "api", n)
	}
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ?, default_branch = NULL WHERE slug = 'web'`, workspace.StatusPending)
		return err
	})
	w.wsWrite = w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"workspaces:write"}})
	return w
}

// evalRow is one evaluate request, by credential (dana's key for "") on
// slug (api for ""), with body, a value sent as JSON or a string sent as
// it is, and the answer it wants: status, with errorType and details
// (when not nil) for a refusal, and for 200 the landing status, the rules
// of the requirements in order and the policy revision.
type evalRow struct {
	name, slug, credential string
	body                   any
	status                 int
	errorType              string
	details                map[string]any
	landing                string
	rules                  []string
	revision               float64
}

// evaluate sends the row's request.
func (w *evalWorld) evaluate(t *testing.T, r evalRow) answer {
	t.Helper()
	slug, credential := cmp.Or(r.slug, "api"), cmp.Or(r.credential, w.danaKey)
	return w.send(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/policy/evaluate", credential, r.body, nil)
}

// check sends the row's request, asserts the answer it wants and returns
// it. Every 200 holds exactly policy_revision, status and requirements,
// counts no approval and has every check pending.
func (w *evalWorld) check(t *testing.T, r evalRow) answer {
	t.Helper()
	a := w.evaluate(t, r)
	if a.status != r.status {
		t.Fatalf("= %d %s, want %d", a.status, a.body, r.status)
	}
	if r.status != http.StatusOK {
		if a.errorType() != r.errorType {
			t.Errorf("= %d %s, want %s", a.status, a.body, r.errorType)
		}
		if details := a.json["error"].(map[string]any)["details"]; r.details != nil && !reflect.DeepEqual(details, r.details) {
			t.Errorf("details = %v, want %v", details, r.details)
		}
		return a
	}
	reqs, _ := a.json["requirements"].([]any)
	var rules []string
	for _, q := range reqs {
		req, _ := q.(map[string]any)
		rules = append(rules, req["rule"].(string))
		if approvals, _ := req["approvals"].(map[string]any); approvals["counted"] != 0.0 {
			t.Errorf("requirement %v counts %v approvals, want 0", req["rule"], approvals["counted"])
		}
		for _, c := range req["checks"].([]any) {
			if c.(map[string]any)["status"] != "pending" {
				t.Errorf("requirement %v has the check %v, want it pending", req["rule"], c)
			}
		}
	}
	if len(a.json) != 3 || a.json["status"] != r.landing || !slices.Equal(rules, r.rules) || a.json["policy_revision"] != r.revision {
		t.Errorf("= %s, want only policy_revision %v, status %s and the requirements %v", a.body, r.revision, r.landing, r.rules)
	}
	return a
}

// manyPaths are n distinct paths that no rule of threeRules selects.
func manyPaths(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "src/" + strconv.Itoa(i) + ".go"
	}
	return out
}

// stop blocks every file.
const stop = `{"rules": [{"name": "stop", "paths": ["**"], "require": {"block": true}}]}`

func TestEvaluatePolicy(t *testing.T) {
	t.Parallel()
	w := newEvalWorld(t)
	if l := w.landing(t, w.billing); l["status"] != change.LandingReady {
		t.Fatalf("the stored landing of the billing change is %v, want ready", l)
	}
	// snapshot is the last event, the policies and the changes, which no
	// evaluation, valid or refused, changes.
	snapshot := func() []any {
		out := []any{w.seq(t)}
		for _, slug := range []string{"api", "web"} {
			out = append(out, string(w.get(t, "/api/v1/workspaces/"+slug+"/policy", w.danaKey).body))
		}
		for _, n := range []int64{w.billing, w.draft, w.fromF1, w.fromM1} {
			out = append(out, string(w.get(t, "/api/v1/workspaces/api/changes/"+strconv.FormatInt(n, 10), w.danaKey).body))
		}
		return out
	}
	before := snapshot()

	refused := func(name string, body any) evalRow {
		return evalRow{name: name, body: body, status: http.StatusBadRequest, errorType: "invalid_request"}
	}
	notFound := func(name, credential string, n int64) evalRow {
		return evalRow{name: name, credential: credential, body: map[string]any{"change": n},
			status: http.StatusNotFound, errorType: "change_not_found"}
	}
	tooLong := `{"rules": []` + strings.Repeat(" ", 65537-len(`{"rules": []}`)) + `}`
	document := map[string]any{"rule": nil, "index": nil, "field": "document"}
	billing := evalRow{name: "billing twice and docs", body: map[string]any{"paths": []string{"billing/a.go", "docs/a.md", "billing/a.go"}},
		status: http.StatusOK, landing: "pending", rules: []string{"docs", "billing"}, revision: 1}
	foreign := notFound("a fork's token, a change of another fork", w.melM1, w.fromF1)
	outsider := evalRow{name: "an outsider", credential: w.ozKey, body: map[string]any{}, status: http.StatusNotFound, errorType: "not_found"}
	for _, g := range []struct {
		name string
		rows []evalRow
	}{
		{"answers what paths need under the stored document", []evalRow{
			{name: "docs", body: map[string]any{"paths": []string{"docs/a.md"}},
				status: http.StatusOK, landing: "ready", rules: []string{"docs"}, revision: 1},
			billing,
			{name: "billing into a branch its targets do not match", body: map[string]any{"paths": []string{"billing/a.go"}, "target_branch": "dev"},
				status: http.StatusOK, landing: "ready", rules: []string{"default"}, revision: 1},
			{name: "billing into a release branch that does not exist", body: map[string]any{"paths": []string{"billing/a.go"}, "target_branch": "release/9"},
				status: http.StatusOK, landing: "pending", rules: []string{"billing"}, revision: 1},
			{name: "secrets", body: map[string]any{"paths": []string{"deploy/secrets/k"}},
				status: http.StatusOK, landing: "blocked", rules: []string{"secrets"}, revision: 1},
			{name: "2 000 paths", body: map[string]any{"paths": manyPaths(2000)},
				status: http.StatusOK, landing: "ready", rules: []string{"default"}, revision: 1},
			{name: "secrets on a fork's own slug", slug: "api--m1", body: map[string]any{"paths": []string{"deploy/secrets/k"}},
				status: http.StatusOK, landing: "ready", rules: []string{"default"}, revision: 0},
		}},
		{"answers what a change needs without its evidence or cause", []evalRow{
			{name: "an open change whose evidence makes it ready", body: map[string]any{"change": w.billing},
				status: http.StatusOK, landing: "pending", rules: []string{"billing"}, revision: 1},
			{name: "a draft", body: map[string]any{"change": w.draft},
				status: http.StatusOK, landing: "ready", rules: []string{"docs"}, revision: 1},
		}},
		// The answer names the stored revision, whatever document answered.
		{"takes a candidate in place of the stored document", []evalRow{
			{name: "paths against a candidate", body: map[string]any{"paths": []string{"billing/a.go"}, "policy": json.RawMessage(oneRule)},
				status: http.StatusOK, landing: "pending", rules: []string{"all"}, revision: 1},
			{name: "a change against a candidate", body: map[string]any{"change": w.billing, "policy": json.RawMessage(stop)},
				status: http.StatusOK, landing: "blocked", rules: []string{"stop"}, revision: 1},
		}},
		{"refuses an invalid request", []evalRow{
			refused("neither paths nor change", map[string]any{}),
			refused("both paths and change", map[string]any{"paths": []string{"a"}, "change": 1}),
			refused("no path", map[string]any{"paths": []string{}}),
			refused("2 001 paths", map[string]any{"paths": manyPaths(2001)}),
			refused("an empty path", map[string]any{"paths": []string{""}}),
			refused("an absolute path", map[string]any{"paths": []string{"/abs"}}),
			refused("a path that is a number", map[string]any{"paths": []any{3}}),
			refused("paths that are a string", map[string]any{"paths": "a"}),
			refused("paths that are null", map[string]any{"paths": nil}),
			refused("a change that is a string", map[string]any{"change": "1"}),
			refused("a change that is a fraction", map[string]any{"change": 1.5}),
			refused("a target branch with a change", map[string]any{"change": 1, "target_branch": "main"}),
			refused("an empty target branch", map[string]any{"paths": []string{"a"}, "target_branch": ""}),
			refused("a target branch of 256 characters", map[string]any{"paths": []string{"a"}, "target_branch": strings.Repeat("b", 256)}),
			refused("a target branch that is a number", map[string]any{"paths": []string{"a"}, "target_branch": 1}),
			refused("not an object", `[]`),
			{name: "no target branch on a workspace without a default branch", slug: "web", body: map[string]any{"paths": []string{"a"}},
				status: http.StatusBadRequest, errorType: "invalid_request"},
			{name: "a target branch on a workspace without a default branch", slug: "web", body: map[string]any{"paths": []string{"a"}, "target_branch": "main"},
				status: http.StatusOK, landing: "ready", rules: []string{"default"}, revision: 0},
		}},
		{"refuses an invalid candidate as a replacement would", []evalRow{
			{name: "a rule without paths", body: map[string]any{"paths": []string{"a"}, "policy": map[string]any{
				"rules": []any{map[string]any{"name": "x", "paths": []string{}, "require": map[string]any{}}}}},
				status: http.StatusBadRequest, errorType: "invalid_policy", details: map[string]any{"rule": "x", "index": 0.0, "field": "paths"}},
			{name: "a list", body: map[string]any{"paths": []string{"a"}, "policy": []any{}},
				status: http.StatusBadRequest, errorType: "invalid_policy", details: document},
			{name: "a document of 65 537 bytes", body: `{"paths": ["a"], "policy": ` + tooLong + `}`,
				status: http.StatusBadRequest, errorType: "invalid_policy", details: document},
		}},
		{"hides a change of another source from a fork-bound token", []evalRow{
			foreign,
			notFound("a fork's token, no such change", w.melM1, 9999),
			notFound("the owner, no such change", w.danaKey, 9999),
			{name: "a fork's token, a change of its fork", credential: w.melM1, body: map[string]any{"change": w.fromM1},
				status: http.StatusOK, landing: "ready", rules: []string{"docs"}, revision: 1},
		}},
		{"refuses without the scope or read access before the body", []evalRow{
			{name: "a PAT without changes:read", credential: w.wsWrite, body: map[string]any{},
				status: http.StatusForbidden, errorType: "insufficient_scope"},
			outsider,
		}},
	} {
		t.Run(g.name, func(t *testing.T) {
			for _, r := range g.rows {
				t.Run(r.name, func(t *testing.T) { w.check(t, r) })
			}
		})
	}

	t.Run("answers a member and a bound fork token as the owner", func(t *testing.T) {
		owner := w.check(t, billing)
		if req := owner.json["requirements"].([]any)[1].(map[string]any); !reflect.DeepEqual(req["paths"], []any{"billing/a.go"}) {
			t.Errorf("the billing requirement lists %v, want billing/a.go once", req["paths"])
		}
		for name, credential := range map[string]string{"org member": w.melKey, "token bound to the member's fork": w.melM1} {
			r := billing
			r.credential = credential
			if a := w.evaluate(t, r); a.status != http.StatusOK || !bytes.Equal(a.body, owner.body) {
				t.Errorf("%s = %d %s, want 200 %s", name, a.status, a.body, owner.body)
			}
		}
	})
	t.Run("answers a change it hides as one that does not exist", func(t *testing.T) {
		unknown, owners := foreign, foreign
		unknown.body = map[string]any{"change": 9999}
		owners.credential, owners.body = w.danaKey, unknown.body
		a, b, c := w.evaluate(t, foreign), w.evaluate(t, unknown), w.evaluate(t, owners)
		if !bytes.Equal(a.body, b.body) || !bytes.Equal(a.body, c.body) {
			t.Errorf("a change of another fork = %s, no such change = %s and to the owner %s, want identical bodies", a.body, b.body, c.body)
		}
	})
	t.Run("answers an outsider as a workspace that does not exist", func(t *testing.T) {
		nope := outsider
		nope.slug = "nope"
		if a, b := w.evaluate(t, outsider), w.evaluate(t, nope); !bytes.Equal(a.body, b.body) {
			t.Errorf("outsider's answer %s differs from the answer for no workspace %s", a.body, b.body)
		}
	})
	t.Run("writes nothing", func(t *testing.T) {
		if after := snapshot(); !reflect.DeepEqual(after, before) {
			t.Errorf("after the evaluations the last event, the policies and the changes are\n%v\nwant\n%v", after, before)
		}
	})
	// Last, since it archives api.
	t.Run("answers on an archived workspace", func(t *testing.T) {
		w.setStatus(t, "api", workspace.StatusArchived)
		w.check(t, evalRow{name: "docs", body: map[string]any{"paths": []string{"docs/a.md"}},
			status: http.StatusOK, landing: "ready", rules: []string{"docs"}, revision: 1})
	})
}
