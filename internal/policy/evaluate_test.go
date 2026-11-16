package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
)

func TestTargetMatches_TakesTheBranchGlobMatcher(t *testing.T) {
	for _, tc := range []struct {
		globs  []string
		branch string
		want   bool
	}{
		{nil, "anything/at/all", true},
		{[]string{"main"}, "main", true},
		{[]string{"main"}, "main2", false},
		{[]string{"release/*"}, "release/1.2", true},
		{[]string{"release/*"}, "release/1/2", false},
		{[]string{"release/**"}, "release/1/2", true},
		{[]string{"*"}, "main", true},
		{[]string{"*"}, "feature/x", false},
		{[]string{"**"}, "feature/x", true},
		{[]string{"rel*"}, "release", true},
		{[]string{"main", "release/*"}, "release/2", true},
	} {
		if got := targetMatches(tc.globs, tc.branch); got != tc.want {
			t.Errorf("targetMatches(%q, %q) = %v, want %v", tc.globs, tc.branch, got, tc.want)
		}
	}
}

func TestSelect_TakesTheFirstQualifyingRuleOrTheDefault(t *testing.T) {
	doc := &Document{Default: NeverSet.Default, Rules: []Rule{
		{Name: "docs", Paths: []string{"docs/**", "**/*.md"}, Exclude: []string{"docs/internal/**"}},
		{Name: "billing", Paths: []string{"billing/**"}, Exclude: []string{}, Targets: []string{"main", "release/*"}},
		{Name: "all-billing", Paths: []string{"billing/**"}, Exclude: []string{}},
		{Name: "secrets", Paths: []string{"deploy/secrets/**"}, Exclude: []string{}},
	}}
	for _, tc := range []struct {
		doc          *Document
		file, target string
		want         string
	}{
		{doc, "docs/a.txt", "main", "docs"},
		{doc, "billing/README.md", "main", "docs"},
		{doc, "docs/internal/x.txt", "main", "default"},
		{doc, "billing/a.go", "main", "billing"},
		{doc, "billing/a.go", "release/2", "billing"},
		{doc, "billing/a.go", "dev", "all-billing"},
		{doc, "src/a.go", "main", "default"},
		{doc, "deploy/secrets/k", "dev", "secrets"},
		{&NeverSet, "docs/a.txt", "main", "default"},
		{&NeverSet, "billing/a.go", "dev", "default"},
	} {
		if got := Select(tc.doc, tc.target, tc.file); got != tc.want {
			t.Errorf("Select(%d rules, %q, %q) = %q, want %q", len(tc.doc.Rules), tc.target, tc.file, got, tc.want)
		}
	}
}

// filesStub answers the files of a change, or fails with err.
type filesStub struct {
	files []string
	err   error
}

func (f filesStub) Files(context.Context, Querier, string, int64) ([]string, error) {
	return f.files, f.err
}

// evidenceStub answers every question with answer, fails the failAt-th one
// (counted from 1) with err, and records what it was asked.
type evidenceStub struct {
	answer EvidenceAnswer
	failAt int
	err    error
	asked  []EvidenceQuery
}

func (e *evidenceStub) Evidence(_ context.Context, _ Querier, in EvidenceQuery) (EvidenceAnswer, error) {
	e.asked = append(e.asked, in)
	if len(e.asked) == e.failAt {
		return EvidenceAnswer{}, e.err
	}
	return e.answer, nil
}

// example is a document with a rule for docs, one for billing on some
// targets, one that blocks secrets, and a default that names defaultChecks.
func example(defaultChecks ...string) *Document {
	nothing := Require{Approvers: []string{"owner"}, Checks: []string{}}
	return &Document{Default: Require{Approvers: []string{"owner"}, Checks: append([]string{}, defaultChecks...)}, Rules: []Rule{
		{Name: "docs", Paths: []string{"docs/**", "**/*.md"}, Exclude: []string{"docs/internal/**"}, Require: nothing},
		{Name: "billing", Paths: []string{"billing/**"}, Exclude: []string{}, Targets: []string{"main", "release/*"},
			Require: Require{Approvals: 1, Approvers: []string{"owner", "user:dana"}, Checks: []string{"test", "billing-e2e"}, FreshChecks: true}},
		{Name: "secrets", Paths: []string{"deploy/secrets/**"}, Exclude: []string{}, Require: Require{Approvers: []string{"owner"}, Checks: []string{}, Block: true}},
	}}
}

