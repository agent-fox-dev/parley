package sandbox

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/scrub"
)

// LogCollectInterval is the time between two rounds of the log collector on
// the hub's clock: a sandbox that became ready is read from within it, and
// one that left ready, or whose log stream broke, no later than it.
const LogCollectInterval = 5 * time.Second

// logState is what the service keeps for the log collector.
type logState struct {
	// stored is notified each time the collector stored lines.
	stored notifier
	// unsupported holds the ids of the sandboxes whose runtime answered
	// that it keeps no logs. That answer does not change, and the collector
	// asks again every round, so it is logged once per sandbox.
	unsupported sync.Map
}

// LogLinesStored returns a channel closed the next time the collector
// stores lines of any sandbox, for a read that follows a sandbox's logs.
func (s *Service) LogLinesStored() <-chan struct{} { return s.logs.stored.next() }

// CollectLogs stores the output of the main process and the policy log of
// every ready sandbox until ctx ends. It finds them by looking: right away
// and then every LogCollectInterval on the hub's clock it reads the
// sandboxes in state ready from the store, starts a follower for each that
// has none and stops the followers of the others. It needs no word from
// the code that moves a sandbox, and after a restart it takes up the
// sandboxes it read before by the same rule. It returns once every follower
// has stopped. With sandboxes off it returns at once.
func (s *Service) CollectLogs(ctx context.Context) {
	if s.client == nil || s.deps.Store == nil {
		return
	}
	followers := map[string]*logFollower{}
	defer func() {
		for _, f := range followers {
			f.cancel()
		}
		for _, f := range followers {
			<-f.done
		}
	}()
	for {
		// The timer of the next round is asked for before this one runs, so
		// that a round that takes long does not push the next one back.
		t := s.newTimer(LogCollectInterval)
		s.collectRound(ctx, followers)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		t.Stop()
	}
}

// logFollower is the follower of one sandbox: cancel stops it, and done is
// closed once it has stopped. ended is set as soon as one of its logs has
// ended, before it stops reading the other: from then on it is stopping by
// itself.
type logFollower struct {
	cancel context.CancelFunc
	done   chan struct{}
	ended  atomic.Bool
}

// collectRound is one round of CollectLogs over followers, the followers
// by sandbox id. A follower that stopped by itself, as when its stream
// broke, is started again when its sandbox is still ready.
func (s *Service) collectRound(ctx context.Context, followers map[string]*logFollower) {
	if ctx.Err() != nil {
		return
	}
	recs, err := ListByState(ctx, s.deps.Store.Main(), StateReady)
	if err != nil {
		if ctx.Err() == nil {
			s.logger().Warn("the log collector cannot read the ready sandboxes", "error", s.scrub(err.Error()))
		}
		return
	}
	ready := make(map[string]bool, len(recs))
	for _, rec := range recs {
		ready[rec.ID] = true
	}
	for id, f := range followers {
		// The follower of a sandbox no longer ready is waited for, so that a
		// sandbox that is ready again by the next round never has two
		// followers. One that is stopping by itself is waited for too, so
		// that a stream that broke before this round is opened again by it
		// and not only by the next one.
		if !ready[id] || f.ended.Load() {
			f.cancel()
			<-f.done
			delete(followers, id)
		}
	}
	for _, rec := range recs {
		if followers[rec.ID] != nil {
			continue
		}
		fctx, cancel := context.WithCancel(ctx)
		f := &logFollower{cancel: cancel, done: make(chan struct{})}
		followers[rec.ID] = f
		go func() {
			defer close(f.done)
			defer cancel()
			s.followLogs(fctx, rec.ID, &f.ended)
		}()
	}
}

// followLogs reads both logs of the sandbox id until ctx ends or one of
// them ends: ended is set then, the other is stopped too, and the next
// round of the collector opens both again.
func (s *Service) followLogs(ctx context.Context, id string, ended *atomic.Bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sources := []string{LogSourceProcess, LogSourcePolicy}
	errs := make(chan error, len(sources))
	for _, source := range sources {
		go func() {
			err := s.followLog(ctx, id, source)
			ended.Store(true)
			cancel()
			errs <- err
		}()
	}
	for range sources {
		err := <-errs
		if err == nil || errors.Is(err, context.Canceled) {
			continue
		}
		if errors.Is(err, runtime.ErrUnsupported) {
			if _, logged := s.logs.unsupported.LoadOrStore(id, true); logged {
				continue
			}
		}
		text := s.scrub(s.ScrubSandbox(context.WithoutCancel(ctx), id, err.Error()))
		s.logger().Warn("stopped reading a sandbox's log", "sandbox_id", id, "error", text)
	}
}

