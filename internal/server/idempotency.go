package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IdempotencyTTL is how long a create's answer is replayed.
const IdempotencyTTL = 24 * time.Hour

// MaxIdempotencyKeyLength is the longest Idempotency-Key accepted.
const MaxIdempotencyKeyLength = 255

// IdempotencyRecord is the recorded answer of a successful create.
type IdempotencyRecord struct {
	// Key is the Idempotency-Key.
	Key string
	// Caller is the caller it was recorded for: "user:<id>" or "admin".
	Caller string
	// Route is the method and the concrete request path
	// ("POST /api/v1/workspaces/w/tokens").
	Route string
	// BodyHash is the hexadecimal SHA-256 of the request body.
	BodyHash string
	// Response is the response body without the route's show-once members.
	Response []byte
	// CreatedAt is when the record was written.
	CreatedAt time.Time
}

// IdempotencyStore keeps idempotency records. The server's default keeps
// them in memory; the hub installs one over its durable store, so that a
// record outlives a restart.
type IdempotencyStore interface {
	// Find returns the record for key, caller and route, or nil.
	Find(ctx context.Context, key, caller, route string) (*IdempotencyRecord, error)
	// Put writes a record, replacing one with the same key, caller and
	// route.
	Put(ctx context.Context, rec IdempotencyRecord) error
	// DeleteBefore removes every record written before cutoff, or at it.
	DeleteBefore(ctx context.Context, cutoff time.Time) error
}

// MemoryIdempotencyStore is an IdempotencyStore in memory.
type MemoryIdempotencyStore struct {
	mu      sync.Mutex
	records map[string]IdempotencyRecord
}

// NewMemoryIdempotencyStore builds an empty in-memory store.
func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{records: map[string]IdempotencyRecord{}}
}

func recordID(key, caller, route string) string {
	return key + "\x00" + caller + "\x00" + route
}

// Find implements IdempotencyStore.
func (m *MemoryIdempotencyStore) Find(_ context.Context, key, caller, route string) (*IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[recordID(key, caller, route)]
	if !ok {
		return nil, nil
	}
	return &rec, nil
}

// Put implements IdempotencyStore.
func (m *MemoryIdempotencyStore) Put(_ context.Context, rec IdempotencyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[recordID(rec.Key, rec.Caller, rec.Route)] = rec
	return nil
}

// DeleteBefore implements IdempotencyStore.
func (m *MemoryIdempotencyStore) DeleteBefore(_ context.Context, cutoff time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range m.records {
		if !rec.CreatedAt.After(cutoff) {
			delete(m.records, id)
		}
	}
	return nil
}

// keyGates orders concurrent requests with one key, caller and route: the
// first runs, the others wait for it. A gate exists only while used.
type keyGates struct {
	mu    sync.Mutex
	gates map[string]*keyGate
}

type keyGate struct {
	mu   sync.Mutex
	refs int
}

func (g *keyGates) lock(id string) (unlock func()) {
	g.mu.Lock()
	if g.gates == nil {
		g.gates = map[string]*keyGate{}
	}
	gate := g.gates[id]
	if gate == nil {
		gate = &keyGate{}
		g.gates[id] = gate
	}
	gate.refs++
	g.mu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		g.mu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(g.gates, id)
		}
		g.mu.Unlock()
	}
}

// idempotencyCaller is the caller a record belongs to: the user, or the
// admin token. Without an authenticated caller (a public create route) it is
// the client address.
func idempotencyCaller(r *http.Request) string {
	c := CallerFrom(r.Context())
	switch {
	case c == nil:
		return "address:" + clientAddress(r)
	case c.Admin:
		return "admin"
	default:
		return "user:" + c.UserID
	}
}

// runCreate is the idempotency stage of a create route, between the access
// checks and the handler. Without an Idempotency-Key it just runs the
// handler. With one, a fresh record for the same key, caller and route is
// replayed as 200 (or refused 409 conflict for another request body);
// otherwise the handler runs and a 2xx answer is recorded without the
// route's show-once members.
func (s *Server) runCreate(w http.ResponseWriter, r *http.Request, rt *Route) error {
	values, present := r.Header["Idempotency-Key"]
	if !present {
		return rt.Handler(w, r)
	}
	key := values[0]
	if key == "" || len(key) > MaxIdempotencyKeyLength {
		return InvalidRequest("Idempotency-Key must be 1 to 255 characters")
	}
	caller := idempotencyCaller(r)
	route := r.Method + " " + r.URL.EscapedPath()
	sum := sha256.Sum256(Body(r))
	bodyHash := hex.EncodeToString(sum[:])

	unlock := s.idemGates.lock(recordID(key, caller, route))
	defer unlock()

	ctx := r.Context()
	now := s.now()
	rec, err := s.idem.Find(ctx, key, caller, route)
	if err != nil {
		return err
	}
	if rec != nil && now.Sub(rec.CreatedAt) < IdempotencyTTL {
		if rec.BodyHash != bodyHash {
			return Conflict("Idempotency-Key was already used with another request body")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(rec.Response)
		return nil
	}

	cw := &captureWriter{header: http.Header{}}
	if err := rt.Handler(cw, r); err != nil && !cw.wrote {
		s.writeFailure(cw, r, err)
	}
	status := cw.statusOrOK()
	if status >= 200 && status < 300 {
		stored := IdempotencyRecord{
			Key: key, Caller: caller, Route: route, BodyHash: bodyHash,
			Response: withoutMembers(cw.body.Bytes(), rt.ShowOnce), CreatedAt: now,
		}
		if err := s.idem.DeleteBefore(ctx, now.Add(-IdempotencyTTL)); err != nil {
			s.logger.Error("idempotency: dropping expired records", "request_id", RequestID(ctx), "error", err.Error())
		}
		if err := s.idem.Put(ctx, stored); err != nil {
			s.logger.Error("idempotency: recording a create", "request_id", RequestID(ctx), "error", err.Error())
		}
	}
	for k, v := range cw.header {
		w.Header()[k] = v
	}
	w.WriteHeader(status)
	w.Write(cw.body.Bytes())
	return nil
}

// withoutMembers removes the dotted member paths from a JSON object body; a
// body that is not a JSON object is returned as it is.
func withoutMembers(body []byte, paths []string) []byte {
	if len(paths) == 0 {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return body
	}
	for _, p := range paths {
		parts := strings.Split(p, ".")
		obj := doc
		for i, part := range parts {
			if i == len(parts)-1 {
				delete(obj, part)
				break
			}
			next, ok := obj[part].(map[string]any)
			if !ok {
				break
			}
			obj = next
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// captureWriter holds a handler's answer so that it can be recorded before
// it is sent.
type captureWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
	wrote  bool
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	if !c.wrote {
		c.status, c.wrote = status, true
	}
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.status, c.wrote = http.StatusOK, true
	}
	return c.body.Write(b)
}

func (c *captureWriter) statusOrOK() int {
	if !c.wrote {
		return http.StatusOK
	}
	return c.status
}
