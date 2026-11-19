package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The bounds of an exec request and of its collected answer.
const (
	// maxStdin is the most standard input an exec takes, after decoding.
	maxStdin = 4 << 20
	// defaultExecTimeout is the timeout of an exec whose body names none.
	defaultExecTimeout = time.Minute
	// maxCollected is the most of each output stream a collected answer
	// holds.
	maxCollected = 1 << 20
)

// execRequest is an exec body once its members are judged.
type execRequest struct {
	command []string
	// workDir is "" for the sandbox's working directory.
	workDir string
	// env is the request's environment, which the runtime lays over the
	// sandbox's.
	env     map[string]string
	stdin   []byte
	timeout time.Duration
}

// executor runs the commands of exec and counts, per sandbox id, those
// running, so that no sandbox runs more than [sandbox].max_concurrent_execs
// at once. The count is held in memory: a restart of the hub ends every
// exec request anyway.
type executor struct {
	s       *Service
	mu      sync.Mutex
	running map[string]int
}

func newExecutor(s *Service) *executor {
	return &executor{s: s, running: map[string]int{}}
}

// validate reads the exec body of r against the sandbox of u and leaves it
// in u, answering the first member it refuses with 400 invalid_request
// "<member>: <reason>". A message names a key and never quotes a value of
// env or stdin, which may be secrets.
func (x *executor) validate(r *http.Request, u *useRequest) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return server.InvalidRequest("request body could not be read")
	}
	body, err := readBody(raw)
	if err != nil {
		return err
	}
	req := execRequest{timeout: min(defaultExecTimeout, x.s.settings.MaxExecTimeout)}
	// The secrets the sandbox exposes are refused as names, as a create
	// refuses those of its profile.
	exposed := Profile{Name: u.rec.Profile, ExposeSecrets: u.rec.ExposedSecrets}
	steps := []struct {
		member string
		read   func(raw json.RawMessage) error
	}{
		{"command", func(raw json.RawMessage) (err error) {
			if raw == nil {
				return invalid("is required")
			}
			req.command, err = readCommand(raw)
			return err
		}},
		{"workdir", func(raw json.RawMessage) error {
			dir, err := readString(raw)
			if dir != nil {
				req.workDir = *dir
			}
			return err
		}},
		{"env", func(raw json.RawMessage) (err error) {
			req.env, err = readEnv(raw, exposed)
			return err
		}},
		{"stdin", func(raw json.RawMessage) error {
			text, err := readString(raw)
			if err != nil || text == nil {
				return err
			}
			if body["stdin_base64"] != nil {
				return invalid("must not be sent with stdin_base64")
			}
			req.stdin = []byte(*text)
			return checkStdin(req.stdin)
		}},
		{"stdin_base64", func(raw json.RawMessage) error {
			encoded, err := readString(raw)
			if err != nil || encoded == nil {
				return err
			}
			if req.stdin, err = base64.StdEncoding.DecodeString(*encoded); err != nil {
				return invalid("must be base64")
			}
			return checkStdin(req.stdin)
		}},
		{"timeout", func(raw json.RawMessage) (err error) {
			req.timeout, err = readExecTimeout(raw, req.timeout, x.s.settings.MaxExecTimeout)
			return err
		}},
	}
	for _, step := range steps {
		if err := step.read(body[step.member]); err != nil {
			if errors.Is(err, errInvalid) {
				return server.InvalidRequest(step.member + ": " + strings.TrimPrefix(err.Error(), errInvalid.Error()+": "))
			}
			return err
		}
	}
	u.exec = req
	return nil
}

// checkStdin refuses a standard input over maxStdin bytes.
func checkStdin(stdin []byte) error {
	if len(stdin) > maxStdin {
		return invalid("must have at most %d bytes", maxStdin)
	}
	return nil
}

// readExecTimeout reads the timeout member in the hub's duration grammar:
// above zero and at most max; def when it is absent. A timeout over max is
// refused, not lowered, so that a caller learns its command would not get
// the time it asked for.
func readExecTimeout(raw json.RawMessage, def, max time.Duration) (time.Duration, error) {
	value, err := readString(raw)
	if err != nil || value == nil {
		return def, err
	}
	d, err := timefmt.ParseDuration(*value)
	if err != nil {
		return 0, invalid("must be a whole number followed by s, m, h or d, such as 60s")
	}
	if d <= 0 || d > max {
		return 0, invalid("must be above 0s and at most %s", formatDuration(max))
	}
	return d, nil
}

// take takes a place for a command in the sandbox id, and reports false
// when every place is taken.
func (x *executor) take(id string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.running[id] >= x.s.settings.MaxConcurrentExecs {
		return false
	}
	x.running[id]++
	return true
}

// release gives back a place take took in the sandbox id.
func (x *executor) release(id string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.running[id]--; x.running[id] <= 0 {
		delete(x.running, id)
	}
}

// execution is a command exec started: its output, under a context that
// ends at its timeout, when the caller leaves or when the hub begins to
// stop, and the place it holds in its sandbox until end.
type execution struct {
	stream runtime.ExecStream
	// started is when the command started, on the hub's clock.
	started time.Time
	// caller is the request's context, which ends when the caller leaves.
	caller   context.Context
	cancel   context.CancelFunc
	timer    Timer
	timedOut atomic.Bool
	stopping atomic.Bool
	ended    chan struct{}
	once     sync.Once
	release  func()
}

