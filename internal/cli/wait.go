package cli

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/pflag"
)

// Outcome is what a record of a long operation says of it.
type Outcome int

// The outcomes a Wait's Classify gives.
const (
	// NotTerminal is a state the operation leaves on its own (queued,
	// running, waiting to retry): the wait goes on.
	NotTerminal Outcome = iota
	// Succeeded is a terminal state that is a success.
	Succeeded
	// Unsuccessful is a terminal state that is not a success.
	Unsuccessful
)

// Wait declares a command's long operation: the declared route is the
// request that starts it, Poll gives the path (relative to /api/v1) that
// reads it back with GET, and Classify says what a record's state is.
type Wait struct {
	Poll     func(record []byte) (string, error)
	Classify func(record []byte) (outcome Outcome, state string)
	// Print, when set, prints a document of its own for the final record
	// in place of the record.
	Print func(c *Call, record []byte) error
}

// The defaults of --timeout and --poll-interval.
const (
	defaultWaitTimeout  = "5m"
	defaultPollInterval = "5s"
)

// pollIntervalHook, when positive, replaces the --poll-interval of every
// wait; tests set it below the flag's smallest value of 1s.
var pollIntervalHook time.Duration

// durationUnits are the units of a --timeout or --poll-interval value.
var durationUnits = map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}

// parseDuration reads a positive whole number followed by exactly one of
// s, m, h and d (24 hours). Zero, a sign, a fraction, a missing or unknown
// unit, spaces and compound forms are refused.
func parseDuration(text string) (time.Duration, error) {
	refuse := errors.New("want a positive whole number followed by s, m, h or d, such as 90s, 5m, 2h or 1d")
	if len(text) < 2 {
		return 0, refuse
	}
	unit, ok := durationUnits[text[len(text)-1]]
	digits := text[:len(text)-1]
	if !ok {
		return 0, refuse
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, refuse
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 || n > math.MaxInt64/int64(unit) {
		return 0, refuse
	}
	return time.Duration(n) * unit, nil
}

// durationValue is a --timeout or --poll-interval flag; its type is
// duration.
type durationValue struct {
	d    time.Duration
	text string
}

func newDurationValue(text string) *durationValue {
	d, err := parseDuration(text)
	if err != nil {
		panic("cli: bad default duration " + text)
	}
	return &durationValue{d: d, text: text}
}

func (v *durationValue) String() string { return v.text }
func (v *durationValue) Type() string   { return "duration" }

func (v *durationValue) Set(text string) error {
	d, err := parseDuration(text)
	if err != nil {
		return err
	}
	v.d, v.text = d, text
	return nil
}

// addWaitFlags registers --wait, --timeout and --poll-interval. A refused
// duration is a flag error, so it is a usage error naming the flag before
// any request, with or without --wait.
func addWaitFlags(fs *pflag.FlagSet) {
	fs.Bool("wait", false, "wait until the operation ends")
	fs.Var(newDurationValue(defaultWaitTimeout), "timeout", "how long --wait waits, such as 90s, 5m, 2h or 1d")
	fs.Var(newDurationValue(defaultPollInterval), "poll-interval", "how often --wait reads the operation, such as 5s or 1m")
}

func durationFlag(fs *pflag.FlagSet, name string) time.Duration {
	if f := fs.Lookup(name); f != nil {
		if v, ok := f.Value.(*durationValue); ok {
			return v.d
		}
	}
	return 0
}

// runWait sends the request that starts the operation and, with --wait,
// waits for it to end; without --wait it prints the start request's answer
// and sends nothing else.
func runWait(c *Call, s *Spec) error {
	resp, err := c.Client.Send(c.Ctx, s.Method, expandPath(s.Path, c.Args), nil, nil)
	if err != nil {
		return err
	}
	record, err := resp.Result()
	if err != nil {
		return err
	}
	return c.Await(record)
}

// Await ends a command whose declaration has a Wait, given the record the
// request that started the operation answered: without --wait it prints the
// record; with --wait it polls the operation once per poll interval until a
// terminal state or the timeout, writing "waiting: <state>" on stderr when
// the wait begins and at each change of state, and prints the final record
// alone on stdout. An unsuccessful state is wait_failed; the timeout is
// wait_timeout, with nothing on stdout and no request that changes the
// operation; a poll the hub refuses ends the wait with its error, and a
// poll that fails on the client side with a client error.
func (c *Call) Await(record []byte) error {
	w := c.spec.Wait
	if on, _ := c.Cmd.Flags().GetBool("wait"); !on {
		return printResult(c.Stdout, record)
	}
	timeout := durationFlag(c.Cmd.Flags(), "timeout")
	interval := durationFlag(c.Cmd.Flags(), "poll-interval")
	if pollIntervalHook > 0 {
		interval = pollIntervalHook
	}
	limit := c.Cmd.Flags().Lookup("timeout").Value.String()
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(c.Ctx, deadline)
	defer cancel()

	outcome, state := w.Classify(record)
	progressf(c.Stderr, "%s", state)
	for outcome == NotTerminal {
		path, err := w.Poll(record)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return waitEnded(ctx, limit)
		case <-time.After(interval):
		}
		resp, err := c.Client.Send(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			if ctx.Err() != nil {
				return waitEnded(ctx, limit)
			}
			return err
		}
		if record, err = resp.Result(); err != nil {
			return err
		}
		var next string
		outcome, next = w.Classify(record)
		if next != state {
			progressf(c.Stderr, "%s", next)
			state = next
		}
	}
	if w.Print != nil {
		if err := w.Print(c, record); err != nil {
			return err
		}
	} else if err := printResult(c.Stdout, record); err != nil {
		return err
	}
	if outcome == Unsuccessful {
		return WaitFailed("the operation ended in state %q", state)
	}
	return nil
}

// waitEnded is the error of a wait whose context ended: wait_timeout when
// its deadline passed, otherwise a client error (the command was stopped).
func waitEnded(ctx context.Context, timeout string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return WaitTimeout("the operation did not end within %s", timeout)
	}
	return ClientError("the wait was stopped: %v", context.Cause(ctx))
}
