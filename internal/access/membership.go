package access

import "context"

// Memberships is the one question the rule asks about a user's
// organizations. Identity owns the memberships and the organizations' status;
// the hub wires the real source, and a test can stub it.
type Memberships interface {
	// ActiveOrgIDs returns the ids of the active organizations the user
	// userID is a member of. A blocked or deleted organization is never in
	// the answer, so a membership of one grants nothing.
	ActiveOrgIDs(ctx context.Context, userID string) ([]string, error)
}

// MembershipsFunc adapts a function to Memberships.
type MembershipsFunc func(ctx context.Context, userID string) ([]string, error)

// ActiveOrgIDs calls f.
func (f MembershipsFunc) ActiveOrgIDs(ctx context.Context, userID string) ([]string, error) {
	return f(ctx, userID)
}
