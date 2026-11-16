package policy

import "context"

// Facts is the evidence source of the verdicts the component records,
// which the hub registers in place of NoEvidence (RegisterEvidence). It
// reads the component's dependencies when it is asked, so it may be made
// before Bind.
func (c *Component) Facts() Evidence { return facts{c} }

// facts answers the evaluator from the verdicts stored on the version it
// asks about, carried copies included.
type facts struct{ c *Component }

// Evidence counts the verdicts on in.Version, read through q, the caller's
// transaction: the approve verdicts that count toward a requirement whose
// approvers are in.Approvers, and whether a reject verdict counts toward
// it. The version is always the one asked, never the change's current
// one, since a land of an older version is judged with that version's
// evidence; a verdict on any other version contributes nothing.
func (f facts) Evidence(ctx context.Context, q Querier, in EvidenceQuery) (EvidenceAnswer, error) {
	verdicts, err := readVerdicts(ctx, q, in.Workspace, in.Number, verdictFilter{Version: in.Version})
	if err != nil || len(verdicts) == 0 {
		return EvidenceAnswer{}, err
	}
	k, err := f.c.newCounter(ctx, q, in.Workspace, in.Number, in.Version, nil)
	if err != nil {
		return EvidenceAnswer{}, err
	}
	var a EvidenceAnswer
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
