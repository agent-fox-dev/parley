package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/pflag"
)

// The flags of a command that can wait for the operation it starts, and
// their defaults.
const (
	waitFlag                = "wait"
	timeoutFlag             = "timeout"
	pollIntervalFlag        = "poll-interval"
	defaultWaitTimeout      = "5m"
	defaultWaitPollInterval = "5s"
)

// outcome is what a record says of the operation a command waits for.
type outcome int

const (
	// notTerminal: the operation goes on ("waiting to retry" included).
	notTerminal outcome = iota
	// successful: the operation ended as it should.
	successful
	// unsuccessful: the operation ended and did not do what it was for.
	unsuccessful
)

// waitFor is what a command that can wait for the operation it starts
// declares: how to start it, how to poll it and how to read a record of
// it. The flags, the polling, the deadline, the progress and the printing
// are the same for every such command (wait).
type waitFor struct {
	// Start builds the request that starts the operation; without it, the
	// command sends its Method to its Path, :params filled in.
	Start func(c *call) (apiRequest, error)
	// Poll builds the request that asks for the operation's state, from
	// the record of it last received (the start's answer at first).
	Poll func(c *call, record []byte) (apiRequest, error)
	// Classify reads a record: the operation's state, as the progress
	// lines and the wait_failed message name it, and whether it is
	// terminal.
	Classify func(record []byte) (state string, end outcome, err error)
	// Document, when set, builds the document printed in place of the
	// final record, from the start's answer and the final record.
	Document func(c *call, start, final []byte) ([]byte, error)
}

// byState is a Classify that reads the string member member of a record,
// a JSON object: one of successes is a successful end, one of failures an
// unsuccessful one, and any other state is not terminal.
func byState(member string, successes, failures []string) func(record []byte) (string, outcome, error) {
	return func(record []byte) (string, outcome, error) {
		var doc map[string]json.RawMessage
		var state string
		if err := json.Unmarshal(record, &doc); err != nil || json.Unmarshal(doc[member], &state) != nil || state == "" {
			return "", notTerminal, fmt.Errorf("the record has no %s", member)
		}
		switch {
		case slices.Contains(successes, state):
			return state, successful, nil
		case slices.Contains(failures, state):
			return state, unsuccessful, nil
		}
		return state, notTerminal, nil
	}
}

// durationUnits are the units of a --timeout or --poll-interval value.
var durationUnits = map[byte]time.Duration{
	's': time.Second,
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
}

// parseDuration reads a duration written as a positive whole number
// followed by exactly one of the units s, m, h and d (24 hours), such as
// 90s, 5m, 2h or 1d; leading zeros are allowed (05s), as the hub's own
// durations allow them. Zero, a sign, a fraction, a missing or unknown
// unit, spaces, a compound form (1m30s) and a value too large for a
// time.Duration are refused.
func parseDuration(s string) (time.Duration, error) {
	const form = "want a whole number above 0 followed by s, m, h or d, such as 90s or 5m"
	if len(s) < 2 {
		return 0, errors.New(form)
	}
	digits, unit := s[:len(s)-1], durationUnits[s[len(s)-1]]
	if unit == 0 {
		return 0, errors.New(form)
	}
	// strconv would accept a sign and, with base 0, underscores and
	// prefixes; the form allows digits alone.
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, errors.New(form)
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	switch {
	case err != nil || n > math.MaxInt64/int64(unit):
		return 0, errors.New("too large")
	case n == 0:
		return 0, errors.New(form)
	}
	return time.Duration(n) * unit, nil
}

// durationValue is the value of a --timeout or --poll-interval flag. It
// refuses what parseDuration refuses when the command line is parsed, so a
// value it cannot read is a usage error before any request, with or
// without --wait.
type durationValue struct {
	d    time.Duration
	text string
}

func newDurationValue(text string) *durationValue {
	// The defaults are constants that parse.
	d, _ := parseDuration(text)
	return &durationValue{d: d, text: text}
}

func (v *durationValue) String() string { return v.text }

func (v *durationValue) Set(s string) error {
	d, err := parseDuration(s)
	if err != nil {
		return err
	}
	v.d, v.text = d, s
	return nil
}

// Type is the type name the JSON help reports.
func (v *durationValue) Type() string { return "duration" }

