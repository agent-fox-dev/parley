package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// ErrorTypeCredentialMissing is the refusal of a write to a target for
// which the workspace stores no credential.
const ErrorTypeCredentialMissing = "forge_credential_missing"

// writeRoute is a forge write route, which writeRoute builds into a server
// route on the shared checks of Route, with the scope forge:write and the
// need write. Its checks run in this order, each refusal sending nothing to
// the forge:
//
//  1. the PAT scope and write access, the fork's parent's included, and the
//     query parameter target, as for every forge route;
//  2. before, the route's path parameters and the body rules that do not
//     depend on the forge;
//  3. the requested workspace is archived (409 workspace_archived), or the
//     task fork's parent is (409 parent_archived);
//  4. the resolution of the target's forge (forge_api_url_invalid,
//     forge_unknown);
//  5. the length of the texts the route measures, against the forge's
//     limit (413 payload_too_large), then after, the route's rules that
//     depend on the forge's kind;
//  6. the target has no stored credential (422 forge_credential_missing):
//     the hub never writes to a forge anonymously.
//
// Then serve makes the write. When it handed at least one request to the
// forge client, also one whose connection failed or whose deadline passed,
// the write records exactly one event of the route's kind, ok or not, and
// only then is the caller answered: a write that reached the forge is never
// answered without its event.
type writeRoute struct {
	method, pattern string
	// event is the kind of the event the route records.
	event eventKind
	// before checks the path parameters and the body, and keeps what it
	// read in call.Params; nil for none. It reads the body with the reader
	// of the shape the route takes, such as readObject, and its fields with
	// the members' rules. When what it keeps is measured, its texts are
	// held to the forge's limit before after runs.
	before func(r *http.Request, call *Call) error
	// after refuses a request by a rule of the target's forge kind; nil
	// for none.
	after func(r *http.Request, call *Call) error
	// serve makes the write on the forge and returns the answer. Its
	// context is not ended by the caller leaving, so that a write is
	// carried out to its end. With an error it still returns what its
	// event knows of the write (written.event), its answer left zero.
	serve func(ctx context.Context, call *Call) (written, error)
}

// eventKind is the kind of a write route's event: its type, whose last
// segment is the action, and the type of the resource it is about.
type eventKind struct {
	typ, resource string
}

// written is a write route's answer: its status, and its body, written in
// JSON, or none when it is nil; and what the write's event says of it. The
// frame writes the answer, so that the write is recorded before the caller
// is answered.
type written struct {
	status int
	body   any
	event  writeEvent
}

// writeEvent is what a route tells of a write for its event, beyond what
// the frame knows (the target, the outcome, the fork). No member may hold
// a text of the request, a credential or the forge's message.
type writeEvent struct {
	// number is the issue or pull request number the write worked on or
	// created; 0 for none, such as a create that failed.
	number int64
	// id is the resource id when it is no number, such as a label's name.
	id string
	// url is the web URL of what the write created or changed, or of the
	// comment it posted; "" for none.
	url string
	// extra are the route's own metadata members, such as the fields an
	// update sent, as far as they are known.
	extra map[string]any
}

// resourceID is the event's resource id: id, else the number, else none,
// as for a create that failed.
func (e writeEvent) resourceID() string {
	switch {
	case e.id != "":
		return e.id
	case e.number != 0:
		return strconv.FormatInt(e.number, 10)
	}
	return "none"
}

// writeRoutes are the forge write routes.
func (c *Component) writeRoutes() []server.Route {
	return []server.Route{
		c.writeRoute(writeRoute{method: http.MethodPost, pattern: "/workspaces/:slug/issues",
			event: eventKind{"parley.forge.issue.create", "issue"}, before: readIssueCreate, serve: createIssue}),
	}
}

// maxWriteBody is the body limit of a write route. It is far above the
// platform's default, so that a text at the longest forge limit is measured
// by the hub, whatever its characters' length in bytes, rather than refused
// by the platform first.
const maxWriteBody = 8 << 20

// writeRoute builds the server route of wr (see writeRoute).
func (c *Component) writeRoute(wr writeRoute) server.Route {
	rt := c.Route(Route{
		Method: wr.method, Pattern: wr.pattern, Scope: ScopeWrite, Need: access.NeedWrite,
		Params: func(r *http.Request, call *Call) error {
			if wr.before != nil {
				if err := wr.before(r, call); err != nil {
					return err
				}
			}
			return archived(call)
		},
		Check: func(r *http.Request, call *Call) error {
			if m, ok := call.Params.(measured); ok {
				if err := limitTexts(call.Resolution.Kind, m.texts()); err != nil {
					return err
				}
			}
			if wr.after != nil {
				if err := wr.after(r, call); err != nil {
					return err
				}
			}
			if call.Resolution.Credential.Kind == CredentialNone {
				return server.NewError(ErrorTypeCredentialMissing, http.StatusUnprocessableEntity,
					fmt.Sprintf("workspace %q stores no forge credential for the target %s; a write needs one",
						call.Workspace.Slug, call.Resolution.Target))
			}
			return nil
		},
		Serve: func(w http.ResponseWriter, r *http.Request, call *Call) error {
			return c.serveWrite(w, r, call, wr)
		},
	})
	rt.MaxBodySize = maxWriteBody
	return rt
}

