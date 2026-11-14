package event

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestLog_ReturnsAPage(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emitMany(10)
	w.clock.Advance(time.Hour)
	w.emitMany(110)
	if oldest, err := w.events.Expire(context.Background(), w.clock.Now().Add(-time.Minute)); err != nil || oldest != 11 {
		t.Fatalf("Expire = %d, %v, want 11", oldest, err)
	}
	stored := w.logJSON(10)
	dev := w.key("dev")

	first := w.getLog(t, dev, "")
	got := first.seqs(t)
	if want := seqRange(11, 60); !slices.Equal(got, want) {
		t.Errorf("first page seqs = %v, want %v", got, want)
	}
	if first.page.OldestSeq != 11 || first.page.LastSeq != 120 {
		t.Errorf("first page bounds = %d, %d, want 11, 120", first.page.OldestSeq, first.page.LastSeq)
	}
	for i, item := range first.page.Items {
		if string(item) != stored[got[i]] {
			t.Errorf("item %d = %s, want the stored record %s", got[i], item, stored[got[i]])
		}
	}

	if got := w.getLog(t, dev, "after=118").seqs(t); !slices.Equal(got, []int64{119, 120}) {
		t.Errorf("after=118: seqs = %v, want [119 120]", got)
	}
	if last := w.getLog(t, dev, "after=120"); !bytes.Contains(last.body, []byte(`"items":[]`)) {
		t.Errorf("after=120: body = %s, want items == []", last.body)
	}
}

func TestLog_NextAfterNeverSkipsOrRepeats(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var api []int64
	for i := range 26 {
		if i%2 == 1 {
			w.emit(on("parley.change.open", "web"))
			continue
		}
		api = append(api, w.emit(on("parley.change.open", "api")).Seq)
	}
	mem := w.key("mem")

	var collected []int64
	for after, pages := int64(0), 0; ; pages++ {
		if pages > 10 {
			t.Fatalf("still paging after 10 pages, collected %v", collected)
		}
		r := w.getLog(t, mem, "limit=5&after="+strconv.FormatInt(after, 10))
		got := r.seqs(t)
		collected = append(collected, got...)
		if len(got) < 5 {
			if r.page.NextCursor != nil || r.page.NextAfter != 26 || r.page.LastSeq != 26 || len(got) == 0 || got[len(got)-1] != 25 {
				t.Errorf("last page %v: next_cursor = %v, next_after = %d, last_seq = %d, want it to end at 25 with null, 26, 26",
					got, r.page.NextCursor, r.page.NextAfter, r.page.LastSeq)
			}
			break
		}
		want := strconv.FormatInt(got[len(got)-1], 10)
		if r.page.NextCursor == nil || *r.page.NextCursor != want || r.page.NextAfter != got[len(got)-1] {
			t.Errorf("full page %v: next_cursor = %v, next_after = %d, want %s for both", got, r.page.NextCursor, r.page.NextAfter, want)
		}
		after = r.page.NextAfter
	}
	if !slices.Equal(collected, api) {
		t.Errorf("collected %v, want the 13 events of api once each in order: %v", collected, api)
	}

	none := w.getLog(t, mem, "types=parley.nothing.*")
	if len(none.page.Items) != 0 || none.page.NextAfter != 26 || !bytes.Contains(none.body, []byte(`"items":[]`)) {
		t.Errorf("types=parley.nothing.*: body = %s, want items == [] and next_after == 26", none.body)
	}
}

func TestLog_CursorIsAnotherNameForAfter(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emitMany(10)
	dev := w.key("dev")

	byAfter := w.getLog(t, dev, "after=4")
	if got := byAfter.seqs(t); len(got) == 0 || got[0] != 5 {
		t.Errorf("after=4: seqs = %v, want them to start at 5", got)
	}
	for _, query := range []string{"cursor=4", "after=4&cursor=4"} {
		if got := w.getLog(t, dev, query); !bytes.Equal(got.body, byAfter.body) {
			t.Errorf("%s: body = %s, want the body of after=4, %s", query, got.body, byAfter.body)
		}
	}
}

func TestLog_LongPoll(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emitMany(1)
	mem := w.key("mem")

	t.Run("an event that arrives ends the wait", func(t *testing.T) {
		held := w.holdLog(mem, "wait=30&after="+strconv.FormatInt(w.bounds().LastSeq, 10))
		w.clock.WaitTimers(t, 1)
		rec := w.emit(on("parley.change.open", "api"))
		if r := answerWithin(t, time.Second, held); r.status != http.StatusOK || !slices.Equal(r.seqs(t), []int64{rec.Seq}) {
			t.Errorf("answer = %d %s, want 200 with the event %d", r.status, r.body, rec.Seq)
		}
	})

	t.Run("events the caller may not see or the filters exclude do not end it", func(t *testing.T) {
		after := w.bounds().LastSeq
		held := w.holdLog(mem, "wait=30&types=parley.claim.*&after="+strconv.FormatInt(after, 10))
		w.clock.WaitTimers(t, 1)
		w.emit(on("parley.change.open", "web"))
		w.emit(on("parley.change.open", "api"))
		select {
		case r := <-held:
			t.Fatalf("answered %d %s while nothing visible and matching was committed", r.status, r.body)
		case <-time.After(300 * time.Millisecond):
		}
		w.clock.Advance(30 * time.Second)
		r := answerWithin(t, time.Second, held)
		if r.status != http.StatusOK || len(r.page.Items) != 0 || r.page.NextAfter != after+2 || r.page.LastSeq != after+2 {
			t.Errorf("answer = %d %s, want 200, items == [] and next_after == last_seq == %d", r.status, r.body, after+2)
		}
	})

	t.Run("wait=0 answers at once", func(t *testing.T) {
		after := w.bounds().LastSeq
		r := w.getLog(t, mem, "wait=0&after="+strconv.FormatInt(after, 10))
		if !bytes.Contains(r.body, []byte(`"items":[]`)) || r.page.NextAfter != after {
			t.Errorf("body = %s, want items == [] and next_after == %d", r.body, after)
		}
	})
}

func TestLog_ShutdownReleasesLongPolls(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emitMany(1)
	dev := w.key("dev")

	first, second := w.holdLog(dev, "wait=60&after=1"), w.holdLog(dev, "wait=60&after=1")
	w.clock.WaitTimers(t, 2)
	w.shutDown()
	for i, held := range []<-chan answer{first, second} {
		if r := answerWithin(t, time.Second, held); r.status != http.StatusOK || !bytes.Contains(r.body, []byte(`"items":[]`)) {
			t.Errorf("request %d: answer = %d %s, want 200 with items == []", i+1, r.status, r.body)
		}
	}
}

func TestLog_UsesCurrentAccess(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	out := w.key("out")

	e1 := w.emit(on("parley.change.open", "api"))
	if got := w.getLog(t, out, "after=0").seqs(t); len(got) != 0 {
		t.Fatalf("out without access: seqs = %v, want none", got)
	}
	w.addMember(w.acme, "out")
	e2 := w.emit(on("parley.change.open", "api"))
	if got, want := w.getLog(t, out, "after=0").seqs(t), []int64{e1.Seq, e2.Seq}; !slices.Equal(got, want) {
		t.Errorf("out after joining acme: seqs = %v, want the earlier and the later event, %v", got, want)
	}
	w.removeMember(w.acme, "out")
	w.emitMany(1)
	if got := w.getLog(t, out, "after=0").seqs(t); len(got) != 0 {
		t.Errorf("out after leaving acme: seqs = %v, want none", got)
	}
}
