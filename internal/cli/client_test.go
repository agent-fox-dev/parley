package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestClient_EveryArgumentIsOnePathSegment(t *testing.T) {
	hub := newRecordingHub(t, 200, `{}`)
	alphabet := []string{"a", "Z", "7", "/", "?", "#", "%", " ", "+", ".", "..", "&", "=", "ü", "ö", "é"}
	values := []string{"a/b", "feature/x y", "100%", "..", "a?b#c", "ü/ö"}
	rng := rand.New(rand.NewSource(7))
	for range 200 {
		var b strings.Builder
		for range 1 + rng.Intn(20) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		values = append(values, b.String())
	}
	for _, v := range values {
		cliEnv(t)
		before := len(hub.all())
		r := runProbe(t, []*cobra.Command{defaultProbe()}, "probe", "get", v, "--endpoint-url", hub.URL, "--api-key", "k")
		if r.code != 0 {
			t.Fatalf("%q: exit %d %s", v, r.code, r.stderr)
		}
		rec := hub.all()[before]
		seg, ok := strings.CutPrefix(rec.raw, "/api/v1/things/")
		if !ok || strings.ContainsAny(seg, "/?# ") || rec.query != "" {
			t.Errorf("%q: raw path %q query %q", v, rec.raw, rec.query)
			continue
		}
		if back, err := url.PathUnescape(seg); err != nil || back != v {
			t.Errorf("%q: segment %q decodes to %q", v, seg, back)
		}
	}
}

func TestClient_ARequestThatTakesTooLongIsAClientError(t *testing.T) {
	if requestTimeout != 30*time.Second {
		t.Errorf("default request timeout = %v", requestTimeout)
	}
	var arrived atomic.Int32
	slow := func(d time.Duration, half bool) *httptest.Server {
		var once sync.Once
		release := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			arrived.Add(1)
			if half {
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(200)
				io.WriteString(w, `{"a":`)
				w.(http.Flusher).Flush()
			}
			select {
			case <-time.After(d):
			case <-release:
			}
			if !half {
				io.WriteString(w, `{}`)
			}
		}))
		t.Cleanup(func() { once.Do(func() { close(release) }); s.Close() })
		return s
	}
	stated := func(d time.Duration) *cobra.Command {
		return defaultProbeWith(func(s *Spec) { s.Timeout = d })
	}
	tests := []struct {
		name    string
		hub     *httptest.Server
		cmd     *cobra.Command
		short   time.Duration
		timeout time.Duration
		ok      bool
	}{
		{"default shortened", slow(2*time.Second, false), defaultProbe(), 100 * time.Millisecond, 100 * time.Millisecond, false},
		{"stated, in time", slow(200*time.Millisecond, false), stated(300 * time.Millisecond), 0, 300 * time.Millisecond, true},
		{"stated, too slow", slow(2*time.Second, false), stated(300 * time.Millisecond), 0, 300 * time.Millisecond, false},
		{"half a body then a stall", slow(2*time.Second, true), defaultProbe(), 100 * time.Millisecond, 100 * time.Millisecond, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cliEnv(t)
			if tt.short > 0 {
				old := requestTimeout
				requestTimeout = tt.short
				t.Cleanup(func() { requestTimeout = old })
			}
			arrived.Store(0)
			start := time.Now()
			r := runProbe(t, []*cobra.Command{tt.cmd}, "probe", "get", "x", "--endpoint-url", tt.hub.URL, "--api-key", "k")
			if n := arrived.Load(); n != 1 {
				t.Errorf("the hub received %d requests", n)
			}
			if tt.ok {
				if r.code != 0 {
					t.Errorf("exit %d %s", r.code, r.stderr)
				}
				return
			}
			if r.code != 2 || r.stdout != "" || r.envelope(t)["error_type"] != "client_error" {
				t.Errorf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
			}
			if d := time.Since(start); d >= tt.timeout+time.Second {
				t.Errorf("returned after %v", d)
			}
		})
	}
}

func defaultProbeWith(extra func(*Spec)) *cobra.Command { return defaultProbe(extra) }

func TestClient_AConnectionThatFailsNamesTheHub(t *testing.T) {
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedURL := "http://" + closed.Addr().String()
	closed.Close()
	tlsHub := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsHub.Close()
	for _, hub := range []string{closedURL, "http://no-such-host.invalid", tlsHub.URL} {
		cliEnv(t)
		r := runProbe(t, []*cobra.Command{defaultProbe()}, "probe", "get", "x", "--endpoint-url", hub, "--api-key", "k")
		u, _ := url.Parse(hub)
		e := r.envelope(t)
		if r.code != 2 || r.stdout != "" || e["code"] != float64(0) || e["error_type"] != "client_error" || !strings.Contains(e["message"].(string), u.Host) {
			t.Errorf("%s: exit %d %v", hub, r.code, e)
		}
	}
}

func TestClient_AnAnswerThatCannotBeDecodedNamesTheStatus(t *testing.T) {
	answers := []struct {
		status int
		body   string
	}{
		{200, "<html>"}, {200, `{"a":`}, {502, "<html>Bad Gateway</html>"}, {404, "not found"},
		{500, "{}"}, {500, `{"error":"boom"}`}, {403, `{"error":{"message":"x"}}`},
	}
	for _, a := range answers {
		cliEnv(t)
		hub := newRecordingHub(t, a.status, a.body)
		r := runProbe(t, []*cobra.Command{defaultProbe()}, "probe", "get", "x", "--endpoint-url", hub.URL, "--api-key", "k")
		e := r.envelope(t)
		if r.code != 2 || r.stdout != "" || e["error_type"] != "client_error" || !strings.Contains(e["message"].(string), strconv.Itoa(a.status)) {
			t.Errorf("%d %q: exit %d %v", a.status, a.body, r.code, e)
		}
	}
}

func TestClient_EveryRequestIsSentOnce(t *testing.T) {
	envelope := func(code int, typ string) string {
		return fmt.Sprintf(`{"error":{"code":%d,"message":"m","error_type":%q}}`, code, typ)
	}
	cases := []struct {
		status int
		body   string
		hangUp bool
	}{
		{429, envelope(429, "rate_limited"), false},
		{500, envelope(500, "internal"), false},
		{502, envelope(502, "upstream_error"), false},
		{503, envelope(503, "unavailable"), false},
		{0, "", true},
	}
	for _, c := range cases {
		cliEnv(t)
		var count atomic.Int32
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			if c.hangUp {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			if c.status == 429 {
				w.Header().Set("Retry-After", "1")
			}
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		}))
		r := runProbe(t, []*cobra.Command{defaultProbe()}, "probe", "get", "x", "--endpoint-url", hub.URL, "--api-key", "k")
		if c.status == 429 {
			time.Sleep(1500 * time.Millisecond)
			if r.code != 1 || r.envelope(t)["error_type"] != "rate_limited" {
				t.Errorf("429: exit %d %s", r.code, r.stderr)
			}
		}
		if n := count.Load(); n != 1 {
			t.Errorf("%d: %d requests", c.status, n)
		}
		hub.Close()
	}
	// A connection that served one request and breaks under the next: the
	// next request is not sent again on a new connection.
	var count atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 2 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer hub.Close()
	c, err := newClient(hub.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(context.Background(), http.MethodGet, "/a", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(context.Background(), http.MethodGet, "/a", nil, nil); err == nil {
		t.Error("a broken connection answered")
	}
	if n := count.Load(); n != 2 {
		t.Errorf("two sends arrived as %d requests", n)
	}
}
