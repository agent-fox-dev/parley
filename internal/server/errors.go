package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
)

// Error is an error of the REST API: a machine-readable type, the HTTP
// status it is answered with, a message for people, and optional extra
// members written beside them. Handlers and checks return it as an error;
// WriteError answers it as the error envelope.
//
// An Error is not changed once built: With and WithDetails return a copy, so
// a shared value can be extended safely.
type Error struct {
	errorType string
	status    int
	message   string
	extras    map[string]any
}

// reserved are the members every envelope has; an extra member must not
// replace one of them.
var reserved = []string{"code", "message", "error_type"}

// NewError builds an error of a type outside the catalogue, answered with
// status. Packages use it for the types they define themselves, such as
// workspace_busy. It panics when errorType is empty or status is not a 4xx or
// 5xx status: both are programming errors. An empty message is replaced by
// the status's standard text, so the envelope never carries an empty message.
func NewError(errorType string, status int, message string) *Error {
	if errorType == "" {
		panic("server.NewError: empty error type")
	}
	if status < 400 || status > 599 {
		panic(fmt.Sprintf("server.NewError: status %d of %q is not an error status", status, errorType))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &Error{errorType: errorType, status: status, message: message}
}

// Error returns the type and the message.
func (e *Error) Error() string {
	return e.errorType + ": " + e.message
}

// Type returns the machine-readable error type, the envelope's error_type.
func (e *Error) Type() string { return e.errorType }

// Status returns the HTTP status, the envelope's code.
func (e *Error) Status() int { return e.status }

// Message returns the message for people.
func (e *Error) Message() string { return e.message }

// With returns a copy of e that also writes the member name with value
// inside the error object. A later member of the same name replaces an
// earlier one. It panics when name is code, message or error_type, so an
// extra member can never replace a standard one.
func (e *Error) With(name string, value any) *Error {
	if slices.Contains(reserved, name) {
		panic(fmt.Sprintf("server.Error.With: %q is a standard member of the error envelope", name))
	}
	c := *e
	c.extras = maps.Clone(e.extras)
	if c.extras == nil {
		c.extras = make(map[string]any, 1)
	}
	c.extras[name] = value
	return &c
}

// WithDetails returns a copy of e that writes details as the member details
// inside the error object.
func (e *Error) WithDetails(details any) *Error {
	return e.With("details", details)
}

// The catalogue: one constructor per generic error type and per type the
// platform defines, each with its fixed status.

// InvalidRequest is a request the hub refuses as malformed (400).
func InvalidRequest(message string) *Error { return NewError("invalid_request", 400, message) }

// Unauthenticated is a request without a valid credential (401).
func Unauthenticated(message string) *Error { return NewError("unauthenticated", 401, message) }

// Forbidden is a request whose caller may not do what it asks (403).
func Forbidden(message string) *Error { return NewError("forbidden", 403, message) }

// NotFound is a request for something that does not exist (404).
func NotFound(message string) *Error { return NewError("not_found", 404, message) }

// MethodNotAllowed is a request with a method its path does not serve (405).
func MethodNotAllowed(message string) *Error { return NewError("method_not_allowed", 405, message) }

// Conflict is a request that conflicts with the current state (409).
func Conflict(message string) *Error { return NewError("conflict", 409, message) }

// Gone is a request for something that existed and no longer does (410).
func Gone(message string) *Error { return NewError("gone", 410, message) }

// PayloadTooLarge is a request whose body is over the limit (413).
func PayloadTooLarge(message string) *Error { return NewError("payload_too_large", 413, message) }

// UnsupportedMediaType is a request whose body has a content type the route
// does not take (415).
func UnsupportedMediaType(message string) *Error {
	return NewError("unsupported_media_type", 415, message)
}

// RateLimited is a request over the rate limit (429).
func RateLimited(message string) *Error { return NewError("rate_limited", 429, message) }

// Internal is a failure inside the hub (500).
func Internal(message string) *Error { return NewError("internal", 500, message) }

// UpstreamError is a failure of a service the hub called (502).
func UpstreamError(message string) *Error { return NewError("upstream_error", 502, message) }

// Unavailable is a hub that cannot serve the request now (503).
func Unavailable(message string) *Error { return NewError("unavailable", 503, message) }

// InsufficientScope is a token that lacks the scope the route needs (403).
func InsufficientScope(message string) *Error { return NewError("insufficient_scope", 403, message) }

// TokenBound is a workspace-bound token used outside what it is bound to (403).
func TokenBound(message string) *Error { return NewError("token_bound", 403, message) }

// PathNotAllowed is a write to a path the caller may not change (403).
func PathNotAllowed(message string) *Error { return NewError("path_not_allowed", 403, message) }

// WorkspaceArchived is a change asked of an archived workspace (409).
func WorkspaceArchived(message string) *Error { return NewError("workspace_archived", 409, message) }

// contentTypeJSON is the content type of every JSON body the server writes.
const contentTypeJSON = "application/json; charset=utf-8"

// WriteError answers e as the error envelope
// {"error":{"code":…,"message":…,"error_type":…}}, with e's extra members
// inside error after the standard three, in name order. code is e's status,
// which is also the response's status.
func WriteError(w http.ResponseWriter, e *Error) {
	body, err := envelope(e)
	if err != nil {
		// An extra member that cannot be encoded is a programming error; the
		// client still gets an envelope, without the member and without the
		// encoding error's text.
		e = Internal("internal error")
		body, _ = envelope(e)
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(e.status)
	// The status line is already sent; a failed write means the client went
	// away, and there is no one left to tell.
	_, _ = w.Write(body)
}

// envelope encodes e as the error envelope. It writes the members itself,
// rather than through a map, so that the standard members come first.
func envelope(e *Error) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"error":{"code":`)
	fmt.Fprintf(&b, "%d", e.status)
	member := func(name string, value any) error {
		v, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode error member %q: %w", name, err)
		}
		b.WriteString(",")
		k, _ := json.Marshal(name)
		b.Write(k)
		b.WriteString(":")
		b.Write(v)
		return nil
	}
	if err := member("message", e.message); err != nil {
		return nil, err
	}
	if err := member("error_type", e.errorType); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(e.extras)) {
		if err := member(name, e.extras[name]); err != nil {
			return nil, err
		}
	}
	b.WriteString("}}\n")
	return b.Bytes(), nil
}

// WriteJSON answers status with v encoded as JSON, the one way the server
// writes a success body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	// The status line is already sent; a failed write means the client went
	// away, and there is no one left to tell.
	_ = json.NewEncoder(w).Encode(v)
}
