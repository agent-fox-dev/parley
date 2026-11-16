package policy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
)

// approves and rejects are the bodies of a verdict on the current version.
var (
	approves = map[string]any{"verdict": "approve"}
	rejects  = map[string]any{"verdict": "reject"}
)

// give posts body as a verdict on the change number of api with credential
// and fails the test unless it is recorded; it returns the approval.
func (w *world) give(t *testing.T, number int64, credential string, body any) map[string]any {
	t.Helper()
	r := w.postVerdict(t, number, credential, body)
	if r.status != http.StatusCreated && r.status != http.StatusOK {
		t.Fatalf("verdict %v on %d = %d %s, want 201 or 200", body, number, r.status, r.body)
	}
	return r.json
}

// edited records version head+1 of the change from branch of api, which
// sets the file path to new content, so that it is patch-equivalent to no
// earlier version.
func (w *world) edited(t *testing.T, branch, path string) {
	t.Helper()
	head := commit(t, w.api, branch, branch, map[string]string{path: "edited " + branch + "\n"})
	w.record(t, branch, head, change.KindCommit, w.danaCaller())
}

func TestFacts_CountTheApproveVerdictsOnTheCurrentVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 2, "approvers": ["owner", "member"], "checks": []}}`)
	p := w.openPushed(t, "work")
	req := func() map[string]any { return only(w.landing(t, p.number)) }
	steps := []struct {
		name       string
		credential string
		// version, when set, records a version that is patch-equivalent to
		// none instead of posting a verdict.
		version   bool
		counted   float64
		satisfied bool
	}{
		{name: "no verdict"},
		{name: "dana approves with her API key", credential: w.danaKey, counted: 1},
		{name: "dana approves again", credential: w.danaKey, counted: 1},
		{name: "mel approves", credential: w.melKey, counted: 2, satisfied: true},
		{name: "dana approves with the PAT that created version 1", credential: p.patPush, counted: 1},
		{name: "a new version that is patch-equivalent to none", version: true},
	}
	for _, s := range steps {
		switch {
		case s.version:
			w.edited(t, "work", "work.txt")
		case s.credential != "":
			w.give(t, p.number, s.credential, approves)
		}
		r := req()
		if r["approvals"].(map[string]any)["counted"] != s.counted || r["satisfied"] != s.satisfied {
			t.Errorf("%s: requirement %v, want counted %v, satisfied %v", s.name, r, s.counted, s.satisfied)
		}
		if l := w.landing(t, p.number); s.satisfied && l["status"] != change.LandingReady {
			t.Errorf("%s: landing %v, want ready", s.name, l)
		}
	}
}

func TestFacts_ACountedRejectKeepsTheRequirementUnsatisfied(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["owner", "member"], "checks": []}}`)
	// rejected is a change dana approved and mel then rejected, with the
	// id of mel's verdict.
	rejected := func(t *testing.T, branch string) (int64, string) {
		t.Helper()
		n := w.openPushed(t, branch).number
		w.give(t, n, w.danaKey, approves)
		if l := w.landing(t, n); l["status"] != change.LandingReady {
			t.Fatalf("landing after dana approved = %v, want ready", l)
		}
		id := w.give(t, n, w.melKey, rejects)["id"].(string)
		l := w.landing(t, n)
		if r := only(l); l["status"] != change.LandingPending || r["satisfied"] != false || r["approvals"].(map[string]any)["counted"] != 1.0 {
			t.Fatalf("landing after mel rejected = %v, want pending, unsatisfied with 1 counted", l)
		}
		return n, id
	}
	for i, out := range []struct {
		name string
		way  func(t *testing.T, n int64, reject, branch string)
	}{
		{"mel approves instead", func(t *testing.T, n int64, _, _ string) { w.give(t, n, w.melKey, approves) }},
		{"mel withdraws his verdict", func(t *testing.T, n int64, reject, _ string) {
			if r := w.withdraw(t, n, reject, w.melKey); r.status != http.StatusNoContent {
				t.Fatalf("withdraw = %d %s, want 204", r.status, r.body)
			}
		}},
		{"the owner withdraws mel's verdict", func(t *testing.T, n int64, reject, _ string) {
			if r := w.withdraw(t, n, reject, w.danaKey); r.status != http.StatusNoContent {
				t.Fatalf("withdraw = %d %s, want 204", r.status, r.body)
			}
		}},
		{"a version patch-equivalent to none, approved by dana", func(t *testing.T, n int64, _, branch string) {
			w.edited(t, branch, branch+".txt")
			w.give(t, n, w.danaKey, approves)
		}},
	} {
		t.Run(out.name, func(t *testing.T) {
			branch := "out" + strconv.Itoa(i)
			n, reject := rejected(t, branch)
			out.way(t, n, reject, branch)
			if l := w.landing(t, n); l["status"] != change.LandingReady {
				t.Errorf("landing = %v, want ready", l)
			}
		})
	}
	t.Run("a pure rebase", func(t *testing.T) {
		n, _ := rejected(t, "rebased")
		head := w.rebased(t, "rebased", map[string]string{"rebased.txt": "rebased\n"})
		w.record(t, "rebased", head, change.KindRebase, w.danaCaller())
		l := w.landing(t, n)
		if r := only(l); l["status"] != change.LandingPending || r["satisfied"] != false || r["approvals"].(map[string]any)["counted"] != 1.0 {
			t.Errorf("landing = %v, want pending, unsatisfied with dana's carried approval counted", l)
		}
	})
	t.Run("a reject by a user who is not an approver", func(t *testing.T) {
		w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["owner"], "checks": []}}`)
		n := w.openPushed(t, "owners").number
		w.give(t, n, w.danaKey, approves)
		w.give(t, n, w.melKey, rejects)
		if l := w.landing(t, n); l["status"] != change.LandingReady || only(l)["satisfied"] != true {
			t.Errorf("landing = %v, want ready and satisfied", l)
		}
	})
}

func TestFacts_CountsForNamesTheRequirementsAVerdictCountsToward(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["member"], "checks": []},
"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"approvals": 1, "approvers": ["owner"]}}]}`)
	p := w.openPushedFiles(t, "work", map[string]string{"billing/a.go": "package billing\n", "README.md": "work\n"})
	byUser := func(t *testing.T) map[string]any {
		t.Helper()
		r := w.get(t, approvalsPath(p.number), w.danaKey)
		out := map[string]any{}
		for _, item := range r.json["items"].([]any) {
			a := item.(map[string]any)
			out[a["by"].(map[string]any)["username"].(string)] = a["counts_for"]
		}
		return out
	}
	for _, s := range []struct {
		name, credential string
		want             []any
	}{
		{"dana with her API key", w.danaKey, []any{"billing", "default"}},
		{"mel", w.melKey, []any{"default"}},
		{"dana with the PAT that created version 1", p.patPush, []any{}},
	} {
		if got := w.give(t, p.number, s.credential, approves)["counts_for"]; !reflect.DeepEqual(got, s.want) {
			t.Errorf("%s: counts_for %v, want %v", s.name, got, s.want)
		}
	}
	if got, want := byUser(t), map[string]any{"dana": []any{}, "mel": []any{"default"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("listed counts_for %v, want %v", got, want)
	}
	w.give(t, p.number, w.danaKey, approves)
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["user:mel"], "checks": []},
"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"approvals": 1, "approvers": ["user:mel"]}}]}`)
	if got, want := byUser(t), map[string]any{"dana": []any{}, "mel": []any{"billing", "default"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("listed counts_for after the policy names mel %v, want %v", got, want)
	}
	w.edited(t, "work", "billing/a.go")
	if got, want := byUser(t), map[string]any{"dana": []any{}, "mel": []any{}}; !reflect.DeepEqual(got, want) {
		t.Errorf("listed counts_for after a new version %v, want %v", got, want)
	}
}

