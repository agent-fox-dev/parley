package secret

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// errStoreDown is the failure of a store the tests break on purpose.
var errStoreDown = errors.New("store down")

// row is a stored secret as the table holds it.
type row struct {
	ID                  int64
	Scope, ScopeID, Key string
	Sealed              []byte
	KeyID               string
	Created, Updated    int64
}

// rows returns every stored secret in the order of its id.
func (s *storeTest) rows() []row {
	s.t.Helper()
	rs, err := s.st.Main().QueryContext(s.ctx,
		`SELECT id, scope, scope_id, key, sealed, key_id, created_at, updated_at FROM secrets ORDER BY id`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.ID, &r.Scope, &r.ScopeID, &r.Key, &r.Sealed, &r.KeyID, &r.Created, &r.Updated); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// under stores a secret sealed under key.
func (s *storeTest) under(key Key, scope Scope, id, name, value string) {
	s.t.Helper()
	s.c = s.component(Keys{Current: key})
	s.put(scope, id, name, value)
}

// pass is what a run of the re-encryption returned and logged.
type pass struct {
	err     error
	log     string
	reports []map[string]any
}

// reencrypt runs the re-encryption with keys and converters, over the table
// wrapped by wrap when it is not nil, with a captured logger.
func (s *storeTest) reencrypt(keys Keys, wrap func(rowStore) rowStore, converters ...Converter) pass {
	s.t.Helper()
	c := s.component(keys)
	for _, fn := range converters {
		c.RegisterConverter(fn)
	}
	var rows rowStore = table{c}
	if wrap != nil {
		rows = wrap(rows)
	}
	var buf bytes.Buffer
	p := pass{err: c.reencrypt(s.ctx, slog.New(slog.NewJSONHandler(&buf, nil)), rows)}
	p.log = buf.String()
	for line := range strings.SplitSeq(strings.TrimSpace(p.log), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec["msg"] == "secret re-encryption" {
			p.reports = append(p.reports, rec)
		}
	}
	return p
}

// wantReport fails the test unless the run logged exactly one info line
// `secret re-encryption` with these counts.
func (p pass) wantReport(t *testing.T, converted, remaining int) {
	t.Helper()
	if len(p.reports) != 1 {
		t.Fatalf("%d secret re-encryption lines, want 1; log = %s", len(p.reports), p.log)
	}
	rec := p.reports[0]
	want := map[string]any{"level": "INFO", "converted": float64(converted), "remaining": float64(remaining)}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %v, want %v; log = %s", k, rec[k], v, p.log)
		}
	}
}

// hookedRows wraps the table: afterRead runs once, after the first read and
// before any write, and the failWrite-th write fails.
type hookedRows struct {
	rowStore
	afterRead func()
	failWrite int
	writes    int
}

func (h *hookedRows) underKey(ctx context.Context, keyID string, afterID int64, limit int) ([]sealedRow, error) {
	rs, err := h.rowStore.underKey(ctx, keyID, afterID, limit)
	if h.afterRead != nil {
		h.afterRead()
		h.afterRead = nil
	}
	return rs, err
}

func (h *hookedRows) reseal(ctx context.Context, r sealedRow, s Sealed) (bool, error) {
	h.writes++
	if h.writes == h.failWrite {
		return false, errStoreDown
	}
	return h.rowStore.reseal(ctx, r, s)
}

