package access

import (
	"context"
	"fmt"
)

// Need is what a workspace route declares it requires of its caller.
type Need string

// The needs. NeedRead is for every route a member may call, those of a
// member's own contributions included; NeedWrite for a route that changes
// the workspace; NeedOwner for a route that changes nothing but is private
// to the owner.
const (
	NeedRead  Need = "read"
	NeedWrite Need = "write"
	NeedOwner Need = "owner"
)

// Outcome is the answer a decision gives.
type Outcome string

// The outcomes. Granted lets the caller in; NotFound answers as for a slug
// with no workspace; ReadOnly refuses a member a route that needs write or
// owner.
const (
	Granted  Outcome = "granted"
	NotFound Outcome = "not_found"
	ReadOnly Outcome = "read_only"
)

// Decision is the rule's answer for one caller, slug and need: the outcome,
// with the caller's level and the workspace's record as read while
// deciding. A NotFound decision holds no level and no record, so that one
// for a workspace the caller cannot read equals one for a slug with no
// workspace. Decisions are comparable with ==.
type Decision struct {
	Outcome Outcome
	Level   Level
	Record  Record
}

// Decide decides whether the caller c may use a route that needs need on
// the workspace slug. A slug with no record, or one on which c's level is
// none, is NotFound for every need; a member is ReadOnly for need write or
// owner, in every status of the workspace; everything else is Granted. A
// failed read, or a need other than the three, is an error with no decision,
// never a grant.
//
// The slug's shape is not checked: a string that cannot be a slug simply
// has no record, so it is answered as every other missing workspace.
func (s *Service) Decide(ctx context.Context, c Caller, slug string, need Need) (Decision, error) {
	switch need {
	case NeedRead, NeedWrite, NeedOwner:
	default:
		return Decision{}, fmt.Errorf("unknown need %q", need)
	}
	level, rec, _, err := s.Level(ctx, c, slug)
	if err != nil {
		return Decision{}, err
	}
	switch {
	case level == LevelNone:
		// Level returns the record of an existing workspace with level
		// none; it is dropped so the caller learns nothing of it.
		return Decision{Outcome: NotFound}, nil
	case level == LevelMember && need != NeedRead:
		return Decision{Outcome: ReadOnly, Level: level, Record: rec}, nil
	default:
		return Decision{Outcome: Granted, Level: level, Record: rec}, nil
	}
}
