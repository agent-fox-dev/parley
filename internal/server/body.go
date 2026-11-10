package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// DefaultBodyLimit is the server's request body limit, 1 MiB, unless the
// options set another.
const DefaultBodyLimit int64 = 1 << 20

// bodyLimit is the limit that applies to rt: its own, else the server's.
func (s *Server) bodyLimit(rt *Route) int64 {
	if rt.BodyLimit > 0 {
		return rt.BodyLimit
	}
	if s.opts.BodyLimit > 0 {
		return s.opts.BodyLimit
	}
	return DefaultBodyLimit
}

// checkBody is the pipeline's body stage. It reads the whole body, up to one
// byte past the limit that applies, so that a body sent without a declared
// length is measured too; it refuses an over-limit body with 413 and then,
// on POST, PUT and PATCH with a non-empty body, a content type other than
// application/json (or the route's own) with 415. The buffered body replaces
// the request's for the later checks and the handler.
func (s *Server) checkBody(r *http.Request, rt *Route) (*http.Request, error) {
	limit := s.bodyLimit(rt)
	if r.ContentLength > limit {
		return r, PayloadTooLarge("request body is larger than the limit")
	}
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			return r, InvalidRequest("cannot read the request body")
		}
		if int64(len(b)) > limit {
			return r, PayloadTooLarge("request body is larger than the limit")
		}
		body = b
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		if len(body) > 0 && !acceptsMediaType(r.Header.Get("Content-Type"), rt.ContentType) {
			return r, UnsupportedMediaType("unsupported content type")
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), keyBody, body))
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return r, nil
}

// acceptsMediaType compares a Content-Type header as a media type, ignoring
// letter case and parameters, with the route's type or application/json.
func acceptsMediaType(header, routeType string) bool {
	want := "application/json"
	if routeType != "" {
		want = routeType
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return strings.EqualFold(mt, want)
}

// Body returns the request body the body stage buffered.
func Body(r *http.Request) []byte {
	b, _ := r.Context().Value(keyBody).([]byte)
	return b
}

// DecodeJSON decodes the request body, which must be one JSON object, into
// v; unknown members are ignored. An empty body, invalid JSON, a value that
// is not an object, or bytes other than white space after the object are
// 400 invalid_request, and v is then not to be used.
func DecodeJSON(r *http.Request, v any) error {
	body, ok := r.Context().Value(keyBody).([]byte)
	if !ok {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return InvalidRequest("cannot read the request body")
		}
		body = b
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return InvalidRequest("request body must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(v); err != nil {
		return InvalidRequest("request body is not valid JSON: " + err.Error())
	}
	if rest := bytes.TrimSpace(trimmed[dec.InputOffset():]); len(rest) > 0 {
		return InvalidRequest("request body has data after the JSON object")
	}
	return nil
}