// openChange is an open change into main with no structural cause.
var openChange = ChangeFacts{Workspace: "api", Number: 7, Version: 3, State: change.StateOpen, TargetBranch: "main", Mergeable: "clean"}

func evaluate(t *testing.T, doc *Document, ch ChangeFacts, files []string, ev Evidence) Result {
	t.Helper()
	res, err := Evaluate(context.Background(), nil, doc, ch, filesStub{files: files}, ev)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return res
}

func requirementOf(t *testing.T, res Result, rule string) Requirement {
	t.Helper()
	for _, r := range res.Requirements {
		if r.Rule == rule {
			return r
		}
	}
	t.Fatalf("no requirement %q in %+v", rule, res.Requirements)
	return Requirement{}
}

func deref(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

func rules(res Result) []string {
	names := []string{}
	for _, r := range res.Requirements {
		names = append(names, r.Rule)
	}
	return names
}

func TestEvaluate_ListsOneRequirementPerSelectedRule(t *testing.T) {
	res := evaluate(t, example("test"), openChange, []string{"src/a.go", "billing/b.go", "billing/a.go", "docs/x.md"}, NoEvidence)
	want := []Requirement{
		{Rule: "docs", Paths: []string{"docs/x.md"}, Approvals: Approvals{Approvers: []string{"owner"}}, Checks: []Check{}, Satisfied: true},
		{Rule: "billing", Paths: []string{"billing/a.go", "billing/b.go"}, Approvals: Approvals{Required: 1, Approvers: []string{"owner", "user:dana"}},
			Checks: []Check{{"test", "pending"}, {"billing-e2e", "pending"}}},
		{Rule: "default", Paths: []string{"src/a.go"}, Approvals: Approvals{Approvers: []string{"owner"}}, Checks: []Check{{"test", "pending"}}},
	}
	if !reflect.DeepEqual(res.Requirements, want) {
		t.Errorf("requirements = %+v\nwant %+v", res.Requirements, want)
	}

	var many, lowest []string
	for i := 119; i >= 0; i-- {
		many = append(many, fmt.Sprintf("billing/f%03d.go", i))
	}
	for i := range 50 {
		lowest = append(lowest, fmt.Sprintf("billing/f%03d.go", i))
	}
	if got := requirementOf(t, evaluate(t, example(), openChange, many, NoEvidence), "billing").Paths; !reflect.DeepEqual(got, lowest) {
		t.Errorf("paths of 120 files = %q, want the 50 lowest %q", got, lowest)
	}

	twice := &Document{Default: NeverSet.Default, Rules: []Rule{{Name: "ci", Paths: []string{"**"}, Exclude: []string{},
		Require: Require{Approvers: []string{"owner"}, Checks: []string{"test", "lint", "test"}}}}}
	if got := requirementOf(t, evaluate(t, twice, openChange, []string{"a"}, NoEvidence), "ci").Checks; !reflect.DeepEqual(got, []Check{{"test", "pending"}, {"lint", "pending"}}) {
		t.Errorf("checks of a rule naming test twice = %+v", got)
	}
}

func TestEvaluate_SatisfiesARequirementOnItsEvidence(t *testing.T) {
	gate := Require{Approvals: 1, Approvers: []string{"owner", "user:dana"}, Checks: []string{"test", "e2e"}, FreshChecks: true}
	both := func(test, e2e string) map[string]string { return map[string]string{"test": test, "e2e": e2e} }
	for _, tc := range []struct {
		name       string
		require    Require
		answer     EvidenceAnswer
		satisfied  bool
		wantChecks []Check
	}{
		{"no approval counted", gate, EvidenceAnswer{0, false, both("passed", "passed")}, false, []Check{{"test", "passed"}, {"e2e", "passed"}}},
		{"approved, checks passed", gate, EvidenceAnswer{1, false, both("passed", "passed")}, true, []Check{{"test", "passed"}, {"e2e", "passed"}}},
		{"more approvals than required", gate, EvidenceAnswer{2, false, both("passed", "passed")}, true, []Check{{"test", "passed"}, {"e2e", "passed"}}},
		{"a rejection outstanding", gate, EvidenceAnswer{1, true, both("passed", "passed")}, false, []Check{{"test", "passed"}, {"e2e", "passed"}}},
		{"a check failed", gate, EvidenceAnswer{1, false, both("passed", "failed")}, false, []Check{{"test", "passed"}, {"e2e", "failed"}}},
		{"a check pending", gate, EvidenceAnswer{1, false, both("passed", "pending")}, false, []Check{{"test", "passed"}, {"e2e", "pending"}}},
		{"a check with no result", gate, EvidenceAnswer{1, false, map[string]string{"test": "passed"}}, false, []Check{{"test", "passed"}, {"e2e", "pending"}}},
		{"nothing required", Require{Approvers: []string{"owner"}, Checks: []string{}}, EvidenceAnswer{}, true, []Check{}},
		{"a block", Require{Approvers: []string{"owner"}, Checks: []string{}, Block: true}, EvidenceAnswer{Counted: 5}, false, []Check{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := &Document{Default: NeverSet.Default, Rules: []Rule{{Name: "r", Paths: []string{"**"}, Exclude: []string{}, Require: tc.require}}}
			ev := &evidenceStub{answer: tc.answer}
			got := requirementOf(t, evaluate(t, doc, openChange, []string{"a.go"}, ev), "r")
			if got.Satisfied != tc.satisfied || got.Block != tc.require.Block || got.Approvals.Counted != tc.answer.Counted || !reflect.DeepEqual(got.Checks, tc.wantChecks) {
				t.Errorf("requirement = %+v, want satisfied %v, block %v, counted %d, checks %+v", got, tc.satisfied, tc.require.Block, tc.answer.Counted, tc.wantChecks)
			}
			want := []EvidenceQuery{{Workspace: "api", Number: 7, Version: 3, Approvers: tc.require.Approvers, Checks: tc.require.Checks, FreshChecks: tc.require.FreshChecks}}
			if !reflect.DeepEqual(ev.asked, want) {
				t.Errorf("evidence asked %+v, want %+v", ev.asked, want)
			}
		})
	}
}

func TestEvaluate_DerivesTheStatusFromTheRequirements(t *testing.T) {
	doc := &Document{Default: NeverSet.Default, Rules: []Rule{
		{Name: "ok", Paths: []string{"ok/**"}, Exclude: []string{}, Require: NeverSet.Default},
		{Name: "wait", Paths: []string{"wait/**"}, Exclude: []string{}, Require: Require{Approvals: 1, Approvers: []string{"owner"}, Checks: []string{}}},
		{Name: "stop", Paths: []string{"stop/**"}, Exclude: []string{}, Require: Require{Approvers: []string{"owner"}, Checks: []string{}, Block: true}},
	}}
	for _, tc := range []struct {
		files []string
		want  string
	}{
		{[]string{"ok/a", "elsewhere/b"}, change.LandingReady},
		{[]string{"ok/a", "wait/a"}, change.LandingPending},
		{[]string{"wait/a", "stop/a"}, change.LandingBlocked},
		{[]string{"stop/a"}, change.LandingBlocked},
		{[]string{"ok/a", "stop/a"}, change.LandingBlocked},
	} {
		res := evaluate(t, doc, openChange, tc.files, NoEvidence)
		if res.Status != tc.want || res.Reason != nil {
			t.Errorf("files %q: status %q, reason %v, want %q and no reason", tc.files, res.Status, res.Reason, tc.want)
		}
	}
}

func TestEvaluate_BlocksForADraftOrAStructuralCauseAndStillListsTheRequirements(t *testing.T) {
	satisfiedDefault := []Requirement{{Rule: "default", Paths: []string{"docs/x.md"}, Approvals: Approvals{Approvers: []string{"owner"}}, Checks: []Check{}, Satisfied: true}}
	for _, tc := range []struct {
		state      string
		cause      *string
		wantStatus string
		wantReason *string
	}{
		{change.StateDraft, nil, change.LandingBlocked, new("draft")},
		{change.StateOpen, new("nothing_to_land"), change.LandingBlocked, new("nothing_to_land")},
		{change.StateOpen, new("target_not_found"), change.LandingBlocked, new("target_not_found")},
		{change.StateOpen, new("merge_commits"), change.LandingBlocked, new("merge_commits")},
		{change.StateOpen, new("unrelated_history"), change.LandingBlocked, new("unrelated_history")},
		{change.StateDraft, new("merge_commits"), change.LandingBlocked, new("draft")},
		{change.StateOpen, nil, change.LandingReady, nil},
	} {
		ch := openChange
		ch.State, ch.Cause = tc.state, tc.cause
		res := evaluate(t, &NeverSet, ch, []string{"docs/x.md"}, NoEvidence)
		if res.Status != tc.wantStatus || !reflect.DeepEqual(res.Reason, tc.wantReason) || !reflect.DeepEqual(res.Requirements, satisfiedDefault) {
			t.Errorf("%s with cause %s: %+v (reason %s), want %s, reason %s, the satisfied default",
				tc.state, deref(tc.cause), res, deref(res.Reason), tc.wantStatus, deref(tc.wantReason))
		}
	}
}

func TestEvaluate_IsReadyWithNoFileOrUnderTheNeverSetDocument(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doc   *Document
		files []string
		want  []Requirement
	}{
		{"never set, no file", &NeverSet, nil, []Requirement{}},
		{"never set, two files", &NeverSet, []string{"b/c", "a"},
			[]Requirement{{Rule: "default", Paths: []string{"a", "b/c"}, Approvals: Approvals{Approvers: []string{"owner"}}, Checks: []Check{}, Satisfied: true}}},
		{"three rules, no file", example("test"), []string{}, []Requirement{}},
	} {
		res := evaluate(t, tc.doc, openChange, tc.files, NoEvidence)
		if res.Status != change.LandingReady || res.Reason != nil || res.Requirements == nil || !reflect.DeepEqual(res.Requirements, tc.want) {
			t.Errorf("%s: %+v, want ready with %+v", tc.name, res, tc.want)
		}
	}
}

