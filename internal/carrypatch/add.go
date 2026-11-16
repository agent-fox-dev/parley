package carrypatch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// EventPatchCreate is the event of a registered patch.
const EventPatchCreate = "parley.patch.create"

// Emitter writes one hub event in the caller's transaction, so that the
// event commits or rolls back with the patch it reports (ADR 05).
type Emitter interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// addRequest is one patch of the body of POST /workspaces/:slug/patches,
// as validated: a member that was absent or null is at its zero value.
type addRequest struct {
	branch      string
	position    int // 0 for none: the end of the list
	upstreamPR  *string
	description *string
	skipCheck   bool
	ifNotExists bool
}

// addRoute answers POST /workspaces/:slug/patches. The body is one patch
// object or, for a batch, an array of them; anything else is 400
// invalid_request. The body is validated before the guard and before any
// lookup, so that an invalid body is answered as such on any workspace and
// changes nothing; the patch list is then checked before the branch is
// resolved, so that neither patch_exists nor the existing patch of
// if_not_exists creates a ref.
func (c *Component) addRoute(w http.ResponseWriter, r *http.Request) error {
	raw, err := decodeAddBody(r)
	if err != nil {
		return err
	}
	if raw[0] == '[' {
		return c.addBatch(w, r, raw)
	}
	req, err := addRequestOf(raw)
	if err != nil {
		return err
	}
	if err := c.checkNotIntegration(r, req.branch, ""); err != nil {
		return err
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	existing, found, err := LivePatchOfBranch(ctx, c.d.Store.Main(), ws.Slug, req.branch)
	if err != nil {
		return fmt.Errorf("add the patch of %s to %s: %w", req.branch, ws.Slug, err)
	}
	if found {
		return answerExisting(w, existing, req)
	}
	// No lock is held while the branch is resolved: the store's write
	// transaction below is what orders concurrent adds.
	resolution, err := c.ResolveBranch(ctx, ws, req.branch, req.skipCheck)
	if err != nil {
		return err
	}
	actor, err := actorOf(r)
	if err != nil {
		return err
	}
	p, purged, err := c.insertPatch(ctx, ws.Slug, req, resolution, actor)
	if errors.Is(err, ErrPatchExists) {
		// A concurrent add registered the branch after the check above:
		// the answer is the one the check would have given.
		existing, found, rerr := LivePatchOfBranch(ctx, c.d.Store.Main(), ws.Slug, req.branch)
		if rerr != nil || !found {
			return errPatchExists(req.branch)
		}
		return answerExisting(w, existing, req)
	}
	if err != nil {
		return err
	}
	if purged {
		c.deleteBackupRef(ctx, ws.Slug, req.branch)
	}
	server.WriteJSON(w, http.StatusCreated, ObjectOf(p, ""))
	return nil
}

// addBatch answers a body that is an array of patch objects. Batches are
// not registered by this route yet, so the array is refused as a body the
// route does not take.
func (c *Component) addBatch(_ http.ResponseWriter, _ *http.Request, _ json.RawMessage) error {
	return server.InvalidRequest("request body must be one patch object")
}

// decodeAddBody reads the body as one JSON value, which must be an object
// or an array.
func decodeAddBody(r *http.Request) (json.RawMessage, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, server.InvalidRequest("request body could not be read")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, server.InvalidRequest("request body is empty; want a patch object or an array of them")
		}
		return nil, server.InvalidRequest("request body is not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, server.InvalidRequest("request body has data after the JSON value")
	}
	if raw[0] != '{' && raw[0] != '[' {
		return nil, server.InvalidRequest("request body must be a patch object or an array of them")
	}
	return raw, nil
}

