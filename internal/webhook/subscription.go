package webhook

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/google/uuid"
)

// The types of the events the changes of a subscription write.
const (
	EventCreate = "parley.webhook.create"
	EventUpdate = "parley.webhook.update"
	EventDelete = "parley.webhook.delete"
)

// resourceType is the resource_type of every parley.webhook.* event.
const resourceType = "event_webhook"

// The states of a subscription.
const (
	StateActive   = "active"
	StateFailing  = "failing"
	StateDisabled = "disabled"
)

// The limits of the create body.
const (
	maxURLLen         = 2048
	maxTypes          = 20
	maxDescriptionLen = 1024
)

// errorTypeLimitReached answers a create on a workspace that holds
// [webhooks].max_per_workspace subscriptions, with 429.
const errorTypeLimitReached = "webhook_limit_reached"

// secretPrefix starts every signing secret, so that a receiver's operator
// recognises one.
const secretPrefix = "whsec_"

// Subscription is a stored subscription: every member of the subscription
// object, the signing secret sealed under the operator's key and the
// position, the seq up to which the subscription has examined the event
// log. A nullable member is nil while it has no value.
type Subscription struct {
	ID             string
	Workspace      string
	URL            string
	Types          []string
	IncludeForks   bool
	Description    string
	State          string
	CreatedBy      *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastDeliveryAt *time.Time
	LastStatus     *string
	FailingSince   *time.Time
	Sealed         []byte
	Position       int64
}

// objectJSON is the subscription object every webhook route answers: its
// thirteen members, each always present, the nullable ones null while they
// have no value, and times in RFC 3339, UTC, with whole seconds. The signing
// secret is never one of them.
type objectJSON struct {
	ID             string        `json:"id"`
	Workspace      string        `json:"workspace"`
	URL            string        `json:"url"`
	Types          []string      `json:"types"`
	IncludeForks   bool          `json:"include_forks"`
	Description    string        `json:"description"`
	State          string        `json:"state"`
	CreatedBy      *string       `json:"created_by"`
	CreatedAt      timefmt.Time  `json:"created_at"`
	UpdatedAt      timefmt.Time  `json:"updated_at"`
	LastDeliveryAt *timefmt.Time `json:"last_delivery_at"`
	LastStatus     *string       `json:"last_status"`
	FailingSince   *timefmt.Time `json:"failing_since"`
}

// withSecret is the subscription object with the signing secret, the one
// answer that shows it.
type withSecret struct {
	objectJSON
	Secret string `json:"secret"`
}

// objectOf returns the subscription object of s.
func objectOf(s Subscription) objectJSON {
	return objectJSON{
		ID: s.ID, Workspace: s.Workspace, URL: s.URL, Types: s.Types, IncludeForks: s.IncludeForks,
		Description: s.Description, State: s.State, CreatedBy: s.CreatedBy,
		CreatedAt: timefmt.Time(s.CreatedAt), UpdatedAt: timefmt.Time(s.UpdatedAt),
		LastDeliveryAt: timeOrNil(s.LastDeliveryAt), LastStatus: s.LastStatus, FailingSince: timeOrNil(s.FailingSince),
	}
}

func timeOrNil(t *time.Time) *timefmt.Time {
	if t == nil {
		return nil
	}
	v := timefmt.Time(*t)
	return &v
}

// createRequest is a create body that has passed the field rules, with
// the defaults of the members it left out: the url as given and parsed.
type createRequest struct {
	rawURL       string
	url          *url.URL
	types        []string
	includeForks bool
	description  string
}

// parseCreate checks the members of a create body, in the order url, types,
// include_forks, description, and answers the first that breaks its rule
// 400 invalid_request naming it. Members it does not know are ignored.
func parseCreate(body map[string]json.RawMessage, s Settings) (createRequest, error) {
	req := createRequest{types: []string{"*"}, includeForks: true}
	raw, ok := body["url"]
	if !ok {
		return req, server.InvalidRequest("url is required")
	}
	var err error
	if req.rawURL, req.url, err = urlField(raw, s); err != nil {
		return req, err
	}
	if raw, ok := body["types"]; ok {
		if req.types, err = parseTypes(raw); err != nil {
			return req, err
		}
	}
	if raw, ok := body["include_forks"]; ok {
		if req.includeForks, err = boolField("include_forks", raw); err != nil {
			return req, err
		}
	}
	if raw, ok := body["description"]; ok {
		if req.description, err = descriptionField(raw); err != nil {
			return req, err
		}
	}
	return req, nil
}