func TestFacts_VerdictsCountOnlyOnTheVersionTheyAreOn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["owner"], "checks": []}}`)
	n := w.openPushed(t, "work").number
	first := w.readChange(t, "api", n).HeadSHA
	w.edited(t, "work", "work.txt")
	before := w.landing(t, n)
	if before["status"] != change.LandingPending {
		t.Fatalf("landing before any verdict = %v, want pending", before)
	}
	r := w.postVerdict(t, n, w.danaKey, map[string]any{"verdict": "approve", "version": 1})
	if r.status != http.StatusCreated || r.json["version"] != 1.0 || !reflect.DeepEqual(r.json["counts_for"], []any{}) {
		t.Fatalf("approve version 1 = %d %s, want 201 on version 1 counting for nothing", r.status, r.body)
	}
	if after := w.landing(t, n); !reflect.DeepEqual(after, before) {
		t.Errorf("landing after approving version 1 = %v, want %v", after, before)
	}
	if got := w.get(t, approvalsPath(n)+"?version=1", w.danaKey).json["items"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != r.json["id"] {
		t.Errorf("verdicts on version 1 = %v, want only %v", got, r.json["id"])
	}
	if got := w.get(t, approvalsPath(n)+"?version=2", w.danaKey).json["items"].([]any); len(got) != 0 {
		t.Errorf("verdicts on version 2 = %v, want none", got)
	}

	w.give(t, n, w.danaKey, approves)
	if l := w.landing(t, n); l["status"] != change.LandingReady {
		t.Fatalf("landing after approving version 2 = %v, want ready", l)
	}
	if r := w.postVerdict(t, n, w.danaKey, map[string]any{"verdict": "reject", "version": 1}); r.status != http.StatusOK {
		t.Fatalf("reject version 1 = %d %s, want 200", r.status, r.body)
	}
	if l := w.landing(t, n); l["status"] != change.LandingReady {
		t.Errorf("landing after rejecting version 1 = %v, want ready", l)
	}
	// A land of version 1 is judged with the verdicts on version 1.
	res, _, err := w.policy.JudgeVersion(ctx, "api", n, 1, w.base, first)
	if err != nil || res.Status != change.LandingPending || res.Requirements[0].Approvals.Counted != 0 {
		t.Errorf("JudgeVersion of version 1 = %+v, %v; want pending with none counted", res, err)
	}
}

// requirementFor is the requirement of the rule rule in the landing l, nil
// when it has none.
func requirementFor(l map[string]any, rule string) map[string]any {
	reqs, _ := l["requirements"].([]any)
	for _, r := range reqs {
		if req, _ := r.(map[string]any); req["rule"] == rule {
			return req
		}
	}
	return nil
}

// statuses are the named checks with their status, in the requirement's
// order, as the landing shows them.
func statuses(pairs ...string) []any {
	out := []any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"name": pairs[i], "status": pairs[i+1]})
	}
	return out
}

func TestFacts_ChecksHaveTheStatusOfTheResultThatAppliesToTheCurrentVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 0, "checks": ["test", "lint"]}}`)
	files := map[string]string{"work.txt": "work\n"}
	n := w.openPushedFiles(t, "work", files).number
	steps := []struct {
		name string
		do   func(t *testing.T)
		// status is the landing's, checks the requirement's.
		status string
		checks []any
	}{
		{"no result", func(*testing.T) {}, change.LandingPending, statuses("test", "pending", "lint", "pending")},
		{"test passed", func(t *testing.T) { w.report(t, n, "test", map[string]any{"status": "passed"}) },
			change.LandingPending, statuses("test", "passed", "lint", "pending")},
		{"lint failed", func(t *testing.T) { w.report(t, n, "lint", map[string]any{"status": "failed"}) },
			change.LandingPending, statuses("test", "passed", "lint", "failed")},
		{"lint passed", func(t *testing.T) { w.report(t, n, "lint", map[string]any{"status": "passed"}) },
			change.LandingReady, statuses("test", "passed", "lint", "passed")},
		{"docs, which no requirement names, failed", func(t *testing.T) { w.report(t, n, "docs", map[string]any{"status": "failed"}) },
			change.LandingReady, statuses("test", "passed", "lint", "passed")},
		{"a pure rebase", func(t *testing.T) {
			w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, w.danaCaller())
		}, change.LandingReady, statuses("test", "passed", "lint", "passed")},
		{"test pending on the rebased version", func(t *testing.T) { w.report(t, n, "test", map[string]any{"status": "pending"}) },
			change.LandingPending, statuses("test", "pending", "lint", "passed")},
		{"a version that changes content", func(t *testing.T) { w.edited(t, "work", "work.txt") },
			change.LandingPending, statuses("test", "pending", "lint", "pending")},
	}
	for _, s := range steps {
		s.do(t)
		l := w.landing(t, n)
		if l["status"] != s.status || !reflect.DeepEqual(only(l)["checks"], s.checks) {
			t.Errorf("after %s: landing %v, want %s with checks %v", s.name, l, s.status, s.checks)
		}
	}
}

