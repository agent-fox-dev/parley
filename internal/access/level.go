package access

import (
	"context"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/server"
)

// Level is what a caller is on one workspace. Every answer of the rule is
// derived from it.
type Level string

// The levels. LevelAdmin is the admin token's on every workspace; LevelOwner
// a user's on a workspace they own; LevelMember a user's on a workspace of
// an active organization they belong to; LevelNone everyone else's, to whom
// the workspace does not exist.
const (
	LevelNone   Level = ""
	LevelAdmin  Level = "admin"
	LevelOwner  Level = "owner"
	LevelMember Level = "member"
)

// Caller is who the rule decides for: the admin token, or a user. The kind
// of credential and the user's role are not part of it, because the rule
// gives a user the same level by API key as by PAT, and the role admin
// grants nothing on a workspace.
type Caller struct {
	// Admin marks the admin token.
	Admin bool
	// UserID is the user the credential acts for; empty for the admin token.
	UserID string
}

// CallerFrom returns the rule's caller for the caller the pipeline's
// authenticator reported.
func CallerFrom(c server.Caller) Caller {
	return Caller{Admin: c.Admin, UserID: c.UserID}
}

// Deps are what the service reads: the workspace records and the
// memberships of users in active organizations.
type Deps struct {
	Records     RecordReader
	Memberships Memberships
}

// Service is the access rule. The hub registers its zero value when it
// composes the pipeline and binds it to the store in a start hook, before
// the first request.
type Service struct {
	deps Deps
}

// Bind sets the service's dependencies. The hub calls it once, from a start
// hook, before it serves any request.
func (s *Service) Bind(d Deps) {
	s.deps = d
}

// Level resolves the caller c's level on the workspace slug and returns it
// with the workspace's record. A slug with no record is LevelNone with found
// = false and no error, whoever asks. A failed read is LevelNone, no record
// and the error, never a grant.
//
// The workspace's status is not read: a caller has the same level in every
// status, and the routes check the status after the access step.
func (s *Service) Level(ctx context.Context, c Caller, slug string) (Level, Record, bool, error) {
	rec, found, err := s.deps.Records.Record(ctx, slug)
	if err != nil {
		return LevelNone, Record{}, false, err
	}
	if !found {
		return LevelNone, Record{}, false, nil
	}
	level, err := s.levelOn(ctx, c, rec)
	if err != nil {
		return LevelNone, Record{}, false, err
	}
	return level, rec, true, nil
}

// levelOn resolves c's level on the workspace whose record is rec.
func (s *Service) levelOn(ctx context.Context, c Caller, rec Record) (Level, error) {
	switch {
	case c.Admin:
		return LevelAdmin, nil
	case c.UserID == "":
		return LevelNone, nil
	case rec.Kind == KindFork:
		return s.levelOnFork(ctx, c.UserID, rec)
	default:
		return s.levelOnRepository(ctx, c.UserID, rec)
	}
}

// levelOnRepository resolves the user userID's level on the repository
// rec. The owner is decided before the memberships are read, so that an
// owner's level never depends on their organizations.
func (s *Service) levelOnRepository(ctx context.Context, userID string, rec Record) (Level, error) {
	if userID == rec.OwnerID {
		return LevelOwner, nil
	}
	orgs, err := s.deps.Memberships.ActiveOrgIDs(ctx, userID)
	if err != nil {
		return LevelNone, fmt.Errorf("read the active orgs of the caller: %w", err)
	}
	if slices.Contains(orgs, rec.OrgID) {
		return LevelMember, nil
	}
	return LevelNone, nil
}

// levelOnFork resolves the user userID's level on the fork rec. A fork never
// grants more than its parent: no level on the parent, or a parent with no
// record, is no level on the fork, even for the fork's owner. Otherwise the
// fork's owner and the parent's owner are owners of the fork, and every
// other user with a level on the parent is a member.
func (s *Service) levelOnFork(ctx context.Context, userID string, rec Record) (Level, error) {
	parent, found, err := s.deps.Records.Record(ctx, rec.ParentSlug)
	if err != nil {
		return LevelNone, err
	}
	// A fork's parent is a repository; anything else is an inconsistency,
	// answered on the safe side, which also keeps the resolution from
	// following a chain of parents.
	if !found || parent.Kind != KindRepository {
		return LevelNone, nil
	}
	onParent, err := s.levelOnRepository(ctx, userID, parent)
	if err != nil || onParent == LevelNone {
		return LevelNone, err
	}
	if userID == rec.OwnerID || userID == parent.OwnerID {
		return LevelOwner, nil
	}
	return LevelMember, nil
}
