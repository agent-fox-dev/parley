package policy

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/change"
)

// Facts is the evidence source of the verdicts and check results the
// component records, which the hub registers in place of NoEvidence
// (RegisterEvidence). It reads the component's dependencies when it is
// asked, so it may be made before Bind.
func (c *Component) Facts() Evidence { return facts{c} }

// facts answers the evaluator from the verdicts stored on the version it
// asks about, carried copies included, and from the check results that
// apply to that version.
type facts struct{ c *Component }

// Evidence counts the verdicts on in.Version, read through q, the caller's
// transaction: the approve verdicts that count toward a requirement whose
// approvers are in.Approvers, and whether a reject verdict counts toward
// it; and it gives the status of each check in.Checks names that has a
// result applying to in.Version. The version is always the one asked,
// never the change's current one, since a land of an older version is
// judged with that version's evidence; a verdict on any other version
// contributes nothing.
func (f facts) Evidence(ctx context.Context, q Querier, in EvidenceQuery) (EvidenceAnswer, error) {
	checks, err := f.checkStatuses(ctx, q, in)
	if err != nil {
		return EvidenceAnswer{}, err
	}
	a := EvidenceAnswer{Checks: checks}
	verdicts, err := readVerdicts(ctx, q, in.Workspace, in.Number, verdictFilter{Version: in.Version})
	if err != nil || len(verdicts) == 0 {
		return a, err
	}
	k, err := f.c.newCounter(ctx, q, in.Workspace, in.Number, in.Version, nil)
	if err != nil {
		return EvidenceAnswer{}, err
	}
	for _, v := range verdicts {
		ok, err := k.counts(ctx, v, in.Approvers)
		switch {
		case err != nil:
			return EvidenceAnswer{}, err
		case !ok:
		case v.Verdict == verdictApprove:
			a.Counted++
		case v.Verdict == verdictReject:
			a.RejectionOutstanding = true
		}
	}
	return a, nil
}

// checkStatuses is the status of each check in.Checks names that has a
// result applying to in.Version, read through q; a name with none is
// left out, which the evaluator reads as pending. With in.FreshChecks
// only a result reported on in.Version itself applies, so the versions
// it reaches are not read.
func (f facts) checkStatuses(ctx context.Context, q Querier, in EvidenceQuery) (map[string]string, error) {
	if len(in.Checks) == 0 {
		return nil, nil
	}
	results, err := readChecks(ctx, q, in.Workspace, in.Number)
	if err != nil || len(results) == 0 {
		return nil, err
	}
	var versions []change.Version
	if !in.FreshChecks {
		if versions, err = f.c.deps.Changes.ReadVersions(ctx, q, in.Workspace, in.Number); err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for _, name := range in.Checks {
		if r, ok := applying(versions, results, in.Version, name); ok {
			out[name] = r.Status
		}
	}
	return out, nil
}
