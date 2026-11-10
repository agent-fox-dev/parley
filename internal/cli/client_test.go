package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

type hubRecord struct {
	method string
	raw    string // raw path
	query  string
	header http.Header
	body   []byte
}

// recordingHub answers 200 with body on any path and records every request.
type recordingHub struct {
	*httptest.Server
	mu      sync.Mutex
	records []hubRecord
	status  int
	body    string
}

func newRecordingHub(t *testing.T, status int, body string) *recordingHub {
	t.Helper()
	h := &recordingHub{status: status, body: body}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.records = append(h.records, hubRecord{method: r.Method, raw: r.URL.EscapedPath(), query: r.URL.RawQuery, header: r.Header.Clone(), body: b})
		status, body := h.status, h.body
		h.mu.Unlock()
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *recordingHub) all() []hubRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hubRecord(nil), h.records...)
}

// probeGet is 'probe get <name>': GET /things/:name with auth user; its run
// step sends the request and writes the body it got to stdout.
func probeGet() *cobra.Command {
	return Group("probe", "test commands", Declare(Spec{
		Use: "get", Description: "gets a thing", Method: "GET", Path: "/things/:name",
		Args: []Arg{{Name: "name", Required: true}}, Auth: AuthUser,
		Run: func(c *Call) error {
			resp, err := c.Client.Send(c.Ctx, "GET", "/things/"+c.Args["name"], nil, nil)
			if err != nil {
				return err
			}
			c.Stdout.Write(resp.Body)
			return nil
		},
	}))
}

func TestClient_SendsTheRequestUnderAPIV1WithTheCredential(t *testing.T) {
	hub := newRecordingHub(t, 200, `{}`)
	tests := []struct {
		base   string
		method string
		path   string
		query  url.Values
		body   any
		prefix string
	}{
		{hub.URL, "GET", "/things/x", nil, nil, ""},
		{hub.URL + "/", "GET", "/things/x", url.Values{"limit": {"5"}}, nil, ""},
		{hub.URL + "//", "DELETE", "/things/x", nil, nil, ""},
		{hub.URL + "/prefix", "GET", "/things/x", nil, nil, "/prefix"},
		{hub.URL, "POST", "/things", nil, map[string]any{"name": "x", "n": 1}, ""},
		{hub.URL, "PATCH", "/things/x", nil, map[string]any{}, ""},
	}
	for _, tt := range tests {
		before := len(hub.all())
		c, err := newClient(tt.base, "k")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Send(context.Background(), tt.method, tt.path, tt.query, tt.body); err != nil {
			t.Fatalf("%+v: %v", tt, err)
		}
		r := hub.all()[before]
		if r.method != tt.method || r.raw != tt.prefix+"/api/v1"+tt.path || r.query != tt.query.Encode() || r.header.Get("Authorization") != "Bearer k" {
			t.Errorf("%+v: got %s %s ?%s %q", tt, r.method, r.raw, r.query, r.header.Get("Authorization"))
		}
		if tt.body != nil {
			want, _ := json.Marshal(tt.body)
			var got, w any
			json.Unmarshal(r.body, &got)
			json.Unmarshal(want, &w)
			if r.header.Get("Content-Type") != "application/json" || !jsonEqual(got, w) {
				t.Errorf("%+v: body %s content type %q", tt, r.body, r.header.Get("Content-Type"))
			}
		} else if r.header.Get("Content-Type") != "" || len(r.body) != 0 {
			t.Errorf("%+v: a bodiless request carries %q %q", tt, r.header.Get("Content-Type"), r.body)
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

