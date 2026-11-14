package event

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"
)

// logAnswer is the body of a page of GET /events/log.
type logAnswer struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor *string           `json:"next_cursor"`
	NextAfter  int64             `json:"next_after"`
	OldestSeq  int64             `json:"oldest_seq"`
	LastSeq    int64             `json:"last_seq"`
}

// logReply is an answer of GET /events/log: its status, its body decoded
// and as sent.
type logReply struct {
	status int
	page   logAnswer
	raw    []byte
}

// seqs are the seqs of the page's items, in their order.
func (r logReply) seqs(t testing.TB) []int64 {
	t.Helper()
	var out []int64
	for _, item := range r.page.Items {
		var it struct{ Seq int64 }
		if err := json.Unmarshal(item, &it); err != nil {
			t.Fatalf("item %s: %v", item, err)
		}
		out = append(out, it.Seq)
	}
	return out
}

// holdLog sends GET /events/log?query with credential and returns the
// channel its answer arrives on, so that a test can hold the request.
func (w *world) holdLog(credential, query string) <-chan logReply {
	out := make(chan logReply, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events/log?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+credential)
		rec := httptest.NewRecorder()
		w.srv.Handler().ServeHTTP(rec, req)
		reply := logReply{status: rec.Code, raw: rec.Body.Bytes()}
		if rec.Code == http.StatusOK {
			_ = json.Unmarshal(reply.raw, &reply.page)
		}
		out <- reply
	}()
	return out
}

// answerWithin waits up to d of real time for the answer of a held request
// and fails the test when it does not arrive.
func answerWithin(t testing.TB, d time.Duration, held <-chan logReply) logReply {
	t.Helper()
	select {
	case r := <-held:
		return r
	case <-time.After(d):
		t.Fatalf("no answer within %v", d)
		return logReply{}
	}
}

// getLog requests GET /events/log?query with credential and fails the test
// unless it is answered 200 within 2 s, which a request that is not meant
// to wait always is: the fake clock does not move by itself.
func (w *world) getLog(t testing.TB, credential, query string) logReply {
	t.Helper()
	r := answerWithin(t, 2*time.Second, w.holdLog(credential, query))
	if r.status != http.StatusOK {
		t.Fatalf("GET /events/log?%s = %d %s, want 200", query, r.status, r.raw)
	}
	return r
}

func TestLog_ReturnsAPage(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for range 10 {
		w.emit(on("parley.change.open", "api"))
	}
	w.clock.Advance(time.Hour)
	for range 110 {
		w.emit(on("parley.change.open", "api"))
	}
	if oldest, err := w.events.Expire(context.Background(), w.clock.Now().Add(-time.Minute)); err != nil || oldest != 11 {
		t.Fatalf("Expire = %d, %v, want 11", oldest, err)
	}
	stored := map[int64][]byte{}
	for _, rec := range w.readAll(0) {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		stored[rec.Seq] = raw
	}
	dev := w.key("dev")

	first := w.getLog(t, dev, "")
	if got, want := first.seqs(t), seqRange(11, 60); !slices.Equal(got, want) {
		t.Errorf("first page seqs = %v, want %v", got, want)
	}
	if first.page.OldestSeq != 11 || first.page.LastSeq != 120 {
		t.Errorf("first page bounds = %d, %d, want 11, 120", first.page.OldestSeq, first.page.LastSeq)
	}
	for _, item := range first.page.Items {
		var it struct{ Seq int64 }
		_ = json.Unmarshal(item, &it)
		if !bytes.Equal(item, stored[it.Seq]) {
			t.Errorf("item %d = %s, want the stored record %s", it.Seq, item, stored[it.Seq])
		}
	}

	if got := w.getLog(t, dev, "after=118").seqs(t); !slices.Equal(got, []int64{119, 120}) {
		t.Errorf("after=118: seqs = %v, want [119 120]", got)
	}
	last := w.getLog(t, dev, "after=120")
	if !bytes.Contains(last.raw, []byte(`"items":[]`)) {
		t.Errorf("after=120: body = %s, want items == []", last.raw)
	}
}

// seqRange is from, from+1, ..., to.
func seqRange(from, to int64) []int64 {
	var out []int64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func TestLog_NextAfterNeverSkipsOrRepeats(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i := range 26 {
		slug := "api"
		if i%2 == 1 {
			slug = "web"
		}
		w.emit(on("parley.change.open", slug))
	}
	mem := w.key("mem")

	var collected []int64
	after := int64(0)
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatalf("still paging after 10 pages, collected %v", collected)
		}
		r := w.getLog(t, mem, "limit=5&after="+strconv.FormatInt(after, 10))
		got := r.seqs(t)
		collected = append(collected, got...)
		if len(got) < 5 {
			if r.page.NextCursor != nil || r.page.NextAfter != 26 || r.page.NextAfter != r.page.LastSeq {
				t.Errorf("last page: next_cursor = %v, next_after = %d, last_seq = %d, want null, 26, 26",
					r.page.NextCursor, r.page.NextAfter, r.page.LastSeq)
			}
			if len(got) == 0 || got[len(got)-1] != 25 {
				t.Errorf("last page seqs = %v, want it to end at 25", got)
			}
			break
		}
		want := strconv.FormatInt(got[len(got)-1], 10)
		if r.page.NextCursor == nil || *r.page.NextCursor != want || r.page.NextAfter != got[len(got)-1] {
			t.Errorf("full page %v: next_cursor = %v, next_after = %d, want %s for both", got, r.page.NextCursor, r.page.NextAfter, want)
		}
		after = r.page.NextAfter
	}
	var odd []int64
	for s := int64(1); s <= 26; s += 2 {
		odd = append(odd, s)
	}
	if !slices.Equal(collected, odd) {
		t.Errorf("collected %v, want the 13 events of api once each in order: %v", collected, odd)
	}

	none := w.getLog(t, mem, "types=parley.nothing.*")
	if len(none.page.Items) != 0 || none.page.NextAfter != 26 || !bytes.Contains(none.raw, []byte(`"items":[]`)) {
		t.Errorf("types=parley.nothing.*: body = %s, want items == [] and next_after == 26", none.raw)
	}
}

