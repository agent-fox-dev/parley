package forge

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The bounds of a webhook secret the caller gives: its length in
// characters, each of which is printable ASCII other than the space.
const (
	webhookSecretMin = 16
	webhookSecretMax = 255
)

// webhookSecretBytes is the number of random bytes a generated webhook
// secret is made of, written as twice as many lower-case hex characters.
const webhookSecretBytes = 32

// errWebhookBody is the refusal of a webhook body outside the rule. It is
// one fixed message, so that it never echoes the value submitted.
var errWebhookBody = server.InvalidRequest("the body must be empty or a JSON object whose optional member secret is null " +
	"or a string of 16 to 255 characters, each from ! to ~")

// errSecretLimit is how the webhook route answers a workspace whose secret
// scope has no room for the webhook secret, as the secrets routes do.
var errSecretLimit = server.NewError("secret_limit_reached", http.StatusTooManyRequests, secret.ErrLimitReached.Error())

// webhookAnswer is the answer of a PUT of the webhook. Secret is set only
// when the hub generated it: a stored value is never shown again.
type webhookAnswer struct {
	Configured bool    `json:"configured"`
	URL        *string `json:"url"`
	Secret     string  `json:"secret,omitempty"`
}

// webhookRoutes are the routes that manage a workspace's webhook secret.
// They are plain workspace routes, not built with Route, which switches a
// task fork to its parent: a fork has no origin to receive deliveries for,
// and these routes refuse it.
func (c *Component) webhookRoutes() []server.Route {
	route := func(method string, handler func(http.ResponseWriter, *http.Request) error) server.Route {
		return server.Route{
			Method: method, Pattern: "/workspaces/:slug/forge/webhook", Scope: ScopeWrite,
			Marks:   map[string]string{access.MarkNeed: string(access.NeedWrite)},
			Handler: handler,
		}
	}
	return []server.Route{route(http.MethodPut, c.setWebhook), route(http.MethodDelete, c.unsetWebhook)}
}

// setWebhook stores the workspace's webhook secret, the one the body gives
// or one it generates, and records its event in the same store
// transaction. After the pipeline's scope and access checks it refuses, in
// this order: a task fork, a body outside the rule, an archived workspace,
// a hub without an encryption key and a secret scope without room for a new
// entry.
func (c *Component) setWebhook(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	d, _ := access.DecisionOf(r)
	slug := d.Record.Slug
	if err := refuseFork(d.Record); err != nil {
		return err
	}
	value, given, err := readWebhookBody(r)
	if err != nil {
		return err
	}
	if d.Record.Status == access.StatusArchived {
		return server.WorkspaceArchived(fmt.Sprintf("workspace %q is archived", slug))
	}
	if !c.deps.SecretWriter.Available() {
		return secret.UnavailableError
	}
	if !given {
		if value, err = generateWebhookSecret(); err != nil {
			return err
		}
	}
	actorType, actorID := actorOf(ctx)
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := c.deps.SecretWriter.Put(ctx, tx, secret.ScopeWorkspace, slug, SecretWebhook, value); err != nil {
			return err
		}
		_, err := c.deps.Log.Emit(ctx, tx, event.Input{
			Type: "parley.forge.webhook.set", Workspace: &slug, ActorType: actorType, ActorID: actorID,
			ResourceType: "webhook", ResourceID: slug, Metadata: map[string]any{"generated": !given},
		})
		return err
	})
	switch {
	case errors.Is(err, secret.ErrUnavailable):
		return secret.UnavailableError
	case errors.Is(err, secret.ErrLimitReached):
		return errSecretLimit
	case err != nil:
		return fmt.Errorf("store the webhook secret of %s: %w", slug, err)
	}
	answer := webhookAnswer{Configured: true, URL: c.webhookURL(slug)}
	if !given {
		answer.Secret = value
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// unsetWebhook removes the workspace's webhook secret and records its
// event, whether or not a secret existed, in the same store transaction.
// It serves an archived workspace too, so that its ingress can be turned
// off, and refuses only a task fork after the pipeline's scope and access
// checks.
func (c *Component) unsetWebhook(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	d, _ := access.DecisionOf(r)
	slug := d.Record.Slug
	if err := refuseFork(d.Record); err != nil {
		return err
	}
	actorType, actorID := actorOf(ctx)
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		existed, err := c.deps.SecretWriter.Delete(ctx, tx, secret.ScopeWorkspace, slug, SecretWebhook)
		if err != nil {
			return err
		}
		_, err = c.deps.Log.Emit(ctx, tx, event.Input{
			Type: "parley.forge.webhook.unset", Workspace: &slug, ActorType: actorType, ActorID: actorID,
			ResourceType: "webhook", ResourceID: slug, Metadata: map[string]any{"existed": existed},
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("remove the webhook secret of %s: %w", slug, err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// refuseFork refuses a webhook or delivery log route on a task fork, which
// has no origin of its own.
func refuseFork(rec access.Record) error {
	if rec.Kind != access.KindFork {
		return nil
	}
	return server.NewError("fork_has_no_origin", http.StatusBadRequest,
		fmt.Sprintf("workspace %q is a task fork, which has no origin to receive webhook deliveries for", rec.Slug))
}

// readWebhookBody reads the body of a PUT of the webhook: an empty body,
// an object without secret and a null secret ask the hub to generate one
// (given false); a secret within the bounds is given. Anything else is
// refused with errWebhookBody. Members other than secret are ignored.
func readWebhookBody(r *http.Request) (value string, given bool, err error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", false, server.InvalidRequest("request body could not be read")
	}
	if len(raw) == 0 {
		return "", false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var members map[string]json.RawMessage
	if err := dec.Decode(&members); err != nil || members == nil {
		return "", false, errWebhookBody
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", false, errWebhookBody
	}
	field, ok := members["secret"]
	if !ok || string(field) == "null" {
		return "", false, nil
	}
	if err := json.Unmarshal(field, &value); err != nil || !validWebhookSecret(value) {
		return "", false, errWebhookBody
	}
	return value, true, nil
}

// validWebhookSecret reports whether s has 16 to 255 characters, each from
// ! to ~.
func validWebhookSecret(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < webhookSecretMin || n > webhookSecretMax {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < '!' || r > '~' }) < 0
}

// generateWebhookSecret is a new webhook secret from the cryptographic
// random source.
func generateWebhookSecret() (string, error) {
	b := make([]byte, webhookSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a webhook secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// webhookURL is the hub's URL the forge delivers the webhooks of the
// workspace slug to, nil when the hub has no external URL.
func (c *Component) webhookURL(slug string) *string {
	if c.deps.ExternalURL == "" {
		return nil
	}
	return new(strings.TrimSuffix(c.deps.ExternalURL, "/") + "/hooks/forge/" + slug)
}

// TxEmitter records a hub event inside the caller's store transaction, so
// that it commits or rolls back with the change it reports, as the event
// component's Emit does.
type TxEmitter interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// SecretWriter stores and removes a workspace secret inside the caller's
// store transaction, and tells whether an encryption key is configured, as
// the secret component does.
type SecretWriter interface {
	Put(ctx context.Context, tx *sql.Tx, scope secret.Scope, scopeID, name, value string) error
	Delete(ctx context.Context, tx *sql.Tx, scope secret.Scope, scopeID, name string) (bool, error)
	Available() bool
}