// addWaitFlags registers --wait, --timeout and --poll-interval on fs.
func addWaitFlags(fs *pflag.FlagSet) {
	fs.Bool(waitFlag, false, "wait until the operation ends and print its final record")
	fs.Var(newDurationValue(defaultWaitTimeout), timeoutFlag,
		"with --wait, how long to wait for the operation to end: a whole number followed by s, m, h or d")
	fs.Var(newDurationValue(defaultWaitPollInterval), pollIntervalFlag,
		"with --wait, how long to wait between two requests for the operation's state: a whole number followed by s, m, h or d")
}

// durationFlag is the value of the duration flag name of c's command.
func (c *call) durationFlag(name string) *durationValue {
	return c.cmd.Flags().Lookup(name).Value.(*durationValue)
}

// wait is the run step of a command that declares Wait. It sends the
// request that starts the operation. Without --wait it prints the answer.
// With --wait it reads the start's record and, until a record is terminal,
// polls one poll interval after the previous answer, under one deadline
// that counts --timeout from the moment the start's answer came. It
// writes "waiting: <state>" to stderr when the wait begins and whenever
// the state changes, and prints only the final record (or the command's
// own document): with no error on a successful end, with wait_failed on
// an unsuccessful one. At the deadline it abandons a poll in flight and
// returns wait_timeout, printing nothing; it never asks the hub to cancel
// or change the operation.
func (c *call) wait() error {
	w := c.decl.Wait
	if c.client == nil {
		// A command that waits calls the hub, so its auth is not none.
		return fmt.Errorf("%s has no client", c.cmd.CommandPath())
	}
	start := apiRequest{method: c.decl.Method, path: routePath(c.decl.Path, c.args)}
	if w.Start != nil {
		var err error
		if start, err = w.Start(c); err != nil {
			return err
		}
	}
	started, err := c.client.send(c.ctx, start)
	if err != nil {
		return err
	}
	if waiting, _ := c.cmd.Flags().GetBool(waitFlag); !waiting {
		return c.printAnswer(started)
	}
	timeout := c.durationFlag(timeoutFlag)
	ctx, cancel := context.WithTimeout(c.ctx, timeout.d)
	defer cancel()
	interval := c.durationFlag(pollIntervalFlag).d
	if c.opts.pollInterval > 0 {
		interval = c.opts.pollInterval
	}

	record := started.body
	state, end, err := c.classify(record)
	if err != nil {
		return err
	}
	progress(c.stderr, "waiting: %s", state)
	for end == notTerminal {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return c.waitEnded(ctx, timeout, state, ctx.Err())
		case <-timer.C:
		}
		poll, err := w.Poll(c, record)
		if err != nil {
			return err
		}
		polled, err := c.client.send(ctx, poll)
		if err != nil {
			return c.waitEnded(ctx, timeout, state, err)
		}
		record = polled.body
		previous := state
		if state, end, err = c.classify(record); err != nil {
			return err
		}
		if state != previous {
			progress(c.stderr, "waiting: %s", state)
		}
	}

	document := record
	if w.Document != nil {
		if document, err = w.Document(c, started.body, record); err != nil {
			return err
		}
	}
	err = c.printResult(document)
	if end == successful || (err != nil && exitCode(err) != exitConditionNotMet) {
		return err
	}
	return waitFailed("the operation ended in the state %q, which is not a success", state)
}

// classify is the command's Classify of record, a failure to read it a
// client_error.
func (c *call) classify(record []byte) (string, outcome, error) {
	state, end, err := c.decl.Wait.Classify(record)
	if err != nil {
		return "", notTerminal, clientError("cannot read the state of the operation from the hub's answer: %v", err)
	}
	return state, end, nil
}

// waitEnded is the error a wait under ctx ends with when err stopped it:
// wait_timeout once ctx's deadline has passed, unless the run itself was
// cancelled, and err otherwise. A poll abandoned at the deadline fails
// with the client's error, which is not the reason the wait ended.
func (c *call) waitEnded(ctx context.Context, timeout *durationValue, state string, err error) error {
	if c.ctx.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return waitTimeout("the operation did not end within %s; its last state was %q", timeout.text, state)
	}
	if c.ctx.Err() != nil {
		return clientError("the wait was interrupted: %v", c.ctx.Err())
	}
	return err
}
