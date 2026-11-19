package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The terminal of an attach.
const (
	// defaultCols and defaultRows are the size of a terminal whose request
	// names none.
	defaultCols = 80
	defaultRows = 24
	// maxTerminalSide is the most columns or rows a terminal may have.
	maxTerminalSide = 1000
	// terminalChunk is the most terminal output one binary frame carries.
	terminalChunk = 32 << 10
)

// attachShell is the command an attach runs: a new shell, never the main
// process.
var attachShell = []string{"/bin/sh"}

// attachRequest is an attach request once its query is judged.
type attachRequest struct {
	size runtime.TerminalSize
}

// validateAttach checks that r is a WebSocket upgrade and reads the size of
// its terminal into u, answering 400 invalid_request otherwise. It checks
// every header the upgrade needs, so that once the terminal is open the
// upgrade does not fail for a reason a client could have been told.
func validateAttach(r *http.Request, u *useRequest) error {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(r.Header, "Connection", "upgrade") ||
		r.Header.Get("Sec-WebSocket-Key") == "" ||
		r.Header.Get("Sec-WebSocket-Version") != "13" {
		return server.InvalidRequest("this route takes a WebSocket upgrade (version 13)")
	}
	query := r.URL.Query()
	cols, err := readTerminalSide(query, "cols", defaultCols)
	if err != nil {
		return err
	}
	rows, err := readTerminalSide(query, "rows", defaultRows)
	if err != nil {
		return err
	}
	u.attach = attachRequest{size: runtime.TerminalSize{Cols: cols, Rows: rows}}
	return nil
}

// headerHasToken reports whether one of the comma-separated values of the
// header name is token, in any case.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// readTerminalSide reads the query parameter name as a number of columns or
// rows, def when it is absent.
func readTerminalSide(query map[string][]string, name string, def uint32) (uint32, error) {
	values, ok := query[name]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(values[0])
	if err != nil || n < 1 || n > maxTerminalSide {
		return 0, server.InvalidRequest(name + ": must be an integer from 1 to " + strconv.Itoa(maxTerminalSide))
	}
	return uint32(n), nil
}

// attach opens a new shell on a terminal in the sandbox of u and serves it
// over the WebSocket r asks for. The terminal is opened before the upgrade,
// so that a runtime that cannot open it is still answered with the
// runtime-error envelope. Once the connection is upgraded nothing more is
// answered: every error after it is logged.
func (s *Service) attach(w http.ResponseWriter, r *http.Request, u useRequest) error {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// The runtime holds the sandbox's environment since its creation, so
	// none is sent.
	term, err := s.Client().ExecInteractive(ctx, u.rec.ID, runtime.ExecRequest{Command: attachShell, WorkDir: WorkDir}, u.attach.size)
	if err != nil {
		return err
	}
	defer func() { _ = term.Close() }()
	opened := u.act.start(OperationAttach)
	ws := websocket.Server{
		// A terminal client is no browser and sends no Origin; the caller
		// is authenticated by its Authorization header, as on every route.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(conn *websocket.Conn) {
			s.relay(ctx, conn, term, u.rec.ID, s.inputActivity(ctx, u.rec.ID, opened))
		},
	}
	ws.ServeHTTP(hijacker{w}, r)
	return nil
}

// hijacker lets websocket.Server take the connection of w, which it asks
// for as an http.Hijacker: the server's own writers wrap the connection's
// and reach it only through http.ResponseController.
type hijacker struct {
	http.ResponseWriter
}

func (h hijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(h.ResponseWriter).Hijack()
}

// inputActivity returns what an attach to the sandbox id, whose activity
// was last written at last, calls for each frame its caller sends: it
// writes the frame's time as activity once attachTouchInterval has passed
// since the last write. It is called from one goroutine.
func (s *Service) inputActivity(ctx context.Context, id string, last time.Time) func() {
	return func() {
		if now := s.deps.Now(); now.Sub(last) >= attachTouchInterval {
			last = now
			s.touch(ctx, id, now)
		}
	}
}