func TestFacts_FreshChecksCountOnlyResultsOnTheCurrentVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 0, "checks": ["test"]},
"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"checks": ["test"], "fresh_checks": true}}]}`)
	files := map[string]string{"billing/a.go": "package billing\n", "README.md": "work\n"}
	n := w.openPushedFiles(t, "work", files).number
	w.report(t, n, "test", map[string]any{"status": "passed"})
	if l := w.landing(t, n); l["status"] != change.LandingReady {
		t.Fatalf("landing after test passed on version 1 = %v, want ready", l)
	}
	w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, w.danaCaller())
	l := w.landing(t, n)
	if r := requirementFor(l, "billing"); !reflect.DeepEqual(r["checks"], statuses("test", "pending")) || r["satisfied"] != false {
		t.Errorf("billing requirement after a pure rebase = %v, want test pending and unsatisfied", r)
	}
	if r := requirementFor(l, "default"); !reflect.DeepEqual(r["checks"], statuses("test", "passed")) || r["satisfied"] != true {
		t.Errorf("default requirement after a pure rebase = %v, want test passed and satisfied", r)
	}
	if l["status"] != change.LandingPending {
		t.Errorf("landing after a pure rebase = %v, want pending", l["status"])
	}
	w.report(t, n, "test", map[string]any{"status": "passed"})
	if l := w.landing(t, n); l["status"] != change.LandingReady {
		t.Errorf("landing after test passed on version 2 = %v, want ready", l)
	}
}

