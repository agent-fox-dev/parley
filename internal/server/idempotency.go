package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// idempotencyTTL is how long a record is replayed; a record this old or older
// is treated as absent.
const idempotencyTTL = 24 * time.Hour

// idempotencySweepInterval is the shortest time between two sweeps of the
// expired records. A sweep of the in-memory store visits every record, so
// running it on every write would make each create cost time in proportion
// to the last day's records; an expired record left until the next sweep is
// never replayed, because serveCreate checks its age.
const idempotencySweepInterval = time.Minute

// maxIdempotencyKey is the longest Idempotency-Key accepted, in characters.
const maxIdempotencyKey = 255

// adminCaller is the caller of a record made with the admin token. User ids
// are recorded with a "user:" prefix, so no user id can equal it.
const adminCaller = "admin-token"

// IdempotencyRecord is the recorded outcome of a successful create sent with
// an Idempotency-Key. Key, Caller and Route identify it.
type IdempotencyRecord struct {
	// Key is the Idempotency-Key, 1 to 255 characters.
	Key string
	// Caller is "user:" and the caller's user id, or "admin-token" for the
	// admin token.
	Caller string
	// Route is the method and the concrete request path, separated by one
	// space: "POST /api/v1/workspaces/w1/tokens".
	Route string
	// RequestHash is the SHA-256 of the request body's bytes.
	RequestHash [sha256.Size]byte
	// Body is the response body the handler answered, without the route's
	// show-once members.
	Body []byte
	// Header is the response headers the handler set before it answered,
	// such as Content-Type and Location, without Content-Length.
	Header http.Header
	// WrittenAt is when the record was written, by the server's clock.
	WrittenAt time.Time
}

// IdempotencyStore keeps the idempotency records. The server decides from
// WrittenAt whether a record is still valid, so a store only finds, writes
// and deletes. Every method must be safe for concurrent use.
type IdempotencyStore interface {
	// Find returns the record of key, caller and route, and false when there
	// is none. An expired record not yet deleted may be returned.
	Find(ctx context.Context, key, caller, route string) (IdempotencyRecord, bool, error)
	// Write stores rec, replacing any record with the same key, caller and
	// route.
	Write(ctx context.Context, rec IdempotencyRecord) error
	// DeleteExpired removes every record written at or before cutoff.
	DeleteExpired(ctx context.Context, cutoff time.Time) error
}

type recordKey struct {
	key, caller, route string
}

// MemoryIdempotencyStore keeps idempotency records in memory, so they are
// lost when the process ends. It is the server's store when
// Options.IdempotencyStore is nil.
type MemoryIdempotencyStore struct {
	mu      sync.Mutex
	records map[recordKey]IdempotencyRecord
}

// NewMemoryIdempotencyStore returns an empty in-memory store.
func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{records: make(map[recordKey]IdempotencyRecord)}
}

// Find returns the record of key, caller and route.
func (m *MemoryIdempotencyStore) Find(_ context.Context, key, caller, route string) (IdempotencyRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[recordKey{key, caller, route}]
	rec.Body = bytes.Clone(rec.Body)
	rec.Header = rec.Header.Clone()
	return rec, ok, nil
}

// Write stores rec, replacing any record with the same key, caller and route.
func (m *MemoryIdempotencyStore) Write(_ context.Context, rec IdempotencyRecord) error {
	rec.Body = bytes.Clone(rec.Body)
	rec.Header = rec.Header.Clone()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[recordKey{rec.Key, rec.Caller, rec.Route}] = rec
	return nil
}

// DeleteExpired removes every record written at or before cutoff.
func (m *MemoryIdempotencyStore) DeleteExpired(_ context.Context, cutoff time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.records, func(_ recordKey, rec IdempotencyRecord) bool {
		return !rec.WrittenAt.After(cutoff)
	})
	return nil
}

// idempotencyCaller is the caller part of a record's identity: the
// admin-token marker, else the user id, else the credential id, so that two
// callers never share a record. A caller with none of them has no identity
// and gets "", and its requests are never recorded.
func idempotencyCaller(c Caller) string {
	switch {
	case c.Admin:
		return adminCaller
	case c.UserID != "":
		return "user:" + c.UserID
	case c.CredentialID != "":
		return "credential:" + c.CredentialID
	default:
		return ""
	}
}

