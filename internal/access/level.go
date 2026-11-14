package access

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
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
// grants nothing on a workspace. A PAT bound to a workspace also carries its
// binding, which Level and Decide do not read and which cuts the readable
// set down to what the binding covers.
type Caller struct {
	// Admin marks the admin token.
	Admin bool
	// UserID is the user the credential acts for; empty for the admin token.
	UserID string
	// TokenID is the token_id of a bound PAT; empty for every other
	// credential.
	TokenID string
	// Workspace is the slug a PAT is bound to; empty for every other
	// credential.
	Workspace string
}

// CallerFrom returns the rule's caller for the caller the pipeline's
// authenticator reported. The binding of a PAT comes from the principal
// identity's authenticator puts in the caller's detail.
func CallerFrom(c server.Caller) Caller {
	caller := Caller{Admin: c.Admin, UserID: c.UserID}
	if p, ok := c.Detail.(*identity.Principal); ok && p != nil && p.Type == identity.TypePAT && p.Workspace != nil {
		caller.TokenID = p.CredentialID
		caller.Workspace = *p.Workspace
	}
	return caller
}

// Deps are what the service reads: the workspace records and the
// memberships of users in active organizations, where the binding guard
// reports its refusals (nil discards them), and the event emitter through
// which the service emits its own events.
type Deps struct {
	Records     RecordReader
	Memberships Memberships
	Refusals    RefusalReporter
	Events      EventEmitter
	Store       *store.Store
	Now         func() time.Time
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
// = false, whoever asks; for a user it still costs the reads of a repository
// the user cannot see, so it also fails when they fail. A failed read is
// LevelNone, no record and the error, never a grant.
//
// The workspace's status is not read: a caller has the same level in every
// status, and the routes check the status after the access step.
func (s *Service) Level(ctx context.Context, c Caller, slug string) (Level, Record, bool, error) {
	rec, found, err := s.deps.Records.Record(ctx, slug)
	if err != nil {
		return LevelNone, Record{}, false, err
	}
	if !found {
		return LevelNone, Record{}, false, s.readAsInvisible(ctx, c)
	}
	level, err := s.levelOn(ctx, c, rec)
	if err != nil {
		return LevelNone, Record{}, false, err
	}
	return level, rec, true, nil
}

// readAsInvisible does, for a slug with no record, the reads that resolving
// c's level on a repository c cannot see would do, and returns their error.
// A user is not told that a repository exists by a lookup that answers the
// same bytes but costs a membership read more. A fork the user cannot see
// costs one record read more again (its parent's), which is not matched.
func (s *Service) readAsInvisible(ctx context.Context, c Caller) error {
	if c.Admin || c.UserID == "" {
		return nil
	}
	if _, err := s.userLevels(c.UserID).orgs(ctx); err != nil {
		return fmt.Errorf("read the active orgs of the caller: %w", err)
	}
	return nil
}

// levelOn resolves c's level on the workspace whose record is rec.
func (s *Service) levelOn(ctx context.Context, c Caller, rec Record) (Level, error) {
	switch {
	case c.Admin:
		return LevelAdmin, nil
	case c.UserID == "":
		return LevelNone, nil
	default:
		return s.userLevels(c.UserID).on(ctx, rec)
	}
}

// userLevels is the resolver for the user userID that reads the user's
// active orgs and a fork's parent from the service's sources, each only
// when the answer needs it.
func (s *Service) userLevels(userID string) userLevels {
	return userLevels{
		userID: userID,
		orgs: func(ctx context.Context) ([]string, error) {
			return s.deps.Memberships.ActiveOrgIDs(ctx, userID)
		},
		parent: s.deps.Records.Record,
	}
}

// userLevels resolves one user's level on a workspace. The resolution is
// written once, here, and both Level and Readable use it, so that the
// readable set cannot disagree with the level; they differ only in where
// the user's active orgs and a fork's parent come from.
type userLevels struct {
	userID string
	// orgs returns the ids of the active orgs the user is a member of.
	orgs func(ctx context.Context) ([]string, error)
	// parent reads the record of a fork's parent; see RecordReader.Record.
	parent func(ctx context.Context, slug string) (Record, bool, error)
}

// on resolves the user's level on the workspace whose record is rec.
func (u userLevels) on(ctx context.Context, rec Record) (Level, error) {
	if rec.Kind == KindFork {
		return u.onFork(ctx, rec)
	}
	return u.onRepository(ctx, rec)
}

// onRepository resolves the user's level on the repository rec. The owner
// is decided before the memberships are read, so that an owner's level
// never depends on their organizations.
func (u userLevels) onRepository(ctx context.Context, rec Record) (Level, error) {
	if u.userID == rec.OwnerID {
		return LevelOwner, nil
	}
	orgs, err := u.orgs(ctx)
	if err != nil {
		return LevelNone, fmt.Errorf("read the active orgs of the caller: %w", err)
	}
	if slices.Contains(orgs, rec.OrgID) {
		return LevelMember, nil
	}
	return LevelNone, nil
}

// onFork resolves the user's level on the fork rec. A fork never grants
// more than its parent: no level on the parent, or a parent with no record,
// is no level on the fork, even for the fork's owner. Otherwise the fork's
// owner and the parent's owner are owners of the fork, and every other user
// with a level on the parent is a member.
func (u userLevels) onFork(ctx context.Context, rec Record) (Level, error) {
	parent, found, err := u.parent(ctx, rec.ParentSlug)
	if err != nil {
		return LevelNone, err
	}
	// A fork's parent is a repository; anything else is an inconsistency,
	// answered on the safe side, which also keeps the resolution from
	// following a chain of parents.
	if !found || parent.Kind != KindRepository {
		return LevelNone, nil
	}
	onParent, err := u.onRepository(ctx, parent)
	if err != nil || onParent == LevelNone {
		return LevelNone, err
	}
	if u.userID == rec.OwnerID || u.userID == parent.OwnerID {
		return LevelOwner, nil
	}
	return LevelMember, nil
}

// CanWrite reports whether the user userID, who need not be the caller of
// the request at hand, has write access to the workspace slug: exactly when
// their level on it is owner. A member, a user with no level and a slug with
// no record are no. The routes whose conditions ask about someone other than
// the caller, such as the author of a change, use it. A failed read is false
// with the error.
func (s *Service) CanWrite(ctx context.Context, userID, slug string) (bool, error) {
	level, _, _, err := s.Level(ctx, Caller{UserID: userID}, slug)
	if err != nil {
		return false, err
	}
	return level == LevelOwner, nil
}
