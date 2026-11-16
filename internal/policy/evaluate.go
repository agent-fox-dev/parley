package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Select returns the name of the rule of doc that governs file on a change
// into the target branch: the first rule in document order whose targets
// match target, whose paths match file and whose exclude does not, or
// "default" when no rule qualifies.
func Select(doc *Document, target, file string) string {
	return newSelector(doc, target).selectRule(file)
}

// selector selects the rule of each file of one evaluation: the rules whose
// targets match its target branch, decided once, with their globs compiled
// once.
type selector struct {
	rules []compiledRule
	path  *subject
}

// compiledRule is a rule whose targets match, with its globs compiled.
type compiledRule struct {
	name           string
	paths, exclude []pathGlob
}

func newSelector(doc *Document, target string) *selector {
	gs := newGlobSet()
	rules := make([]compiledRule, 0, len(doc.Rules))
	for _, r := range doc.Rules {
		if targetMatches(r.Targets, target) {
			rules = append(rules, compiledRule{name: r.Name, paths: gs.compileAll(r.Paths), exclude: gs.compileAll(r.Exclude)})
		}
	}
	return &selector{rules: rules, path: gs.subject()}
}

// selectRule is Select for file.
func (sel *selector) selectRule(file string) string {
	sel.path.reset(file)
	for _, r := range sel.rules {
		if sel.path.matchAny(r.paths) && !sel.path.matchAny(r.exclude) {
			return r.name
		}
	}
	return defaultRule
}

// targetMatches reports whether a rule with the targets globs applies to a
// change into branch: absent targets apply to every branch. A targets entry
// is a branch glob, matched by the one branch-glob matcher of the hub so that
// it means what the same glob means in protected_branches.
func targetMatches(globs []string, branch string) bool {
	return globs == nil || workspace.MatchAnyBranchGlob(globs, branch)
}

// Result is the landing of a change: its status, the structural reason it
// is blocked for (null otherwise), and one requirement per selected rule.
type Result struct {
	Status       string        `json:"status"`
	Reason       *string       `json:"reason"`
	Requirements []Requirement `json:"requirements"`
}

// Requirement is what one selected rule, or the default, demands of a
// change, and whether the change meets it.
type Requirement struct {
	Rule      string    `json:"rule"`
	Paths     []string  `json:"paths"`
	Approvals Approvals `json:"approvals"`
	Checks    []Check   `json:"checks"`
	Block     bool      `json:"block"`
	Satisfied bool      `json:"satisfied"`
}

// Approvals is how many approvals a requirement needs, from whom, and how
// many count.
type Approvals struct {
	Required  int      `json:"required"`
	Counted   int      `json:"counted"`
	Approvers []string `json:"approvers"`
}

// Check is a named check of a requirement with its status.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Evaluate computes the landing of a change from doc: the files of its
// current version, read through files, are grouped by the rule each
// selects; each group becomes a requirement whose approvals and checks are
// counted from ev, asked once per requirement through q. A draft, or a
// change with a structural cause, is blocked whatever its requirements say,
// and they are still listed. A failure to read the files or the evidence,
// or the end of ctx, fails the evaluation as a whole, with the zero Result.
func Evaluate(ctx context.Context, q Querier, doc *Document, ch ChangeFacts, files ChangeFiles, ev Evidence) (Result, error) {
	paths, err := files.Files(ctx, q, ch.Workspace, ch.Number)
	if err != nil {
		return Result{}, fmt.Errorf("read the files of change %d: %w", ch.Number, err)
	}
	groups, err := group(ctx, doc, ch.TargetBranch, paths)
	if err != nil {
		return Result{}, err
	}
	reqs := make([]Requirement, 0)
	for _, g := range groups {
		a, err := ev.Evidence(ctx, q, EvidenceQuery{
			Workspace: ch.Workspace, Number: ch.Number, Version: ch.Version,
			Approvers: g.require.Approvers, Checks: distinct(g.require.Checks), FreshChecks: g.require.FreshChecks,
		})
		if err != nil {
			return Result{}, fmt.Errorf("evidence for rule %q of change %d: %w", g.rule, ch.Number, err)
		}
		reqs = append(reqs, requirement(g, a))
	}
	cause := ch.Cause
	if ch.State == change.StateDraft {
		cause = new(reasonDraft)
	}
	return result(reqs, cause), nil
}

