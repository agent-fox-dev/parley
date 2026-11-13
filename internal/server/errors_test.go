package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// writeErrorTo answers e on a fresh recorder and returns what it recorded.
func writeErrorTo(e *Error) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	WriteError(rec, e)
	return rec
}

func TestWriteError_AnswersTheEnvelopeWithCodeEqualToTheStatus(t *testing.T) {
	t.Parallel()
	rec := writeErrorTo(Conflict("workspace is busy"))

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if got := rec.Header().Get("Content-Type"); got != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", got, jsonContentType)
	}
	members := errorMembers(t, rec.Body.Bytes())
	want := map[string]any{"code": float64(409), "message": "workspace is busy", "error_type": "conflict"}
	if len(members) != len(want) {
		t.Errorf("error members = %v, want exactly %v", members, want)
	}
	for name, value := range want {
		if members[name] != value {
			t.Errorf("error.%s = %v, want %v", name, members[name], value)
		}
	}
}

func TestCatalogue_EachConstructorAnswersItsTypeAndStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		errorType string
		status    int
		build     func(string) *Error
	}{
		{"invalid_request", 400, InvalidRequest},
		{"unauthenticated", 401, Unauthenticated},
		{"forbidden", 403, Forbidden},
		{"not_found", 404, NotFound},
		{"method_not_allowed", 405, MethodNotAllowed},
		{"conflict", 409, Conflict},
		{"gone", 410, Gone},
		{"payload_too_large", 413, PayloadTooLarge},
		{"unsupported_media_type", 415, UnsupportedMediaType},
		{"rate_limited", 429, RateLimited},
		{"internal", 500, Internal},
		{"upstream_error", 502, UpstreamError},
		{"unavailable", 503, Unavailable},
		{"insufficient_scope", 403, InsufficientScope},
		{"token_bound", 403, TokenBound},
		{"path_not_allowed", 403, PathNotAllowed},
		{"workspace_archived", 409, WorkspaceArchived},
	}
	for _, tc := range cases {
		t.Run(tc.errorType, func(t *testing.T) {
			t.Parallel()
			rec := writeErrorTo(tc.build("something went wrong"))
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			members := errorMembers(t, rec.Body.Bytes())
			if members["code"] != float64(tc.status) {
				t.Errorf("error.code = %v, want %d", members["code"], tc.status)
			}
			if members["error_type"] != tc.errorType {
				t.Errorf("error.error_type = %v, want %q", members["error_type"], tc.errorType)
			}
			if members["message"] != "something went wrong" {
				t.Errorf("error.message = %v, want %q", members["message"], "something went wrong")
			}
		})
	}
}

func TestNewError_AnswersTheCallersOwnTypeAndStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		errorType string
		status    int
	}{
		{"workspace_busy", 409},
		{"unknown_session", 400},
		{"cursor_expired", 410},
	}
	for _, tc := range cases {
		t.Run(tc.errorType, func(t *testing.T) {
			t.Parallel()
			handler := func() error { return NewError(tc.errorType, tc.status, "it failed") }

			var apiErr *Error
			if err := handler(); !errors.As(err, &apiErr) {
				t.Fatalf("handler returned %v, not an API error", err)
			}
			rec := writeErrorTo(apiErr)
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			members := errorMembers(t, rec.Body.Bytes())
			if members["code"] != float64(tc.status) {
				t.Errorf("error.code = %v, want %d", members["code"], tc.status)
			}
			if members["error_type"] != tc.errorType {
				t.Errorf("error.error_type = %v, want %q", members["error_type"], tc.errorType)
			}
		})
	}
}

func TestWriteError_WritesExtraMembersBesideTheStandardOnes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		err   *Error
		check func(t *testing.T, members map[string]any)
	}{
		{
			name: "named member",
			err:  NewError("cursor_expired", 410, "the cursor is too old").With("oldest_seq", 17),
			check: func(t *testing.T, members map[string]any) {
				if members["oldest_seq"] != float64(17) {
					t.Errorf("error.oldest_seq = %v, want 17", members["oldest_seq"])
				}
			},
		},
		{
			name: "details object",
			err:  NewError("ref_moved", 409, "the ref moved").WithDetails(map[string]string{"current_sha": "abc"}),
			check: func(t *testing.T, members map[string]any) {
				details, ok := members["details"].(map[string]any)
				if !ok {
					t.Fatalf("error.details = %v, want an object", members["details"])
				}
				if details["current_sha"] != "abc" {
					t.Errorf("error.details.current_sha = %v, want %q", details["current_sha"], "abc")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := writeErrorTo(tc.err)
			members := errorMembers(t, rec.Body.Bytes())
			if members["code"] != float64(tc.err.Status()) {
				t.Errorf("error.code = %v, want %d", members["code"], tc.err.Status())
			}
			if members["message"] != tc.err.Message() {
				t.Errorf("error.message = %v, want %q", members["message"], tc.err.Message())
			}
			if members["error_type"] != tc.err.Type() {
				t.Errorf("error.error_type = %v, want %q", members["error_type"], tc.err.Type())
			}
			tc.check(t, members)
		})
	}
}

func TestError_WithRefusesToReplaceAStandardMember(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"code", "message", "error_type"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("With(%q, …) did not panic", name)
				}
			}()
			Conflict("workspace is busy").With(name, "replaced")
		})
	}
}
