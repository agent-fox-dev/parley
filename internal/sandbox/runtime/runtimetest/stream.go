package runtimetest

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// pipe is the queue behind an open stream: what the fake produces is pushed
// in, the reader takes it out, and the stream ends once, with io.EOF or the
// error of a break. What was pushed before the end is still read.
type pipe[T any] struct {
	mu     sync.Mutex
	items  []T
	ending bool
	err    error
	signal chan struct{} // closed and replaced on every change
	done   chan struct{} // closed when the stream ended
	onEnd  []func()
}

func newPipe[T any]() *pipe[T] {
	return &pipe[T]{signal: make(chan struct{}), done: make(chan struct{})}
}

// push adds v unless the stream is ending, and reports whether it did.
func (p *pipe[T]) push(v T) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ending {
		return false
	}
	p.items = append(p.items, v)
	close(p.signal)
	p.signal = make(chan struct{})
	return true
}

// whenEnded runs fn when the stream ends, before a reader can see the end.
func (p *pipe[T]) whenEnded(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onEnd = append(p.onEnd, fn)
}

// end ends the stream with err, once; a later end does nothing. It must not
// be called with the fake's lock held, since what runs when the stream ends
// takes it.
func (p *pipe[T]) end(err error) {
	p.mu.Lock()
	if p.ending {
		p.mu.Unlock()
		return
	}
	p.ending = true
	fns := p.onEnd
	p.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
	close(p.signal)
	close(p.done)
}

// next returns the next item, blocking until there is one or the stream
// ended.
func (p *pipe[T]) next() (T, error) {
	for {
		p.mu.Lock()
		if len(p.items) > 0 {
			v := p.items[0]
			p.items = p.items[1:]
			p.mu.Unlock()
			return v, nil
		}
		if p.err != nil {
			err := p.err
			p.mu.Unlock()
			var zero T
			return zero, err
		}
		signal := p.signal
		p.mu.Unlock()
		<-signal
	}
}

// result waits until the stream ended and returns what it ended with.
func (p *pipe[T]) result() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// broken is the error a stream that BreakStream broke ends with.
func broken(op string) error {
	return runtime.NewError(runtime.ErrUnavailable, op, "the stream broke")
}

// observeRead moves the client's flag when a read ends the stream with a
// failure; io.EOF is no failure.
func (c *client) observeRead(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return c.reach.Observe(err)
}

type watch struct {
	c   *client
	out *pipe[runtime.SandboxEvent]
}

func (w *watch) Next() (runtime.SandboxEvent, error) {
	ev, err := w.out.next()
	return ev, w.c.observeRead(err)
}

func (w *watch) Close() error {
	w.out.end(io.EOF)
	return nil
}

type logStream struct {
	c   *client
	out *pipe[runtime.LogEntry]
}

func (l *logStream) Next() (runtime.LogEntry, error) {
	e, err := l.out.next()
	return e, l.c.observeRead(err)
}

func (l *logStream) Close() error {
	l.out.end(io.EOF)
	return nil
}

type execStream struct {
	c        *client
	out      *pipe[runtime.Output]
	finished chan struct{} // closed when the command returned
	code     int
	ended    bool // the command ended on its own
}

func (s *execStream) Next() (runtime.Output, error) {
	o, err := s.out.next()
	return o, s.c.observeRead(err)
}

func (s *execStream) ExitCode() (int, error) {
	<-s.finished
	if !s.ended {
		return -1, runtime.NewError(runtime.ErrUnavailable, runtime.OpExecStream, "the command did not end")
	}
	return s.code, nil
}

func (s *execStream) Close() error {
	s.out.end(io.EOF)
	return nil
}

// outputWriter delivers what a command writes to one of its streams.
type outputWriter struct {
	out    *pipe[runtime.Output]
	stream runtime.OutputStream
}

func (w outputWriter) Write(b []byte) (int, error) {
	if !w.out.push(runtime.Output{Stream: w.stream, Data: append([]byte(nil), b...)}) {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

// terminal echoes its input: what is written is read back.
type terminal struct {
	c    *client
	out  *pipe[[]byte]
	mu   sync.Mutex
	rest []byte
}

func (t *terminal) Read(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.rest) == 0 {
		chunk, err := t.out.next()
		if err != nil {
			return 0, t.c.observeRead(err)
		}
		t.rest = chunk
	}
	n := copy(b, t.rest)
	t.rest = t.rest[n:]
	return n, nil
}

func (t *terminal) Write(b []byte) (int, error) {
	if !t.out.push(append([]byte(nil), b...)) {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

func (t *terminal) Resize(runtime.TerminalSize) error {
	select {
	case <-t.out.done:
		return io.ErrClosedPipe
	default:
		return nil
	}
}

func (t *terminal) ExitCode() (int, error) {
	if err := t.out.result(); !errors.Is(err, io.EOF) {
		return -1, err
	}
	return 0, nil
}

func (t *terminal) Close() error {
	t.out.end(io.EOF)
	return nil
}

// tracked is an open stream in the fake's list.
type tracked struct {
	info Stream
	end  func(err error)
	// watch is set for a watch, follow for a followed log.
	watch  *pipe[runtime.SandboxEvent]
	follow *pipe[runtime.LogEntry]
	source runtime.LogSource
}

// track lists a stream as open until it ends, and ends it with io.EOF when
// ctx ends. It must be called without the fake's lock.
func track[T any](f *Fake, ctx context.Context, tr *tracked, p *pipe[T]) {
	prepare(f, tr, p)
	f.mu.Lock()
	f.register(tr)
	f.mu.Unlock()
	endWithContext(ctx, p)
}

// prepare makes a stream leave the list when it ends; it comes before the
// stream is listed, so that no end is missed.
func prepare[T any](f *Fake, tr *tracked, p *pipe[T]) {
	tr.end = p.end
	p.whenEnded(func() {
		f.mu.Lock()
		delete(f.streams, tr.info.ID)
		f.mu.Unlock()
	})
}

// register lists a prepared stream as open. The caller holds f.mu.
func (f *Fake) register(tr *tracked) {
	f.nextStream++
	tr.info.ID = f.nextStream
	f.streams[tr.info.ID] = tr
}

// endWithContext ends a stream with io.EOF when ctx ends.
func endWithContext[T any](ctx context.Context, p *pipe[T]) {
	stop := context.AfterFunc(ctx, func() { p.end(io.EOF) })
	p.whenEnded(func() { stop() })
}
