package harness

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Delivery is one request a Receiver received.
type Delivery struct {
	Method string
	Header http.Header
	// Body is the request's body byte for byte.
	Body []byte
	At   time.Time
	// Status is what the receiver answered.
	Status int
}

// Seq is the delivery's Parley-Seq, 0 when it has none.
func (d Delivery) Seq() int64 {
	n, _ := strconv.ParseInt(d.Header.Get("Parley-Seq"), 10, 64)
	return n
}

// Receiver is a webhook endpoint on a loopback port that records every
// request and answers with the status the scenario sets. A hub sends to it
// only with [webhooks] allow_insecure = true, since it is plain HTTP.
type Receiver struct {
	URL    string
	t      testing.TB
	mu     sync.Mutex
	status int
	got    []Delivery
	notify chan struct{}
}

// NewReceiver starts a receiver that answers 200 until told otherwise; it
// is closed when the test ends.
func NewReceiver(tb testing.TB) *Receiver {
	tb.Helper()
	r := &Receiver{t: tb, status: http.StatusOK, notify: make(chan struct{}, 1)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		status := r.status
		r.got = append(r.got, Delivery{Method: req.Method, Header: req.Header.Clone(), Body: body, At: time.Now(), Status: status})
		r.mu.Unlock()
		select {
		case r.notify <- struct{}{}:
		default:
		}
		w.WriteHeader(status)
	}))
	tb.Cleanup(srv.Close)
	r.URL = srv.URL
	return r
}

// SetStatus makes the receiver answer status from now on.
func (r *Receiver) SetStatus(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

// Deliveries is every request received so far, in order of arrival.
func (r *Receiver) Deliveries() []Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Delivery(nil), r.got...)
}

// Wait returns the first n requests once n have arrived, and fails the
// test when they have not within timeout.
func (r *Receiver) Wait(n int, timeout time.Duration) []Delivery {
	r.t.Helper()
	deadline := time.After(timeout)
	for {
		if got := r.Deliveries(); len(got) >= n {
			return got[:n]
		}
		select {
		case <-r.notify:
		case <-deadline:
			r.t.Fatalf("%d of %d webhook requests arrived within %v", len(r.Deliveries()), n, timeout)
		}
	}
}

// Last is the request received last, failing the test when none was.
func (r *Receiver) Last() Delivery {
	r.t.Helper()
	got := r.Deliveries()
	if len(got) == 0 {
		r.t.Fatal("the webhook receiver got no request")
	}
	return got[len(got)-1]
}

// WaitUntilSeq returns every request received, once one carrying the
// Parley-Seq seq was answered with a 2xx status, and fails the test when
// none was within timeout.
func (r *Receiver) WaitUntilSeq(seq int64, timeout time.Duration) []Delivery {
	r.t.Helper()
	deadline := time.After(timeout)
	for {
		got := r.Deliveries()
		for _, d := range got {
			if d.Seq() == seq && d.Status >= 200 && d.Status < 300 {
				return got
			}
		}
		select {
		case <-r.notify:
		case <-deadline:
			r.t.Fatalf("no webhook request of the seq %d was accepted within %v; %d requests arrived", seq, timeout, len(got))
		}
	}
}

// VerifySignature reports whether the delivery's Parley-Signature is
// sha256= and the lower-case hex HMAC-SHA256, keyed with secret, of its
// Parley-Timestamp, a dot and its body.
func VerifySignature(secret string, d Delivery) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(d.Header.Get("Parley-Timestamp") + "."))
	mac.Write(d.Body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(d.Header.Get("Parley-Signature")), []byte(want))
}
