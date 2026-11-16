package policy

import (
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
)

// createdBy is a version of a change numbered n, created by the user with
// the credential of the type and id; a user "" with an empty type is the
// hub, and a user with an empty type created it with no recorded
// credential.
func createdBy(n int64, user, credType, credID string) change.Version {
	v := change.Version{Number: n, Credential: change.Credential{Type: credType, ID: credID}}
	switch {
	case user == "":
		v.CreatedBy.ActorType = change.ActorSystem
	case credType == "":
		v.CreatedBy = change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(user)}
	default:
		v.CreatedBy = change.Actor{ActorType: credType, ActorID: new(user)}
	}
	return v
}

// by is a verdict on the version of the user, whose username is the same
// as its id, given with the credential of the type and id.
func by(user, credType, credID string, version int64) verdict {
	return verdict{UserID: user, Username: user, Credential: change.Credential{Type: credType, ID: credID}, Version: version}
}

func TestCounts_ExcludesAVerdictGivenWithACredentialThatCreatedAVersionUpToItsOwn(t *testing.T) {
	t.Parallel()
	const pat, key = change.CredentialPAT, change.CredentialAPIKey
	pool := approverPool{Owner: "alice", Members: map[string]bool{"alice": true, "bob": true}}
	ownerOnly, ownerAndBob := []string{"owner"}, []string{"owner", "user:bob"}
	threeVersions := []change.Version{createdBy(1, "alice", pat, "X"), createdBy(2, "", "", ""), createdBy(3, "agent", key, "K2")}
	twoVersions := []change.Version{createdBy(1, "alice", pat, "X"), createdBy(2, "alice", pat, "Y")}
	unrecorded := []change.Version{createdBy(1, "alice", "", "")}
	patX := []change.Version{createdBy(1, "alice", pat, "X")}
	for _, tc := range []struct {
		name      string
		approvers []string
		versions  []change.Version
		verdict   verdict
		want      bool
	}{
		{"the owner with a credential that created no version", ownerOnly, threeVersions, by("alice", key, "K1", 3), true},
		{"the owner with the credential that created version 1", ownerOnly, threeVersions, by("alice", pat, "X", 3), false},
		{"the owner with another credential of the same type", ownerOnly, threeVersions, by("alice", pat, "Y", 3), true},
		{"a creator who is not an approver", ownerOnly, threeVersions, by("agent", key, "K2", 3), false},
		{"a user who is not an approver", ownerOnly, threeVersions, by("bob", key, "Kb", 3), false},
		{"a credential that created only a later version", ownerOnly, twoVersions, by("alice", pat, "Y", 1), true},
		{"the credential that created the verdict's own version", ownerOnly, twoVersions, by("alice", pat, "X", 1), false},
		{"the creator of a version with no recorded credential, with a key", ownerAndBob, unrecorded, by("alice", key, "K1", 1), false},
		{"the creator of a version with no recorded credential, with a PAT", ownerAndBob, unrecorded, by("alice", pat, "X", 1), false},
		{"another approver beside a version with no recorded credential", ownerAndBob, unrecorded, by("bob", key, "Kb", 1), true},
		{"the creating credential's id with another type", ownerOnly, patX, by("alice", key, "X", 1), true},
		{"the creating credential's type with another id", ownerOnly, patX, by("alice", pat, "x", 1), true},
		{"the creating credential's type and id", ownerOnly, patX, by("alice", pat, "X", 1), false},
	} {
		if got := counts(tc.verdict, tc.approvers, pool, creatorsOf(tc.versions)); got != tc.want {
			t.Errorf("%s: counts(%+v, %q) = %v, want %v", tc.name, tc.verdict, tc.approvers, got, tc.want)
		}
	}
}

func TestCounts_MatchesTheOwnerTheOrganizationsMembersAndExactUsernames(t *testing.T) {
	t.Parallel()
	pool := approverPool{Owner: "alice", Members: map[string]bool{"alice": true, "bob": true}}
	hubOnly := creatorsOf([]change.Version{createdBy(1, "", "", "")})
	for _, tc := range []struct {
		approvers []string
		want      map[string]bool
	}{
		{[]string{"owner"}, map[string]bool{"alice": true, "bob": false, "carol": false}},
		{[]string{"member"}, map[string]bool{"alice": true, "bob": true, "carol": false}},
		{[]string{"user:carol"}, map[string]bool{"alice": false, "bob": false, "carol": true}},
		{[]string{"user:Carol"}, map[string]bool{"alice": false, "bob": false, "carol": false}},
		{[]string{"owner", "user:bob"}, map[string]bool{"alice": true, "bob": true, "carol": false}},
		{[]string{}, map[string]bool{"alice": false, "bob": false, "carol": false}},
	} {
		for user, want := range tc.want {
			if got := counts(by(user, change.CredentialAPIKey, "k-"+user, 1), tc.approvers, pool, hubOnly); got != want {
				t.Errorf("counts(verdict of %s, %q) = %v, want %v", user, tc.approvers, got, want)
			}
		}
	}
}