// urlField checks a url member: a JSON string that parseURL accepts.
func urlField(raw json.RawMessage, s Settings) (string, *url.URL, error) {
	text, ok := stringOf(raw)
	if !ok {
		return "", nil, server.InvalidRequest("url must be a string")
	}
	u, err := parseURL(text, s.AllowInsecure)
	if err != nil {
		return "", nil, err
	}
	return text, u, nil
}

// boolField checks the member name, which must be a JSON bool.
func boolField(name string, raw json.RawMessage) (bool, error) {
	v, ok := boolOf(raw)
	if !ok {
		return false, server.InvalidRequest(name + " must be true or false")
	}
	return v, nil
}

// descriptionField checks a description member: a JSON string of at most
// 1,024 characters.
func descriptionField(raw json.RawMessage) (string, error) {
	text, ok := stringOf(raw)
	if !ok {
		return "", server.InvalidRequest("description must be a string")
	}
	if utf8.RuneCountInString(text) > maxDescriptionLen {
		return "", server.InvalidRequest(fmt.Sprintf("description is longer than %d characters", maxDescriptionLen))
	}
	return text, nil
}

// parseURL checks the shape of a destination URL: at most 2,048
// characters, absolute, with a host, without userinfo and without a
// fragment, and https, or http when allowInsecure is true and the host is a
// loopback address. Where the host resolves to is not its concern.
func parseURL(text string, allowInsecure bool) (*url.URL, error) {
	if utf8.RuneCountInString(text) > maxURLLen {
		return nil, server.InvalidRequest(fmt.Sprintf("url is longer than %d characters", maxURLLen))
	}
	u, err := url.Parse(text)
	switch {
	case err != nil:
		return nil, server.InvalidRequest("url is not a valid URL")
	case !u.IsAbs():
		return nil, server.InvalidRequest("url must be an absolute URL")
	case u.Opaque != "" || u.Hostname() == "":
		return nil, server.InvalidRequest("url has no host")
	case u.User != nil:
		return nil, server.InvalidRequest("url must not hold userinfo")
	case strings.Contains(text, "#"):
		// url.Parse drops an empty fragment, so the text is what tells.
		return nil, server.InvalidRequest("url must not have a fragment")
	case u.Scheme == "https":
		return u, nil
	case u.Scheme == "http" && allowInsecure && loopbackHost(u.Hostname()):
		return u, nil
	case u.Scheme == "http" && allowInsecure:
		return nil, server.InvalidRequest("url must use https; http is accepted only for a loopback host")
	default:
		return nil, server.InvalidRequest("url must use https")
	}
}

// loopbackHost reports whether host is a loopback address as
// [webhooks].allow_insecure takes it: a literal address in 127.0.0.0/8, ::1,
// or the name localhost.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.Is4() && addr.As4()[0] == 127 || addr == netip.IPv6Loopback()
}

// parseTypes checks types: a JSON list of 1 to 20 strings, which, joined,
// the event component's filter syntax accepts, so that a subscription's
// types mean what they mean on the event stream. An entry may not hold a
// comma, which would split it there.
func parseTypes(raw json.RawMessage) ([]string, error) {
	var types []string
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &types) != nil {
		return nil, server.InvalidRequest("types must be a list of strings")
	}
	switch {
	case len(types) == 0:
		return nil, server.InvalidRequest("types must not be empty")
	case len(types) > maxTypes:
		return nil, server.InvalidRequest(fmt.Sprintf("types has more than %d entries", maxTypes))
	}
	for _, t := range types {
		if strings.Contains(t, ",") {
			return nil, server.InvalidRequest(fmt.Sprintf("types entry %q holds a comma", t))
		}
	}
	if _, err := event.ParseTypes(strings.Join(types, ",")); err != nil {
		return nil, err
	}
	return types, nil
}

