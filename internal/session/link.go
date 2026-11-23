package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// ErrUnknownSession is the refusal of a session id that names no session,
// or that is not a UUID.
var ErrUnknownSession = errors.New("unknown session")

// Kind is the kind of a session link: what the linked piece of work is.
type Kind string

// The kinds of a session link.
const (
	KindFork    Kind = "fork"
	KindSandbox Kind = "sandbox"
	KindVersion Kind = "version"
	KindPush    Kind = "push"
	KindCommit  Kind = "commit"
	KindClaim   Kind = "claim"
	KindLand    Kind = "land"
)

// Link is the fact that one piece of work belongs to a session: the
// session, the kind of the work, the workspace it was done on and the key
// that names it there. Number and Version are the change number and the
// version number of a version link, and zero for the other kinds. Build a
// link with the constructor of its kind, which writes its key; Record
// refuses a link whose kind, workspace or key is malformed.
type Link struct {
	SessionID       string
	Kind            Kind
	Workspace       string
	Key             string
	Number, Version int64
}

// ForkLink links the task fork forkSlug, created from the workspace parent,
// to the session: the link's workspace is the parent, where the fork
// operation runs, and its key the fork's slug, which the session's forks
// list.
func ForkLink(sessionID, parent, forkSlug string) Link {
	return Link{SessionID: sessionID, Kind: KindFork, Workspace: parent, Key: forkSlug}
}

// SandboxLink links the sandbox sandboxID of the workspace to the session.
func SandboxLink(sessionID, workspace, sandboxID string) Link {
	return Link{SessionID: sessionID, Kind: KindSandbox, Workspace: workspace, Key: sandboxID}
}

// VersionLink links the version version of the change number of the
// workspace to the session. Both numbers start at 1.
func VersionLink(sessionID, workspace string, number, version int64) Link {
	return Link{
		SessionID: sessionID, Kind: KindVersion, Workspace: workspace, Key: versionKey(number, version),
		Number: number, Version: version,
	}
}

// PushLink links the update of the ref ref to the commit newSHA, pushed to
// the workspace, to the session; each ref a push updates is a link of its
// own.
func PushLink(sessionID, workspace, ref, newSHA string) Link {
	key := ""
	if ref != "" && newSHA != "" {
		// A ref name holds no space (git check-ref-format), so the key
		// tells every ref and commit apart.
		key = ref + " " + newSHA
	}
	return Link{SessionID: sessionID, Kind: KindPush, Workspace: workspace, Key: key}
}

// CommitLink links the commit sha, made through the API on the workspace,
// to the session.
func CommitLink(sessionID, workspace, sha string) Link {
	return Link{SessionID: sessionID, Kind: KindCommit, Workspace: workspace, Key: sha}
}

// ClaimLink links the claim claimID on the workspace to the session.
func ClaimLink(sessionID, workspace, claimID string) Link {
	return Link{SessionID: sessionID, Kind: KindClaim, Workspace: workspace, Key: claimID}
}

// LandLink links the land attempt landID on the workspace to the session.
func LandLink(sessionID, workspace, landID string) Link {
	return Link{SessionID: sessionID, Kind: KindLand, Workspace: workspace, Key: landID}
}

// versionKey is the key of the version version of the change number.
func versionKey(number, version int64) string {
	return fmt.Sprintf("%d/%d", number, version)
}

// validate refuses a link whose kind is not one of the seven, whose
// workspace or key is empty, or, for a version link, whose numbers are not
// both positive or whose key is not the one they make.
func (l Link) validate() error {
	switch l.Kind {
	case KindFork, KindSandbox, KindPush, KindCommit, KindClaim, KindLand:
	case KindVersion:
		if l.Number < 1 || l.Version < 1 || l.Key != versionKey(l.Number, l.Version) {
			return fmt.Errorf("session link: version %d of change %d with key %q is not a version", l.Version, l.Number, l.Key)
		}
	default:
		return fmt.Errorf("session link: unknown kind %q", l.Kind)
	}
	if l.Workspace == "" {
		return fmt.Errorf("session link: %s link without a workspace", l.Kind)
	}
	if l.Key == "" {
		return fmt.Errorf("session link: %s link without a key", l.Kind)
	}
	return nil
}

// Record stores the link l in the caller's transaction tx, with the
// clock's time, so that it exists exactly when tx commits. A link equal to
// a stored one in its session, kind, workspace and key is left as it is,
// with its original time, and Record returns nil. It refuses a malformed
// link with a validation error and a session id that is not a UUID or
// names no session with ErrUnknownSession, storing nothing. The session's
// status does not matter: work may be linked to an ended session.
//
// Record never begins, commits or rolls back a transaction, and leaves tx
// usable after any error it returns.
func (c *Component) Record(ctx context.Context, tx *sql.Tx, l Link) error {
	if err := l.validate(); err != nil {
		return err
	}
	if !server.IsUUID(l.SessionID) {
		return ErrUnknownSession
	}
	l.SessionID = strings.ToLower(l.SessionID)
	found, err := sessionExists(ctx, tx, l.SessionID)
	if err != nil {
		return err
	}
	if !found {
		return ErrUnknownSession
	}
	return insertLink(ctx, tx, l, c.deps.Now().UTC().Truncate(time.Second))
}