func TestReencrypt_ResealsThePreviousKeysValuesUnderTheCurrentKey(t *testing.T) {
	a, b := newKey(t), newKey(t)
	s := newStoreTest(t, Keys{Current: a})
	values := map[[3]string]string{
		{"user", "u1", "GIT_PAT"}: "p1", {"user", "u1", "Token"}: "p2",
		{"org", "o1", "GIT_PAT"}: "p3", {"org", "o1", "DEPLOY"}: "p4",
		{"workspace", "acme", "GIT_PAT"}: "p5",
	}
	for k, v := range values {
		s.under(a, Scope(k[0]), k[1], k[2], v)
	}
	for k, v := range map[[3]string]string{{"workspace", "acme", "NEW1"}: "n1", {"workspace", "acme", "NEW2"}: "n2"} {
		s.under(b, Scope(k[0]), k[1], k[2], v)
		values[k] = v
	}
	s.now = s.now.Add(2 * time.Hour)

	p := s.reencrypt(Keys{Current: b, Previous: a}, nil)
	if p.err != nil {
		t.Fatalf("reencrypt: %v", p.err)
	}
	p.wantReport(t, 5, 0)
	for _, r := range s.rows() {
		if r.KeyID != KeyID(b) {
			t.Errorf("%s %s %s is under key %s, want the current key's", r.Scope, r.ScopeID, r.Key, r.KeyID)
		}
	}
	onlyCurrent := s.component(Keys{Current: b})
	for k, v := range values {
		if got, _ := s.get(onlyCurrent, Scope(k[0]), k[1], k[2]); got != v {
			t.Errorf("Get %v with only the current key = %q, want %q", k, got, v)
		}
	}
	s.noSecretEvents()
}

func TestReencrypt_ResealsWhatThePreviousKeyOpensAndCountsTheRest(t *testing.T) {
	a, b, c := newKey(t), newKey(t), newKey(t)
	rotation := func(s *storeTest) {
		s.under(a, ScopeUser, "u1", "OLD1", "sv-1")
		s.under(a, ScopeOrg, "o1", "OLD2", "sv-2")
		s.under(b, ScopeWorkspace, "acme", "NEW", "sv-3")
	}
	reports := func(converted, remaining int, err error) Converter {
		return func(context.Context, Keys) (int, int, error) { return converted, remaining, err }
	}
	cases := []struct {
		name                 string
		keys                 Keys
		seed                 func(s *storeTest)
		converters           []Converter
		resealed             int
		converted, remaining int
		wantErr              error
	}{
		{"values under the previous key", Keys{Current: b, Previous: a}, rotation, nil, 2, 2, 0, nil},
		{"more values than one batch", Keys{Current: b, Previous: a}, func(s *storeTest) {
			s.c = s.component(Keys{Current: a})
			s.fill(ScopeUser, "u1", 100)
			s.fill(ScopeOrg, "o1", 50)
		}, nil, 150, 150, 0, nil},
		{"values under a key no longer configured", Keys{Current: b, Previous: a}, func(s *storeTest) {
			rotation(s)
			s.under(c, ScopeOrg, "o1", "GONE1", "sv-4")
			s.under(c, ScopeOrg, "o1", "GONE2", "sv-5")
		}, nil, 2, 2, 2, nil},
		{"a value that fails to open", Keys{Current: b, Previous: a}, func(s *storeTest) {
			for _, k := range []string{"K1", "K2", "K3"} {
				s.under(a, ScopeUser, "u1", k, "sv-"+k)
			}
			r := s.rows()[1]
			r.Sealed[20] ^= 1
			s.exec(`UPDATE secrets SET sealed = ? WHERE id = ?`, r.Sealed, r.ID)
		}, nil, 2, 2, 1, nil},
		{"nothing stored", Keys{Current: b, Previous: a}, func(*storeTest) {}, nil, 0, 0, 0, nil},
		{"current key only", Keys{Current: b}, rotation, nil, 0, 0, 2, nil},
		{"previous key only", Keys{Previous: a}, rotation, nil, 0, 0, 3, nil},
		{"no key", Keys{}, rotation, nil, 0, 0, 3, nil},
		{"a registered converter", Keys{Current: b, Previous: a}, rotation,
			[]Converter{reports(3, 1, nil)}, 2, 5, 1, nil},
		{"a converter that fails", Keys{Current: b, Previous: a}, rotation,
			[]Converter{reports(1, 2, errStoreDown), reports(1, 0, nil)}, 2, 4, 2, errStoreDown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStoreTest(t, Keys{Current: b})
			tc.seed(s)
			before := s.rows()
			s.now = s.now.Add(2 * time.Hour)
			p := s.reencrypt(tc.keys, nil, tc.converters...)
			if !errors.Is(p.err, tc.wantErr) || (tc.wantErr == nil) != (p.err == nil) {
				t.Errorf("reencrypt error = %v, want %v", p.err, tc.wantErr)
			}
			p.wantReport(t, tc.converted, tc.remaining)
			resealed := 0
			for i, r := range s.rows() {
				was := before[i]
				switch {
				case reflect.DeepEqual(r, was):
				case r.KeyID == KeyID(b) && was.KeyID != r.KeyID && r.ID == was.ID && r.Key == was.Key &&
					r.Created == was.Created && r.Updated == was.Updated:
					resealed++
				default:
					t.Errorf("row changed other than by a reseal under the current key: %+v, was %+v", r, was)
				}
			}
			if resealed != tc.resealed {
				t.Errorf("%d rows resealed, want %d", resealed, tc.resealed)
			}
			if strings.Contains(p.log, "sv-") {
				t.Errorf("the log holds a value: %s", p.log)
			}
		})
	}
}