// stringOf is raw as a JSON string; false for any other JSON value, null
// included.
func stringOf(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// boolOf is raw as a JSON bool; false for any other JSON value, null
// included.
func boolOf(raw json.RawMessage) (bool, bool) {
	switch string(raw) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// create answers POST /workspaces/:slug/webhooks: once the body passes the
// field rules, 400 webhook_destination_refused when checkDestination
// refuses the url, 409 workspace_archived on an archived workspace, 429
// webhook_limit_reached when the workspace holds [webhooks].max_per_workspace
// subscriptions in any state, and 503 secrets_unavailable without a key.
// Otherwise it stores the subscription with a new signing secret, sealed,
// writes parley.webhook.create in the same transaction, sets the position
// to that event's seq, so that the first event the subscription can receive
// is the next one written, starts its delivery loop once the transaction has
// committed, and answers 201 with the object and the secret.
func (c *Component) create(w http.ResponseWriter, r *http.Request, q request) error {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	in, err := parseCreate(body, c.deps.Settings)
	if err != nil {
		return err
	}
	if err := c.checkDestination(r.Context(), in.url); err != nil {
		return err
	}
	slug := q.slug()
	if err := refuseArchived(q); err != nil {
		return err
	}
	caller, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return server.Internal("the webhook create found no caller")
	}
	plain, err := newSecret()
	if err != nil {
		return err
	}
	now := c.deps.Clock.Now().UTC().Truncate(time.Second)
	sub := Subscription{
		ID: uuid.NewString(), Workspace: slug, URL: in.rawURL, Types: in.types, IncludeForks: in.includeForks,
		Description: in.description, State: StateActive, CreatedBy: createdBy(caller), CreatedAt: now, UpdatedAt: now,
	}
	ctx := r.Context()
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		n, err := countIn(ctx, tx, slug)
		if err != nil {
			return err
		}
		if limit := c.deps.Settings.MaxPerWorkspace; n >= limit {
			return server.NewError(errorTypeLimitReached, http.StatusTooManyRequests,
				fmt.Sprintf("workspace %s holds %d event webhooks, the most [webhooks].max_per_workspace allows", slug, limit))
		}
		if !c.deps.Available() {
			return secret.UnavailableError
		}
		if sub.Sealed, err = c.seal(sub.ID, plain); err != nil {
			return err
		}
		if err := insert(ctx, tx, sub); err != nil {
			return err
		}
		rec, err := c.emit(ctx, tx, EventCreate, sub, caller,
			map[string]any{"webhook_id": sub.ID, "host": in.url.Hostname(), "types": sub.Types, "include_forks": sub.IncludeForks})
		if err != nil {
			return err
		}
		sub.Position = rec.Seq
		return setPosition(ctx, tx, sub.ID, rec.Seq)
	})
	if err != nil {
		return err
	}
	c.startLoop(sub.ID)
	server.WriteJSON(w, http.StatusCreated, withSecret{objectOf(sub), plain})
	return nil
}

// updateFields are the members a change may give, in the order the
// parley.webhook.update event names them.
var updateFields = []string{"url", "types", "include_forks", "description", "active"}

// updateRequest is a change body that has passed the field rules: fields
// are the names it gives, in the order of updateFields, and each member it
// gives is set, the url as given and parsed.
type updateRequest struct {
	fields       []string
	rawURL       *string
	url          *url.URL
	types        []string
	includeForks *bool
	description  *string
	active       *bool
}

