package policy

import (
	"context"
	"database/sql"
	"slices"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
)

// VersionObserver is the change component's version observer that carries
// verdicts over (AddVersionObserver): a new version that is
// patch-equivalent to an earlier one receives that version's verdicts, so
// that an approval survives a pure rebase and nothing survives a change of
// content. It reads the component's dependencies when it is told, so it
// may be made before Bind.
func (c *Component) VersionObserver() change.VersionObserver { return carrier{c} }

// carrier carries the verdicts of a version to the versions
// patch-equivalent to it.
type carrier struct{ c *Component }

// VersionRecorded copies, in tx, every verdict of the version the new
// version v is patch-equivalent to onto v, and then evaluates the change's
// landing again, since the change component stored it before telling its
// observers. The copies write no event: nobody gave them. Any error is
// returned unchanged, so that the version is not recorded without them.
func (o carrier) VersionRecorded(ctx context.Context, tx *sql.Tx, ch change.Change, v change.Version, _ change.Credential) error {
	versions, err := o.c.deps.Changes.ReadVersions(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return err
	}
	// The version as stored, not v: the stack linker may have measured it
	// against a new base after v was built, and patch_equivalent_to with it.
	source := equivalentOf(versions, v.Number)
	if source == 0 {
		return nil
	}
	verdicts, err := readVerdicts(ctx, tx, ch.WorkspaceSlug, ch.Number, verdictFilter{Version: source})
	if err != nil || len(verdicts) == 0 {
		return err
	}
	for _, src := range verdicts {
		if err := insertVerdict(ctx, tx, carriedTo(src, v.Number, src.origin())); err != nil {
			return err
		}
	}
	_, _, err = o.c.ReevaluateIn(ctx, tx, ch.WorkspaceSlug, ch.Number)
	return err
}

// carryToCurrent copies v, just recorded on an earlier version of its
// change, onto the change's current version current, in place of the
// verdict v's user had there, when current reaches v's version through
// patch_equivalent_to: v still applies to the current version, and only
// the verdicts on that version count toward its landing.
func (c *Component) carryToCurrent(ctx context.Context, tx *sql.Tx, v storedVerdict, current int64) error {
	if v.Version == current {
		return nil
	}
	versions, err := c.deps.Changes.ReadVersions(ctx, tx, v.Workspace, v.Number)
	if err != nil || !slices.Contains(equivalenceChain(versions, current), v.Version) {
		return err
	}
	_, err = replaceVerdict(ctx, tx, carriedTo(v, current, v.origin()))
	return err
}

// origin is the version v was first given on: the one it was carried
// from, else its own.
func (v storedVerdict) origin() int64 {
	if v.CarriedFrom != nil {
		return *v.CarriedFrom
	}
	return v.Version
}

// carriedTo is a copy of v on version version of its change, carried from
// version from: a verdict of its own, with a new id, that keeps what v's
// user gave, with what credential and when.
func carriedTo(v storedVerdict, version, from int64) storedVerdict {
	v.ID, v.Version, v.CarriedFrom = uuid.NewV4().String(), version, &from
	return v
}

// equivalentOf is the earlier version that version of versions, a change's
// versions as ReadVersions reads them, is patch-equivalent to, 0 for none.
func equivalentOf(versions []change.Version, version int64) int64 {
	for _, v := range versions {
		if v.Number == version && v.PatchEquivalentTo != nil && *v.PatchEquivalentTo < version {
			return *v.PatchEquivalentTo
		}
	}
	return 0
}

// equivalenceChain is version of versions followed by the versions it
// reaches through patch_equivalent_to, nearest first: those whose
// evidence still applies to it. Each step names an earlier version, so
// the chain ends.
func equivalenceChain(versions []change.Version, version int64) []int64 {
	chain := []int64{version}
	for n := equivalentOf(versions, version); n != 0; n = equivalentOf(versions, n) {
		chain = append(chain, n)
	}
	return chain
}
