package policy_test

import (
	"bytes"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
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
		{"owner of the archived workspace", func() { w.archive(t, "api") }, w.danaKey},
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