func TestFacts_AResultOnAVersionTheCurrentOneDoesNotReachChangesNoStatus(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 0, "checks": ["test"]}}`)
	n := w.openPushed(t, "work").number
	w.edited(t, "work", "work.txt")
	before, seq := w.landing(t, n), w.seq(t)
	if before["status"] != change.LandingPending {
		t.Fatalf("landing before = %v, want pending", before)
	}
	r := w.putCheck(t, n, "test", w.danaKey, map[string]any{"status": "passed", "version": 1})
	if r.status != http.StatusOK || r.json["version"] != 1.0 {
		t.Fatalf("report on version 1 = %d %s, want 200 on version 1", r.status, r.body)
	}
	if after := w.landing(t, n); !reflect.DeepEqual(after, before) {
		t.Errorf("landing = %v, want it as before: %v", after, before)
	}
	if items := w.get(t, checksPath(n), w.danaKey).json["items"]; !reflect.DeepEqual(items, []any{}) {
		t.Errorf("results of the current version = %v, want none", items)
	}
	if items, _ := w.get(t, checksPath(n)+"?version=1", w.danaKey).json["items"].([]any); len(items) != 1 ||
		items[0].(map[string]any)["status"] != "passed" {
		t.Errorf("results of version 1 = %v, want test passed", items)
	}
	evs := w.eventsAfter(t, seq, eventCheck)
	var meta map[string]any
	if len(evs) != 1 || json.Unmarshal(evs[0].Metadata, &meta) != nil || meta["version"] != 1.0 {
		t.Errorf("check events = %v, want one with metadata.version 1", evs)
	}
}
