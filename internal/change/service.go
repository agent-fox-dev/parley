package change

import "context"

// The component is the one way the hub's other features reach the changes.
// They call:
//
//   - in a store transaction of theirs, which the component neither begins
//     nor commits: Open, RecordVersion, SourceDeleted, ForkDeleted,
//     WorkspaceDeleted, SetAutoLand and the setters (SetDependsOn, SetBase,
//     SetMergeable, SetNeedsRestack, SetLanding, SetLanded, SetReverts,
//     SetRevertedBy, SetAbandoned);
//   - through a Querier, their transaction or nil for the store: ReadChange,
//     ReadVersions, OpenChangeOfBranch, ChangeOfCommit, ChangesOfSession,
//     ChangeNumbersOfSource, CheckOpen and AutoLandSetter; OpenCounts reads
//     the store;
//   - without the store: Authorize, the access rule of an open;
//   - while the hub is composed, to register on the instance what they
//     compute about a change: RegisterSessionLinker, RegisterStackLinker,
//     RegisterLandingEvaluator, AddVersionObserver, RegisterAutoLandGuard,
//     RegisterForkActivity and RegisterExpander.
//
// RefUpdated is the stage the hub registers with the ref writer, through
// which every write of a source branch records its version.

// CheckOpen runs every check Open runs for in, in the same order, reading
// through q (the caller's transaction, or nil for the store), and writes
// nothing: it answers nil when Open would open the change as things stand,
// and otherwise the refusal Open would answer. A write that opens a change
// once it has moved the source branch asks it before the branch moves, with
// in.Head the commit it moves the branch to, so that a refused change
// leaves the branch where it was.
func (c *Component) CheckOpen(ctx context.Context, q Querier, in OpenInput) error {
	_, err := c.check(ctx, c.querier(q), c.collaborators(), in)
	return err
}
