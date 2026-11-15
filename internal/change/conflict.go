package change

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// restackConflicted stores, in a transaction of its own, that the restack
// of the change ch, whose commits after base up to head were replayed onto
// onto, found the conflict of res.report, and moves nothing: no branch, and
// no restack of the changes above, which still sit on ch as it is. The
// change needs a restack for the conflict, set before its mergeability is
// stored with the report, so that the parley.change.conflict the storing
// function writes, when the change was not in conflict already, says so; a
// change in conflict already gets the new report and no event. The pending
// record goes. When the change got a version, or the commit it must sit on
// moved, since the replay, the conflict is that of a pair the change no
// longer has, and the restack starts again. The result carries the
// report as stored.
func (c *Component) restackConflicted(ctx context.Context, j restackJob, ch Change, res restackResult, onto string) (restackResult, bool, error) {
	again := false
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		now, err := readChange(ctx, tx, j.workspace, j.number)
		if err != nil || !active(now) {
			res.outcome = restackClosed
			return c.ignoreGone(ctx, tx, j, err)
		}
		ontoNow, err := c.sitOn(ctx, tx, now, j.onto)
		if err != nil {
			return err
		}
		if again = now.HeadSHA != ch.HeadSHA || ontoNow != onto; again {
			return nil
		}
		if _, err := dropRestack(ctx, tx, j); err != nil {
			return err
		}
		if err := c.SetNeedsRestack(ctx, tx, j.workspace, j.number, true, new(RestackConflict)); err != nil {
			return err
		}
		res.report = restackReport(res.report, onto, res.head, baseColumn(now.BaseSHA))
		return c.StoreMergeability(ctx, tx, j.workspace, j.number, res.head, onto, res.report)
	})
	if err != nil || again {
		return restackResult{head: res.head}, again, err
	}
	return res, false, nil
}

// restackReport is the report of a restack that replayed the commits of
// a change after base up to head onto onto: the replay's report, measured
// from the change's own base rather than a merge base, since only the
// change's commits were replayed. Which landed changes the conflict
// involves, and the change itself, are not known here, so they stay empty.
func restackReport(r *gitrepo.ConflictReport, onto, head, base string) *gitrepo.ConflictReport {
	out := *r
	out.TargetSHA, out.HeadSHA, out.BaseSHA = onto, head, base
	out.LandedChanges, out.Change = nil, nil
	out.Files = make([]gitrepo.ConflictFile, len(r.Files))
	for i, f := range r.Files {
		f.LandedBy = nil
		out.Files[i] = f
	}
	return &out
}

// holdsRestackConflict reports whether the change ch holds the conflict its
// last restack stored, for its pair as it is, since no trigger has set its
// mergeable to unknown after it. A check computes the same value, but its
// report replays the commits of the change below that ch still carries, and
// would replace the report of ch's own commits that the restack stored.
func holdsRestackConflict(ch Change) bool {
	return ch.Mergeable == MergeableConflict && ch.NeedsRestack && reasonOf(ch) == RestackConflict
}