func TestReencrypt_KeepsWhatARequestChangedMeanwhile(t *testing.T) {
	a, b := newKey(t), newKey(t)
	s := newStoreTest(t, Keys{Current: a})
	for _, k := range []string{"KEEP", "UPDATED", "DELETED"} {
		s.under(a, ScopeWorkspace, "acme", k, "old")
	}
	rotated := s.component(Keys{Current: b, Previous: a})
	updatedAt := s.now.Add(time.Hour)
	p := s.reencrypt(Keys{Current: b, Previous: a}, func(rows rowStore) rowStore {
		return &hookedRows{rowStore: rows, afterRead: func() {
			s.now = updatedAt
			if err := s.tx(true, func(tx *sql.Tx) error {
				if _, err := rotated.UpdateEntry(s.ctx, tx, ScopeWorkspace, "acme", "UPDATED", "newer"); err != nil {
					return err
				}
				_, err := rotated.DeleteEntry(s.ctx, tx, ScopeWorkspace, "acme", "DELETED")
				return err
			}); err != nil {
				t.Fatalf("change between the read and the write: %v", err)
			}
		}}
	})
	if p.err != nil {
		t.Fatalf("reencrypt: %v", p.err)
	}
	p.wantReport(t, 1, 0)
	onlyCurrent := s.component(Keys{Current: b})
	if v, _ := s.get(onlyCurrent, ScopeWorkspace, "acme", "UPDATED"); v != "newer" {
		t.Errorf("updated secret = %q, want newer", v)
	}
	if v, _ := s.get(onlyCurrent, ScopeWorkspace, "acme", "KEEP"); v != "old" {
		t.Errorf("untouched secret = %q, want old", v)
	}
	for _, r := range s.rows() {
		if r.Key == "UPDATED" && r.Updated != updatedAt.UnixNano() {
			t.Errorf("updated_at = %d, want the update's %d", r.Updated, updatedAt.UnixNano())
		}
	}
	if _, found := s.get(onlyCurrent, ScopeWorkspace, "acme", "DELETED"); found {
		t.Error("the deleted secret is back")
	}
}

func TestReencrypt_StopsAtAStoreFailureKeepingWhatItConverted(t *testing.T) {
	a, b := newKey(t), newKey(t)
	s := newStoreTest(t, Keys{Current: a})
	keys := []string{"K1", "K2", "K3", "K4", "K5"}
	for _, k := range keys {
		s.under(a, ScopeOrg, "o1", k, "v-"+k)
	}
	p := s.reencrypt(Keys{Current: b, Previous: a}, func(rows rowStore) rowStore {
		return &hookedRows{rowStore: rows, failWrite: 3}
	})
	if !errors.Is(p.err, errStoreDown) {
		t.Fatalf("reencrypt error = %v, want the store's", p.err)
	}
	for i, r := range s.rows() {
		if under := r.KeyID == KeyID(b); under != (i < 2) {
			t.Errorf("%s under the current key = %v, want %v", r.Key, under, i < 2)
		}
	}
	both := s.component(Keys{Current: b, Previous: a})
	for _, k := range keys {
		if v, _ := s.get(both, ScopeOrg, "o1", k); v != "v-"+k {
			t.Errorf("Get %s = %q, want %q", k, v, "v-"+k)
		}
	}
}
