package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// uuidV4 is a version 4 UUID in lower case.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestEmit_CommitsWithStateChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clock.Set(time.Date(2026, 11, 14, 11, 0, 7, 600_000_000, time.FixedZone("CET", 3600)))
	ctx := context.Background()

	var got Record
	err := f.store.Tx(ctx, func(tx *sql.Tx) error {
		if err := insertRow(ctx, tx, "landed"); err != nil {
			return err
		}
		var err error
		got, err = f.events.Emit(ctx, tx, valid("parley.change.land"))
		return err
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}

	if got.Seq != 1 {
		t.Errorf("seq = %d, want 1", got.Seq)
	}
	if !uuidV4.MatchString(got.ID) {
		t.Errorf("id = %q, want a lower-case version 4 UUID", got.ID)
	}
	if want := time.Date(2026, 11, 14, 10, 0, 7, 0, time.UTC); !got.Time.Equal(want) || got.Time.Location() != time.UTC {
		t.Errorf("time = %v, want %v", got.Time, want)
	}
	want := Record{
		Seq: 1, ID: got.ID, Type: "parley.change.land", Time: got.Time, Workspace: ptr("api"),
		ActorType: "pat", ActorID: ptr("u_1"), ResourceType: "change", ResourceID: "42",
		Action: "land", Metadata: json.RawMessage(`{}`),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Emit = %+v, want %+v", got, want)
	}
	if recs := f.readAll(0); !reflect.DeepEqual(recs, []Record{want}) {
		t.Errorf("ReadAfter(0) = %+v, want exactly %+v", recs, want)
	}
	if !f.hasRow("landed") {
		t.Error("the state change of the transaction is missing")
	}

	second := f.emit(valid("parley.change.land"))
	if second.Seq != 2 {
		t.Errorf("second seq = %d, want 2", second.Seq)
	}
	if second.ID == got.ID {
		t.Errorf("second id = %q, the same as the first", second.ID)
	}
}

func TestEmit_RollbackLeavesNoEvent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fail func() error
	}{
		{"the function returns an error", func() error { return errors.New("refused") }},
		{"the function panics", func() error { panic("boom") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			first := f.emit(valid("parley.change.open"))

			err := f.store.Tx(ctx, func(tx *sql.Tx) error {
				if err := insertRow(ctx, tx, "rolled back"); err != nil {
					return err
				}
				if _, err := f.events.Emit(ctx, tx, valid("parley.change.land")); err != nil {
					return err
				}
				return tc.fail()
			})
			if err == nil {
				t.Fatal("transaction = nil error, want the failure")
			}

			if recs := f.readAll(0); !reflect.DeepEqual(recs, []Record{first}) {
				t.Errorf("ReadAfter(0) = %+v, want only the committed event", recs)
			}
			if got := f.lastSeq(); got != 1 {
				t.Errorf("last_seq = %d, want 1", got)
			}
			if f.hasRow("rolled back") {
				t.Error("the state change of the rolled-back transaction is present")
			}
			if next := f.emit(valid("parley.change.land")); next.Seq != 2 {
				t.Errorf("next committed seq = %d, want 2", next.Seq)
			}
		})
	}
}

func TestEmit_StoreFailureFailsTheRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.store.Main().Exec(`DROP TABLE hub_events`); err != nil {
		t.Fatal(err)
	}
	var emitErr error
	srv := server.New(server.Options{})
	srv.Register(server.Route{
		Method:  http.MethodPost,
		Pattern: "/test-emit",
		Public:  true,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			err := f.store.Tx(r.Context(), func(tx *sql.Tx) error {
				if err := insertRow(r.Context(), tx, "unreported"); err != nil {
					return err
				}
				_, emitErr = f.events.Emit(r.Context(), tx, valid("parley.change.land"))
				return emitErr
			})
			if err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
	})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/test-emit", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body %s)", rec.Code, rec.Body.Bytes())
	}
	var body struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.ErrorType != "internal" {
		t.Errorf("body = %s, want error_type internal", rec.Body.Bytes())
	}
	if emitErr == nil || errors.Is(emitErr, ErrInvalidEvent) {
		t.Errorf("Emit error = %v, want a store error that is not ErrInvalidEvent", emitErr)
	}
	if f.hasRow("unreported") {
		t.Error("the state change is present although its event could not be written")
	}
}

