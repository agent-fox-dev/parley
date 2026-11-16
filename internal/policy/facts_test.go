package policy_test

import (
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/policy"
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

func TestFacts_EachWriteOfEvidenceReevaluatesTheLanding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["owner"], "checks": ["test"]}}`)
	open := w.openPushed(t, "work").number
	draft := w.openPushed(t, "draft", func(in *change.OpenInput) { in.Draft = true }).number
	// approved is the id of dana's last approval on each change.
	approved := map[int64]string{}
	steps := []struct {
		name string
		do   func(t *testing.T, n int64)
		// want is the landing's status on the open change.
		want string
	}{
		{"no evidence", func(*testing.T, int64) {}, change.LandingPending},
		{"test passed", func(t *testing.T, n int64) { w.report(t, n, "test", map[string]any{"status": "passed"}) },
			change.LandingPending},
		{"dana approves", func(t *testing.T, n int64) { approved[n] = w.give(t, n, w.danaKey, approves)["id"].(string) },
			change.LandingReady},
		{"dana withdraws her approval", func(t *testing.T, n int64) {
			if r := w.withdraw(t, n, approved[n], w.danaKey); r.status != http.StatusNoContent {
				t.Fatalf("withdraw = %d %s, want 204", r.status, r.body)
			}
		}, change.LandingPending},
		{"dana approves again", func(t *testing.T, n int64) { w.give(t, n, w.danaKey, approves) }, change.LandingReady},
		{"test failed", func(t *testing.T, n int64) { w.report(t, n, "test", map[string]any{"status": "failed"}) },
			change.LandingPending},
		{"dana rejects instead", func(t *testing.T, n int64) { w.give(t, n, w.danaKey, rejects) }, change.LandingPending},
		{"test passed again", func(t *testing.T, n int64) { w.report(t, n, "test", map[string]any{"status": "passed"}) },
			change.LandingPending},
	}
	for _, s := range steps {
		s.do(t, open)
		if l := w.landing(t, open); l["status"] != s.want {
			t.Errorf("after %s: landing of the open change %v, want %s", s.name, l, s.want)
		}
		s.do(t, draft)
		if l := w.landing(t, draft); l["status"] != change.LandingBlocked || l["reason"] != "draft" {
			t.Errorf("after %s: landing of the draft %v, want blocked for draft", s.name, l)
		}
	}
}

// downWriter passes the policy component's events on, and refuses every
// one while down is set, as an event log that cannot be written.
type downWriter struct {
	policy.EventWriter
	down atomic.Bool
}

func (d *downWriter) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	if d.down.Load() {
		return event.Record{}, errors.New("writer down")
	}
	return d.EventWriter.Emit(ctx, tx, in)
}

// rows is every row of the store table table, each as its column values,
// in the order of its first columns.
func (w *world) rows(t *testing.T, table string) [][]any {
	t.Helper()
	rs, err := w.Store.Main().Query(`SELECT * FROM ` + table + ` ORDER BY 1, 2, 3, 4`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rs.Next() {
		row, ptrs := make([]any, len(cols)), make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEvidenceWrites_AFailingPartRollsTheWholeWriteBack(t *testing.T) {
	t.Parallel()
	writer := &downWriter{}
	w := newWorld(t, func(d *policy.Deps) { writer.EventWriter, d.Events = d.Events, writer })
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["owner"], "checks": ["test"]}}`)
	n := w.openPushed(t, "work").number
	approved := w.give(t, n, w.danaKey, approves)["id"].(string)
	w.report(t, n, "test", map[string]any{"status": "passed"})
	// Once the mergeability is known nothing writes the change again
	// until a write of the test, which leaves its landing ready.
	w.checked(t, "api", n)
	if l := w.landing(t, n); l["status"] != change.LandingReady {
		t.Fatalf("landing before the writes = %v, want ready", l)
	}
	triggers := map[string]string{
		"refuse_verdict_insert": `BEFORE INSERT ON change_approvals`, "refuse_verdict_delete": `BEFORE DELETE ON change_approvals`,
		"refuse_check_insert": `BEFORE INSERT ON change_checks`, "refuse_check_update": `BEFORE UPDATE ON change_checks`,
	}
	exec := func(t *testing.T, stmt string) {
		t.Helper()
		if _, err := w.Store.Main().Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	parts := []struct {
		name, cause string
		fail        func(t *testing.T) (restore func())
	}{
		{"the event writer fails", "writer down", func(*testing.T) func() {
			writer.down.Store(true)
			return func() { writer.down.Store(false) }
		}},
		{"the evaluator fails", "the evidence cannot be read", func(*testing.T) func() {
			w.policy.RegisterEvidence(&evidence{fail: n})
			return func() { w.policy.RegisterEvidence(w.policy.Facts()) }
		}},
		{"the row write fails", "refused", func(t *testing.T) func() {
			for name, when := range triggers {
				exec(t, `CREATE TRIGGER `+name+` `+when+` BEGIN SELECT RAISE(ABORT, 'refused'); END`)
			}
			return func() {
				for name := range triggers {
					exec(t, `DROP TRIGGER `+name)
				}
			}
		}},
	}
	writes := []struct {
		name string
		// emits is whether the write records an event.
		emits bool
		send  func(t *testing.T) answer
	}{
		{"record a verdict", true, func(t *testing.T) answer { return w.postVerdict(t, n, w.danaKey, rejects) }},
		{"withdraw a verdict", false, func(t *testing.T) answer { return w.withdraw(t, n, approved, w.danaKey) }},
		{"report a check", true, func(t *testing.T) answer {
			return w.putCheck(t, n, "test", w.danaKey, map[string]any{"status": "failed"})
		}},
	}
	for _, write := range writes {
		for _, part := range parts {
			if part.cause == "writer down" && !write.emits {
				continue
			}
			t.Run(write.name+", "+part.name, func(t *testing.T) {
				verdicts, checks, landing, seq := w.rows(t, "change_approvals"), w.rows(t, "change_checks"), w.landing(t, n), w.seq(t)
				restore := part.fail(t)
				r := write.send(t)
				restore()
				if r.status != http.StatusInternalServerError || r.errorType() != "internal" || strings.Contains(string(r.body), part.cause) {
					t.Errorf("answer = %d %s, want 500 internal without %q", r.status, r.body, part.cause)
				}
				if got := w.rows(t, "change_approvals"); !reflect.DeepEqual(got, verdicts) {
					t.Errorf("verdicts = %v, want them as before: %v", got, verdicts)
				}
				if got := w.rows(t, "change_checks"); !reflect.DeepEqual(got, checks) {
					t.Errorf("check results = %v, want them as before: %v", got, checks)
				}
				if got := w.landing(t, n); !reflect.DeepEqual(got, landing) {
					t.Errorf("landing = %v, want it as before: %v", got, landing)
				}
				if evs := w.eventsAfter(t, seq, ""); len(evs) != 0 {
					t.Errorf("events committed = %v, want none", evs)
				}
			})
		}
	}
}