func TestLog_CursorIsAnotherNameForAfter(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for range 10 {
		w.emit(on("parley.change.open", "api"))
	}
	dev := w.key("dev")

	byAfter := w.getLog(t, dev, "after=4")
	if got := byAfter.seqs(t); len(got) == 0 || got[0] != 5 {
		t.Errorf("after=4: seqs = %v, want them to start at 5", got)
	}
	for _, query := range []string{"cursor=4", "after=4&cursor=4"} {
		if got := w.getLog(t, dev, query); !bytes.Equal(got.raw, byAfter.raw) {
			t.Errorf("%s: body = %s, want the body of after=4, %s", query, got.raw, byAfter.raw)
		}
	}
}

func TestLog_RejectsBadParameters(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// Events to return, so that an accepted wait answers at once.
	for range 4 {
		w.emit(on("parley.change.open", "api"))
	}
	dev := w.key("dev")

	for _, tc := range []struct {
		query string
		ok    bool
	}{
		{"limit=0", false},
		{"limit=201", false},
		{"limit=x", false},
		{"wait=-1", false},
		{"wait=61", false},
		{"wait=1.5", false},
		{"wait=x", false},
		{"after=3&cursor=4", false},
		{"limit=1", true},
		{"limit=200", true},
		{"wait=0", true},
		{"wait=60", true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := answerWithin(t, 2*time.Second, w.holdLog(dev, tc.query))
			if tc.ok {
				if r.status != http.StatusOK {
					t.Errorf("status = %d %s, want 200", r.status, r.raw)
				}
				return
			}
			var env struct {
				Error struct {
					ErrorType string `json:"error_type"`
				} `json:"error"`
			}
			_ = json.Unmarshal(r.raw, &env)
			if r.status != http.StatusBadRequest || env.Error.ErrorType != "invalid_request" {
				t.Errorf("answer = %d %s, want 400 invalid_request", r.status, r.raw)
			}
		})
	}
}

func TestLog_LongPoll(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emit(on("parley.change.open", "api"))
	mem := w.key("mem")

	t.Run("an event that arrives ends the wait", func(t *testing.T) {
		after := w.lastSeq()
		held := w.holdLog(mem, "wait=30&after="+strconv.FormatInt(after, 10))
		w.clock.WaitTimers(t, 1)
		rec := w.emit(on("parley.change.open", "api"))
		r := answerWithin(t, time.Second, held)
		if r.status != http.StatusOK || !slices.Equal(r.seqs(t), []int64{rec.Seq}) {
			t.Errorf("answer = %d %s, want 200 with the event %d", r.status, r.raw, rec.Seq)
		}
	})

	t.Run("events the caller may not see or the filters exclude do not end it", func(t *testing.T) {
		after := w.lastSeq()
		held := w.holdLog(mem, "wait=30&types=parley.claim.*&after="+strconv.FormatInt(after, 10))
		w.clock.WaitTimers(t, 1)
		w.emit(on("parley.change.open", "web"))
		w.emit(on("parley.change.open", "api"))
		select {
		case r := <-held:
			t.Fatalf("answered %d %s while nothing visible and matching was committed", r.status, r.raw)
		case <-time.After(300 * time.Millisecond):
		}
		w.clock.Advance(30 * time.Second)
		r := answerWithin(t, time.Second, held)
		if r.status != http.StatusOK || len(r.page.Items) != 0 || r.page.NextAfter != after+2 || r.page.LastSeq != after+2 {
			t.Errorf("answer = %d %s, want 200, items == [] and next_after == last_seq == %d", r.status, r.raw, after+2)
		}
	})

	t.Run("wait=0 answers at once", func(t *testing.T) {
		after := w.lastSeq()
		r := w.getLog(t, mem, "wait=0&after="+strconv.FormatInt(after, 10))
		if !bytes.Contains(r.raw, []byte(`"items":[]`)) || r.page.NextAfter != after {
			t.Errorf("body = %s, want items == [] and next_after == %d", r.raw, after)
		}
	})
}

func TestLog_ShutdownReleasesLongPolls(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emit(on("parley.change.open", "api"))
	dev := w.key("dev")

	after := "wait=60&after=" + strconv.FormatInt(w.lastSeq(), 10)
	first, second := w.holdLog(dev, after), w.holdLog(dev, after)
	w.clock.WaitTimers(t, 2)
	w.shutDown()
	for i, held := range []<-chan logReply{first, second} {
		r := answerWithin(t, time.Second, held)
		if r.status != http.StatusOK || !bytes.Contains(r.raw, []byte(`"items":[]`)) {
			t.Errorf("request %d: answer = %d %s, want 200 with items == []", i+1, r.status, r.raw)
		}
	}
}