// idempotency is the server's idempotency state: the record store, the
// gate that orders concurrent requests with one key and the time of the last
// sweep of the expired records.
type idempotency struct {
	store IdempotencyStore
	gate  keyGate

	mu    sync.Mutex
	swept time.Time // when the last sweep started; zero before the first
}

// sweepDue reports whether the expired records are to be swept at now, and
// if so takes the sweep, so that concurrent writes do not sweep twice.
func (i *idempotency) sweepDue(now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.swept.IsZero() && now.Sub(i.swept) < idempotencySweepInterval {
		return false
	}
	i.swept = now
	return true
}

func newIdempotency(store IdempotencyStore) *idempotency {
	if store == nil {
		store = NewMemoryIdempotencyStore()
	}
	return &idempotency{store: store, gate: keyGate{held: make(map[recordKey]*gateSlot)}}
}

// serveCreate is the idempotency stage of a create route, run after every
// check and in place of a direct call of the handler. A request without an
// Idempotency-Key, or one with no caller identity (a public route, or a
// caller with neither user nor credential), runs the handler and records
// nothing.
func (s *Server) serveCreate(w http.ResponseWriter, r *http.Request, rt Route) error {
	values, present := r.Header["Idempotency-Key"]
	c, _ := CallerOf(r.Context())
	caller := idempotencyCaller(c)
	if !present || caller == "" {
		// Without a caller, a record would replay one anonymous client's
		// answer to another.
		return rt.Handler(w, r)
	}
	key := values[0]
	if key == "" || utf8.RuneCountInString(key) > maxIdempotencyKey {
		return InvalidRequest(fmt.Sprintf("Idempotency-Key must be 1 to %d characters", maxIdempotencyKey))
	}
	id := recordKey{key: key, caller: caller, route: r.Method + " " + r.URL.Path}
	body, _ := bufferedBody(r)
	hash := sha256.Sum256(body)

	// Requests with the same key, caller and route run one at a time, so
	// that a second one sees the first one's record instead of creating too.
	release, err := s.idem.gate.lock(r.Context(), id)
	if err != nil {
		// The wait ends early only when the request's context does: the
		// client went away. Nobody is left to read an answer, and the hub
		// did not fail, so the request ends unanswered and is not logged
		// as an error.
		s.logger.LogAttrs(r.Context(), slog.LevelDebug, "request ended while waiting for idempotency key",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("error", err.Error()),
		)
		return nil
	}
	defer release()

	rec, found, err := s.idem.store.Find(r.Context(), id.key, id.caller, id.route)
	if err != nil {
		return fmt.Errorf("find idempotency record: %w", err)
	}
	now := s.now()
	if found && now.Sub(rec.WrittenAt) < idempotencyTTL {
		if rec.RequestHash != hash {
			return Conflict("Idempotency-Key was already used with a different request body")
		}
		h := w.Header()
		for name, values := range rec.Header {
			h[name] = slices.Clone(values)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rec.Body)
		return nil
	}

	cw := newCaptureWriter(w)
	if err := rt.Handler(cw, r); err != nil {
		return err
	}
	// A handler that wrote nothing answers 200 with the headers it set.
	cw.answer(http.StatusOK)
	if cw.code < 200 || cw.code > 299 {
		return nil
	}
	stored, err := removeMembers(cw.body.Bytes(), rt.ShowOnce)
	if err == nil {
		err = s.writeRecord(r.Context(), IdempotencyRecord{
			Key: id.key, Caller: id.caller, Route: id.route,
			RequestHash: hash, Body: stored, Header: cw.header, WrittenAt: now,
		})
	}
	if err != nil {
		// The caller already has the handler's answer; a retry then runs
		// the handler again, as if the key had not been sent.
		s.logger.LogAttrs(r.Context(), slog.LevelError, "idempotency record not written",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("error", err.Error()),
		)
	}
	return nil
}

