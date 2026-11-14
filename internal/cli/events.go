package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The waits of parley events tail before it connects again: the first, which
// each failed attempt doubles, and the most it grows to.
const (
	firstReconnectWait = time.Second
	maxReconnectWait   = 30 * time.Second
)

// maxListWait bounds the --wait of parley events list, in seconds, that is
// added to its request timeout.
const maxListWait = 24 * 60 * 60

// maxFrameLine bounds one line of the event stream, in bytes. The hub sends
// an event record on one line, far shorter; a URL that points elsewhere
// could send one line without end.
const maxFrameLine = maxAnswerSize

// stopStatuses are the hub's answers that end parley events tail: asking
// again cannot change them, and going on past one could skip events.
var stopStatuses = []int{
	http.StatusBadRequest,
	http.StatusUnauthorized,
	http.StatusForbidden,
	http.StatusNotFound,
	http.StatusGone,
}

// newEventsCommand builds parley events: the hub's event stream and its
// event log.
func newEventsCommand() *cobra.Command {
	return group("events", "Follow the hub's events, or read them a page at a time",
		declare(command{
			Name:        "tail",
			Description: "Follow the hub's events as they happen, one JSON line each, reconnecting by itself until interrupted",
			Method:      http.MethodGet,
			Path:        "/events",
			Auth:        authUser,
			Flags:       addEventFilterFlags,
			Run:         tailEvents,
		}),
		declare(command{
			Name:        "list",
			Description: "Read one page of the hub's event log, waiting up to --wait seconds for an event when there is none",
			Method:      http.MethodGet,
			Path:        "/events/log",
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				addEventFilterFlags(fs)
				fs.Int("limit", 0, "the most events the page holds; the hub's default, 50, when not given")
				fs.Int("wait", 0, "how many seconds the hub may hold the request while there is no event to return")
			},
			Run: listEvents,
		}),
	)
}

// listEvents is the run step of parley events list. It sends one request
// with the query parameters of the flags given and prints the hub's page.
// The hub may hold the request for --wait seconds before it answers, so the
// request's timeout is the usual one plus that wait.
func listEvents(c *call) error {
	query := eventFilters(c)
	if n, ok := c.after(); ok {
		query.Set("after", strconv.FormatInt(n, 10))
	}
	// An integer the hub would refuse (a limit of 0, a wait of 61) is sent
	// as given, so that the hub's own error says what is wrong with it.
	for _, name := range []string{"limit", "wait"} {
		if f := c.cmd.Flags().Lookup(name); f.Changed {
			query.Set(name, f.Value.String())
		}
	}
	wait, _ := c.cmd.Flags().GetInt("wait")
	// The hub answers a wait above its limit at once; the bound only keeps
	// the sum from overflowing.
	wait = min(max(wait, 0), maxListWait)
	resp, err := c.client.send(c.ctx, apiRequest{
		method:  c.decl.Method,
		path:    c.decl.Path,
		query:   query,
		timeout: c.client.timeout + time.Duration(wait)*time.Second,
	})
	if err != nil {
		return err
	}
	return c.printAnswer(resp)
}

// addEventFilterFlags registers the flags that choose which events a
// command reads, and the position it reads them after.
func addEventFilterFlags(fs *pflag.FlagSet) {
	fs.String("workspace", "", "only the events of this workspace slug")
	fs.String("types", "", "only the events of these comma-separated types; an entry ending in * matches every type that begins with the text before it")
	fs.String("session", "", "only the events of this agent session id")
	fs.Var(&afterValue{}, "after", "only the events with a higher seq: the position to resume from, a non-negative integer")
}

// afterValue is the value of --after: a non-negative integer. It refuses
// anything else when the command line is parsed, so a bad value is a usage
// error before any request.
type afterValue struct {
	n   int64
	set bool
}

func (v *afterValue) String() string {
	if !v.set {
		return ""
	}
	return strconv.FormatInt(v.n, 10)
}

func (v *afterValue) Set(s string) error {
	// strconv would accept a sign; the position is digits alone.
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return errors.New("want a non-negative integer, such as 4")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return errors.New("too large")
	}
	v.n, v.set = n, true
	return nil
}

// Type is the type name the JSON help reports.
func (v *afterValue) Type() string { return "integer" }

// after is the --after of c's command, and whether it was given.
func (c *call) after() (int64, bool) {
	v := c.cmd.Flags().Lookup("after").Value.(*afterValue)
	return v.n, v.set
}

// eventFilters are the query parameters of c's filter flags, each one
// given, an empty value included, so the hub judges it.
func eventFilters(c *call) url.Values {
	q := url.Values{}
	for flag, param := range map[string]string{"workspace": "workspace", "types": "types", "session": "session_id"} {
		if f := c.cmd.Flags().Lookup(flag); f.Changed {
			q.Set(param, f.Value.String())
		}
	}
	return q
}

// tail is one run of parley events tail.
type tail struct {
	*call
	// following ends when the run is interrupted.
	following context.Context
	filters   url.Values
	// position is the id of the last frame received, the --after value
	// before one is, or nil for neither.
	position *int64
	// printed is the seq of the last event printed.
	printed int64
}

