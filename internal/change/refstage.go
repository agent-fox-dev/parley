package change

import (
	"context"
	"database/sql"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

// versionKinds are the kinds of version the ref writer's callers name, as
// the versions record them.
var versionKinds = map[string]string{
	pushrule.VersionKindPush:   KindPush,
	pushrule.VersionKindCommit: KindCommit,
	pushrule.VersionKindRef:    KindRef,
	pushrule.VersionKindRebase: KindRebase,
}

// RefUpdated is the ref-updated stage of the ref writer: it runs in tx, the
// store transaction of every accepted write of refs, once the refs are
// locked at their before values and before they move, so that the version a
// moved source branch makes is stored with the move or not at all
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
//
// For each allowed update of a branch of in.Workspace, the workspace
// written to and so the source workspace, that is not a direct push to a
// protected branch: a deleted branch abandons its draft or open change
// (SourceDeleted); a moved or created one records the next version of its
// draft or open change (RecordVersion) with the kind the write named, by
// the caller with the credential they used, in the write's session. The
// list it returns names, at the index of each update, the version recorded
// for it, nil for none. A landing, a write that names no version kind and
// any other ref record nothing. An error, such as 400 unknown_session for a
// session the session linker refuses, stops the write: no ref moves.
func (c *Component) RefUpdated(ctx context.Context, tx *sql.Tx, in pushrule.StageInput) ([]*pushrule.Version, error) {
	if in.Via == pushrule.ViaLand {
		return nil, nil
	}
	kind, versioned := versionKinds[in.VersionKind]
	caller := Caller{UserID: in.Caller.UserID, Paths: in.Caller.Paths,
		Credential: Credential{Type: in.Caller.CredentialType, ID: in.Caller.CredentialID}}
	out := make([]*pushrule.Version, len(in.Updates))
	for i, d := range in.Updates {
		branch, ok := strings.CutPrefix(d.Ref, "refs/heads/")
		switch {
		case !ok || !d.Allowed || d.Direct:
			continue
		case d.Deleted:
			if err := c.SourceDeleted(ctx, tx, in.Workspace.Slug, branch, caller); err != nil {
				return nil, err
			}
			continue
		case !versioned:
			continue
		}
		v, err := c.RecordVersion(ctx, tx, VersionInput{Workspace: in.Workspace.Slug, Branch: branch, Head: d.After,
			Kind: kind, Caller: caller, SessionID: in.SessionID})
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		ch, _, err := c.OpenChangeOfBranch(ctx, tx, in.Workspace.Slug, branch)
		if err != nil {
			return nil, err
		}
		out[i] = &pushrule.Version{WorkspaceSlug: ch.WorkspaceSlug, Number: int(ch.Number), Version: int(v.Number)}
	}
	return out, nil
}