func TestEmitNow_WritesInOwnTransaction(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	at := time.Date(2026, 11, 14, 12, 34, 56, 0, time.UTC)
	f.clock.Set(at)
	in := valid("parley.git.fetch")
	in.ResourceType, in.ResourceID = "workspace", "api"

	got, err := f.events.EmitNow(context.Background(), in)
	if err != nil {
		t.Fatalf("EmitNow: %v", err)
	}
	if got.Seq != 1 || !got.Time.Equal(at) {
		t.Errorf("EmitNow = seq %d, time %v; want seq 1, time %v", got.Seq, got.Time, at)
	}
	if recs := f.readAll(0); !reflect.DeepEqual(recs, []Record{got}) {
		t.Errorf("ReadAfter(0) = %+v, want exactly %+v", recs, got)
	}
}

func TestEmit_SeveralInOneTransactionAreConsecutive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	f.emit(valid("parley.change.open"))

	var seqs []int64
	err := f.store.Tx(ctx, func(tx *sql.Tx) error {
		for _, ref := range []string{"refs/heads/main", "refs/heads/dev", "refs/tags/v1"} {
			in := valid("parley.git.push")
			in.ResourceType, in.ResourceID = "ref", ref
			rec, err := f.events.Emit(ctx, tx, in)
			if err != nil {
				return err
			}
			seqs = append(seqs, rec.Seq)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(seqs, want) {
		t.Errorf("seqs = %v, want %v", seqs, want)
	}
	var read []string
	for _, rec := range f.readAll(1) {
		read = append(read, rec.ResourceID)
	}
	if want := []string{"refs/heads/main", "refs/heads/dev", "refs/tags/v1"}; !reflect.DeepEqual(read, want) {
		t.Errorf("ReadAfter(1) refs = %v, want %v", read, want)
	}
}

func TestSeq_SurvivesRestartAndIsNeverReused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for range 3 {
		f.emit(valid("parley.change.open"))
	}

	f.reopen()
	if got := f.emit(valid("parley.change.open")).Seq; got != 4 {
		t.Errorf("seq after a restart = %d, want 4", got)
	}

	if _, err := f.store.Main().Exec(`DELETE FROM hub_events`); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if got := f.emit(valid("parley.change.open")).Seq; got != 5 {
		t.Errorf("seq after every event was deleted and a restart = %d, want 5", got)
	}
	if got := f.lastSeq(); got != 5 {
		t.Errorf("last_seq = %d, want 5", got)
	}
}

func TestEmit_ConcurrentWritersStayOrdered(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	const writers, perWriter = 20, 25

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				errs <- f.store.Tx(ctx, func(tx *sql.Tx) error {
					if _, err := f.events.Emit(ctx, tx, valid("parley.change.open")); err != nil {
						return err
					}
					// Some writers hold their transaction open after the
					// emit, so a reader runs while a seq is uncommitted.
					if (w+i)%5 == 0 {
						time.Sleep(time.Millisecond)
					}
					return nil
				})
			}
		})
	}
	writing := make(chan struct{})
	go func() {
		wg.Wait()
		close(writing)
	}()

	var longest int
	for reading := true; reading; {
		select {
		case <-writing:
			reading = false
		default:
		}
		got := seqs(f.readAll(0))
		for i, seq := range got {
			if seq != int64(i+1) {
				t.Fatalf("a read returned seqs %v, not a prefix of 1..n without a hole", got)
			}
		}
		if len(got) < longest {
			t.Fatalf("a read returned %d events after one returned %d", len(got), longest)
		}
		longest = len(got)
	}

	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a writer failed: %v", err)
		}
	}
	recs := f.readAll(0)
	ids := map[string]bool{}
	for i, rec := range recs {
		if rec.Seq != int64(i+1) {
			t.Fatalf("the log holds seqs %v, want 1..%d", seqs(recs), writers*perWriter)
		}
		ids[rec.ID] = true
	}
	if len(recs) != writers*perWriter || len(ids) != writers*perWriter {
		t.Errorf("the log holds %d events with %d distinct ids, want %d of each", len(recs), len(ids), writers*perWriter)
	}
	if got := f.events.Counters(); got.LastSeq != writers*perWriter || got.WrittenByType["parley.change.open"] != writers*perWriter {
		t.Errorf("Counters = %+v, want last_seq and parley.change.open both %d", got, writers*perWriter)
	}
}

