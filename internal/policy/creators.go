package policy

import "github.com/agent-fox-dev/parley/internal/change"

// versionCreator is who created one version of a change, as the counting
// rule compares it with a verdict: its number and either the credential
// that created it or, for a version with no recorded credential that the
// hub did not create, the user who created it, all of whose credentials
// it stands for. Both are empty for a version the hub created.
type versionCreator struct {
	Number     int64
	Credential change.Credential
	User       string
}

// creatorsOf is the creators of versions, as the change component stores
// them with each version (ReadVersions): the change component records the
// creating credential when it records the version, and again when a raced
// write re-attributes it, so the credential is read from there rather than
// kept a second time.
func creatorsOf(versions []change.Version) []versionCreator {
	out := make([]versionCreator, 0, len(versions))
	for _, v := range versions {
		c := versionCreator{Number: v.Number}
		switch {
		case v.CreatedBy.ActorType == change.ActorSystem:
		case v.Credential != (change.Credential{}):
			c.Credential = v.Credential
		case v.CreatedBy.ActorID != nil:
			c.User = *v.CreatedBy.ActorID
		}
		out = append(out, c)
	}
	return out
}
