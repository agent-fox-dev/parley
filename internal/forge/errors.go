package forge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The error types of a forge call that failed.
const (
	ErrorTypeTimeout         = "forge_timeout"
	ErrorTypeRateLimited     = "forge_rate_limited"
	ErrorTypeNotFound        = "forge_not_found"
	ErrorTypeUnauthenticated = "forge_unauthenticated"
	ErrorTypeForbidden       = "forge_forbidden"
	ErrorTypeRejected        = "forge_rejected"
	ErrorTypeError           = "forge_error"
	ErrorTypeUnreachable     = "forge_unreachable"
)

// HubError is a failed forge call as the hub answers it.
type HubError struct {
	Type   string
	Status int
	// Message says what went wrong, without the credential, without the
	// user information of a URL and at most scrub.MaxLen bytes long.
	Message string
	// RetryAfter is the value of the answer's Retry-After header: the
	// forge's last stated wait in whole seconds, rounded up, or "" when
	// it stated none.
	RetryAfter string
}

// ServerError is e as the error a handler returns. The handler sets the
// Retry-After header itself.
func (e HubError) ServerError() *server.Error {
	return server.NewError(e.Type, e.Status, e.Message)
}

// MapError maps err, a *Failure or an error that wraps one, to the error
// the hub answers, and is false for any other error, which is the hub's
// own. The message is scrubbed of the scrubber's secrets and of token, the
// credential the call sent ("" for none); a nil scrubber knows no secret
// besides token.
//
// The first kind that matches decides: a redirect not followed is 502
// forge_error; a timeout 504 forge_timeout; a rate limit 429
// forge_rate_limited; the forge's 404 is 404 forge_not_found, 401 502
// forge_unauthenticated, 403 502 forge_forbidden, any other 4xx 422
// forge_rejected; a 5xx, or a body the hub cannot read, is 502 forge_error;
// a forge that cannot be reached is 502 forge_unreachable.
func MapError(err error, scrubber *scrub.Scrubber, token string) (HubError, bool) {
	f, ok := errors.AsType[*Failure](err)
	if !ok {
		return HubError{}, false
	}
	var e HubError
	switch {
	case f.Kind == FailureRedirect:
		e = HubError{Type: ErrorTypeError, Status: http.StatusBadGateway}
	case f.Kind == FailureTimeout:
		e = HubError{Type: ErrorTypeTimeout, Status: http.StatusGatewayTimeout}
	case f.Kind == FailureRateLimited:
		e = HubError{Type: ErrorTypeRateLimited, Status: http.StatusTooManyRequests}
		if f.WaitStated {
			e.RetryAfter = strconv.FormatInt(int64((f.Wait+time.Second-1)/time.Second), 10)
		}
	case f.Kind == FailureStatus && f.Status == http.StatusNotFound:
		e = HubError{Type: ErrorTypeNotFound, Status: http.StatusNotFound}
	case f.Kind == FailureStatus && f.Status == http.StatusUnauthorized:
		e = HubError{Type: ErrorTypeUnauthenticated, Status: http.StatusBadGateway}
	case f.Kind == FailureStatus && f.Status == http.StatusForbidden:
		e = HubError{Type: ErrorTypeForbidden, Status: http.StatusBadGateway}
	case f.Kind == FailureStatus && f.Status < http.StatusInternalServerError:
		e = HubError{Type: ErrorTypeRejected, Status: http.StatusUnprocessableEntity}
	case f.Kind == FailureStatus || f.Kind == FailureUnreadable:
		e = HubError{Type: ErrorTypeError, Status: http.StatusBadGateway}
	default:
		e = HubError{Type: ErrorTypeUnreachable, Status: http.StatusBadGateway}
	}
	if scrubber == nil {
		scrubber = scrub.New()
	}
	e.Message = scrubber.Scrub(message(f), token)
	return e, true
}

// message is what f says to a caller: "the forge answered <status>: <the
// forge's message>" when the forge answered, else a description of the
// answer that is missing.
func message(f *Failure) string {
	switch f.Kind {
	case FailureStatus, FailureRateLimited:
		if m := forgeMessage(f.Body); m != "" {
			return fmt.Sprintf("the forge answered %d: %s", f.Status, m)
		}
		return fmt.Sprintf("the forge answered %d", f.Status)
	case FailureUnreadable:
		if f.Status != 0 {
			return fmt.Sprintf("the forge answered %d with a body the hub cannot read: %v", f.Status, f.Err)
		}
		return fmt.Sprintf("the forge's answer could not be read: %v", f.Err)
	case FailureRedirect:
		return fmt.Sprintf("the forge's redirect was not followed: %v", f.Err)
	case FailureTimeout:
		return "the forge did not answer within the request's deadline"
	default:
		// The request's method and URL say nothing the caller does not
		// know; the cause is what tells why.
		cause := f.Err
		if ue, ok := errors.AsType[*url.Error](cause); ok {
			cause = ue.Err
		}
		if cause == nil {
			return "the forge could not be reached"
		}
		return fmt.Sprintf("the forge could not be reached: %v", cause)
	}
}

// forgeMessage is the forge's own message in an error body: the member
// message (GitHub, GitLab), else error (GitLab), a value that is no string
// in its compact JSON form; "" when the body has neither.
func forgeMessage(body []byte) string {
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) != nil {
		return ""
	}
	for _, name := range []string{"message", "error"} {
		raw, ok := members[name]
		if !ok || string(raw) == "null" {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var compact bytes.Buffer
		if json.Compact(&compact, raw) == nil {
			return compact.String()
		}
	}
	return ""
}