// addRequestOf validates one patch object: branch_name a valid branch
// name, position a JSON integer of at least 1, upstream_pr_url and
// description strings, skip_branch_check and if_not_exists booleans. A null
// member is absent, and members the object does not define are ignored.
// The integration branch is checked by checkNotIntegration, which needs
// the workspace.
func addRequestOf(raw json.RawMessage) (addRequest, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return addRequest{}, server.InvalidRequest("a patch must be a JSON object")
	}
	var req addRequest
	branch, err := stringMember(members, "branch_name")
	if err != nil {
		return addRequest{}, err
	}
	if branch == nil {
		return addRequest{}, server.InvalidRequest("branch_name is required")
	}
	if err := workspace.ValidateBranchName(*branch); err != nil {
		return addRequest{}, server.InvalidRequest(fmt.Sprintf("branch_name %q is not a valid branch name: %v", *branch, err))
	}
	req.branch = *branch
	if req.position, err = positionMember(members); err != nil {
		return addRequest{}, err
	}
	if req.upstreamPR, err = stringMember(members, "upstream_pr_url"); err != nil {
		return addRequest{}, err
	}
	if req.description, err = stringMember(members, "description"); err != nil {
		return addRequest{}, err
	}
	if req.skipCheck, err = boolMember(members, "skip_branch_check"); err != nil {
		return addRequest{}, err
	}
	if req.ifNotExists, err = boolMember(members, "if_not_exists"); err != nil {
		return addRequest{}, err
	}
	return req, nil
}

// member is the value of name in members, nil when it is absent or null.
func member(members map[string]json.RawMessage, name string) json.RawMessage {
	v, ok := members[name]
	if !ok || bytes.Equal(v, []byte("null")) {
		return nil
	}
	return v
}

// stringMember is the string member name, nil when absent or null.
func stringMember(members map[string]json.RawMessage, name string) (*string, error) {
	v := member(members, name)
	if v == nil {
		return nil, nil
	}
	var s string
	if v[0] != '"' || json.Unmarshal(v, &s) != nil {
		return nil, server.InvalidRequest(name + " must be a string")
	}
	return &s, nil
}

// boolMember is the boolean member name, false when absent or null.
func boolMember(members map[string]json.RawMessage, name string) (bool, error) {
	v := member(members, name)
	if v == nil {
		return false, nil
	}
	var b bool
	if json.Unmarshal(v, &b) != nil {
		return false, server.InvalidRequest(name + " must be a boolean")
	}
	return b, nil
}

// positionMember is the member position, 0 when absent or null. It must be
// a JSON integer of at least 1, written without a fraction or an exponent;
// one too large for an int is past the end of any list, and so the end.
func positionMember(members map[string]json.RawMessage) (int, error) {
	v := member(members, "position")
	if v == nil {
		return 0, nil
	}
	refused := server.InvalidRequest("position must be an integer of at least 1")
	for _, b := range v {
		if b < '0' || b > '9' {
			return 0, refused
		}
	}
	n, err := strconv.ParseInt(string(v), 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange) || n > math.MaxInt32:
		return math.MaxInt32, nil
	case err != nil || n < 1:
		return 0, refused
	}
	return int(n), nil
}

// checkNotIntegration refuses branch when it is the integration branch of
// the workspace the request names, which the hub rebuilds and which can
// never be a patch; prefix starts the message. The record is read here,
// before the guard, because a body that names it is invalid whatever the
// workspace's mode and state; a workspace gone since the access check is
// left to the guard.
func (c *Component) checkNotIntegration(r *http.Request, branch, prefix string) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the add found no access decision")
	}
	if c.d.Store == nil {
		return errNotBound
	}
	ws, found, err := workspace.BySlug(r.Context(), c.d.Store.Main(), d.Record.Slug)
	if err != nil {
		return fmt.Errorf("read the workspace %q: %w", d.Record.Slug, err)
	}
	if found && branch == IntegrationBranch(ws) {
		return server.InvalidRequest(fmt.Sprintf("%sbranch_name %q is the workspace's integration branch", prefix, branch))
	}
	return nil
}

