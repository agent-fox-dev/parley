package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The messages of the open route's refusals. The not-found answer is built
// from a constant alone, so its bytes are the same for a workspace the
// caller cannot read, one a bound token does not cover and one that does
// not exist.
const (
	msgWorkspaceNotFound = "workspace not found"
	msgReadOnly          = "org members have read-only access to this workspace"
	msgArchived          = "the workspace is archived and accepts no new session"
	msgIDTaken           = "a session with this id exists for another creator or another workspace"
)

// errNoPrincipal is a request at a session route without the principal
// identity's authenticator reports: a wiring fault, which grants nothing.
var errNoPrincipal = errors.New("no authenticated principal at a session route")

// creator is who opens a session: the user ("" for the admin token) and the
// credential type stored with the session.
type creator struct {
	userID, credentialType string
}

// creatorOf is the creator the principal p is.
func creatorOf(p identity.Principal) creator {
	c := creator{credentialType: p.Type}
	if p.Type == identity.TypeAdmin {
		c.credentialType = CredentialAdminToken
	}
	if p.User != nil {
		c.userID = p.User.ID
	}
	return c
}

// created reports whether the session r was opened by the creator c: the
// same user, or the admin token both times.
func (c creator) created(r row) bool {
	if c.credentialType == CredentialAdminToken || r.CredentialType == CredentialAdminToken {
		return c.credentialType == r.CredentialType
	}
	return c.userID == r.UserID
}

// createRoute serves POST /sessions: it opens an active session for a
// workspace the caller may write to, or answers the stored session to the
// creator who sends its id again. It decides in this order: the body's
// workspace_slug, the coverage of a bound token, write access, the other
// members, the id (a replay creates nothing, so it does not need a
// workspace that accepts new sessions) and the archived status.
func (c *Component) createRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sc, ok := server.CallerOf(ctx)
	p, pok := identity.PrincipalOf(ctx)
	if !ok || !pok {
		return errNoPrincipal
	}
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	slug, err := optionalString(body, "workspace_slug")
	if err != nil || slug == nil || *slug == "" {
		return server.InvalidRequest("workspace_slug is required and must be a non-empty string")
	}
	caller := access.CallerFrom(sc)
	d, err := c.decideWrite(ctx, caller, *slug)
	if err != nil {
		return err
	}
	in, err := openInput(body)
	if err != nil {
		return err
	}
	in.WorkspaceSlug, in.ParentSlug = *slug, ""
	if d.Record.Kind == access.KindFork {
		in.ParentSlug = d.Record.ParentSlug
	}
	who := creatorOf(p)
	in.UserID, in.CredentialType = who.userID, who.credentialType
	in.Status, in.StartedAt = StatusActive, c.deps.Now().UTC().Truncate(time.Second)

	stored, isNew, err := c.open(ctx, in, who, d.Record.Status == access.StatusArchived)
	if err != nil {
		return err
	}
	o, err := object(ctx, c.deps.Store.Main(), stored)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if isNew {
		status = http.StatusCreated
	}
	server.WriteJSON(w, status, o)
	return nil
}

// decideWrite decides whether caller may open a session on the workspace
// slug: a bound token must cover it, and the caller needs write access. A
// refusal is the error to answer.
func (c *Component) decideWrite(ctx context.Context, caller access.Caller, slug string) (access.Decision, error) {
	if c.deps.Coverage != nil {
		bound, covered, err := c.deps.Coverage.Covers(ctx, caller, slug)
		if err != nil {
			return access.Decision{}, fmt.Errorf("ask the coverage of workspace %s: %w", slug, err)
		}
		if bound && !covered {
			return access.Decision{}, server.NotFound(msgWorkspaceNotFound)
		}
	}
	d, err := c.deps.Access.Decide(ctx, caller, slug, access.NeedWrite)
	if err != nil {
		return access.Decision{}, fmt.Errorf("decide access to workspace %s: %w", slug, err)
	}
	switch d.Outcome {
	case access.Granted:
		return d, nil
	case access.ReadOnly:
		return access.Decision{}, server.Forbidden(msgReadOnly)
	default:
		return access.Decision{}, server.NotFound(msgWorkspaceNotFound)
	}
}

// openInput reads the members of an open request other than
// workspace_slug: the id (a new version 4 UUID when none is sent), the run
// id, the three labels and the metadata, kept as the JSON sent, compacted.
func openInput(body map[string]json.RawMessage) (row, error) {
	var in row
	var err error
	if in.ID, err = sessionIDOf(body, "id"); err != nil {
		return row{}, err
	}
	if in.ID == "" {
		in.ID = uuid.NewV4().String()
	}
	if in.RunID, err = runIDOf(body); err != nil {
		return row{}, err
	}
	labels := []struct {
		name string
		dst  **string
	}{{"node_id", &in.NodeID}, {"archetype", &in.Archetype}, {"model", &in.Model}}
	for _, l := range labels {
		if *l.dst, err = label(body, l.name); err != nil {
			return row{}, err
		}
	}
	if raw := body["metadata"]; !isNull(raw) {
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			return row{}, server.InvalidRequest("metadata must be a JSON value")
		}
		in.Metadata = b.Bytes()
	}
	return in, nil
}

// open stores the session in, or finds the one stored with its id, in one
// transaction, so that of concurrent opens with one id exactly one stores
// it. A stored session of the same creator and workspace is answered as it
// is (isNew false); one of another creator or workspace is 409 conflict.
// archived refuses a new session with 409 workspace_archived.
func (c *Component) open(ctx context.Context, in row, who creator, archived bool) (stored row, isNew bool, err error) {
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		existing, found, err := readRow(ctx, tx, in.ID)
		switch {
		case err != nil:
			return err
		case found && who.created(existing) && existing.WorkspaceSlug == in.WorkspaceSlug:
			stored = existing
			return nil
		case found:
			return server.Conflict(msgIDTaken)
		case archived:
			return server.WorkspaceArchived(msgArchived)
		}
		if err := insertRow(ctx, tx, in); err != nil {
			return err
		}
		stored, isNew = in, true
		return nil
	})
	return stored, isNew, err
}
