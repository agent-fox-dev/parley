package policy_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

const policyPath = "/api/v1/workspaces/api/policy"

// oneRule and otherRule are two more documents, to tell revisions apart.
const (
	oneRule   = `{"rules": [{"name": "all", "paths": ["**"], "require": {"approvals": 2}}]}`
	otherRule = `{"default": {"checks": ["test"]}, "rules": [{"name": "src", "paths": ["src/**"], "exclude": ["src/gen/**"], "require": {}}]}`
)

func TestGetPolicy_AnswersRevisionZeroWhileNoPolicyIsSetAndOnAFork(t *testing.T) {
	w := newWorld(t)
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
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
		t.Run(path, func(t *testing.T) {
			r := w.get(t, path, w.danaKey)
			if r.status != http.StatusOK || !reflect.DeepEqual(r.json, want) {
				t.Errorf("= %d %s, want 200 with the revision-0 document", r.status, r.body)
			}
			if got := r.header.Get("ETag"); got != `"0"` {
				t.Errorf("ETag = %q, want %q", got, `"0"`)
			}
		})
	}
}

func TestGetPolicy_AnswersAStoredRevisionAsItWasWritten(t *testing.T) {
	w := newWorld(t)
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	w.setPolicy(t, "api", threeRules, w.byDana(), at.Add(1*time.Second+500*time.Millisecond))
	w.setPolicy(t, "api", otherRule, w.byDana(), at.Add(2*time.Second))
	w.setPolicy(t, "api", oneRule, byAdmin, at.Add(3*time.Second))
	// Each revision answers its document as validated, its writer and its
	// time cut to whole seconds.
	docs := []string{threeRules, otherRule, oneRule}
	writers := []map[string]any{
		{"actor_type": "api_key", "actor_id": w.dana, "username": "dana"},
		{"actor_type": "api_key", "actor_id": w.dana, "username": "dana"},
		{"actor_type": "admin_token", "actor_id": nil, "username": nil},
	}
	cases := []struct {
		query string
		n     int
	}{{"", 3}, {"?revision=1", 1}, {"?revision=2", 2}, {"?revision=3", 3}}
	for _, tc := range cases {
		t.Run("revision "+strconv.Itoa(tc.n)+tc.query, func(t *testing.T) {
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
				t.Errorf("= %d %s\nwant 200 %v", r.status, r.body, want)
			}
			if s, _ := r.json["updated_at"].(string); !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(s) {
				t.Errorf("updated_at = %q, want RFC 3339 UTC in whole seconds", s)
			}
			if got, want := r.header.Get("ETag"), strconv.Quote(strconv.Itoa(tc.n)); got != want {
				t.Errorf("ETag = %q, want %q", got, want)
			}
		})
	}
}

func TestGetPolicy_RefusesARevisionThatIsNotKeptOrNotANonNegativeInteger(t *testing.T) {
	w := newWorld(t)
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	w.setPolicy(t, "api", oneRule, w.byDana(), time.Now())
	cases := []struct {
		path      string
		status    int
		errorType string
	}{
		{policyPath + "?revision=3", http.StatusNotFound, "not_found"},
		{policyPath + "?revision=0", http.StatusNotFound, "not_found"},
		{"/api/v1/workspaces/api--f1/policy?revision=1", http.StatusNotFound, "not_found"},
		{policyPath + "?revision=-1", http.StatusBadRequest, "invalid_request"},
		{policyPath + "?revision=abc", http.StatusBadRequest, "invalid_request"},
		{policyPath + "?revision=1.5", http.StatusBadRequest, "invalid_request"},
		{policyPath + "?revision=+1", http.StatusBadRequest, "invalid_request"},
		{policyPath + "?revision=", http.StatusBadRequest, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if r := w.get(t, tc.path, w.danaKey); r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
			}
		})
	}
}

func TestGetPolicy_AnswersAMemberABoundForkTokenAndAnArchivedWorkspaceAsTheOwner(t *testing.T) {
	w := newWorld(t)
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	owner := w.get(t, policyPath, w.danaKey)
	if owner.status != http.StatusOK || owner.json["revision"] != 1.0 {
		t.Fatalf("owner's read = %d %s, want 200 at revision 1", owner.status, owner.body)
	}
	cases := []struct {
		name       string
		before     func()
		credential string
	}{
		{"org member", nil, w.melKey},
		{"token bound to the member's fork", nil, w.melM1},
		{"owner of the archived workspace", func() { w.setStatus(t, "api", workspace.StatusArchived) }, w.danaKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before()
			}
			if r := w.get(t, policyPath, tc.credential); r.status != http.StatusOK || !bytes.Equal(r.body, owner.body) {
				t.Errorf("= %d %s, want 200 %s", r.status, r.body, owner.body)
			}
		})
	}
}

