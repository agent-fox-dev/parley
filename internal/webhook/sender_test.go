package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/store"
)

// The delivery an attempt of these tests belongs to, and the secret it is
// signed with, as a create shows it.
const (
	testDelivery = "3f0c8f5e-2b1a-4c47-9d6e-8a0b1c2d3e4f"
	testSecret   = "whsec_00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

// senderClock is a clock whose time moves only when the test moves it.
type senderClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSenderClock() *senderClock {
	return &senderClock{now: time.Date(2026, 11, 19, 8, 30, 15, 0, time.UTC)}
}

func (c *senderClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *senderClock) NewTimer(d time.Duration) event.Timer {
	return event.SystemClock(nil).NewTimer(d)
}

func (c *senderClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// hosts answers host names from a list the test changes; a name it does not
// list does not resolve.
type hosts struct {
	mu sync.Mutex
	m  map[string][]netip.Addr
}

func (h *hosts) set(name string, addrs ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string][]netip.Addr{}
	}
	h.m[name] = nil
	for _, a := range addrs {
		h.m[name] = append(h.m[name], netip.MustParseAddr(a))
	}
}

func (h *hosts) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if addrs, ok := h.m[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// dials is the system's dialer, recording every address it is handed.
type dials struct {
	mu    sync.Mutex
	addrs []string
	d     net.Dialer
}

func (r *dials) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, address)
	r.mu.Unlock()
	return r.d.DialContext(ctx, network, address)
}

func (r *dials) handed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addrs...)
}

// bench is the sender of a component bound to settings, a test clock, the
// test's resolver and a recording dialer, trusting roots (nil for the
// system's).
type bench struct {
	component *Component
	clock     *senderClock
	hosts     *hosts
	dials     *dials
}

func newBench(settings Settings, roots *x509.CertPool) *bench {
	b := &bench{component: &Component{}, clock: newSenderClock(), hosts: &hosts{}, dials: &dials{}}
	b.component.Bind(Deps{Settings: settings, Clock: b.clock, Resolver: b.hosts, Dialer: b.dials, RootCAs: roots})
	return b
}

// send makes one attempt of body to target and returns its outcome and how
// long the call took.
func (b *bench) send(target string, body []byte) (outcome, time.Duration) {
	start := time.Now()
	got := b.component.sender.send(context.Background(), attempt{
		url: target, body: body, eventType: "parley.change.open", seq: 42, delivery: testDelivery, secret: testSecret,
	})
	return got, time.Since(start)
}

// received is a request as a receiver got it.
type received struct {
	method string
	uri    string
	header http.Header
	body   []byte
}

// receiver is a webhook receiver that records every request and answers it
// with answer. stop is closed when the test ends, before the server closes,
// so that a handler that stalls lets the test end at once.
type receiver struct {
	*httptest.Server
	mu  sync.Mutex
	got []received
}

func newReceiver(t *testing.T, secure bool, answer func(w http.ResponseWriter, r *http.Request, stop <-chan struct{})) *receiver {
	t.Helper()
	rcv := &receiver{}
	stop := make(chan struct{})
	rcv.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rcv.mu.Lock()
		rcv.got = append(rcv.got, received{method: r.Method, uri: r.RequestURI, header: r.Header.Clone(), body: body})
		rcv.mu.Unlock()
		answer(w, r, stop)
	}))
	if secure {
		rcv.StartTLS()
	} else {
		rcv.Start()
	}
	t.Cleanup(rcv.Close)
	t.Cleanup(func() { close(stop) })
	return rcv
}

func (rcv *receiver) requests() []received {
	rcv.mu.Lock()
	defer rcv.mu.Unlock()
	return append([]received(nil), rcv.got...)
}

// roots is a pool that holds the receiver's certificate alone.
func (rcv *receiver) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(rcv.Certificate())
	return pool
}

// answerStatus answers status with a short body.
func answerStatus(status int) func(http.ResponseWriter, *http.Request, <-chan struct{}) {
	return func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		w.WriteHeader(status)
		io.WriteString(w, "ok")
	}
}

// stall waits for d, for the request to go away or for the test to end.
func stall(r *http.Request, stop <-chan struct{}, d time.Duration) {
	select {
	case <-time.After(d):
	case <-r.Context().Done():
	case <-stop:
	}
}

// tcpListener is a TCP listener on 127.0.0.1 that counts the connections it
// accepts and holds each for hold (closing it at once when 0) or until the
// test ends.
type tcpListener struct {
	net.Listener
	accepted atomic.Int32
}

