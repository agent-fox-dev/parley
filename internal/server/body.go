package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// defaultMaxBodySize is the body limit when neither the route nor the
// server's options state one.
const defaultMaxBodySize = 1 << 20

type bodyKey struct{}

// bodyLimit is the limit that applies to a request to rt: the route's own,
// else the server's.
func (s *Server) bodyLimit(rt Route) int64 {
	if rt.MaxBodySize > 0 {
		return rt.MaxBodySize
	}
	if s.opts.MaxBodySize > 0 {
		return s.opts.MaxBodySize
	}
	return defaultMaxBodySize
}

// readBody is the body stage. It reads the whole body, up to one byte past
// the limit, so that a body sent without a declared length is refused before
// any later check runs too, then checks the content type. The request it
// returns carries the buffered bytes as its body and in its context, where
// later stages read them without consuming the body.
func (s *Server) readBody(r *http.Request) (*http.Request, error) {
	m, _ := MatchedRoute(r)
	limit := s.bodyLimit(m.Route)
	if r.ContentLength > limit {
		return nil, PayloadTooLarge(fmt.Sprintf("request body exceeds %d bytes", limit))
	}

	var buf []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		buf, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			return nil, InvalidRequest("request body could not be read")
		}
		if int64(len(buf)) > limit {
			return nil, PayloadTooLarge(fmt.Sprintf("request body exceeds %d bytes", limit))
		}
	}

	if len(buf) > 0 && typedMethod(r.Method) {
		if err := checkContentType(r.Header.Get("Content-Type"), m.Route.ContentType); err != nil {
			return nil, err
		}
	}

	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, buf))
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	return r, nil
}

// bufferedBody returns the body bytes the body stage read, and false for a
// request that did not pass it.
func bufferedBody(r *http.Request) ([]byte, bool) {
	b, ok := r.Context().Value(bodyKey{}).([]byte)
	return b, ok
}

// typedMethod reports whether a body sent with method must state its media
// type.
func typedMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}

// checkContentType compares the media type of header with the one the route
// states, application/json when it states none, ignoring letter case and
// parameters such as charset.
func checkContentType(header, routeType string) error {
	want := "application/json"
	if routeType != "" {
		want = routeType
		if mt, _, err := mime.ParseMediaType(routeType); err == nil {
			want = mt
		}
	}
	refused := UnsupportedMediaType(fmt.Sprintf("request body must be %s", want))
	if header == "" {
		return refused
	}
	got, _, err := mime.ParseMediaType(header)
	if err != nil || !strings.EqualFold(got, want) {
		return refused
	}
	return nil
}

// DecodeJSON decodes the body of r, which must be one JSON object, into v,
// a pointer. Members v does not know are ignored. An empty body, a body that
// is not valid JSON or not an object, or one with anything but white space
// after the object is answered 400 invalid_request; the handler returns that
// error as it is and leaves v unused.
func DecodeJSON(r *http.Request, v any) error {
	body, ok := bufferedBody(r)
	if !ok {
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			return InvalidRequest("request body could not be read")
		}
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return InvalidRequest("request body is empty; want a JSON object")
		}
		return InvalidRequest("request body is not valid JSON")
	}
	if raw[0] != '{' {
		return InvalidRequest("request body must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return InvalidRequest("request body has data after the JSON object")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		var notPointer *json.InvalidUnmarshalError
		if errors.As(err, &notPointer) {
			// The handler's mistake, not the client's.
			return fmt.Errorf("decode request body: %w", err)
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return InvalidRequest(fmt.Sprintf("request body member %q has the wrong type", typeErr.Field))
		}
		// A member's own decoding refused its value.
		return InvalidRequest("request body has a member with a value that is not valid")
	}
	return nil
}