// relay copies the caller's binary frames on conn to the terminal's input
// and each chunk of the terminal's output to the caller as one binary
// frame, and applies the caller's resize frames, until one side ends; it
// calls input for each frame the caller sends. When the caller leaves, ctx
// ends or the hub begins to stop, the terminal is closed and nothing else.
// When the terminal ends or fails, the output it gave is sent and then the
// connection is closed.
func (s *Service) relay(ctx context.Context, conn *websocket.Conn, term runtime.Terminal, id string, input func()) {
	callerLeft := make(chan struct{})
	go func() {
		defer close(callerLeft)
		s.readCaller(ctx, conn, term, id, input)
	}()
	outputEnded := make(chan struct{})
	go func() {
		defer close(outputEnded)
		s.sendOutput(ctx, conn, term, id)
	}()
	select {
	case <-outputEnded:
	case <-callerLeft:
	case <-ctx.Done():
	case <-s.deps.Shutdown:
	}
	_ = term.Close()
	_ = conn.Close()
	<-callerLeft
	<-outputEnded
}

// terminalFrame is a frame the caller sent: its payload and whether it
// came as text.
type terminalFrame struct {
	data []byte
	text bool
}

// terminalFrames reads a frame with its type, which websocket.Message loses
// when it reads into bytes.
var terminalFrames = websocket.Codec{
	Marshal: websocket.Message.Marshal,
	Unmarshal: func(data []byte, payloadType byte, v any) error {
		f, ok := v.(*terminalFrame)
		if !ok {
			return websocket.ErrNotSupported
		}
		*f = terminalFrame{data: data, text: payloadType == websocket.TextFrame}
		return nil
	},
}

// resizeFrame is the one text frame an attach acts on.
type resizeFrame struct {
	Resize *struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	} `json:"resize"`
}

// readCaller writes the payload of every binary frame the caller sends to
// the terminal and applies every resize frame, ignoring any other text
// frame, until the caller closes the connection or a write fails. It calls
// input for every frame.
func (s *Service) readCaller(ctx context.Context, conn *websocket.Conn, term runtime.Terminal, id string, input func()) {
	for {
		var f terminalFrame
		if err := terminalFrames.Receive(conn, &f); err != nil {
			return
		}
		input()
		if !f.text {
			if _, err := term.Write(f.data); err != nil {
				return
			}
			continue
		}
		var frame resizeFrame
		if json.Unmarshal(f.data, &frame) != nil || frame.Resize == nil {
			continue
		}
		cols, rows := frame.Resize.Cols, frame.Resize.Rows
		if cols < 1 || cols > maxTerminalSide || rows < 1 || rows > maxTerminalSide {
			continue
		}
		if err := term.Resize(runtime.TerminalSize{Cols: uint32(cols), Rows: uint32(rows)}); err != nil {
			s.logTerminal(ctx, id, "resize", err)
		}
	}
}

// sendOutput sends each chunk the terminal writes to the caller as one
// binary frame, until the terminal ends or fails or a send fails.
func (s *Service) sendOutput(ctx context.Context, conn *websocket.Conn, term runtime.Terminal, id string) {
	buf := make([]byte, terminalChunk)
	for {
		n, err := term.Read(buf)
		if n > 0 {
			if sendErr := websocket.Message.Send(conn, buf[:n]); sendErr != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.logTerminal(ctx, id, "read", err)
			}
			return
		}
	}
}

// logTerminal logs a failure of the terminal of the sandbox id, its text
// cleared of the sandbox's secret values and then of the hub's.
func (s *Service) logTerminal(ctx context.Context, id, what string, err error) {
	// The caller may have left; the values are still needed for the log.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.logger().Warn("a sandbox terminal's "+what+" failed", "sandbox_id", id, "error", s.scrub(s.ScrubSandbox(ctx, id, err.Error())))
}