func TestEvidenceWrites_ConcurrentWritesOfOneVerdictOrOneCheckBothSucceed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["member"], "checks": ["test"]}}`)
	n := w.openPushed(t, "work").number
	reports := []struct{ name, status string }{{"test", "passed"}, {"test", "failed"}, {"lint", "passed"}}
	for round := range 20 {
		var (
			wg       sync.WaitGroup
			verdicts [2]answer
			checks   [3]answer
		)
		for i, body := range []map[string]any{approves, rejects} {
			wg.Go(func() { verdicts[i] = w.postVerdict(t, n, w.melKey, body) })
		}
		wg.Wait()
		answered := []int{verdicts[0].status, verdicts[1].status}
		slices.Sort(answered)
		if !slices.Equal(answered, []int{http.StatusOK, http.StatusCreated}) {
			t.Fatalf("round %d: verdicts = %d %s and %d %s, want one 201 and one 200",
				round, verdicts[0].status, verdicts[0].body, verdicts[1].status, verdicts[1].body)
		}
		kept := verdicts[0]
		if verdicts[1].status == http.StatusOK {
			kept = verdicts[1]
		}
		if items := w.get(t, approvalsPath(n), w.danaKey).json["items"].([]any); len(items) != 1 || !reflect.DeepEqual(items[0], kept.json) {
			t.Errorf("round %d: listed verdicts %v, want only the one answered 200: %v", round, items, kept.json)
		}

		for i, c := range reports {
			wg.Go(func() { checks[i] = w.putCheck(t, n, c.name, w.danaKey, map[string]any{"status": c.status}) })
		}
		wg.Wait()
		for i, r := range checks {
			if r.status != http.StatusOK {
				t.Fatalf("round %d: report %v = %d %s, want 200", round, reports[i], r.status, r.body)
			}
		}
		stored := w.storedResult(t, n, 1, "test")
		var tests []any
		for _, item := range w.get(t, checksPath(n), w.danaKey).json["items"].([]any) {
			if item.(map[string]any)["name"] == "test" {
				tests = append(tests, item)
			}
		}
		if len(tests) != 1 || tests[0].(map[string]any)["status"] != stored["status"] {
			t.Errorf("round %d: listed test results %v, want one with the stored status %v", round, tests, stored["status"])
		}
		// The stored evidence evaluates to: mel's verdict counted when it
		// approves, test with its stored status, ready only with both.
		counted, status := 0.0, change.LandingPending
		if kept.json["verdict"] == "approve" {
			counted = 1
		}
		if counted == 1 && stored["status"] == "passed" {
			status = change.LandingReady
		}
		l := w.landing(t, n)
		if r := only(l); l["status"] != status || r["approvals"].(map[string]any)["counted"] != counted ||
			!reflect.DeepEqual(r["checks"], statuses("test", stored["status"].(string))) {
			t.Errorf("round %d: landing %v, want %s with %v counted and test %v", round, l, status, counted, stored["status"])
		}
		if r := w.withdraw(t, n, kept.json["id"].(string), w.melKey); r.status != http.StatusNoContent {
			t.Fatalf("round %d: withdraw = %d %s, want 204", round, r.status, r.body)
		}
	}
}

func TestFacts_IncludeAddsTheVerdictsAndResultsOfTheCurrentVersion(t *testing.T) {
	t.Parallel()
	clock := &stepClock{at: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	w := newWorld(t, func(d *policy.Deps) { d.Now = clock.now })
	files := map[string]string{"work.txt": "work\n"}
	n := w.openPushedFiles(t, "work", files).number
	w.give(t, n, w.melKey, approves)
	w.report(t, n, "test", map[string]any{"status": "passed"})
	clock.tick()
	w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, w.danaCaller())
	w.give(t, n, w.danaKey, rejects)
	path := "/api/v1/workspaces/api/changes/" + strconv.FormatInt(n, 10)

	c := w.get(t, path+"?include=approvals,checks", w.danaKey).json
	approvals, _ := c["approvals"].([]any)
	var names []any
	for _, a := range approvals {
		a := a.(map[string]any)
		names = append(names, a["by"].(map[string]any)["username"])
		if a["version"] != 2.0 {
			t.Errorf("approval %v, want it on version 2", a)
		}
	}
	if !reflect.DeepEqual(names, []any{"dana", "mel"}) || approvals[1].(map[string]any)["carried_from"] != 1.0 {
		t.Errorf("approvals = %v, want dana's and then mel's, carried from 1", approvals)
	}
	if listed := w.get(t, approvalsPath(n)+"?version=2", w.danaKey).json["items"]; !reflect.DeepEqual(c["approvals"], listed) {
		t.Errorf("approvals = %v, want the verdicts listed on version 2: %v", c["approvals"], listed)
	}
	if listed := w.get(t, checksPath(n), w.danaKey).json["items"].([]any); len(listed) != 1 || !reflect.DeepEqual(c["checks"], listed) {
		t.Errorf("checks = %v, want the one result listed for the current version: %v", c["checks"], listed)
	}
	for include, want := range map[string][]bool{"": {false, false}, "?include=approvals": {true, false}, "?include=checks": {false, true}} {
		got := w.get(t, path+include, w.danaKey).json
		_, hasApprovals := got["approvals"]
		_, hasChecks := got["checks"]
		if hasApprovals != want[0] || hasChecks != want[1] {
			t.Errorf("GET %s has approvals %v and checks %v, want %v", include, hasApprovals, hasChecks, want)
		}
	}
}
