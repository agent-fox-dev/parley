package forge

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
)

// IngressPattern is the pattern, in http.ServeMux syntax, the ingress is
// mounted under: outside the REST API, so that a delivery passes neither
// its authentication nor its rate limit nor its body limit, none of which
// a forge can satisfy.
const IngressPattern = "POST /hooks/forge/{slug}"

// maxDeliveryBytes is the largest delivery body the ingress reads.
const maxDeliveryBytes = 1 << 20

// maxDeliveryIDBytes is the longest delivery id the ingress takes.
const maxDeliveryIDBytes = 255

// The headers a delivery is told apart and authenticated by.
const (
	headerGitHubSignature = "X-Hub-Signature-256"
	headerGitHubDelivery  = "X-GitHub-Delivery"
	headerGitHubEvent     = "X-GitHub-Event"
	headerGitLabToken     = "X-Gitlab-Token"
	headerGitLabEvent     = "X-Gitlab-Event"
	headerGitLabUUID      = "X-Gitlab-Event-UUID"
	headerIdempotencyKey  = "Idempotency-Key"
)

// The ingress's refusals. A workspace that cannot receive deliveries, for
// whatever reason, and a delivery that is not proven, for whatever reason,
// each get one fixed answer, so that the answer tells an unauthenticated
// caller nothing about the workspace or the secret.
var (
	errNoIngress       = server.NotFound("not found")
	errUnauthenticated = server.Unauthenticated("delivery signature missing or invalid")
	errDeliveryTooBig  = server.PayloadTooLarge(fmt.Sprintf("the delivery body is larger than %d bytes", maxDeliveryBytes))
	errDeliveryID      = server.InvalidRequest(fmt.Sprintf("the delivery id header is missing, empty, longer than %d characters "+
		"or holds a character outside space to ~", maxDeliveryIDBytes))
	errDeliveryEvent = server.InvalidRequest("the delivery's event name header is missing or empty")
	errMalformed     = server.InvalidRequest("the delivery's payload is not a JSON object of the form its event has")
)

// Ingress is the handler of the forge webhook deliveries of every
// workspace, which the hub mounts under IngressPattern. It authenticates a
// delivery by the workspace's webhook secret alone, read on every request,
// and answers in a fixed order: the workspace, the scheme and its
// credential, the body, the delivery id and event name, then the outcome
// of recording it. It never calls the forge, and logs neither the body,
// the signature, the token nor the secret.
func (c *Component) Ingress() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outcome, err := c.ingest(r)
		if err == nil && outcome != OutcomeMalformed {
			server.WriteJSON(w, http.StatusAccepted, map[string]Outcome{"outcome": outcome})
			return
		}
		var e *server.Error
		switch {
		case err == nil:
			// A malformed delivery is refused, and its record kept, so
			// that the forge's redelivery is answered as a duplicate.
			e = errMalformed
		case errors.As(err, &e):
		case errors.Is(err, errWorkspaceGone):
			// Deleted after its record was read: answered as every
			// workspace without a webhook is.
			e = errNoIngress
		default:
			// The cause names the workspace and the step, never the
			// payload or the secret.
			if c.deps.Logger != nil {
				c.deps.Logger.LogAttrs(r.Context(), slog.LevelError, "internal error",
					slog.String("request_id", server.RequestID(r.Context())),
					slog.String("error", err.Error()))
			}
			e = server.Internal("internal error")
		}
		server.WriteError(w, e)
	})
}

// ingest decides a delivery and, once it is proven, records it, returning
// its outcome; a refusal is a *server.Error.
func (c *Component) ingest(r *http.Request) (Outcome, error) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	ws, ok, err := c.deps.Workspaces(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("read workspace %s: %w", slug, err)
	}
	// The secret is read for every slug, an unknown one and a fork's
	// included, so that the refusals cost the same reads and their time
	// does not tell an unauthenticated caller which workspaces exist. A
	// secret that cannot be read fails the delivery as the hub's fault: it
	// is never taken for a workspace without a webhook.
	key, found, err := c.deps.Secrets.Get(ctx, secret.ScopeWorkspace, slug, SecretWebhook)
	if err != nil {
		return "", fmt.Errorf("read the webhook secret of %s: %w", slug, err)
	}
	if !ok || ws.Kind == access.KindFork || !found || key == "" {
		return "", errNoIngress
	}

	signatures, tokens := r.Header.Values(headerGitHubSignature), r.Header.Values(headerGitLabToken)
	var kind ForgeKind
	switch {
	case len(signatures) > 0:
		kind = KindGitHub
	case len(tokens) > 0:
		kind = KindGitLab
		// A GitLab token is checked before the body is read, so that an
		// unproven caller cannot make the hub read a megabyte.
		if len(tokens) != 1 || !sameSecret(tokens[0], key) {
			return "", errUnauthenticated
		}
	default:
		return "", errUnauthenticated
	}

	body, err := readDelivery(r)
	if err != nil {
		return "", err
	}
	if kind == KindGitHub {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if len(signatures) != 1 || !hmac.Equal([]byte(signatures[0]), []byte(want)) {
			return "", errUnauthenticated
		}
	}

	id, eventName := deliveryHeaders(r.Header, kind)
	if !validDeliveryID(id) {
		return "", errDeliveryID
	}
	if eventName == "" {
		return "", errDeliveryEvent
	}
	return c.RecordDelivery(ctx, slug, kind, id, eventName, body)
}

// readDelivery reads the body of a delivery, refusing one over
// maxDeliveryBytes without reading past the limit, or at all when its
// declared length is already over it.
func readDelivery(r *http.Request) ([]byte, error) {
	if r.ContentLength > maxDeliveryBytes {
		return nil, errDeliveryTooBig
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDeliveryBytes+1))
	if err != nil {
		return nil, server.InvalidRequest("the delivery body could not be read")
	}
	if len(body) > maxDeliveryBytes {
		return nil, errDeliveryTooBig
	}
	return body, nil
}

// deliveryHeaders are the delivery id and the event name of a delivery of
// the forge kind, each the first value of its header. A GitLab delivery's
// id is its Idempotency-Key, or its event UUID when it has none.
func deliveryHeaders(h http.Header, kind ForgeKind) (id, eventName string) {
	if kind == KindGitHub {
		return h.Get(headerGitHubDelivery), h.Get(headerGitHubEvent)
	}
	if values := h.Values(headerIdempotencyKey); len(values) > 0 {
		return values[0], h.Get(headerGitLabEvent)
	}
	return h.Get(headerGitLabUUID), h.Get(headerGitLabEvent)
}

// validDeliveryID reports whether id is 1 to maxDeliveryIDBytes characters,
// each from space to ~.
func validDeliveryID(id string) bool {
	if id == "" || len(id) > maxDeliveryIDBytes {
		return false
	}
	for i := range len(id) {
		if id[i] < ' ' || id[i] > '~' {
			return false
		}
	}
	return true
}

// sameSecret compares a GitLab token with the secret in constant time,
// lengths included: their digests, unlike the values, are of one length,
// which subtle.ConstantTimeCompare would otherwise answer at once.
func sameSecret(token, key string) bool {
	a, b := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