// parseUpdate checks a change body: it must give at least one of the
// fields, nothing else, and each with a value its rule accepts, the rules of
// the create for url, types, include_forks and description and a bool for
// active. A body that breaks one is answered 400 invalid_request.
func parseUpdate(body map[string]json.RawMessage, s Settings) (updateRequest, error) {
	var req updateRequest
	if len(body) == 0 {
		return req, server.InvalidRequest("the body must give at least one of url, types, include_forks, description and active")
	}
	var unknown []string
	for name := range body {
		if !slices.Contains(updateFields, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return req, server.InvalidRequest(fmt.Sprintf("%s is not a field of an event webhook; a change gives url, types, include_forks, description or active", unknown[0]))
	}
	for _, name := range updateFields {
		raw, ok := body[name]
		if !ok {
			continue
		}
		req.fields = append(req.fields, name)
		var err error
		switch name {
		case "url":
			var text string
			if text, req.url, err = urlField(raw, s); err == nil {
				req.rawURL = &text
			}
		case "types":
			req.types, err = parseTypes(raw)
		case "include_forks":
			var v bool
			if v, err = boolField(name, raw); err == nil {
				req.includeForks = &v
			}
		case "description":
			var text string
			if text, err = descriptionField(raw); err == nil {
				req.description = &text
			}
		case "active":
			var v bool
			if v, err = boolField(name, raw); err == nil {
				req.active = &v
			}
		}
		if err != nil {
			return req, err
		}
	}
	return req, nil
}

// apply changes s as req asks. active sets the state: false switches the
// subscription off, true on.
func (req updateRequest) apply(s *Subscription) {
	if req.rawURL != nil {
		s.URL = *req.rawURL
	}
	if req.types != nil {
		s.Types = req.types
	}
	if req.includeForks != nil {
		s.IncludeForks = *req.includeForks
	}
	if req.description != nil {
		s.Description = *req.description
	}
	if req.active != nil {
		s.State = StateActive
		if !*req.active {
			s.State = StateDisabled
		}
	}
}

// update answers PATCH /workspaces/:slug/webhooks/:id: once the body
// passes parseUpdate, 400 webhook_destination_refused when it gives a url
// checkDestination refuses, then 409 workspace_archived on an archived
// workspace.
// Otherwise it changes exactly the fields given, sets updated_at, writes
// parley.webhook.update with the names of the fields given and the state
// after the change in the same transaction, and answers 200 with the
// object.
func (c *Component) update(w http.ResponseWriter, r *http.Request, q request) error {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	in, err := parseUpdate(body, c.deps.Settings)
	if err != nil {
		return err
	}
	if in.url != nil {
		if err := c.checkDestination(r.Context(), in.url); err != nil {
			return err
		}
	}
	if err := refuseArchived(q); err != nil {
		return err
	}
	caller, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return server.Internal("the webhook update found no caller")
	}
	now := c.deps.Clock.Now().UTC().Truncate(time.Second)
	ctx := r.Context()
	var sub Subscription
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if sub, err = c.reread(ctx, tx, q); err != nil {
			return err
		}
		in.apply(&sub)
		sub.UpdatedAt = now
		if err := save(ctx, tx, sub); err != nil {
			return err
		}
		_, err = c.emit(ctx, tx, EventUpdate, sub, caller,
			map[string]any{"webhook_id": sub.ID, "fields": in.fields, "state": sub.State})
		return err
	})
	if err != nil {
		return err
	}
	// The loop reads the subscription again: a disabled one ends.
	c.wakeLoop(sub.ID)
	server.WriteJSON(w, http.StatusOK, objectOf(sub))
	return nil
}

// remove answers DELETE /workspaces/:slug/webhooks/:id in every status of
// the workspace: it deletes the subscription with its deliveries and its
// sealed signing secret, writes parley.webhook.delete in the same
// transaction, and answers 204.
func (c *Component) remove(w http.ResponseWriter, r *http.Request, q request) error {
	caller, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return server.Internal("the webhook delete found no caller")
	}
	ctx := r.Context()
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		sub, err := c.reread(ctx, tx, q)
		if err != nil {
			return err
		}
		if err := deleteSubscription(ctx, tx, sub.ID); err != nil {
			return err
		}
		_, err = c.emit(ctx, tx, EventDelete, sub, caller, map[string]any{"webhook_id": sub.ID, "host": hostOf(sub.URL)})
		return err
	})
	if err != nil {
		return err
	}
	// The loop finds the subscription gone and ends.
	c.wakeLoop(q.subscription.ID)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// rotate answers POST /workspaces/:slug/webhooks/:id/rotate-secret: 409