func newTCPListener(t *testing.T, hold time.Duration) *tcpListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &tcpListener{Listener: ln}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() {
		close(stop)
		ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			l.accepted.Add(1)
			wg.Go(func() {
				defer conn.Close()
				select {
				case <-time.After(hold):
				case <-stop:
				}
			})
		}
	})
	return l
}

func (l *tcpListener) port() string {
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// signature is what a receiver computes to verify a delivery: sha256= and
// the hexadecimal HMAC-SHA256, keyed with the secret string, of the
// timestamp, a dot and the body.
func signature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSend_PostsTheEventRecordWithTheDeliveryHeaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{event.SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	rcv := newReceiver(t, true, answerStatus(http.StatusNoContent))
	b := newBench(Settings{Timeout: time.Second, AllowPrivate: true}, rcv.roots())
	log, err := event.New(event.Deps{Store: st, Clock: b.clock})
	if err != nil {
		t.Fatal(err)
	}
	slug := "demo"
	rec, err := log.EmitNow(ctx, event.Input{
		Type: "parley.change.open", Workspace: &slug, ActorType: event.ActorSystem,
		ResourceType: "change", ResourceID: "c1", Metadata: map[string]any{"a": 1},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	read, _, err := log.ReadAfter(ctx, rec.Seq-1, 1)
	if err != nil || len(read) != 1 {
		t.Fatalf("read the log after %d = %v, %v; want the event", rec.Seq-1, read, err)
	}
	body, err := read[0].MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	got := b.component.sender.send(ctx, attempt{
		url: rcv.URL + "/h?t=1", body: body, eventType: rec.Type, seq: rec.Seq, delivery: testDelivery, secret: testSecret,
	})
	if !got.success || got.responseCode != http.StatusNoContent {
		t.Fatalf("outcome = %+v, want success with 204", got)
	}
	reqs := rcv.requests()
	if len(reqs) != 1 {
		t.Fatalf("the receiver got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost || r.uri != "/h?t=1" {
		t.Errorf("request = %s %s, want POST /h?t=1", r.method, r.uri)
	}
	if !bytes.Equal(r.body, body) {
		t.Errorf("body = %s\nwant   %s (the log's record)", r.body, body)
	}
	stamp := strconv.FormatInt(b.clock.Now().Unix(), 10)
	for name, want := range map[string]string{
		"Content-Type":     "application/json",
		"Parley-Event":     rec.Type,
		"Parley-Seq":       strconv.FormatInt(rec.Seq, 10),
		"Parley-Delivery":  testDelivery,
		"Parley-Timestamp": stamp,
		"Parley-Signature": signature(testSecret, stamp, body),
		"User-Agent":       "parley-webhook/" + buildinfo.Get().Version,
	} {
		if got := r.header.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	allowed := map[string]bool{
		"Content-Type": true, "Parley-Event": true, "Parley-Seq": true, "Parley-Delivery": true,
		"Parley-Timestamp": true, "Parley-Signature": true, "User-Agent": true,
		"Host": true, "Content-Length": true, "Accept-Encoding": true,
	}
	for name := range r.header {
		if !allowed[name] {
			t.Errorf("the request carries the header %s = %q, which is not one of the named", name, r.header.Values(name))
		}
	}
}

func TestSend_SignsTheTimestampAndBodyWithTheSecretAtEveryAttempt(t *testing.T) {
	t.Parallel()
	rcv := newReceiver(t, false, answerStatus(http.StatusOK))
	b := newBench(Settings{Timeout: time.Second, AllowInsecure: true}, nil)
	shape := regexp.MustCompile(`^sha256=[0-9a-f]{64}$`)
	slug := "demo"
	at := b.clock.Now()
	for _, tc := range []struct {
		name string
		rec  event.Record
	}{
		{"an event with empty metadata", event.Record{
			Seq: 7, ID: "9b1c0f3e-5d2a-4e8b-9c7d-6a5b4c3d2e1f", Type: "parley.change.open", Time: at, Workspace: &slug,
			ActorType: event.ActorSystem, ResourceType: "change", ResourceID: "c1", Action: "open",
		}},
		{"an event whose metadata holds non-ASCII text", event.Record{
			Seq: 8, ID: "1e2d3c4b-5a69-4788-9a0b-1c2d3e4f5a6b", Type: "parley.change.comment", Time: at, Workspace: &slug,
			ActorType: event.ActorSystem, ResourceType: "change", ResourceID: "c1", Action: "comment",
			Metadata: json.RawMessage(`{"text":"Grüße, naïve café — 日本語 ✓"}`),
		}},
		{"a ping", event.Record{
			Seq: 0, ID: "5f4e3d2c-1b0a-4987-8a6b-5c4d3e2f1a0b", Type: "parley.webhook.ping", Time: at, Workspace: &slug,
			ActorType: event.ActorSystem, ResourceType: "event_webhook", ResourceID: testDelivery, Action: "ping",
			Metadata: json.RawMessage(`{}`),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.rec.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			before := len(rcv.requests())
			b.send(rcv.URL+"/h", body)
			b.clock.advance(time.Hour)
			b.send(rcv.URL+"/h", body)
			reqs := rcv.requests()
			if len(reqs) != before+2 {
				t.Fatalf("the receiver got %d requests, want 2", len(reqs)-before)
			}
			first, second := reqs[before], reqs[before+1]
			for _, r := range []received{first, second} {
				if !bytes.Equal(r.body, body) {
					t.Fatalf("body = %s, want %s", r.body, body)
				}
				stamp, sig := r.header.Get("Parley-Timestamp"), r.header.Get("Parley-Signature")
				if !shape.MatchString(sig) {
					t.Errorf("signature = %q, want sha256= and 64 lower-case hexadecimal characters", sig)
				}
				if want := signature(testSecret, stamp, body); sig != want {
					t.Errorf("signature = %q, want %q for the timestamp %s and the body", sig, want, stamp)
				}
				changed := bytes.Clone(body)
				changed[len(changed)/2] ^= 1
				if sig == signature(testSecret, stamp, changed) {
					t.Errorf("the signature verifies a body with one byte changed")
				}
				n, _ := strconv.ParseInt(stamp, 10, 64)
				if sig == signature(testSecret, strconv.FormatInt(n+1, 10), body) {
					t.Errorf("the signature verifies the timestamp plus one")
				}
			}
			t1, _ := strconv.ParseInt(first.header.Get("Parley-Timestamp"), 10, 64)
			t2, _ := strconv.ParseInt(second.header.Get("Parley-Timestamp"), 10, 64)
			if want := b.clock.Now().Unix(); t2 != want || t2-t1 != 3600 {
				t.Errorf("timestamps = %d, %d; want the second %d, 3600 after the first", t1, t2, want)
			}
			if first.header.Get("Parley-Signature") == second.header.Get("Parley-Signature") {
				t.Errorf("the second attempt carries the first one's signature")
			}
			if d1, d2 := first.header.Get("Parley-Delivery"), second.header.Get("Parley-Delivery"); d1 != testDelivery || d2 != testDelivery {
				t.Errorf("Parley-Delivery = %q, %q; want %q at both attempts", d1, d2, testDelivery)
			}
		})
	}
}

func TestSend_SucceedsOnA2xxStatusWithoutKeepingTheBody(t *testing.T) {
	t.Parallel()
	const marker = "RESPONSE-MARKER"
	b := newBench(Settings{Timeout: time.Second, AllowInsecure: true}, nil)
	for _, tc := range []struct {
		name   string
		status int
		answer func(w http.ResponseWriter, r *http.Request, stop <-chan struct{})
	}{
		{"200", 200, nil},
		{"201", 201, nil},
		{"204", 204, nil},
		{"299", 299, nil},
		{"200 with the body held open for 5s", 200, func(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, marker)
			w.(http.Flusher).Flush()
			stall(r, stop, 5*time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answer := tc.answer
			if answer == nil {
				answer = func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
					w.WriteHeader(tc.status)
					io.WriteString(w, marker+" and more")
				}
			}
			rcv := newReceiver(t, false, answer)
			got, took := b.send(rcv.URL+"/h", []byte(`{}`))
			if !got.success || got.responseCode != tc.status || got.failure != "" || got.durationMS < 0 {
				t.Errorf("outcome = %+v, want success, response code %d, no error and a duration of 0 ms or more", got, tc.status)
			}
			if strings.Contains(fmt.Sprintf("%#v", got), marker) {
				t.Errorf("the outcome %#v holds the response body", got)
			}
			if took >= 2*time.Second {
				t.Errorf("the attempt took %s, want under 2s", took)
			}
		})
	}
}

func TestSend_FailsAndRecordsWhyOnAnyOtherEnd(t *testing.T) {
	t.Parallel()
	// The sender trusts no certificate, so that a TLS receiver's is
	// untrusted; every other receiver is plain HTTP on 127.0.0.1.
	b := newBench(Settings{Timeout: time.Second, AllowPrivate: true, AllowInsecure: true}, x509.NewCertPool())
	for _, tc := range []struct {
		name    string
		target  func(t *testing.T) (string, func(t *testing.T))
		code    int
		failure string
	}{
		{name: "404", code: 404, target: func(t *testing.T) (string, func(*testing.T)) {
			return newReceiver(t, false, answerStatus(404)).URL + "/h", nil
		}},
		{name: "500", code: 500, target: func(t *testing.T) (string, func(*testing.T)) {
			return newReceiver(t, false, answerStatus(500)).URL + "/h", nil
		}},
		{name: "302 to a second receiver", code: 302, failure: "redirect", target: func(t *testing.T) (string, func(*testing.T)) {
			second := newReceiver(t, false, answerStatus(200))
			first := newReceiver(t, false, func(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
				http.Redirect(w, r, second.URL+"/elsewhere", http.StatusFound)
			})
			return first.URL + "/h", func(t *testing.T) {
				if n := len(second.requests()); n != 0 {
					t.Errorf("the redirect's target got %d requests, want 0", n)
				}
			}
		}},
		{name: "a receiver that accepts and sends nothing for 3s", failure: "timeout", target: func(t *testing.T) (string, func(*testing.T)) {
			return "http://127.0.0.1:" + newTCPListener(t, 3*time.Second).port() + "/h", nil
		}},
		{name: "a receiver that sends its status after 3s", failure: "timeout", target: func(t *testing.T) (string, func(*testing.T)) {
			return newReceiver(t, false, func(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
				stall(r, stop, 3*time.Second)
				w.WriteHeader(http.StatusOK)
			}).URL + "/h", nil
		}},
		{name: "a closed port", failure: "connection_failed", target: func(t *testing.T) (string, func(*testing.T)) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			ln.Close()
			return "http://" + addr + "/h", nil
		}},
		{name: "a name that does not resolve", failure: "connection_failed", target: func(*testing.T) (string, func(*testing.T)) {
			return "https://nowhere.example/h", nil
		}},
		{name: "a TLS receiver whose certificate is not trusted", failure: "tls_error", target: func(t *testing.T) (string, func(*testing.T)) {
			return newReceiver(t, true, answerStatus(200)).URL + "/h", nil
		}},
		{name: "a plain-HTTP receiver behind an https url", failure: "tls_error", target: func(t *testing.T) (string, func(*testing.T)) {
			plain := newReceiver(t, false, answerStatus(200))
			return "https://" + plain.Listener.Addr().String() + "/h", nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target, after := tc.target(t)
			got, took := b.send(target, []byte(`{}`))
			if got.success || got.responseCode != tc.code || got.failure != tc.failure {
				t.Errorf("outcome = %+v, want a failure with response code %d and error %q", got, tc.code, tc.failure)
			}
			if took >= 2*time.Second {
				t.Errorf("the attempt took %s, want under 2s with a timeout of 1s", took)
			}
			if after != nil {
				after(t)
			}
		})
	}
}

func TestSend_RefusesADestinationAtConnectionTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		addrs []string
		// reaches tells whether, with allow_private, the attempt connects
		// to the listener.
		reaches bool
	}{
		{"the local listener's address", []string{"127.0.0.1"}, true},
		{"a private address", []string{"10.0.0.5"}, false},
		{"the IPv6 loopback address", []string{"::1"}, false},
		{"two addresses, the listener's first", []string{"127.0.0.1", "203.0.113.10"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ln := newTCPListener(t, 0)
			target := "https://moved.example:" + ln.port() + "/h"
			u, _ := url.Parse(target)
			// With allow_private only the rows that reach the listener are
			// sent, so that no test connects into the machine's network.
			runs := []Settings{{Timeout: time.Second}}
			if tc.reaches {
				runs = append(runs, Settings{Timeout: time.Second, AllowPrivate: true})
			}
			for _, settings := range runs {
				b := newBench(settings, nil)
				b.hosts.set("moved.example", "203.0.113.10")
				if err := b.component.checkDestination(context.Background(), u); err != nil {
					t.Fatalf("the url is refused while its name resolves outside: %v", err)
				}
				b.hosts.set("moved.example", tc.addrs...)
				before := ln.accepted.Load()
				got, _ := b.send(target, []byte(`{}`))
				accepted := ln.accepted.Load() - before
				if !settings.AllowPrivate {
					if got.success || got.responseCode != 0 || got.failure != "destination_refused" {
						t.Errorf("outcome = %+v, want a failure with no response code and error destination_refused", got)
					}
					if accepted != 0 || len(b.dials.handed()) != 0 {
						t.Errorf("the attempt opened connections: the listener accepted %d, the dialer was handed %v", accepted, b.dials.handed())
					}
					continue
				}
				if got.failure == "destination_refused" {
					t.Errorf("with allow_private the attempt is refused: %+v", got)
				}
				if accepted != 1 {
					t.Errorf("with allow_private the listener accepted %d connections, want 1 (dialer handed %v)", accepted, b.dials.handed())
				}
			}
		})
	}
}