// serveWrite makes the write of wr, records its event when it reached the
// forge, and answers. A failed forge call is mapped here, once, so that its
// event and its answer have the same error type; the mapped error passes
// Route's own mapping unchanged.
func (c *Component) serveWrite(w http.ResponseWriter, r *http.Request, call *Call, wr writeRoute) error {
	// The write and its event are carried out to their end when the caller
	// leaves, so that a write the forge made is never left unrecorded.
	ctx := context.WithoutCancel(r.Context())
	out, err := wr.serve(ctx, call)
	if call.sent == 0 {
		if err != nil {
			return err
		}
		answer(w, out)
		return nil
	}
	if err == nil {
		if err := c.record(ctx, call, wr.event, out.event, ""); err != nil {
			return err
		}
		answer(w, out)
		return nil
	}
	mapped, isForge := MapError(err, c.deps.Scrubber, call.Resolution.Credential.Token)
	errorType := mapped.Type
	if !isForge {
		errorType = "internal"
		if e, ok := errors.AsType[*server.Error](err); ok {
			errorType = e.Type()
		}
	}
	if err := c.record(ctx, call, wr.event, out.event, errorType); err != nil {
		return err
	}
	if !isForge {
		return err
	}
	c.logFailure(r, call, mapped)
	if mapped.RetryAfter != "" {
		w.Header().Set("Retry-After", mapped.RetryAfter)
	}
	return mapped.ServerError()
}

// answer writes out's status and body.
func answer(w http.ResponseWriter, out written) {
	if out.body == nil {
		w.WriteHeader(out.status)
		return
	}
	server.WriteJSON(w, out.status, out.body)
}

// record records the event of kind of the write call made, which ev
// describes: ok when errorType is "", else failed with errorType. The
// event is on the written workspace, a task fork's parent when the call
// came through the fork, whose slug is then the metadata member fork. An
// event that cannot be recorded is an internal error: the caller is not
// told the write's outcome without it, and what the forge did is not
// undone.
func (c *Component) record(ctx context.Context, call *Call, kind eventKind, ev writeEvent, errorType string) error {
	meta := map[string]any{}
	maps.Copy(meta, ev.extra)
	meta["target"], meta["ok"] = call.Target, errorType == ""
	if errorType != "" {
		meta["error_type"] = errorType
	}
	if ev.number != 0 {
		meta["number"] = ev.number
	}
	if ev.url != "" {
		meta["url"] = ev.url
	}
	if call.Requested.Slug != call.Workspace.Slug {
		meta["fork"] = call.Requested.Slug
	}
	actorType, actorID := actorOf(ctx)
	_, err := c.deps.Events.EmitNow(ctx, event.Input{
		Type: kind.typ, Workspace: &call.Workspace.Slug, ActorType: actorType, ActorID: actorID,
		ResourceType: kind.resource, ResourceID: ev.resourceID(), Metadata: meta,
	})
	if err != nil {
		return fmt.Errorf("record %s of workspace %s: %w", kind.typ, call.Workspace.Slug, err)
	}
	return nil
}

// actorOf is the event actor of the request whose context ctx is: the
// type of its credential and the user it acts for, none for the admin
// token.
func actorOf(ctx context.Context) (string, *string) {
	p, _ := identity.PrincipalOf(ctx)
	typ := p.Type
	if typ == identity.TypeAdmin {
		typ = event.ActorAdminToken
	}
	if p.User == nil {
		return typ, nil
	}
	return typ, &p.User.ID
}

// archived refuses a write to an archived workspace, or through a task
// fork to its archived parent.
func archived(call *Call) error {
	switch {
	case call.Requested.Status == workspace.StatusArchived:
		return server.WorkspaceArchived(fmt.Sprintf("workspace %q is archived", call.Requested.Slug))
	case call.Workspace.Slug != call.Requested.Slug && call.Workspace.Status == workspace.StatusArchived:
		return server.NewError("parent_archived", http.StatusConflict,
			fmt.Sprintf("workspace %q is read-only: its parent workspace %q is archived", call.Requested.Slug, call.Workspace.Slug))
	}
	return nil
}

// send sends method <the API URL><path> to the target's forge with the
// target's credential, and body in JSON unless it is nil. Every forge
// request of a route goes through it, so that it is counted as sent.
func (c *Call) send(ctx context.Context, method, path string, body any) (*Response, error) {
	c.sent++
	return c.Client.Do(ctx, Request{Method: method, URL: c.Resolution.APIURL + path,
		Kind: c.Resolution.Kind, Credential: c.Resolution.Credential, Body: body})
}