// answerExisting answers an add whose branch is that of the patch existing,
// which is not deleted: 200 with it unchanged for if_not_exists, else 409
// patch_exists.
func answerExisting(w http.ResponseWriter, existing Patch, req addRequest) error {
	if !req.ifNotExists {
		return errPatchExists(req.branch)
	}
	server.WriteJSON(w, http.StatusOK, ObjectOf(existing, ""))
	return nil
}

// errPatchExists is 409 patch_exists for branch.
func errPatchExists(branch string) error {
	return server.NewError("patch_exists", http.StatusConflict, fmt.Sprintf("branch %q is a patch of the workspace already", branch))
}

// actor is who an event names as its actor.
type actor struct {
	typ string
	id  *string
}

// actorOf is the authenticated caller of r: the credential type, and the
// user's id, none for the admin token.
func actorOf(r *http.Request) (actor, error) {
	p, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return actor{}, server.Unauthenticated("a patch is added by an authenticated caller")
	}
	a := actor{typ: p.Type}
	if p.Type == identity.TypeAdmin {
		a.typ = event.ActorAdminToken
	}
	if p.User != nil {
		a.id = &p.User.ID
	}
	return a, nil
}

// insertPatch registers the patch req of the workspace slug in one store
// transaction, with its event: it removes for good the deleted patches of
// the same branch (purged reports that it did, so that the backup ref
// goes), places the patch at its position, moving the patches at and after
// it down, and writes parley.patch.create. The store runs one write
// transaction at a time, so the count and the positions it reads cannot
// change before it commits. A branch another add registered first is an
// error matching ErrPatchExists, and nothing is written.
func (c *Component) insertPatch(ctx context.Context, slug string, req addRequest, resolution string, by actor) (Patch, bool, error) {
	if c.d.Events == nil {
		return Patch{}, false, errNotBound
	}
	now := c.now().UTC().Truncate(time.Second)
	p := Patch{
		ID: uuid.New().String(), Workspace: slug, BranchName: req.branch, Status: StatusActive,
		ConflictFiles: []string{}, UpstreamPRURL: nonEmpty(req.upstreamPR), Description: nonEmpty(req.description),
		AddedAt: now, UpdatedAt: now,
	}
	purged := false
	err := c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		purged = false
		for {
			old, found, err := DeletedPatchOfBranch(ctx, tx, slug, req.branch)
			if err != nil {
				return err
			}
			if !found {
				break
			}
			if err := DeletePatch(ctx, tx, slug, old.ID); err != nil {
				return err
			}
			purged = true
		}
		n, err := CountLivePatches(ctx, tx, slug)
		if err != nil {
			return err
		}
		pos := n + 1
		if req.position != 0 && req.position <= n {
			pos = req.position
			if err := ShiftPositions(ctx, tx, slug, pos); err != nil {
				return err
			}
		}
		p.Position = &pos
		if err := InsertPatch(ctx, tx, p); err != nil {
			return err
		}
		_, err = c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventPatchCreate, Workspace: &slug, ActorType: by.typ, ActorID: by.id,
			ResourceType: "patch", ResourceID: p.ID,
			Metadata: map[string]any{"branch_name": p.BranchName, "position": pos, "branch_resolution": resolution},
		})
		return err
	})
	if err != nil {
		return Patch{}, false, err
	}
	return p, purged, nil
}

// deleteBackupRef deletes the backup ref of branch in the copy of the
// workspace slug, once the deleted patch it belonged to is gone. The patch
// is registered whatever happens here, so a failure is logged, with the
// ref, and not answered.
func (c *Component) deleteBackupRef(ctx context.Context, slug, branch string) {
	ref := ReplacedRefPrefix + branch
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		repo, err := c.d.Git.Open(ctx, c.d.RepoPath(slug))
		if err != nil {
			return err
		}
		return repo.DeleteRef(ctx, ref)
	})
	if err != nil {
		c.logger().LogAttrs(ctx, slog.LevelError, "delete the backup ref of a removed patch",
			slog.String("slug", slug), slog.String("ref", ref), slog.String("error", err.Error()))
	}
}

// nonEmpty is s, nil when it is nil or empty.
func nonEmpty(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}