func TestEvaluate_CountsNothingWithNoEvidence(t *testing.T) {
	res := evaluate(t, example(), openChange, []string{"docs/a.md", "billing/a.go"}, NoEvidence)
	docs, billing := requirementOf(t, res, "docs"), requirementOf(t, res, "billing")
	if res.Status != change.LandingPending || !docs.Satisfied || billing.Satisfied || billing.Approvals.Counted != 0 ||
		!reflect.DeepEqual(billing.Checks, []Check{{"test", "pending"}, {"billing-e2e", "pending"}}) {
		t.Errorf("docs and billing: %+v", res)
	}
	for files, want := range map[string]string{"docs/a.md": change.LandingReady, "deploy/secrets/k": change.LandingBlocked} {
		if got := evaluate(t, example(), openChange, []string{files}, NoEvidence).Status; got != want {
			t.Errorf("%s: status %q, want %q", files, got, want)
		}
	}
}

func TestEvidenceSlot_HoldsNoEvidenceUntilASourceIsRegistered(t *testing.T) {
	var slot EvidenceSlot
	if slot.InstalledEvidence() != NoEvidence {
		t.Errorf("zero slot holds %T, want NoEvidence", slot.InstalledEvidence())
	}
	stub := &evidenceStub{}
	slot.RegisterEvidence(stub)
	if slot.InstalledEvidence() != Evidence(stub) {
		t.Errorf("after registering, slot holds %T", slot.InstalledEvidence())
	}
	slot.RegisterEvidence(nil)
	if slot.InstalledEvidence() != NoEvidence {
		t.Errorf("after registering nil, slot holds %T, want NoEvidence", slot.InstalledEvidence())
	}
}

