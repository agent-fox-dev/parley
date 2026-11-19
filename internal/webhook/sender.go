package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
)

// The error of an attempt that ended without a 2xx status, as a delivery
// records it. A status other than 2xx or 3xx has no error: its code says why.
const (
	// failureRedirect is a 3xx status, which is not followed.
	failureRedirect = "redirect"
	// failureTimeout is an attempt that got no status within
	// [webhooks].timeout.
	failureTimeout = "timeout"
	// failureConnection is a host that does not resolve or a connection that
	// cannot be opened.
	failureConnection = "connection_failed"
	// failureTLS is a TLS handshake that fails, an untrusted certificate
	// included.
	failureTLS = "tls_error"
	// failureRefused is a host that resolves, at the time of the attempt, to
	// an address the destination rules refuse.
	failureRefused = "destination_refused"
)

// Dialer opens the connections of the attempts; *net.Dialer is one.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// attempt is one request of a delivery: the url, the event's JSON form as
// the event log returns it, the event's type and seq, the delivery's id, and
// the signing secret as the owner was shown it. The secret is read for every
// attempt, so that a rotated secret signs from the next attempt on.
type attempt struct {
	url       string
	body      []byte
	eventType string
	seq       int64
	delivery  string
	secret    string
}

// outcome is how an attempt ended: success for a 2xx status; responseCode
// the status, 0 when none arrived; failure the error of the delivery record,
// "" for none; and durationMS how long the attempt took. It never holds the
// response's body.
type outcome struct {
	success      bool
	responseCode int
	failure      string
	durationMS   int64
}

// sender makes the attempts of every subscription. It verifies certificates
// against roots, the system's when nil, and has no setting that turns the
// verification off.
type sender struct {
	settings  Settings
	clock     event.Clock
	userAgent string
	resolver  Resolver
	dialer    Dialer
	roots     *x509.CertPool
}

// newSender returns the sender of a component bound to d, which names itself
// as parley-webhook/version.
func newSender(d Deps, version string) *sender {
	s := &sender{
		settings:  d.Settings,
		clock:     d.Clock,
		userAgent: "parley-webhook/" + version,
		resolver:  d.Resolver,
		dialer:    d.Dialer,
		roots:     d.RootCAs,
	}
	if s.settings.Timeout <= 0 {
		s.settings.Timeout = DefaultTimeout
	}
	return s
}

// send makes one attempt: a POST of a.body to a.url, signed with a.secret
// over the clock's time, and judges how it ended. It ends within
// [webhooks].timeout, or when ctx ends; a caller whose ctx ended discards the
// outcome.
func (s *sender) send(ctx context.Context, a attempt) outcome {
	start := time.Now()
	out := s.post(ctx, a)
	out.durationMS = time.Since(start).Milliseconds()
	return out
}

func (s *sender) post(ctx context.Context, a attempt) outcome {
	ctx, cancel := context.WithTimeout(ctx, s.settings.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(a.body))
	if err != nil {
		return outcome{failure: failureConnection}
	}
	stamp := strconv.FormatInt(s.clock.Now().Unix(), 10)
	// The header is built whole, so that nothing but these names leaves the
	// hub: no credential, no cookie, nothing that identifies a user.
	req.Header = http.Header{
		"Content-Type":     {"application/json"},
		"Parley-Event":     {a.eventType},
		"Parley-Delivery":  {a.delivery},
		"Parley-Seq":       {strconv.FormatInt(a.seq, 10)},
		"Parley-Timestamp": {stamp},
		"Parley-Signature": {sign(a.secret, stamp, a.body)},
		"User-Agent":       {s.userAgent},
	}
	transport := s.transport()
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return outcome{failure: failureOf(err)}
	}
	// The body is not read: the status decides the outcome, and a receiver
	// that keeps its body open must not hold the attempt.
	_ = resp.Body.Close()
	switch code := resp.StatusCode; {
	case code >= 200 && code <= 299:
		return outcome{success: true, responseCode: code}
	case code >= 300 && code <= 399:
		return outcome{responseCode: code, failure: failureRedirect}
	default:
		return outcome{responseCode: code}
	}
}

// transport is the transport of one attempt. Every attempt has its own, so
// that every attempt opens its connection, and the address it connects to is
// judged at that moment. It uses no proxy of the environment, which would
// connect in the destination's stead.
func (s *sender) transport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, s.settings.Timeout)
			defer cancel()
			return s.dial(ctx, network, address, "http")
		},
		DialTLSContext:     s.dialTLS,
		DisableCompression: true,
	}
}

// dialTLS opens the connection of an https url and makes its TLS handshake
// against the sender's trust roots. A handshake that fails is a
// handshakeError, unless the time ran out.
func (s *sender) dialTLS(ctx context.Context, network, address string) (net.Conn, error) {
	// The transport dials apart from the request's deadline, so the dial
	// and the handshake are bounded here.
	ctx, cancel := context.WithTimeout(ctx, s.settings.Timeout)
	defer cancel()
	conn, err := s.dial(ctx, network, address, "https")
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(address)
	tc := tls.Client(conn, &tls.Config{RootCAs: s.roots, ServerName: host, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &handshakeError{err: err}
	}
	return tc, nil
}

// dial resolves the host of address and, when the destination rules allow
// every address it resolves to for the scheme, connects to the first that
// answers. A refused address fails the dial before any connection is
// opened: the name may resolve elsewhere than when the url was accepted.
func (s *sender) dial(ctx context.Context, network, address, scheme string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := addressesOf(ctx, s.resolver, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s resolves to no address", host)
	}
	for _, addr := range addrs {
		if !destinationAllowed(addr, scheme, s.settings) {
			return nil, &refusedError{host: host, addr: addr}
		}
	}
	var last error
	for _, addr := range addrs {
		conn, err := s.dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// refusedError is a dial the destination rules refused.
type refusedError struct {
	host string
	addr netip.Addr
}

func (e *refusedError) Error() string {
	return "destination refused: " + e.host + " resolves to " + e.addr.String()
}

// handshakeError is a TLS handshake that failed.
type handshakeError struct{ err error }

func (e *handshakeError) Error() string { return "tls handshake: " + e.err.Error() }

func (e *handshakeError) Unwrap() error { return e.err }

// failureOf is the delivery error of an attempt that got no status.
func failureOf(err error) string {
	var refused *refusedError
	var handshake *handshakeError
	var netErr net.Error
	switch {
	case errors.As(err, &refused):
		return failureRefused
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return failureTimeout
	case errors.As(err, &handshake):
		return failureTLS
	default:
		return failureConnection
	}
}

// sign is the Parley-Signature of body sent at stamp: sha256= and the
// lower-case hexadecimal HMAC-SHA256 of the stamp, a dot and the body, keyed
// with the secret as the owner holds it, the ASCII of whsec_ and its 64
// hexadecimal characters.
func sign(secret, stamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
