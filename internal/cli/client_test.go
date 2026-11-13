package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestClient_SendsTheRequestToTheAPIPathOfTheHub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		suffix        string // appended to the hub's URL
		method, path  string
		query         url.Values
		body          any
		wantPathStart string
	}{
		{name: "GET", method: http.MethodGet, path: "/things/x"},
		{name: "trailing slash and a query", suffix: "/", method: http.MethodGet, path: "/things/x", query: url.Values{"limit": {"5"}}},
		{name: "two trailing slashes", suffix: "//", method: http.MethodDelete, path: "/things/x"},
		{name: "a path in the hub URL", suffix: "/prefix", method: http.MethodGet, path: "/things/x", wantPathStart: "/prefix"},
		{name: "POST with a body", method: http.MethodPost, path: "/things", body: map[string]any{"name": "x", "n": 1}},
		{name: "PATCH with an empty object", method: http.MethodPatch, path: "/things/x", body: map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			_, err := newClient(hub.URL+tt.suffix, "k").request(context.Background(), tt.method, tt.path, tt.query, tt.body)

			if err != nil {
				t.Fatalf("request() = %v, want no error", err)
			}
			got := hub.recorded()
			if len(got) != 1 {
				t.Fatalf("the hub got %d requests, want 1", len(got))
			}
			r := got[0]
			if want := tt.wantPathStart + "/api/v1" + tt.path; r.method != tt.method || r.rawPath != want {
				t.Errorf("request = %s %s, want %s %s", r.method, r.rawPath, tt.method, want)
			}
			if want := tt.query.Encode(); r.query != want {
				t.Errorf("query = %q, want %q", r.query, want)
			}
			if auth := r.header.Get("Authorization"); auth != "Bearer k" {
				t.Errorf("Authorization = %q, want %q", auth, "Bearer k")
			}
			if tt.body == nil {
				if ct, ok := r.header["Content-Type"]; ok {
					t.Errorf("Content-Type = %q, want none without a body", ct)
				}
				if r.contentLength != 0 || r.body != "" {
					t.Errorf("content length %d, body %q, want no body", r.contentLength, r.body)
				}
				return
			}
			if ct := r.header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var sent, want any
			if err := json.Unmarshal([]byte(r.body), &sent); err != nil {
				t.Fatalf("body %q is not JSON: %v", r.body, err)
			}
			encoded, _ := json.Marshal(tt.body)
			json.Unmarshal(encoded, &want)
			if !reflect.DeepEqual(sent, want) {
				t.Errorf("body = %s, want %s", strings.TrimSpace(r.body), encoded)
			}
		})
	}
}