func TestGetPolicy_RefusesWithoutTheScopeOrReadAccess(t *testing.T) {
	w := newWorld(t)
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
	w := newWorld(t)
	r1 := w.put(t, policyPath, w.danaKey, threeRules, "")
	if r1.status != http.StatusOK || r1.json["revision"] != 1.0 || r1.header.Get("ETag") != `"1"` {
		t.Fatalf("first put = %d ETag %q %s, want 200 at revision 1 with ETag \"1\"", r1.status, r1.header.Get("ETag"), r1.body)
	}
	rules, _ := r1.json["rules"].([]any)
	if first, _ := rules[0].(map[string]any); !reflect.DeepEqual(first["require"].(map[string]any)["approvers"], []any{"owner"}) {
		t.Errorf("the first rule's require = %v, want the normal form with approvers [owner]", first["require"])
	}
	if by, _ := r1.json["updated_by"].(map[string]any); by["actor_id"] != w.dana || by["actor_type"] != "api_key" {
		t.Errorf("updated_by = %v, want dana's API key", r1.json["updated_by"])
	}
	if g := w.get(t, policyPath, w.danaKey); !bytes.Equal(g.body, r1.body) || g.header.Get("ETag") != `"1"` {
		t.Errorf("GET after the put = %s ETag %q, want %s", g.body, g.header.Get("ETag"), r1.body)
	}
	if r2 := w.put(t, policyPath, w.danaKey, oneRule, "1"); r2.status != http.StatusOK || r2.json["revision"] != 2.0 {
		t.Errorf("put with If-Match 1 = %d %s, want 200 at revision 2", r2.status, r2.body)
	}
	r3 := w.put(t, policyPath, w.Token, threeRules, `"2"`)
	if by, _ := r3.json["updated_by"].(map[string]any); r3.status != http.StatusOK || r3.json["revision"] != 3.0 || by["actor_type"] != "admin_token" {
		t.Errorf(`admin's put with If-Match "2" = %d %s, want 200 at revision 3 by the admin token`, r3.status, r3.body)
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
	w := newWorld(t)
	before := len(w.updates(t, "api"))
	if r := w.put(t, policyPath, w.danaKey, threeRules, ""); r.status != http.StatusOK {
		t.Fatalf("put = %d %s, want 200", r.status, r.body)
	}
	after := w.updates(t, "api")
	if len(after) != before+1 {
		t.Fatalf("%d update events after the put, want %d", len(after), before+1)
	}
	e := after[len(after)-1]
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
	w := newWorld(t)
	for range 2 {
		if r := w.put(t, policyPath, w.danaKey, threeRules, ""); r.status != http.StatusOK {
			t.Fatalf("put = %d %s, want 200", r.status, r.body)
		}
	}
	stored, events := w.get(t, policyPath, w.danaKey), len(w.updates(t, "api"))
	for _, ifMatch := range []string{"1", `"1"`, "3", "0"} {
		t.Run(ifMatch, func(t *testing.T) {
			if r := w.put(t, policyPath, w.danaKey, oneRule, ifMatch); r.status != http.StatusPreconditionFailed || r.errorType() != "revision_mismatch" {
				t.Errorf("= %d %s, want 412 revision_mismatch", r.status, r.body)
			}
			if g := w.get(t, policyPath, w.danaKey); !bytes.Equal(g.body, stored.body) {
				t.Errorf("policy after the refusal = %s, want %s", g.body, stored.body)
			}
			if n := len(w.updates(t, "api")); n != events {
				t.Errorf("%d update events, want %d", n, events)
			}
		})
	}
	if r := w.put(t, "/api/v1/workspaces/web/policy", w.danaKey, oneRule, "0"); r.status != http.StatusOK || r.json["revision"] != 1.0 {
		t.Errorf("If-Match 0 on a policy never set = %d %s, want 200 at revision 1", r.status, r.body)
	}
}

func TestPutPolicy_RefusesAnInvalidBodyDocumentOrIfMatch(t *testing.T) {
	w := newWorld(t)
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	events := len(w.updates(t, "api"))
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
			if n := len(w.updates(t, "api")); n != events {
				t.Errorf("%d update events, want %d", n, events)
			}
		})
	}
}

func TestPutPolicy_RefusesAForkBeforeTheBodyAndAnArchivedWorkspaceAfterIt(t *testing.T) {
	w := newWorld(t)
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
			if rev := w.revisionOf(t, tc.path); rev != 0.0 {
				t.Errorf("revision after the refusal = %v, want 0", rev)
			}
		})
	}
}

func TestPutPolicy_RefusesACallerThatIsNotTheOwnerWithTheScope(t *testing.T) {
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
