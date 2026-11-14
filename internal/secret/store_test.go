package secret

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/store"
)

// LogLines decodes the lines of a JSON log whose msg is msg, and fails the
// test on a line that is not JSON. It is exported for the tests of package
// secret_test, which read a running hub's log with it too.
func LogLines(t *testing.T, log, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil && line != "" {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// errRollback makes a transaction of the tests roll back.
var errRollback = errors.New("roll back")

// storeTest is a real store in a temporary directory with the secrets and
// the hub events in it, and a component bound to it.
type storeTest struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	scrub *scrub.Scrubber
	now   time.Time
	c     *Component
}

// newStoreTest opens the store and binds a component to it with keys.
func newStoreTest(t *testing.T, keys Keys) *storeTest {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{event.SchemaStep, SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := &storeTest{t: t, ctx: context.Background(), st: st, scrub: scrub.New(),
		now: time.Date(2026, 11, 14, 9, 30, 15, 0, time.UTC)}
	s.c = s.component(keys)
	return s
}

// component returns another component on the same store with other keys.
func (s *storeTest) component(keys Keys) *Component {
	return New(Deps{Store: s.st, Keys: keys, Now: func() time.Time { return s.now }, Scrubber: s.scrub})
}

// tx runs fn in a transaction and rolls it back when commit is false.
func (s *storeTest) tx(commit bool, fn func(tx *sql.Tx) error) error {
	s.t.Helper()
	err := s.st.Tx(s.ctx, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if !commit {
			return errRollback
		}
		return nil
	})
	if errors.Is(err, errRollback) {
		return nil
	}
	return err
}

// put stores one secret through the component and fails the test on error.
func (s *storeTest) put(scope Scope, id, key, value string) {
	s.t.Helper()
	if err := s.tx(true, func(tx *sql.Tx) error { return s.c.Put(s.ctx, tx, scope, id, key, value) }); err != nil {
		s.t.Fatalf("Put %s %s %s: %v", scope, id, key, err)
	}
}

// fill stores n secrets named K0 … in a scope.
func (s *storeTest) fill(scope Scope, id string, n int) {
	s.t.Helper()
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{Key: fmt.Sprintf("K%d", i), Value: "v"}
	}
	if err := s.tx(true, func(tx *sql.Tx) error {
		_, err := s.c.CreateEntries(s.ctx, tx, scope, id, entries)
		return err
	}); err != nil {
		s.t.Fatalf("fill %s %s: %v", scope, id, err)
	}
}

// get reads a secret and fails the test on an error.
func (s *storeTest) get(c *Component, scope Scope, id, key string) (string, bool) {
	s.t.Helper()
	v, found, err := c.Get(s.ctx, scope, id, key)
	if err != nil {
		s.t.Fatalf("Get %s %s %s: %v", scope, id, key, err)
	}
	return v, found
}

// count is the number of secrets in a scope.
func (s *storeTest) count(scope Scope, id string) int {
	s.t.Helper()
	n, err := s.c.Count(s.ctx, scope, id)
	if err != nil {
		s.t.Fatalf("Count: %v", err)
	}
	return n
}

// exec changes the secret table directly, as a damaged or copied store would.
func (s *storeTest) exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.st.Main().ExecContext(s.ctx, query, args...); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
}

// noSecretEvents fails the test when any parley.secret event was written.
func (s *storeTest) noSecretEvents() {
	s.t.Helper()
	var n int
	if err := s.st.Main().QueryRowContext(s.ctx,
		`SELECT COUNT(*) FROM hub_events WHERE type LIKE 'parley.secret.%'`).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	if n != 0 {
		s.t.Errorf("%d parley.secret events written, want none", n)
	}
}

