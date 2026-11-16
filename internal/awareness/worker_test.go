package awareness_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/event"
)

func TestWorker_IndexesAnOpenedChangeOnlyAfterAPass(t *testing.T) {
	t.Parallel()
	h := newChangeHub(t)
	number := h.open(t, "feat", map[string]string{"api/a.go": "feat\n"})
	ctx := context.Background()
	if got, err := h.comp.InFlight(ctx, nil, "demo", "main"); err != nil || len(got) != 0 {
		t.Fatalf("in flight before a pass = %v, %v; want none", got, err)
	}

	if err := h.comp.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := h.comp.InFlight(ctx, nil, "demo", "main"); err != nil || !slices.Equal(got, []int64{number}) {
		t.Errorf("in flight after a pass = %v, %v; want [%d]", got, err, number)
	}
	if got, err := h.comp.Files(ctx, nil, "demo", number); err != nil || !slices.Equal(got, []string{"api/a.go"}) {
		t.Errorf("files = %v, %v; want [api/a.go]", got, err)
	}
}

func TestWorker_AppliesEachEventOnceAcrossARestartAndAFailedCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := commit(t, f.repo, "main", "", map[string]string{"a.go": "a\n", "b.go": "b\n"})
	var versions []awareness.Change
	for i, files := range []map[string]string{
		{"a.go": "1\n"}, {"b.go": "1\n"}, {"a.go": "2\n", "c.go": "c\n"}, {"b.go": "2\n", "d.go": "d\n"}, {"e.go": "e\n"},
	} {
		head := commit(t, f.repo, "v", base, files)
		versions = append(versions, awareness.Change{Number: int64(i%2 + 1), Version: int64(i/2 + 1), BaseSHA: &base, HeadSHA: head})
	}

	whole := newFixtureOn(t, f.repo)
	for _, v := range versions {
		whole.version(v)
	}
	whole.pass()

	f.version(versions[0])
	f.version(versions[1])
	f.pass()
	f.restart()
	var fifth int64
	for _, v := range versions[2:] {
		fifth = f.version(v)
	}
	f.pass()
	f.pass()

	if got := f.position(); got != fifth {
		t.Errorf("position = %d, want %d, the fifth event's", got, fifth)
	}
	if got, want := f.dump(), whole.dump(); !slices.Equal(got, want) {
		t.Errorf("index after a restart = %v, want that of a run without it, %v", got, want)
	}
	if got := f.changes.readCount(); got != 5 {
		t.Errorf("the change reader was asked %d times, want 5, once per event", got)
	}

	before := f.dump()
	head := commit(t, f.repo, "v", base, map[string]string{"f.go": "f\n"})
	sixth := f.version(awareness.Change{Number: 1, Version: 4, BaseSHA: &base, HeadSHA: head})
	awareness.FailNextCommit(f.comp, errors.New("disk I/O error"))
	if err := f.comp.Pass(context.Background()); err == nil {
		t.Error("a pass whose transaction failed returned no error")
	}
	if got := f.position(); got != fifth {
		t.Errorf("position after a failed transaction = %d, want %d", got, fifth)
	}
	if got := f.dump(); !slices.Equal(got, before) {
		t.Errorf("index after a failed transaction = %v, want it unchanged, %v", got, before)
	}

	f.pass()
	if got := f.position(); got != sixth {
		t.Errorf("position after the next pass = %d, want %d", got, sixth)
	}
	if got := f.files(1); !slices.Equal(got, []string{"f.go"}) {
		t.Errorf("files of 1 after the next pass = %v, want [f.go]", got)
	}
}

// readWatch is the hub's event log as the index worker reads it, a read at
// the start of each pass. Each read first runs probe, when one is set, so
// that probe sees the index as the pass before left it; while hold is set,
// a read waits for the worker's context to be done instead, and closes
// holding as it begins.
type readWatch struct {
	awareness.EventSource
	mu      sync.Mutex
	probe   func()
	hold    bool
	holding chan struct{}
	starts  []time.Time
}

func (w *readWatch) ReadAfter(ctx context.Context, after int64, limit int) ([]event.Record, int64, error) {
	w.mu.Lock()
	w.starts = append(w.starts, time.Now())
	probe, hold := w.probe, w.hold
	if hold {
		w.hold = false
		close(w.holding)
	}
	w.mu.Unlock()
	if hold {
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	if probe != nil {
		probe()
	}
	return w.EventSource.ReadAfter(ctx, after, limit)
}

// setProbe makes probe run at the start of each read from now on.
func (w *readWatch) setProbe(probe func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.probe = probe
}

// startedAfter counts the reads that started after at.
func (w *readWatch) startedAfter(at time.Time) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, s := range w.starts {
		if s.After(at) {
			n++
		}
	}
	return n
}

// watched is a change hub whose index worker reads the event log through
// w, a pass every interval.
func watched(t *testing.T, w *readWatch, interval time.Duration) *changeHub {
	t.Helper()
	return newChangeHubWith(t, nil, func(d *awareness.Deps) {
		w.EventSource = d.Events
		d.Events, d.IndexInterval = w, interval
	})
}

func TestWorker_IndexesARecordedVersionWithinOneIntervalAndOnePass(t *testing.T) {
	t.Parallel()
	// slack is how late a loaded machine may wake the worker after its tick.
	const interval, slack = 200 * time.Millisecond, 200 * time.Millisecond
	w := &readWatch{}
	h := watched(t, w, interval)
	number := h.open(t, "feat", map[string]string{"api/a.go": "feat\n"})
	recorded := time.Now()

	// A pass's look is when it began, and whether the change was in the
	// index then, as the pass before left it.
	type look struct {
		at      time.Time
		indexed bool
	}
	looks := make(chan look, 2)
	w.setProbe(func() {
		got, err := h.comp.InFlight(context.Background(), nil, "demo", "main")
		select {
		case looks <- look{at: time.Now(), indexed: err == nil && slices.Contains(got, number)}:
		default:
		}
	})
	var first, second look
	for i, l := range []*look{&first, &second} {
		select {
		case *l = <-looks:
		case <-time.After(10 * time.Second):
			t.Fatalf("pass %d after the version was recorded did not begin within 10 s", i+1)
		}
	}

	if late := first.at.Sub(recorded); late > interval+slack {
		t.Errorf("the first pass after the version was recorded began %v later, want at most one interval, %v (+%v slack)",
			late, interval, slack)
	}
	if gap := second.at.Sub(first.at); gap > interval+slack {
		t.Errorf("the next pass began %v after it, want at most one interval, %v (+%v slack)", gap, interval, slack)
	}
	if !second.indexed {
		t.Errorf("change %d is not in the index once the first pass after its version was recorded ended", number)
	}
}

func TestWorker_RunsNoPassAfterTheHubStopped(t *testing.T) {
	t.Parallel()
	const interval = 20 * time.Millisecond
	w := &readWatch{hold: true, holding: make(chan struct{})}
	h := watched(t, w, interval)
	select {
	case <-w.holding:
	case <-time.After(10 * time.Second):
		t.Fatal("no pass began within 10 s")
	}

	h.Stop()
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0", code)
	}
	stopped := time.Now()
	if awareness.PassRunning(h.comp) {
		t.Error("the pass that was running when the hub stopped still runs after the stop returned")
	}
	time.Sleep(10 * interval)
	if n := w.startedAfter(stopped); n != 0 || awareness.PassRunning(h.comp) {
		t.Errorf("%d passes began in the ten intervals after the stop returned, and one runs: %v; want none",
			n, awareness.PassRunning(h.comp))
	}
}