// workspace_archived on an archived workspace and 503 secrets_unavailable
// without a key. Otherwise it replaces the sealed signing secret with a new
// one's, writes parley.webhook.update with the field secret in the same
// transaction, and answers 200 with the object and the new secret.
func (c *Component) rotate(w http.ResponseWriter, r *http.Request, q request) error {
	if err := refuseArchived(q); err != nil {
		return err
	}
	if !c.deps.Available() {
		return secret.UnavailableError
	}
	caller, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return server.Internal("the webhook rotation found no caller")
	}
	plain, err := newSecret()
	if err != nil {
		return err
	}
	now := c.deps.Clock.Now().UTC().Truncate(time.Second)
	ctx := r.Context()
	var sub Subscription
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if sub, err = c.reread(ctx, tx, q); err != nil {
			return err
		}
		if sub.Sealed, err = c.seal(sub.ID, plain); err != nil {
			return err
		}
		sub.UpdatedAt = now
		if err := save(ctx, tx, sub); err != nil {
			return err
		}
		_, err = c.emit(ctx, tx, EventUpdate, sub, caller,
			map[string]any{"webhook_id": sub.ID, "fields": []string{"secret"}, "state": sub.State})
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, withSecret{objectOf(sub), plain})
	return nil
}

// refuseArchived answers a change of a subscription of an archived
// workspace 409 workspace_archived.
func refuseArchived(q request) error {
	if q.decision.Record.Status == access.StatusArchived {
		return server.WorkspaceArchived("workspace " + q.slug() + " is archived; its event webhooks cannot be created or changed")
	}
	return nil
}

// reread reads the subscription the guard found again inside tx, so that a
// change starts from what the transaction sees; one deleted in between is
// answered 404 webhook_not_found.
func (c *Component) reread(ctx context.Context, tx *sql.Tx, q request) (Subscription, error) {
	sub, found, err := subscriptionIn(ctx, tx, q.slug(), q.subscription.ID)
	if err != nil {
		return Subscription{}, err
	}
	if !found {
		return Subscription{}, server.NewError(errorTypeWebhookNotFound, http.StatusNotFound, msgWebhookNotFound)
	}
	return sub, nil
}

// seal seals the signing secret plain of the subscription id under the
// current key; without one it answers 503 secrets_unavailable.
func (c *Component) seal(id, plain string) ([]byte, error) {
	sealed, err := secret.SealValue(c.deps.Keys(), binding(id), plain)
	if errors.Is(err, secret.ErrUnavailable) {
		return nil, secret.UnavailableError
	}
	return sealed, err
}

// emit writes the event typ of the subscription s, caused by caller, with
// metadata, inside tx, and returns the stored record. The metadata names the
// destination by its host alone (hostOf).
func (c *Component) emit(ctx context.Context, tx *sql.Tx, typ string, s Subscription, caller identity.Principal, metadata map[string]any) (event.Record, error) {
	actorType, actorID := actorOf(caller)
	return c.deps.Events.Emit(ctx, tx, event.Input{
		Type: typ, Workspace: &s.Workspace, ActorType: actorType, ActorID: actorID,
		ResourceType: resourceType, ResourceID: s.ID, Metadata: metadata,
	})
}

// hostOf is the host of a stored destination URL, the one part of it an
// event names: a URL's path or query may carry a token of the receiver's.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// newSecret returns a new signing secret: whsec_ and 64 lower-case
// hexadecimal characters from 32 bytes of a cryptographic random source.
func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate a signing secret: %w", err)
	}
	return secretPrefix + hex.EncodeToString(b[:]), nil
}

// binding is the binding string a subscription's signing secret is sealed
// under, so that a ciphertext copied to another subscription does not open.
func binding(id string) string { return "webhook:" + id }

// createdBy is the created_by of a subscription the caller p creates: its
// user's id, nil for the admin token.
func createdBy(p identity.Principal) *string {
	if p.Type == identity.TypeAdmin || p.User == nil {
		return nil
	}
	id := p.User.ID
	return &id
}

// actorOf is the actor of an event the caller p causes.
func actorOf(p identity.Principal) (string, *string) {
	switch p.Type {
	case identity.TypeAdmin:
		return event.ActorAdminToken, nil
	case identity.TypePAT:
		return event.ActorPAT, createdBy(p)
	default:
		return event.ActorAPIKey, createdBy(p)
	}
}

// querier is what the store functions read and write through: a
// transaction or the store's database.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// subscriptionColumns are the columns scanSubscription reads, in its order.
const subscriptionColumns = `id, workspace, url, types, include_forks, description, state, created_by,
	created_at, updated_at, last_delivery_at, last_status, failing_since, secret_sealed, position`