// followLog stores the lines of the source of the sandbox id's log until
// the stream ends: it asks the runtime for the lines from the time of the
// last stored line of that source and leaves out the lines of that second
// already stored, so that no line is stored twice. Lines older than the
// retention are not asked for, since the retention would delete them.
func (s *Service) followLog(ctx context.Context, id, source string) error {
	since, skip, err := logResume(ctx, s.deps.Store.Main(), id, source)
	if err != nil {
		return err
	}
	if keep := s.settings.LogRetention; keep > 0 {
		if floor := s.deps.Now().Add(-keep).UTC().Truncate(time.Second); since.Before(floor) {
			since, skip = floor, 0
		}
	}
	stream, err := s.client.Logs(ctx, id, runtime.LogQuery{Source: runtime.LogSource(source), Since: since, Follow: true})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		e, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if e.Time.IsZero() {
			e.Time = s.deps.Now()
		}
		at := e.Time.UTC().Truncate(time.Second)
		if at.Before(since) {
			continue
		}
		v, err := s.ValuesOf(ctx, id)
		if err != nil {
			return err
		}
		lines := logLinesOf(source, e, at, v)
		if at.After(since) {
			skip = 0
		}
		n := min(skip, len(lines))
		lines, skip = lines[n:], skip-n
		if len(lines) == 0 {
			continue
		}
		if err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return insertLogLines(ctx, tx, id, lines) }); err != nil {
			return err
		}
		s.logs.stored.notify()
		// The main process's output is use of its sandbox; the policy log
		// is not.
		if source == LogSourceProcess {
			s.touch(ctx, id, s.deps.Now())
		}
	}
}

// logLinesOf is the lines the entry e of the source stores at the time at,
// with every exact match of a value of v that Scrub replaces (an exposed
// secret's of at least MinExposedLen bytes among them) replaced by
// scrub.Marker in every text. Unlike Scrub it does not cut the text: a
// process entry is split at its line ends, and every line longer than
// MaxLogLine into pieces. A policy entry without a text gets one written
// from its fields.
func logLinesOf(source string, e runtime.LogEntry, at time.Time, v Values) []LogLine {
	redact := redactor(v)
	var out []LogLine
	if source == LogSourcePolicy {
		text := e.Text
		if text == "" {
			text = policyText(e)
		}
		l := LogLine{
			Time: at, Source: LogSourcePolicy, Action: redact(e.Action), Program: redact(e.Program),
			Host: redact(e.Host), Port: e.Port, Credential: redact(e.Provider),
		}
		for _, piece := range cutLine(redact(text)) {
			l.Line = piece
			out = append(out, l)
		}
		return out
	}
	for _, line := range splitLines(redact(e.Text)) {
		for _, piece := range cutLine(line) {
			out = append(out, LogLine{Time: at, Source: LogSourceProcess, Stream: string(e.Stream), Line: piece})
		}
	}
	return out
}

// redactor returns the function that replaces every exact match of a value
// of v that Scrub replaces by scrub.Marker, longer values first.
func redactor(v Values) func(string) string {
	values := slices.DeleteFunc(v.list(), func(s string) bool { return s == "" })
	if len(values) == 0 {
		return func(s string) string { return s }
	}
	slices.SortFunc(values, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	pairs := make([]string, 0, 2*len(values))
	for _, value := range values {
		pairs = append(pairs, value, scrub.Marker)
	}
	return strings.NewReplacer(pairs...).Replace
}

// policyText is the line of a policy entry the runtime gave no text for:
// its action, host and port and program, then its method and path when it
// has them.
func policyText(e runtime.LogEntry) string {
	parts := []string{e.Action, e.Host + ":" + strconv.Itoa(e.Port), e.Program}
	for _, p := range []string{e.Method, e.Path} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// splitLines splits a process entry's text at its line ends, "\n" or
// "\r\n"; a trailing line end ends the last line and starts none.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i := range lines {
		if i < len(lines)-1 || strings.HasSuffix(text, "\n") {
			lines[i] = strings.TrimSuffix(lines[i], "\r")
		}
	}
	return lines
}

// cutLine splits line into consecutive pieces of at most MaxLogLine bytes,
// each cut before a UTF-8 character it would split; text that is not UTF-8
// is cut at MaxLogLine.
func cutLine(line string) []string {
	var out []string
	for len(line) > MaxLogLine {
		n := MaxLogLine
		for i := n; i > n-4 && i > 0; i-- {
			if line[i]&0xC0 != 0x80 {
				n = i
				break
			}
		}
		out = append(out, line[:n])
		line = line[n:]
	}
	return append(out, line)
}