// retryable is a reason the stream ended that a new connection may get
// past: a connection that closed or failed, or an answer that may differ
// next time.
type retryable struct{ reason string }

func (e *retryable) Error() string { return e.reason }

// tailEvents is the run step of parley events tail. It follows the event
// stream and prints the data of each hub_event frame on a line of its own,
// each seq at most once. When the stream ends in a way a new connection may
// get past, it says so on stderr, waits and connects again, resuming after
// the id of the last frame received; the wait is 1 second, doubles after
// each attempt that received no frame up to 30 seconds, and is 1 second
// again after one that did. An interrupt ends it with success; an answer
// in stopStatuses ends it with that error.
func tailEvents(c *call) error {
	// The command follows the hub until the user stops it, so an interrupt
	// is its normal end, not a failure.
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt)
	defer stop()
	t := &tail{call: c, following: ctx, filters: eventFilters(c)}
	if n, ok := c.after(); ok {
		t.position, t.printed = &n, n
	}
	wait := firstReconnectWait
	for {
		received, err := t.follow()
		if ctx.Err() != nil {
			return nil
		}
		var again *retryable
		if !errors.As(err, &again) {
			return err
		}
		if received {
			wait = firstReconnectWait
		}
		progress(c.stderr, "reconnecting in %v: %s", wait, again.reason)
		if c.opts.reconnectWait(ctx, wait) != nil {
			return nil
		}
		wait = min(2*wait, maxReconnectWait)
	}
}

// follow opens the stream once, without a timeout, and reads it until it
// ends. It reports whether a frame was received, and why the stream ended:
// a retryable, or the error that ends the command.
func (t *tail) follow() (bool, error) {
	query := maps.Clone(t.filters)
	if t.position != nil {
		query.Set("after", strconv.FormatInt(*t.position, 10))
	}
	target := t.client.hubURL + apiPrefix + t.decl.Path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	// The stream is open for as long as the user follows it, so it is sent
	// through the shared client's connections without the request timeout
	// client.raw puts around every other request.
	req, err := http.NewRequestWithContext(t.following, t.decl.Method, target, nil)
	if err != nil {
		return false, clientError("cannot build a request to the hub at %s: %v", t.client.hubURL, err)
	}
	if t.client.credential != "" {
		req.Header.Set("Authorization", "Bearer "+t.client.credential)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := t.client.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return false, &retryable{fmt.Sprintf("cannot reach the hub at %s: %v", t.client.hubURL, err)}
	}
	defer resp.Body.Close()
	switch {
	case slices.Contains(stopStatuses, resp.StatusCode):
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerSize+1))
		if err != nil {
			return false, &retryable{fmt.Sprintf("cannot read the answer of the hub at %s: %v", t.client.hubURL, err)}
		}
		if len(body) <= maxAnswerSize && isHubEnvelope(body) {
			return false, &hubError{status: resp.StatusCode, body: body}
		}
		return false, clientError("the hub answered %d without an error envelope", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return false, &retryable{fmt.Sprintf("the hub answered %d", resp.StatusCode)}
	}
	return t.read(resp.Body)
}

// frame is one Server-Sent Events frame, as far as it has been read.
type frame struct {
	id, event, data string
	hasID, hasData  bool
}

// read reads frames from the stream body until it ends, handling each
// complete one, and reports whether it received one.
func (t *tail) read(body io.Reader) (bool, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrameLine)
	received := false
	var f frame
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if f.hasID || f.hasData || f.event != "" {
				received = true
				if err := t.handle(f); err != nil {
					return received, err
				}
			}
			f = frame{}
			continue
		}
		// A line that begins with a colon is a comment, whose field is
		// empty, and is ignored with every other unknown field.
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			f.id, f.hasID = value, true
		case "event":
			f.event = value
		case "data":
			if f.hasData {
				f.data += "\n"
			}
			f.data, f.hasData = f.data+value, true
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// The same line would come again on every reconnect.
			return received, clientError("the hub sent a line longer than %d bytes in the event stream", maxFrameLine)
		}
		return received, &retryable{fmt.Sprintf("the stream broke: %v", err)}
	}
	return received, &retryable{"the stream ended"}
}

// handle takes in one frame: its id, the seq of a hub_event and the position
// a heartbeat stands for, becomes the resume position, and a hub_event whose
// seq is higher than the last one printed is printed.
func (t *tail) handle(f frame) error {
	seq, err := strconv.ParseInt(f.id, 10, 64)
	numbered := f.hasID && err == nil
	if numbered {
		t.position = &seq
	}
	if f.event != "hub_event" || (numbered && seq <= t.printed) {
		return nil
	}
	if _, err := io.WriteString(t.stdout, f.data+"\n"); err != nil {
		return clientError("cannot print an event: %v", err)
	}
	if numbered {
		t.printed = seq
	}
	return nil
}

// sleepUnlessDone waits d, or returns ctx's error as soon as ctx ends.
func sleepUnlessDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