func TestEvaluate_FailsAsAWholeWhenAReadFails(t *testing.T) {
	boom := errors.New("boom")
	files := []string{"docs/a.md", "billing/a.go", "src/a.go"}
	for _, tc := range []struct {
		name  string
		files filesStub
		ev    *evidenceStub
	}{
		{"evidence fails on the second requirement", filesStub{files: files}, &evidenceStub{failAt: 2, err: boom}},
		{"files fail", filesStub{err: boom}, &evidenceStub{}},
	} {
		res, err := Evaluate(context.Background(), nil, example(), openChange, tc.files, tc.ev)
		if !errors.Is(err, boom) || !reflect.DeepEqual(res, Result{}) {
			t.Errorf("%s: res %+v, err %v; want the zero result and boom", tc.name, res, err)
		}
	}
}

func TestPreview_EvaluatesWithoutEvidenceOrCause(t *testing.T) {
	for _, files := range [][]string{
		{"docs/a.md"},
		{"billing/a.go", "docs/a.md"},
		{"deploy/secrets/k", "src/a.go"},
	} {
		got, err := Preview(context.Background(), example("test"), "main", files)
		if err != nil {
			t.Fatalf("Preview(%q): %v", files, err)
		}
		if want := evaluate(t, example("test"), openChange, files, NoEvidence); !reflect.DeepEqual(got, want) {
			t.Errorf("Preview(%q) = %+v, want %+v", files, got, want)
		}
	}
	got, err := Preview(context.Background(), example(), "dev", []string{"billing/a.go", "docs/a.md", "billing/a.go"})
	if err != nil || !reflect.DeepEqual(rules(got), []string{"docs", "default"}) || !reflect.DeepEqual(requirementOf(t, got, "default").Paths, []string{"billing/a.go"}) {
		t.Errorf("Preview on dev with a repeated path: %+v", got)
	}
}

