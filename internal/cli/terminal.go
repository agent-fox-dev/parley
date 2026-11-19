package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"

	"golang.org/x/net/websocket"
	"golang.org/x/term"
)

// The size parley sandbox shell asks for when standard input is not a
// terminal, and the largest the hub accepts.
const (
	defaultTerminalCols = 80
	defaultTerminalRows = 24
	maxTerminalSide     = 1000
)

// terminalInputChunk bounds the bytes of standard input sent in one frame.
const terminalInputChunk = 32 << 10

// terminalFrame is one WebSocket frame of a terminal session with its type:
// websocket.BinaryFrame for the terminal's bytes, websocket.TextFrame for a
// control message.
type terminalFrame struct {
	kind byte
	data []byte
}

// terminalFrames sends and receives a terminalFrame keeping its type, which
// websocket.Message does not report on receipt.
var terminalFrames = websocket.Codec{
	Marshal: func(v any) ([]byte, byte, error) {
		f := v.(terminalFrame)
		return f.data, f.kind, nil
	},
	Unmarshal: func(data []byte, kind byte, v any) error {
		*v.(*terminalFrame) = terminalFrame{kind, data}
		return nil
	},
}

// localTerminal is parley's standard input, and the terminal behind it when
// it is one.
type localTerminal struct {
	in io.Reader
	// fd is the terminal's file descriptor, or -1 when standard input is
	// not a terminal.
	fd int
}

// localTerminalOf is the local terminal of standard input in.
func localTerminalOf(in io.Reader) *localTerminal {
	t := &localTerminal{in: in, fd: -1}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		t.fd = int(f.Fd())
	}
	return t
}

// size is the terminal's columns and rows, at most maxTerminalSide each,
// or 80 and 24 when standard input is not a terminal or its size is unknown.
func (t *localTerminal) size() (cols, rows int) {
	if t.fd >= 0 {
		if w, h, err := term.GetSize(t.fd); err == nil && w > 0 && h > 0 {
			return min(w, maxTerminalSide), min(h, maxTerminalSide)
		}
	}
	return defaultTerminalCols, defaultTerminalRows
}

// relay runs a terminal session on ws, opened at cols by rows: the local
// terminal in raw mode, so that every key reaches the sandbox, standard
// input sent as binary frames, binary frames written to stdout, and a resize
// frame sent when the local terminal's size changes. It ends when the hub
// closes the connection, with success, or when ctx ends, and restores the
// terminal on every way out.
func (t *localTerminal) relay(ctx context.Context, ws *websocket.Conn, stdout io.Writer, cols, rows int) error {
	if t.fd >= 0 {
		state, err := term.MakeRaw(t.fd)
		if err != nil {
			return clientError("cannot put the terminal in raw mode: %v", err)
		}
		defer term.Restore(t.fd, state)
		stopResize := t.watchResize(ws, cols, rows)
		defer stopResize()
	}
	stopClose := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stopClose()
	// The reader stays blocked on standard input once the session has
	// ended; the process exits soon after, and a test closes its pipe.
	go t.sendInput(ws)
	for {
		var f terminalFrame
		if err := terminalFrames.Receive(ws, &f); err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return clientError("the terminal's connection to the hub broke: %v", err)
		}
		if f.kind != websocket.BinaryFrame {
			continue
		}
		if _, err := stdout.Write(f.data); err != nil {
			return clientError("cannot write the terminal's output: %v", err)
		}
	}
}

// sendInput sends what standard input holds as binary frames, as it is
// read, until it ends or the connection fails.
func (t *localTerminal) sendInput(ws *websocket.Conn) {
	buf := make([]byte, terminalInputChunk)
	for {
		n, err := t.in.Read(buf)
		if n > 0 {
			if terminalFrames.Send(ws, terminalFrame{websocket.BinaryFrame, buf[:n]}) != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// watchResize sends a resize frame on ws each time the terminal's size,
// cols by rows at first, changes, until the returned function is called.
func (t *localTerminal) watchResize(ws *websocket.Conn, cols, rows int) (stop func()) {
	changes := make(chan os.Signal, 1)
	notifyResize(changes)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-changes:
				c, r := t.size()
				if c == cols && r == rows {
					continue
				}
				cols, rows = c, r
				if sendResize(ws, cols, rows) != nil {
					return
				}
			}
		}
	}()
	return func() {
		signal.Stop(changes)
		close(done)
	}
}

// sendResize tells the hub that the terminal is now cols by rows, in the
// text frame {"resize":{"cols":…,"rows":…}}.
func sendResize(ws *websocket.Conn, cols, rows int) error {
	type size struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	msg, err := json.Marshal(struct {
		Resize size `json:"resize"`
	}{size{cols, rows}})
	if err != nil {
		return err
	}
	return terminalFrames.Send(ws, terminalFrame{websocket.TextFrame, msg})
}