// Preview computes what the files would need on a change into target under
// doc, before or apart from any change: no evidence is asked, so nothing is
// counted and every check is pending, and no structural cause applies. The
// end of ctx stops it with the zero Result and ctx's error, so that a
// request its caller abandoned stops costing the hub.
func Preview(ctx context.Context, doc *Document, target string, files []string) (Result, error) {
	groups, err := group(ctx, doc, target, files)
	if err != nil {
		return Result{}, err
	}
	reqs := make([]Requirement, 0)
	for _, g := range groups {
		reqs = append(reqs, requirement(g, EvidenceAnswer{}))
	}
	return result(reqs, nil), nil
}

// Landing is r as the change record stores it.
func (r Result) Landing() (change.Landing, error) {
	reqs := make([]json.RawMessage, 0, len(r.Requirements))
	for _, q := range r.Requirements {
		b, err := json.Marshal(q)
		if err != nil {
			return change.Landing{}, err
		}
		reqs = append(reqs, b)
	}
	return change.Landing{Status: r.Status, Reason: r.Reason, Requirements: reqs}, nil
}

// MarshalJSON writes the result with requirements never null.
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	if r.Requirements == nil {
		r.Requirements = []Requirement{}
	}
	return json.Marshal(plain(r))
}

// maxRequirementPaths is how many of its files a requirement lists.
const maxRequirementPaths = 50

// selection is the files that selected one rule, or the default.
type selection struct {
	rule    string
	require Require
	files   []string
}

// group selects a rule for each file and returns one selection per rule
// selected, in document order with the default last, each with its files in
// ascending byte order, a repeated file once, cut to the first
// maxRequirementPaths. It looks at ctx before each file, and returns its
// error once it has ended.
func group(ctx context.Context, doc *Document, target string, files []string) ([]selection, error) {
	sel := newSelector(doc, target)
	byRule := map[string][]string{}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r := sel.selectRule(f)
		byRule[r] = append(byRule[r], f)
	}
	var out []selection
	add := func(rule string, req Require) {
		fs, ok := byRule[rule]
		if !ok {
			return
		}
		slices.Sort(fs)
		fs = slices.Compact(fs)
		out = append(out, selection{rule: rule, require: req, files: fs[:min(len(fs), maxRequirementPaths)]})
	}
	for _, r := range doc.Rules {
		add(r.Name, r.Require)
	}
	add(defaultRule, doc.Default)
	return out, nil
}

// requirement is the requirement of a selection given its evidence. A named
// check with no result is pending.
func requirement(g selection, a EvidenceAnswer) Requirement {
	r := Requirement{
		Rule:  g.rule,
		Paths: g.files,
		Approvals: Approvals{
			Required:  g.require.Approvals,
			Counted:   a.Counted,
			Approvers: append([]string{}, g.require.Approvers...),
		},
		Checks: []Check{},
		Block:  g.require.Block,
	}
	passed := true
	for _, name := range distinct(g.require.Checks) {
		status, ok := a.Checks[name]
		if !ok {
			status = checkPending
		}
		passed = passed && status == checkPassed
		r.Checks = append(r.Checks, Check{Name: name, Status: status})
	}
	r.Satisfied = !r.Block && a.Counted >= r.Approvals.Required && !a.RejectionOutstanding && passed
	return r
}

// reasonDraft is the reason a draft is blocked for, which wins over any
// structural cause: a draft is not measured for landing at all.
const reasonDraft = "draft"

// The statuses of a check.
const (
	checkPassed  = "passed"
	checkPending = "pending"
)

// distinct is names with each name once, at its first position.
func distinct(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// result derives the status of reqs: blocked with cause when there is one,
// else blocked when a requirement blocks, pending when one is unsatisfied,
// ready when every one is satisfied or there is none.
func result(reqs []Requirement, cause *string) Result {
	res := Result{Status: change.LandingReady, Requirements: reqs}
	switch {
	case cause != nil:
		res.Status, res.Reason = change.LandingBlocked, cause
	case slices.ContainsFunc(reqs, func(r Requirement) bool { return r.Block }):
		res.Status = change.LandingBlocked
	case slices.ContainsFunc(reqs, func(r Requirement) bool { return !r.Satisfied }):
		res.Status = change.LandingPending
	}
	return res
}
