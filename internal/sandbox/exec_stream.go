package sandbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// maxFrameOutput is the most output one frame of a streamed exec carries.
const maxFrameOutput = 32 << 10

// eventStreamType is the media type that asks exec for its streamed form.
const eventStreamType = "text/event-stream"

// serve runs the command of u in the form the request asks for: streamed
// when its Accept header names text/event-stream, else collected.
func (x *executor) serve(w http.ResponseWriter, r *http.Request, u useRequest) error {
	if acceptsEventStream(r) {
		return x.stream(w, r, u)
	}
	return x.collect(w, r, u)
}

// acceptsEventStream reports whether an entry of r's Accept header names
// text/event-stream, whatever its parameters.
func acceptsEventStream(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept") {
		for entry := range strings.SplitSeq(value, ",") {
			media, _, err := mime.ParseMediaType(strings.TrimSpace(entry))
			if err == nil && media == eventStreamType {
				return true
			}
		}
	}
	return false
}

// exitFrame is the data of the frame that ends a streamed exec.
type exitFrame struct {
	// ExitCode is nil when the command timed out.
	ExitCode   *int  `json:"exit_code"`
	TimedOut   bool  `json:"timed_out"`
	DurationMS int64 `json:"duration_ms"`
}

// stream runs the command of u and sends its output as server-sent events
// while it runs, without a cap: frames named stdout and stderr in the order
// the output arrives, then one frame named exit. The headers go out only
// once the runtime started the command, so that every refusal before it,
// the exec limit included, is the collected form's JSON answer. When the
// caller leaves or the hub begins to stop, the command is killed and the
// stream ends without an exit frame; so it does when the runtime fails
// after the headers, whose failure then only reaches the hub's log.
func (x *executor) stream(w http.ResponseWriter, r *http.Request, u useRequest) error {
	e, err := x.start(r, u)
	if err != nil {
		return err
	}
	defer e.end()
	h := w.Header()
	h.Set("Content-Type", eventStreamType)
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return nil
	}
	send := func(event string, data any) error {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		return rc.Flush()
	}
	for {
		o, err := e.stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			e.end()
			if e.abandoned() {
				return nil
			}
			return err
		}
		if err := sendOutput(o, send); err != nil {
			// The caller is gone: nobody reads the rest.
			return nil
		}
	}
	var exit exitFrame
	// The timeout is asked first: a runtime may report the code of the
	// process it killed as an exit, and the caller is told it timed out.
	switch code, err := e.stream.ExitCode(); {
	case e.abandoned():
		return nil
	case e.timedOut.Load():
		exit.TimedOut = true
		u.act.exited(nil)
	case err == nil:
		exit.ExitCode = &code
		u.act.exited(&code)
	default:
		return err
	}
	exit.DurationMS = x.s.deps.Now().Sub(e.started).Milliseconds()
	e.end()
	_ = send("exit", exit)
	return nil
}

// sendOutput sends a chunk of output in frames of at most maxFrameOutput
// bytes, named for its stream. A frame carries its bytes as data when they
// are valid UTF-8 and as data_base64 otherwise. A chunk is split before a
// character the cut would leave incomplete, so that a split does not turn
// text into base64.
func sendOutput(o runtime.Output, send func(event string, data any) error) error {
	event := "stdout"
	if o.Stream == runtime.Stderr {
		event = "stderr"
	}
	for b := o.Data; len(b) > 0; {
		piece := b[:min(len(b), maxFrameOutput)]
		if len(piece) < len(b) {
			if cut := cutToRune(piece); len(cut) > 0 {
				piece = cut
			}
		}
		b = b[len(piece):]
		var data any = struct {
			Data string `json:"data"`
		}{string(piece)}
		if !utf8.Valid(piece) {
			data = struct {
				DataBase64 string `json:"data_base64"`
			}{base64.StdEncoding.EncodeToString(piece)}
		}
		if err := send(event, data); err != nil {
			return err
		}
	}
	return nil
}