// start takes a place in the sandbox of u, answering 429 rate_limited when
// none is left, and starts the command of u through the runtime. The 429
// has no Retry-After: the limit is a number of commands, not a rate, so
// there is no time after which a place is known to be free. The caller ends
// what start returns with end.
func (x *executor) start(r *http.Request, u useRequest) (*execution, error) {
	id := u.rec.ID
	if !x.take(id) {
		return nil, server.RateLimited("the sandbox runs as many commands as it may at once; try again once one has ended")
	}
	ctx, cancel := context.WithCancel(r.Context())
	e := &execution{
		started: x.s.deps.Now(), caller: r.Context(), cancel: cancel,
		timer: x.s.newTimer(u.exec.timeout), ended: make(chan struct{}),
		release: func() { x.release(id) },
	}
	go e.watch(x.s.deps.Shutdown)
	// The runtime holds the sandbox's environment since its creation and
	// lays the request's over it, so only the request's is sent.
	stream, err := x.s.Client().ExecStream(ctx, id, runtime.ExecRequest{
		Command: u.exec.command, WorkDir: u.exec.workDir, Env: u.exec.env, Stdin: u.exec.stdin,
	})
	if err != nil {
		e.end()
		return nil, err
	}
	e.stream = stream
	return e, nil
}

// watch ends the command's context when its timeout passes or the hub
// begins to stop, until the execution ends.
func (e *execution) watch(shutdown <-chan struct{}) {
	select {
	case <-e.timer.C():
		e.timedOut.Store(true)
		e.cancel()
	case <-shutdown:
		e.stopping.Store(true)
		e.cancel()
	case <-e.ended:
	}
}

// abandoned reports whether nobody waits for the command's answer any
// more: its caller left or the hub is stopping.
func (e *execution) abandoned() bool {
	return e.stopping.Load() || e.caller.Err() != nil
}

// end kills the command if it still runs and gives back its place, once.
func (e *execution) end() {
	e.once.Do(func() {
		close(e.ended)
		e.timer.Stop()
		e.cancel()
		if e.stream != nil {
			_ = e.stream.Close()
		}
		e.release()
	})
}

// execAnswer is the answer of a collected exec.
type execAnswer struct {
	// ExitCode is nil when the command timed out.
	ExitCode     *int   `json:"exit_code"`
	Stdout       string `json:"stdout"`
	StdoutBase64 string `json:"stdout_base64,omitempty"`
	Stderr       string `json:"stderr"`
	StderrBase64 string `json:"stderr_base64,omitempty"`
	Truncated    bool   `json:"truncated"`
	TimedOut     bool   `json:"timed_out"`
	DurationMS   int64  `json:"duration_ms"`
}

// collect runs the command of u, waits for it to end and answers with its
// exit code and its output, up to maxCollected bytes of each stream. A
// command that outlasts its timeout is killed and answered with the output
// it wrote until then. When the caller leaves or the hub begins to stop,
// the command is killed and no answer is written: the connection is
// dropped.
func (x *executor) collect(w http.ResponseWriter, r *http.Request, u useRequest) error {
	e, err := x.start(r, u)
	if err != nil {
		return err
	}
	defer e.end()
	var stdout, stderr collector
	for {
		o, err := e.stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			e.end()
			if e.abandoned() {
				panic(http.ErrAbortHandler)
			}
			return err
		}
		switch o.Stream {
		case runtime.Stdout:
			stdout.add(o.Data)
		case runtime.Stderr:
			stderr.add(o.Data)
		}
	}
	answer := execAnswer{Truncated: stdout.lost || stderr.lost}
	switch code, err := e.stream.ExitCode(); {
	case e.abandoned():
		e.end()
		panic(http.ErrAbortHandler)
	case err == nil:
		answer.ExitCode = &code
	case e.timedOut.Load():
		answer.TimedOut = true
	default:
		e.end()
		return err
	}
	answer.DurationMS = x.s.deps.Now().Sub(e.started).Milliseconds()
	e.end()
	answer.Stdout, answer.StdoutBase64 = stdout.result()
	answer.Stderr, answer.StderrBase64 = stderr.result()
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// collector keeps the first maxCollected bytes of an output stream and
// drops the rest.
type collector struct {
	data []byte
	// lost is set once a byte was dropped.
	lost bool
}

func (c *collector) add(b []byte) {
	if room := maxCollected - len(c.data); len(b) > room {
		b, c.lost = b[:room], true
	}
	c.data = append(c.data, b...)
}

// result is the stream as the answer gives it: as text when its bytes are
// valid UTF-8, else base64-encoded with an empty text. A stream that lost
// bytes is first cut back to its last complete character, so that a cut
// inside a character does not turn text into base64.
func (c *collector) result() (text, encoded string) {
	data := c.data
	if c.lost {
		data = cutToRune(data)
	}
	if utf8.Valid(data) {
		return string(data), ""
	}
	return "", base64.StdEncoding.EncodeToString(data)
}

// cutToRune cuts b before a character its end leaves incomplete.
func cutToRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}
