package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
)

// Error is an error of the REST API. Handlers and checks return it, and the
// server answers it as the error envelope
//
//	{"error":{"code":409,"message":"workspace is busy","error_type":"conflict"}}
//
// in which code is the HTTP status. Build one with a catalogue constructor
// (Conflict, NotFound, …) or, for a type another package owns, NewError.
type Error struct {
	// Type is the machine-readable error_type.
	Type string
	// Status is the HTTP status, written as code too.
	Status int
	// Message is for people.
	Message string

	members map[string]any
	details any
}

// Error returns the message, so an *Error is a Go error.
func (e *Error) Error() string { return e.Type + ": " + e.Message }

// NewError builds an error of a specific type outside the catalogue, with the
// status the caller gives (for example NewError("workspace_busy", 409, …)).
func NewError(errorType string, status int, message string) *Error {
	return &Error{Type: errorType, Status: status, Message: message}
}

// reservedMembers are the standard members an extra one cannot replace.
var reservedMembers = map[string]bool{"code": true, "message": true, "error_type": true, "details": true}

// With adds a named member written inside error beside the standard three
// (for example oldest_seq). A member named code, message, error_type or
// details is ignored, so that it cannot replace a standard one.
func (e *Error) With(name string, value any) *Error {
	if reservedMembers[name] {
		return e
	}
	if e.members == nil {
		e.members = map[string]any{}
	}
	e.members[name] = value
	return e
}

// WithDetails adds a details object, written as error.details.
func (e *Error) WithDetails(details any) *Error {
	e.details = details
	return e
}

// The catalogue: one constructor per type, each fixing its status.

// InvalidRequest is 400 invalid_request.
func InvalidRequest(message string) *Error { return NewError("invalid_request", 400, message) }

// Unauthenticated is 401 unauthenticated.
func Unauthenticated(message string) *Error { return NewError("unauthenticated", 401, message) }

// Forbidden is 403 forbidden.
func Forbidden(message string) *Error { return NewError("forbidden", 403, message) }

// NotFound is 404 not_found.
func NotFound(message string) *Error { return NewError("not_found", 404, message) }

// MethodNotAllowed is 405 method_not_allowed.
func MethodNotAllowed(message string) *Error { return NewError("method_not_allowed", 405, message) }

// Conflict is 409 conflict.
func Conflict(message string) *Error { return NewError("conflict", 409, message) }

// Gone is 410 gone.
func Gone(message string) *Error { return NewError("gone", 410, message) }

// PayloadTooLarge is 413 payload_too_large.
func PayloadTooLarge(message string) *Error { return NewError("payload_too_large", 413, message) }

// UnsupportedMediaType is 415 unsupported_media_type.
func UnsupportedMediaType(message string) *Error {
	return NewError("unsupported_media_type", 415, message)
}

// RateLimited is 429 rate_limited.
func RateLimited(message string) *Error { return NewError("rate_limited", 429, message) }

// Internal is 500 internal.
func Internal(message string) *Error { return NewError("internal", 500, message) }

// UpstreamError is 502 upstream_error.
func UpstreamError(message string) *Error { return NewError("upstream_error", 502, message) }

// Unavailable is 503 unavailable.
func Unavailable(message string) *Error { return NewError("unavailable", 503, message) }

// InsufficientScope is 403 insufficient_scope.
func InsufficientScope(message string) *Error { return NewError("insufficient_scope", 403, message) }

// TokenBound is 403 token_bound.
func TokenBound(message string) *Error { return NewError("token_bound", 403, message) }

// PathNotAllowed is 403 path_not_allowed.
func PathNotAllowed(message string) *Error { return NewError("path_not_allowed", 403, message) }

// WorkspaceArchived is 409 workspace_archived.
func WorkspaceArchived(message string) *Error { return NewError("workspace_archived", 409, message) }

// MarshalJSON writes the envelope: code, message and error_type first, then
// the named members in name order, then details.
func (e *Error) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"error":{`)
	write := func(name string, v any, first bool) error {
		if !first {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(name)
		b.Write(k)
		b.WriteByte(':')
		val, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b.Write(val)
		return nil
	}
	if err := write("code", e.Status, true); err != nil {
		return nil, err
	}
	if err := write("message", e.Message, false); err != nil {
		return nil, err
	}
	if err := write("error_type", e.Type, false); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(e.members))
	for name := range e.members {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := write(name, e.members[name], false); err != nil {
			return nil, err
		}
	}
	if e.details != nil {
		if err := write("details", e.details, false); err != nil {
			return nil, err
		}
	}
	b.WriteString("}}")
	return b.Bytes(), nil
}

// WriteError answers an API error with its status and the error envelope.
func WriteError(w http.ResponseWriter, e *Error) {
	WriteJSON(w, e.Status, e)
}

// WriteJSON answers status with v as the JSON body and the content type
// application/json; charset=utf-8. Every JSON body of the server is written
// through it.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body, _ = json.Marshal(Internal("internal error"))
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}