// crafted is a document of 190 rules, each one glob of 255 characters made
// of glob, and 2 000 distinct paths of 450 characters that no rule matches:
// the most an evaluation's body lets a caller ask the matcher.
func crafted(glob func(rule int) string) (*Document, []string) {
	doc := &Document{Default: NeverSet.Default, Rules: []Rule{}}
	for i := range 190 {
		doc.Rules = append(doc.Rules, Rule{Name: fmt.Sprintf("r%d", i), Paths: []string{glob(i)}, Exclude: []string{}})
	}
	files := make([]string, 0, 2000)
	for i := range 2000 {
		files = append(files, fmt.Sprintf("%s%04d", strings.Repeat("a", 446), i))
	}
	return doc, files
}

func TestPreview_EvaluatesTheLargestRequestInBoundedTime(t *testing.T) {
	for name, glob := range map[string]func(int) string{
		// fails at its first character
		"dead at once": func(int) string { return "b" + strings.Repeat("*a", 127) },
		// keeps every position alive to its last character
		"alive to the end": func(i int) string { return strings.Repeat("*a", 126) + "*" + string(rune('b'+i%20)) },
	} {
		doc, files := crafted(glob)
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			res, err := Preview(context.Background(), doc, "main", files)
			if err == nil && !reflect.DeepEqual(rules(res), []string{"default"}) {
				err = fmt.Errorf("rules %q, want only the default", rules(res))
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			t.Logf("%s: %v", name, time.Since(start))
		case <-time.After(previewBound):
			t.Fatalf("%s: Preview has not returned after %v", name, previewBound)
		}
	}
}

// previewBound is how long the largest evaluation may take, many times what
// it takes on a loaded machine.
const previewBound = 10 * time.Second

func TestPreview_StopsWhenTheContextEnds(t *testing.T) {
	doc, files := crafted(func(int) string { return strings.Repeat("*a", 126) + "*b" })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Preview(ctx, doc, "main", files)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(res, Result{}) {
		t.Errorf("Preview after the context ended = status %q with %d requirements, %v; want the zero result and context.Canceled", res.Status, len(res.Requirements), err)
	}
}

func TestResult_MarshalsAsTheLandingOfAChange(t *testing.T) {
	res := Result{Status: change.LandingPending, Requirements: []Requirement{{Rule: "billing", Paths: []string{"billing/a.go"},
		Approvals: Approvals{Required: 1, Approvers: []string{"owner", "user:dana"}}, Checks: []Check{{"test", "pending"}}}}}
	wantReq := `{"rule":"billing","paths":["billing/a.go"],"approvals":{"required":1,"counted":0,"approvers":["owner","user:dana"]},` +
		`"checks":[{"name":"test","status":"pending"}],"block":false,"satisfied":false}`
	for _, tc := range []struct {
		res  Result
		want string
	}{
		{res, `{"status":"pending","reason":null,"requirements":[` + wantReq + `]}`},
		{Result{Status: change.LandingBlocked, Reason: new("draft")}, `{"status":"blocked","reason":"draft","requirements":[]}`},
	} {
		b, err := json.Marshal(tc.res)
		if err != nil || string(b) != tc.want {
			t.Errorf("Marshal = %s, %v; want %s", b, err, tc.want)
		}
		l, err := tc.res.Landing()
		if err != nil {
			t.Fatalf("Landing: %v", err)
		}
		if b, _ := json.Marshal(l); string(b) != tc.want {
			t.Errorf("Landing marshals as %s, want %s", b, tc.want)
		}
	}
}