// sendJSON sends a write as send does and reads the forge's answer into v.
func (c *Call) sendJSON(ctx context.Context, method, path string, body, v any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	return Decode(resp, v)
}

// writer is a forge's API for the writes, as api is for the reads.
type writer interface {
	// createIssue creates an issue with the fields of in that are set.
	createIssue(ctx context.Context, call *Call, in issueInput) (Issue, error)
}

func writerOf(kind ForgeKind) writer {
	if kind == KindGitLab {
		return gitlab{}
	}
	return github{}
}

// members are the members of a write body, each as its JSON value. A member
// whose value is null is not among them, so that null counts as absent;
// members no rule reads are ignored.
type members map[string]json.RawMessage

// readObject reads the body of r, which must be one JSON object: a body
// that is missing, not valid JSON or not an object is 400 invalid_request.
func readObject(r *http.Request) (members, error) {
	var m members
	if err := server.DecodeJSON(r, &m); err != nil {
		return nil, err
	}
	for name, v := range m {
		if string(v) == "null" {
			delete(m, name)
		}
	}
	return m, nil
}

// The rules of the members. Each reads the member name into *dst, which it
// leaves nil when the member is absent, and refuses a value that breaks the
// rule with 400 invalid_request naming the member. A route reads its body
// with them in its fields' order and answers the first refusal:
//
//	err := cmp.Or(m.required("title", &in.Title), m.text("body", &in.Body))

// text reads the string member name.
func (m members) text(name string, dst **string) error {
	v, ok := m[name]
	if !ok {
		return nil
	}
	var s string
	if v[0] != '"' || json.Unmarshal(v, &s) != nil {
		return server.InvalidRequest(name + " must be a string")
	}
	*dst = &s
	return nil
}

// filled reads the string member name, which is not blank when it is set.
func (m members) filled(name string, dst **string) error {
	if err := m.text(name, dst); err != nil {
		return err
	}
	if *dst != nil && blank(**dst) {
		return server.InvalidRequest(name + " must not be blank")
	}
	return nil
}

// required reads the string member name, which must be set and not blank.
func (m members) required(name string, dst **string) error {
	if _, ok := m[name]; !ok {
		return server.InvalidRequest(name + " is required")
	}
	return m.filled(name, dst)
}

// maxNames is how many entries a list of names may have.
const maxNames = 100

// names reads the member name, a list of names: an array of at most
// maxNames entries, each a string that is not blank.
func (m members) names(name string, dst **[]string) error {
	v, ok := m[name]
	if !ok {
		return nil
	}
	list, err := readNames(name, v)
	if err != nil {
		return err
	}
	*dst = &list
	return nil
}

// readNames reads v, the JSON value of what is called name, as a list of
// names (see names).
func readNames(name string, v json.RawMessage) ([]string, error) {
	var entries []json.RawMessage
	if v[0] != '[' || json.Unmarshal(v, &entries) != nil {
		return nil, server.InvalidRequest(name + " must be an array of names")
	}
	if len(entries) > maxNames {
		return nil, server.InvalidRequest(fmt.Sprintf("%s holds %d names; at most %d are allowed", name, len(entries), maxNames))
	}
	list := make([]string, len(entries))
	for i, e := range entries {
		if e[0] != '"' || json.Unmarshal(e, &list[i]) != nil {
			return nil, server.InvalidRequest(fmt.Sprintf("%s[%d] must be a string", name, i))
		}
		if blank(list[i]) {
			return nil, server.InvalidRequest(fmt.Sprintf("%s[%d] must not be blank", name, i))
		}
	}
	return list, nil
}

// blank reports whether s is empty or made of Unicode white space only.
func blank(s string) bool {
	return strings.TrimFunc(s, unicode.IsSpace) == ""
}

// measured is what a route's before keeps when the body holds texts whose
// length the forge limits.
type measured interface {
	// texts are the measured texts of the body.
	texts() []text
}

// text is a measured text: its field, and its value, nil when it is not
// set.
type text struct {
	field string
	value *string
}

// textLimit is the most Unicode code points a forge of kind takes in a
// text.
func textLimit(kind ForgeKind) int {
	if kind == KindGitLab {
		return 1_000_000
	}
	return 65_536
}

// limitTexts refuses a text longer than the forge of kind takes, before it
// is sent, so that it is not refused or cut short by the forge.
func limitTexts(kind ForgeKind, texts []text) error {
	limit := textLimit(kind)
	for _, t := range texts {
		if t.value == nil {
			continue
		}
		if n := utf8.RuneCountInString(*t.value); n > limit {
			return server.PayloadTooLarge(fmt.Sprintf("%s is %d characters long; the forge takes at most %d", t.field, n, limit))
		}
	}
	return nil
}