// writeRecord removes the expired records, at most once a sweep interval,
// then writes rec.
func (s *Server) writeRecord(ctx context.Context, rec IdempotencyRecord) error {
	if s.idem.sweepDue(rec.WrittenAt) {
		if err := s.idem.store.DeleteExpired(ctx, rec.WrittenAt.Add(-idempotencyTTL)); err != nil {
			return fmt.Errorf("delete expired idempotency records: %w", err)
		}
	}
	if err := s.idem.store.Write(ctx, rec); err != nil {
		return fmt.Errorf("write idempotency record: %w", err)
	}
	return nil
}

// removeMembers returns body without the members at paths, each a member
// name or a dotted path into nested objects (token.token). A path the body
// does not have is skipped. Every value it keeps is kept as written, apart
// from white space; an object it changes is re-encoded with its members in
// sorted order.
func removeMembers(body []byte, paths []string) ([]byte, error) {
	if len(paths) == 0 {
		return body, nil
	}
	for _, p := range paths {
		var err error
		if body, err = removeMember(body, strings.Split(p, ".")); err != nil {
			return nil, fmt.Errorf("remove show-once member %s: %w", p, err)
		}
	}
	return body, nil
}

func removeMember(object []byte, path []string) ([]byte, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(object, &members); err != nil {
		return nil, fmt.Errorf("response body is not a JSON object: %w", err)
	}
	inner, ok := members[path[0]]
	switch {
	case !ok:
		return object, nil
	case len(path) == 1:
		delete(members, path[0])
	default:
		if !bytes.HasPrefix(bytes.TrimSpace(inner), []byte("{")) {
			return object, nil
		}
		changed, err := removeMember(inner, path[1:])
		if err != nil {
			return nil, err
		}
		members[path[0]] = changed
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	// The members kept must read as the handler wrote them.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(members); err != nil {
		return nil, fmt.Errorf("encode response body: %w", err)
	}
	return out.Bytes(), nil
}

// captureWriter passes a handler's response through unchanged and keeps a
// copy of its status, the headers it set and its body, for the record.
type captureWriter struct {
	http.ResponseWriter
	code   int
	before http.Header // the response headers before the handler ran
	header http.Header // the headers the handler set, once it answered
	body   bytes.Buffer
}

func newCaptureWriter(w http.ResponseWriter) *captureWriter {
	return &captureWriter{ResponseWriter: w, before: w.Header().Clone()}
}

// answer records the status and the headers the handler set, the first time
// it is called: headers set after that are not sent.
func (w *captureWriter) answer(status int) {
	if w.code != 0 {
		return
	}
	w.code = status
	for name, values := range w.ResponseWriter.Header() {
		// The replayed body can be shorter than the one answered, once its
		// show-once members are removed.
		if name == "Content-Length" || slices.Equal(w.before[name], values) {
			continue
		}
		if w.header == nil {
			w.header = make(http.Header)
		}
		w.header[name] = slices.Clone(values)
	}
}

func (w *captureWriter) WriteHeader(status int) {
	if status >= 200 {
		w.answer(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.answer(http.StatusOK)
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController and headersSent reach the writers
// below.
func (w *captureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// keyGate lets one request at a time hold a record key. Each key has its own
// slot, so requests with different keys never wait for one another, and a
// slot is dropped once no request holds or awaits it.
type keyGate struct {
	mu   sync.Mutex
	held map[recordKey]*gateSlot
}

type gateSlot struct {
	token chan struct{}
	refs  int
}

// lock waits until id is free or ctx ends, and returns the function that
// frees it.
func (g *keyGate) lock(ctx context.Context, id recordKey) (func(), error) {
	g.mu.Lock()
	slot := g.held[id]
	if slot == nil {
		slot = &gateSlot{token: make(chan struct{}, 1)}
		g.held[id] = slot
	}
	slot.refs++
	g.mu.Unlock()

	select {
	case slot.token <- struct{}{}:
		return func() {
			<-slot.token
			g.drop(id, slot)
		}, nil
	case <-ctx.Done():
		g.drop(id, slot)
		return nil, ctx.Err()
	}
}

func (g *keyGate) drop(id recordKey, slot *gateSlot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	slot.refs--
	if slot.refs == 0 {
		delete(g.held, id)
	}
}
