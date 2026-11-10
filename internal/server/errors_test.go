package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type envelope struct {
	Error map[string]any `json:"error"`
}

func writeAndDecode(t *testing.T, e *Error) (*httptest.ResponseRecorder, map[string]any, envelope) {
	t.Helper()
	rec := httptest.NewRecorder()
	WriteError(rec, e)
	var top map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return rec, top, env
}

func TestWriteError_WritesTheEnvelopeWithCodeEqualToStatus(t *testing.T) {
	rec, top, env := writeAndDecode(t, Conflict("workspace is busy"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if len(top) != 1 || top["error"] == nil {
		t.Fatalf("top-level members = %v, want error alone", top)
	}
	want := map[string]any{"code": float64(409), "message": "workspace is busy", "error_type": "conflict"}
	if len(env.Error) != len(want) {
		t.Errorf("error = %v, want %v", env.Error, want)
	}
	for k, v := range want {
		if env.Error[k] != v {
			t.Errorf("error.%s = %v, want %v", k, env.Error[k], v)
		}
	}
}

func TestCatalogue_EachTypeAnswersItsStatus(t *testing.T) {
	tests := []struct {
		build  func(string) *Error
		typ    string
		status int
	}{
		{InvalidRequest, "invalid_request", 400},
		{Unauthenticated, "unauthenticated", 401},
		{Forbidden, "forbidden", 403},
		{NotFound, "not_found", 404},
		{MethodNotAllowed, "method_not_allowed", 405},
		{Conflict, "conflict", 409},
		{Gone, "gone", 410},
		{PayloadTooLarge, "payload_too_large", 413},
		{UnsupportedMediaType, "unsupported_media_type", 415},
		{RateLimited, "rate_limited", 429},
		{Internal, "internal", 500},
		{UpstreamError, "upstream_error", 502},
		{Unavailable, "unavailable", 503},
		{InsufficientScope, "insufficient_scope", 403},
		{TokenBound, "token_bound", 403},
		{PathNotAllowed, "path_not_allowed", 403},
		{WorkspaceArchived, "workspace_archived", 409},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			rec, _, env := writeAndDecode(t, tt.build("some message"))
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if env.Error["code"] != float64(tt.status) {
				t.Errorf("error.code = %v, want %d", env.Error["code"], tt.status)
			}
			if env.Error["error_type"] != tt.typ {
				t.Errorf("error.error_type = %v, want %s", env.Error["error_type"], tt.typ)
			}
		})
	}
}

func TestNewError_RaisesASpecificTypeWithItsStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"workspace_busy", 409},
		{"unknown_session", 400},
		{"cursor_expired", 410},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, _, env := writeAndDecode(t, NewError(tt.name, tt.status, "specific failure"))
			if rec.Code != tt.status || env.Error["code"] != float64(tt.status) || env.Error["error_type"] != tt.name {
				t.Errorf("got status %d and error %v, want %d %s", rec.Code, env.Error, tt.status, tt.name)
			}
		})
	}
}

func TestError_ExtraMembersAreWrittenInsideError(t *testing.T) {
	_, _, env := writeAndDecode(t, NewError("cursor_expired", 410, "cursor expired").With("oldest_seq", 17))
	if env.Error["oldest_seq"] != float64(17) {
		t.Errorf("error.oldest_seq = %v, want 17", env.Error["oldest_seq"])
	}
	for _, k := range []string{"code", "message", "error_type"} {
		if _, ok := env.Error[k]; !ok {
			t.Errorf("error.%s is missing", k)
		}
	}

	_, _, env = writeAndDecode(t, NewError("ref_moved", 409, "ref moved").WithDetails(map[string]any{"current_sha": "abc"}))
	details, _ := env.Error["details"].(map[string]any)
	if details["current_sha"] != "abc" {
		t.Errorf("error.details = %v, want current_sha abc", env.Error["details"])
	}

	e := Conflict("busy").With("code", 1).With("message", "replaced").With("error_type", "replaced")
	rec, _, env := writeAndDecode(t, e)
	if rec.Code != 409 || env.Error["code"] != float64(409) || env.Error["message"] != "busy" || env.Error["error_type"] != "conflict" {
		t.Errorf("an extra member replaced a standard one: %v", env.Error)
	}
}

func TestWriteJSON_WritesStatusAndJSONBody(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]string{"id": "1"})
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Body.String() != `{"id":"1"}` {
		t.Errorf("body = %q", rec.Body.String())
	}
}
