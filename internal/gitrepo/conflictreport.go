package gitrepo

import "encoding/json"

// The kinds of a conflicted file.
const (
	// ConflictContent: both sides changed the same lines.
	ConflictContent = "content"
	// ConflictAddAdd: both sides added the path.
	ConflictAddAdd = "add_add"
	// ConflictModifyDelete: the target changed a file the head deleted.
	ConflictModifyDelete = "modify_delete"
	// ConflictDeleteModify: the target deleted a file the head changed.
	ConflictDeleteModify = "delete_modify"
	// ConflictRename: the sides renamed a file in ways that clash.
	ConflictRename = "rename"
	// ConflictMode: the sides gave the path different types, such as a
	// file and a symlink.
	ConflictMode = "mode"
	// ConflictBinary: both sides changed a binary file.
	ConflictBinary = "binary"
	// ConflictSubmodule: both sides moved a submodule.
	ConflictSubmodule = "submodule"
)

// ConflictReport says why the commits of a head could not be rebased onto a
// target: the commit that did not apply and the files it conflicts on. It
// is the hub's one conflict report, whichever operation found the conflict.
// Encoded as JSON, every member is present: a list it has none of is [],
// and Change is null when it names none.
type ConflictReport struct {
	// TargetSHA is the commit the head was rebased onto, HeadSHA the head's
	// commit, and BaseSHA their merge base, "" (null) when they share no
	// ancestor.
	TargetSHA string `json:"target_sha"`
	HeadSHA   string `json:"head_sha"`
	BaseSHA   string `json:"base_sha"`
	// CommitSHA is the first commit of the head that did not apply.
	CommitSHA string `json:"commit_sha"`
	// Files are the conflicted files, in byte order of path.
	Files []ConflictFile `json:"files"`
	// LandedChanges are the changes landed on the target since the merge
	// base that the conflict involves, and Change the change whose head
	// was rebased; none is known where the hub does not record which
	// change landed which commit.
	LandedChanges []any `json:"landed_changes"`
	Change        any   `json:"change"`
}

// ConflictFile is one conflicted file of a ConflictReport.
type ConflictFile struct {
	// Path is the file's path, the first path git names for the conflict.
	Path string `json:"path"`
	// Kind is one of ConflictContent, ConflictAddAdd, ConflictModifyDelete,
	// ConflictDeleteModify, ConflictRename, ConflictMode, ConflictBinary and
	// ConflictSubmodule.
	Kind string `json:"kind"`
	// Base, Ours and Theirs are the file on each side of the three-way
	// merge: the head commit's parent, the target with the commits of the
	// head before this one, and the head commit; nil (null) on a side the
	// file does not exist on.
	Base   *ConflictSide `json:"base"`
	Ours   *ConflictSide `json:"ours"`
	Theirs *ConflictSide `json:"theirs"`
	// LandedBy are the landed changes that changed the file on the target.
	LandedBy []any `json:"landed_by"`
}

// ConflictSide is a file as one side of a merge holds it.
type ConflictSide struct {
	// SHA is the blob id, Mode git's six-digit octal mode, and Path the
	// file's path on that side, which a rename makes differ between sides.
	SHA  string `json:"sha"`
	Mode string `json:"mode"`
	Path string `json:"path"`
}

// MarshalJSON writes every member, a list that holds nothing as [] and a
// missing merge base as null.
func (c ConflictReport) MarshalJSON() ([]byte, error) {
	out := struct {
		TargetSHA     string         `json:"target_sha"`
		HeadSHA       string         `json:"head_sha"`
		BaseSHA       *string        `json:"base_sha"`
		CommitSHA     string         `json:"commit_sha"`
		Files         []ConflictFile `json:"files"`
		LandedChanges []any          `json:"landed_changes"`
		Change        any            `json:"change"`
	}{c.TargetSHA, c.HeadSHA, nil, c.CommitSHA, nonNil(c.Files), nonNil(c.LandedChanges), c.Change}
	if c.BaseSHA != "" {
		out.BaseSHA = &c.BaseSHA
	}
	return json.Marshal(out)
}

// MarshalJSON writes every member, landed_by as [] when it holds nothing.
func (f ConflictFile) MarshalJSON() ([]byte, error) {
	type plain ConflictFile
	out := plain(f)
	out.LandedBy = nonNil(out.LandedBy)
	return json.Marshal(out)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