func TestGet_ReadsTheEntryOfExactlyThatScope(t *testing.T) {
	s := newStoreTest(t, Keys{Current: newKey(t)})
	s.put(ScopeWorkspace, "acme", "Git_Pat", "tok-4be1c0ffee-2d9a")
	s.put(ScopeWorkspace, "acme", "EMPTY", "")

	for _, tc := range []struct {
		scope     Scope
		id, key   string
		want      string
		wantFound bool
	}{
		{ScopeWorkspace, "acme", "GIT_PAT", "tok-4be1c0ffee-2d9a", true},
		{ScopeWorkspace, "acme", "git_pat", "tok-4be1c0ffee-2d9a", true},
		{ScopeWorkspace, "acme", "empty", "", true},
		{ScopeWorkspace, "acme-fork", "GIT_PAT", "", false},
		{ScopeUser, "owner", "GIT_PAT", "", false},
		{ScopeOrg, "acme", "GIT_PAT", "", false},
	} {
		if v, found := s.get(s.c, tc.scope, tc.id, tc.key); v != tc.want || found != tc.wantFound {
			t.Errorf("Get(%s, %s, %s) = %q, %v; want %q, %v", tc.scope, tc.id, tc.key, v, found, tc.want, tc.wantFound)
		}
	}
	// What Get returned, the scrubber now removes from text that leaves the hub.
	if got := s.scrub.Scrub("fetch failed for tok-4be1c0ffee-2d9a"); got != "fetch failed for [redacted]" {
		t.Errorf("Scrub = %q", got)
	}

	if err := s.tx(true, func(tx *sql.Tx) error {
		_, err := s.c.UpdateEntry(s.ctx, tx, ScopeWorkspace, "acme", "GIT_PAT", "b")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.get(s.c, ScopeWorkspace, "acme", "GIT_PAT"); v != "b" {
		t.Errorf("Get after the update = %q, want b", v)
	}
}

func TestGet_FailsAnUnreadableValueWithoutReportingItAbsent(t *testing.T) {
	keyA, keyB := newKey(t), newKey(t)
	for _, tc := range []struct {
		name   string
		keys   Keys
		id     string
		damage func(s *storeTest)
	}{
		{name: "no key configured", keys: Keys{}, id: "acme"},
		{name: "another current key and no previous", keys: Keys{Current: keyB}, id: "acme"},
		{name: "one ciphertext byte flipped", keys: Keys{Current: keyA}, id: "acme", damage: func(s *storeTest) {
			var b []byte
			if err := s.st.Main().QueryRow(`SELECT sealed FROM secrets WHERE key = 'TOKEN'`).Scan(&b); err != nil {
				s.t.Fatal(err)
			}
			b[len(b)-1] ^= 1
			s.exec(`UPDATE secrets SET sealed = ? WHERE key = 'TOKEN'`, b)
		}},
		{name: "the sealed bytes of another entry", keys: Keys{Current: keyA}, id: "acme", damage: func(s *storeTest) {
			s.exec(`UPDATE secrets SET sealed = (SELECT sealed FROM secrets WHERE key = 'OTHER') WHERE key = 'TOKEN'`)
		}},
		{name: "the row moved to another scope id", keys: Keys{Current: keyA}, id: "elsewhere", damage: func(s *storeTest) {
			s.exec(`UPDATE secrets SET scope_id = 'elsewhere' WHERE key = 'TOKEN'`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStoreTest(t, Keys{Current: keyA})
			s.put(ScopeWorkspace, "acme", "TOKEN", "tok-value-1234567890")
			s.put(ScopeWorkspace, "acme", "OTHER", "other-value-0987654321")
			if tc.damage != nil {
				tc.damage(s)
			}
			v, found, err := s.component(tc.keys).Get(s.ctx, ScopeWorkspace, tc.id, "token")
			if !errors.Is(err, ErrUnreadable) {
				t.Fatalf("Get error = %v, want ErrUnreadable", err)
			}
			if err.Error() != `secret "TOKEN" cannot be decrypted` {
				t.Errorf("message = %q", err.Error())
			}
			if v != "" || !found {
				t.Errorf("Get = %q, found %v; want an empty value that is not reported absent", v, found)
			}
		})
	}
}

func TestPut_StoresInTheCallersTransaction(t *testing.T) {
	s := newStoreTest(t, Keys{Current: newKey(t)})
	if err := s.tx(false, func(tx *sql.Tx) error { return s.c.Put(s.ctx, tx, ScopeWorkspace, "w", "GIT_PAT", "p") }); err != nil {
		t.Fatal(err)
	}
	if _, found := s.get(s.c, ScopeWorkspace, "w", "GIT_PAT"); found {
		t.Fatal("the secret is there after a rollback")
	}
	s.put(ScopeWorkspace, "w", "GIT_PAT", "p")
	if v, _ := s.get(s.c, ScopeWorkspace, "w", "GIT_PAT"); v != "p" {
		t.Fatalf("Get = %q, want p", v)
	}
	created := s.now
	s.now = s.now.Add(time.Minute)
	s.put(ScopeWorkspace, "w", "git_pat", "q")

	items, err := s.c.ListPage(s.ctx, ScopeWorkspace, "w", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{{Key: "GIT_PAT", CreatedAt: created, UpdatedAt: s.now}}
	if fmt.Sprint(items) != fmt.Sprint(want) {
		t.Errorf("entries = %v, want %v", items, want)
	}
	if v, _ := s.get(s.c, ScopeWorkspace, "w", "GIT_PAT"); v != "q" {
		t.Errorf("Get = %q, want q", v)
	}
	s.noSecretEvents()
}

func TestPut_RefusesWithATypedErrorAndWritesNothing(t *testing.T) {
	key := newKey(t)
	for _, tc := range []struct {
		name   string
		keys   Keys
		stored int
		key    string
		value  string
		want   error
	}{
		{name: "no current key", keys: Keys{Previous: key}, key: "NEW", value: "v", want: ErrUnavailable},
		{name: "a key starting with a digit", keys: Keys{Current: key}, key: "1A", value: "v", want: ErrInvalid},
		{name: "a value over the limit", keys: Keys{Current: key}, key: "NEW", value: strings.Repeat("x", MaxValueSize+1), want: ErrInvalid},
		{name: "a new key in a full scope", keys: Keys{Current: key}, stored: MaxEntries, key: "NEW", value: "v", want: ErrLimitReached},
		{name: "an existing key in a full scope", keys: Keys{Current: key}, stored: MaxEntries, key: "k7", value: "v"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStoreTest(t, Keys{Current: key})
			s.fill(ScopeOrg, "acme", tc.stored)
			c := s.component(tc.keys)
			var err error
			if txErr := s.tx(true, func(tx *sql.Tx) error {
				err = c.Put(s.ctx, tx, ScopeOrg, "acme", tc.key, tc.value)
				return nil
			}); txErr != nil {
				t.Fatalf("the transaction around Put: %v", txErr)
			}
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("Put error = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				if n := s.count(ScopeOrg, "acme"); n != tc.stored {
					t.Errorf("the scope holds %d secrets after a refusal, want %d", n, tc.stored)
				}
			}
		})
	}

	if s := newStoreTest(t, Keys{Previous: key}); s.c.Available() {
		t.Error("Available() with no current key = true")
	}
	if s := newStoreTest(t, Keys{Current: key}); !s.c.Available() {
		t.Error("Available() with a current key = false")
	}
}

func TestDelete_RemovesTheNamedEntryWithoutOpeningIt(t *testing.T) {
	keyA := newKey(t)
	for _, tc := range []struct {
		name   string
		keys   Keys
		commit bool
	}{
		{name: "with the key", keys: Keys{Current: keyA}, commit: true},
		{name: "without a current key", keys: Keys{}, commit: true},
		{name: "on a value no key opens", keys: Keys{Current: newKey(t)}, commit: true},
		{name: "rolled back", keys: Keys{Current: keyA}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStoreTest(t, Keys{Current: keyA})
			s.put(ScopeWorkspace, "acme", "Forge_Webhook_Secret", "whsec")
			s.put(ScopeWorkspace, "acme", "OTHER", "o")
			c := s.component(tc.keys)
			del := func(commit bool) bool {
				t.Helper()
				var existed bool
				if err := s.tx(commit, func(tx *sql.Tx) (err error) {
					existed, err = c.Delete(s.ctx, tx, ScopeWorkspace, "acme", "FORGE_WEBHOOK_SECRET")
					return err
				}); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				return existed
			}

			if !del(tc.commit) {
				t.Error("Delete reported no entry")
			}
			if v, found := s.get(s.c, ScopeWorkspace, "acme", "FORGE_WEBHOOK_SECRET"); found != !tc.commit || (found && v != "whsec") {
				t.Errorf("Get after Delete = %q, %v", v, found)
			}
			if v, _ := s.get(s.c, ScopeWorkspace, "acme", "OTHER"); v != "o" {
				t.Errorf("the other secret reads %q", v)
			}
			if tc.commit && del(true) {
				t.Error("a second Delete reported an entry")
			}
			s.noSecretEvents()
		})
	}
}

func TestRemoveScope_DeletesOneScopeInTheCallersTransaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(s *storeTest, tx *sql.Tx) error
	}{
		{"RemoveScope", func(s *storeTest, tx *sql.Tx) error {
			return s.c.RemoveScope(s.ctx, tx, ScopeWorkspace, "acme-fork")
		}},
		{"RemoveWorkspace", func(s *storeTest, tx *sql.Tx) error { return RemoveWorkspace(s.ctx, tx, "acme-fork") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStoreTest(t, Keys{Current: newKey(t)})
			s.fill(ScopeWorkspace, "acme", 3)
			s.fill(ScopeWorkspace, "acme-fork", 2)
			s.fill(ScopeUser, "owner", 1)
			counts := func() [3]int {
				return [3]int{s.count(ScopeWorkspace, "acme"), s.count(ScopeWorkspace, "acme-fork"), s.count(ScopeUser, "owner")}
			}

			for _, step := range []struct {
				commit bool
				want   [3]int
			}{{false, [3]int{3, 2, 1}}, {true, [3]int{3, 0, 1}}} {
				if err := s.tx(step.commit, func(tx *sql.Tx) error { return tc.remove(s, tx) }); err != nil {
					t.Fatal(err)
				}
				if got := counts(); got != step.want {
					t.Errorf("commit %v: counts %v, want %v", step.commit, got, step.want)
				}
			}

			s.now = s.now.Add(time.Hour)
			s.put(ScopeWorkspace, "acme-fork", "K0", "new")
			items, err := s.c.ListPage(s.ctx, ScopeWorkspace, "acme-fork", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || !items[0].CreatedAt.Equal(s.now) {
				t.Errorf("entries after re-creating = %v, want one created at %v", items, s.now)
			}
			s.noSecretEvents()
		})
	}
}

