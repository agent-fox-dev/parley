package policy

import (
	"strings"

	"github.com/agent-fox-dev/parley/internal/change"
)

// verdict is what the counting rule knows of a verdict: the user who gave
// it, with the username stored on it, the credential it was given with,
// and the version it is on (for a carried copy, the version it was
// carried to).
type verdict struct {
	UserID, Username string
	Credential       change.Credential
	Version          int64
}

// approverPool is who the approver forms owner and member name in a
// workspace: the id of its owner, and the ids of the members of its
// organization among the users asked about, none for a workspace without
// an organization.
type approverPool struct {
	Owner   string
	Members map[string]bool
}

// counts reports whether v counts toward a requirement whose approvers are
// approvers, in the workspace whose owner and members pool names, on the
// change whose versions creators lists: when its user is among the
// approvers and its credential created no version numbered at or below the
// verdict's own. An approval otherwise lets the credential that produced
// the work certify it.
func counts(v verdict, approvers []string, pool approverPool, creators []versionCreator) bool {
	return isApprover(v, approvers, pool) && !createdUpTo(v, creators)
}

// isApprover reports whether the user of v is among approvers: owner is the
// workspace's owner, member every member of its organization, and
// user:<username> the user whose stored username is exactly that, compared
// case-sensitively, as the document names users. No form matches nobody.
func isApprover(v verdict, approvers []string, pool approverPool) bool {
	for _, a := range approvers {
		switch a {
		case "owner":
			if v.UserID == pool.Owner {
				return true
			}
		case "member":
			if pool.Members[v.UserID] {
				return true
			}
		default:
			if name, ok := strings.CutPrefix(a, "user:"); ok && name == v.Username {
				return true
			}
		}
	}
	return false
}

// createdUpTo reports whether the credential of v created a version
// numbered at or below v's: a version created with a recorded credential
// excludes that credential, equal only in type and id; one with none
// recorded that the hub did not create excludes every credential of the
// user who created it, so that a missing record can only make a verdict
// not count; a version the hub created excludes nobody.
func createdUpTo(v verdict, creators []versionCreator) bool {
	for _, c := range creators {
		if c.Number > v.Version {
			continue
		}
		if c.Credential != (change.Credential{}) && c.Credential == v.Credential {
			return true
		}
		if c.User != "" && c.User == v.UserID {
			return true
		}
	}
	return false
}