func TestCounters_ReportSeqStreamsAndTypes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	f.emit(valid("parley.change.open"))
	f.emit(valid("parley.change.open"))
	f.emit(valid("parley.claim.create"))
	err := f.store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := f.events.Emit(ctx, tx, valid("parley.change.land")); err != nil {
			return err
		}
		return errors.New("refused")
	})
	if err == nil {
		t.Fatal("the rolled-back transaction committed")
	}
	if !f.events.takePlace(500) {
		t.Fatal("takePlace refused the first stream place")
	}

	want := Counters{
		LastSeq:       3,
		OpenStreams:   1,
		WrittenByType: map[string]int64{"parley.change.open": 2, "parley.claim.create": 1},
	}
	if got := f.events.Counters(); !reflect.DeepEqual(got, want) {
		t.Errorf("Counters = %+v, want %+v", got, want)
	}
	f.events.givePlace()
	if got := f.events.Counters().OpenStreams; got != 0 {
		t.Errorf("open_streams after the place was given back = %d, want 0", got)
	}
}

func TestOnCommit_NotifiesCommittedEventsInSeqOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	got := make(chan Record, 10)
	f.events.OnCommit(func(rec Record) { got <- rec })

	err := f.store.Tx(ctx, func(tx *sql.Tx) error {
		for _, typ := range []string{"parley.change.open", "parley.change.update"} {
			if _, err := f.events.Emit(ctx, tx, valid(typ)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := f.events.Emit(ctx, tx, valid("parley.change.abandon")); err != nil {
			return err
		}
		return errors.New("refused")
	})
	if err == nil {
		t.Fatal("the rolled-back transaction committed")
	}
	if _, err := f.events.EmitNow(ctx, valid("parley.change.land")); err != nil {
		t.Fatal(err)
	}

	var types []string
	for range 3 {
		select {
		case rec := <-got:
			if rec.Seq != int64(len(types)+1) {
				t.Errorf("notified seq %d after %v, want seq %d", rec.Seq, types, len(types)+1)
			}
			types = append(types, rec.Type)
		case <-time.After(5 * time.Second):
			t.Fatalf("notified %v after 5 s, want three events", types)
		}
	}
	if want := []string{"parley.change.open", "parley.change.update", "parley.change.land"}; !reflect.DeepEqual(types, want) {
		t.Errorf("notified types %v, want %v", types, want)
	}
	select {
	case rec := <-got:
		t.Errorf("notified %+v, which no transaction committed", rec)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWaitAfter_WakesOnTheNextCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	woken := func(ch <-chan struct{}, within time.Duration) bool {
		select {
		case <-ch:
			return true
		default:
		}
		select {
		case <-ch:
			return true
		case <-time.After(within):
			return false
		}
	}

	waiting := f.events.waitAfter(0)
	if _, err := f.events.EmitNow(ctx, valid("parley.change.open")); err != nil {
		t.Fatal(err)
	}
	if !woken(waiting, time.Second) {
		t.Fatal("a waiter after position 0 was not woken within 1 s of the commit of seq 1")
	}
	if !woken(f.events.waitAfter(0), 0) {
		t.Error("a waiter behind a commit already seen was not woken at once")
	}

	f.emit(valid("parley.change.update"))
	if !woken(f.events.waitAfter(1), time.Second) {
		t.Error("a waiter after position 1 was not woken by the commit of seq 2")
	}

	// Without a commit a waiter is still woken within a second, so a wake-up
	// the component missed delays an event by at most that.
	quiet := f.events.waitAfter(2)
	if woken(quiet, 50*time.Millisecond) {
		t.Fatal("a waiter at the last seq was woken without a commit at once")
	}
	if !woken(quiet, 1500*time.Millisecond) {
		t.Error("a waiter without a commit was not woken within a second")
	}
}