func TestCreateEntries_StoresEachKeyOnceUnderConcurrentCreates(t *testing.T) {
	s := newStoreTest(t, Keys{Current: newKey(t)})
	race := func(scope Scope, id string, key func(i int) string) []error {
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() {
				errs[i] = s.st.Tx(s.ctx, func(tx *sql.Tx) error {
					_, err := s.c.CreateEntries(s.ctx, tx, scope, id, []Entry{{Key: key(i), Value: "v"}})
					return err
				})
			})
		}
		wg.Wait()
		return errs
	}
	tally := func(errs []error, refusal error) (ok, refused int) {
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, refusal):
				refused++
			default:
				t.Errorf("unexpected error %v", err)
			}
		}
		return ok, refused
	}

	if ok, refused := tally(race(ScopeOrg, "one", func(int) string { return "K" }), ErrConflict); ok != 1 || refused != 7 {
		t.Errorf("one key: %d created, %d conflicts; want 1 and 7", ok, refused)
	}
	s.fill(ScopeOrg, "full", MaxEntries-1)
	errs := race(ScopeOrg, "full", func(i int) string { return fmt.Sprintf("NEW%d", i) })
	if ok, refused := tally(errs, ErrLimitReached); ok != 1 || refused != 7 {
		t.Errorf("distinct keys: %d created, %d refused; want 1 and 7", ok, refused)
	}
	if n := s.count(ScopeOrg, "full"); n != MaxEntries {
		t.Errorf("the scope holds %d secrets, want %d", n, MaxEntries)
	}
}