// insert stores the subscription s.
func insert(ctx context.Context, q querier, s Subscription) error {
	types, err := json.Marshal(s.Types)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO webhook_subscriptions (`+subscriptionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Workspace, s.URL, string(types), s.IncludeForks, s.Description, s.State, s.CreatedBy,
		s.CreatedAt.UnixNano(), s.UpdatedAt.UnixNano(), nanosOrNil(s.LastDeliveryAt), s.LastStatus,
		nanosOrNil(s.FailingSince), s.Sealed, s.Position)
	if err != nil {
		return fmt.Errorf("store event webhook %s: %w", s.ID, err)
	}
	return nil
}

// save writes the members of the subscription s a change may alter: url,
// types, include_forks, description, state, updated_at and the sealed
// signing secret.
func save(ctx context.Context, q querier, s Subscription) error {
	types, err := json.Marshal(s.Types)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE webhook_subscriptions
		SET url = ?, types = ?, include_forks = ?, description = ?, state = ?, updated_at = ?, secret_sealed = ?
		WHERE id = ?`,
		s.URL, string(types), s.IncludeForks, s.Description, s.State, s.UpdatedAt.UnixNano(), s.Sealed, s.ID)
	if err != nil {
		return fmt.Errorf("change event webhook %s: %w", s.ID, err)
	}
	return nil
}

// deleteSubscription deletes the subscription id, its sealed signing secret
// with it, and its deliveries.
func deleteSubscription(ctx context.Context, q querier, id string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE subscription_id = ?`, id); err != nil {
		return fmt.Errorf("delete the deliveries of event webhook %s: %w", id, err)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM webhook_subscriptions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete event webhook %s: %w", id, err)
	}
	return nil
}

// setPosition sets the position of the subscription id to seq.
func setPosition(ctx context.Context, q querier, id string, seq int64) error {
	if _, err := q.ExecContext(ctx, `UPDATE webhook_subscriptions SET position = ? WHERE id = ?`, seq, id); err != nil {
		return fmt.Errorf("set the position of event webhook %s: %w", id, err)
	}
	return nil
}

// countIn is the number of subscriptions of the workspace slug, in any
// state.
func countIn(ctx context.Context, q querier, slug string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM webhook_subscriptions WHERE workspace = ?`, slug).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the event webhooks of %s: %w", slug, err)
	}
	return n, nil
}

// subscriptionIn reads the subscription id of the workspace slug; found is
// false when the workspace has none of that id, also when another workspace
// has.
func subscriptionIn(ctx context.Context, q querier, slug, id string) (Subscription, bool, error) {
	s, err := scanSubscription(q.QueryRowContext(ctx,
		`SELECT `+subscriptionColumns+` FROM webhook_subscriptions WHERE workspace = ? AND id = ?`, slug, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, false, nil
	}
	if err != nil {
		return Subscription{}, false, fmt.Errorf("read event webhook %s: %w", id, err)
	}
	return s, true, nil
}

// scanSubscription scans a row of subscriptionColumns.
func scanSubscription(row interface{ Scan(dest ...any) error }) (Subscription, error) {
	var s Subscription
	var types string
	var createdAt, updatedAt int64
	var lastDelivery, failingSince sql.NullInt64
	var createdBy, lastStatus sql.NullString
	if err := row.Scan(&s.ID, &s.Workspace, &s.URL, &types, &s.IncludeForks, &s.Description, &s.State, &createdBy,
		&createdAt, &updatedAt, &lastDelivery, &lastStatus, &failingSince, &s.Sealed, &s.Position); err != nil {
		return Subscription{}, err
	}
	if err := json.Unmarshal([]byte(types), &s.Types); err != nil {
		return Subscription{}, fmt.Errorf("the types of event webhook %s: %w", s.ID, err)
	}
	s.CreatedAt, s.UpdatedAt = time.Unix(0, createdAt).UTC(), time.Unix(0, updatedAt).UTC()
	s.LastDeliveryAt, s.FailingSince = timeOf(lastDelivery), timeOf(failingSince)
	if createdBy.Valid {
		s.CreatedBy = &createdBy.String
	}
	if lastStatus.Valid {
		s.LastStatus = &lastStatus.String
	}
	return s, nil
}

func nanosOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func timeOf(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64).UTC()
	return &t
}
