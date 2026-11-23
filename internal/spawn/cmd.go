// Package spawn runs a command as os/exec does, but on macOS starts it with
// posix_spawn instead of forking the calling process. A fork marks every
// page of the caller copy-on-write, and a large, busy Go process then takes
// a page fault on each page it writes while the child starts: milliseconds
// of the machine's CPU per process for the hub, which runs git thousands of
// times (docs/adr/12-start-git-without-forking-the-hub-on-macos.md).
// Elsewhere, and for what posix_spawn is not used for, it is os/exec.
package spawn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Cmd is a command to run, configured through the exec.Cmd it embeds and
// run through its own Start, Wait, Run, Output, CombinedOutput, StdinPipe
// and StdoutPipe. They keep the semantics of exec.Cmd's: the pipes, the
// copying of a reader or writer that is no file, Cancel on the end of the
// context, WaitDelay and the *exec.ExitError of a failed exit. The other
// methods of exec.Cmd, such as StderrPipe, are os/exec's alone and must not
// be mixed with these.
type Cmd struct {
	*exec.Cmd
	ctx context.Context

	// What a spawned process keeps from Start to Wait; forked says that
	// Start left it to os/exec instead.
	forked     bool
	stdinPipe  *os.File // the child's end of StdinPipe
	stdoutPipe *os.File // the child's end of StdoutPipe
	closeAfter []io.Closer
	goroutines []func() error
	copied     chan struct{} // closed when the copying goroutines have ended
	copyErr    error         // the first of their errors, set before copied closes
	ctxResult  chan error
	started    bool
	waited     bool
}

// Command is exec.Command as a Cmd.
func Command(name string, arg ...string) *Cmd { return &Cmd{Cmd: exec.Command(name, arg...)} }

// CommandContext is exec.CommandContext as a Cmd.
func CommandContext(ctx context.Context, name string, arg ...string) *Cmd {
	return &Cmd{Cmd: exec.CommandContext(ctx, name, arg...), ctx: ctx}
}

// spawns reports whether c is started by spawn: on a platform that has it,
// for a command that asks nothing of the process that only os/exec does,
// and unless a pipe of os/exec's was taken already.
func (c *Cmd) spawns() bool {
	return available && !c.forked && len(c.ExtraFiles) == 0 && supported(c.SysProcAttr)
}

// Run starts the command and waits for it.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Output runs the command and returns its standard output. Without a
// Stderr of the caller's, a failure's *exec.ExitError holds the start of
// its standard error.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var out bytes.Buffer
	c.Stdout = &out
	var errOut *limitedBuffer
	if c.Stderr == nil {
		errOut = &limitedBuffer{limit: 32 << 10}
		c.Stderr = errOut
	}
	err := c.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && errOut != nil {
		exit.Stderr = errOut.buf.Bytes()
	}
	return out.Bytes(), err
}

// CombinedOutput runs the command and returns its standard output and
// standard error together.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return b.Bytes(), err
}

// StdinPipe is exec.Cmd's StdinPipe: the pipe to the command's standard
// input, closed by Wait.
func (c *Cmd) StdinPipe() (io.WriteCloser, error) {
	if !c.spawns() {
		c.forked = true
		return c.Cmd.StdinPipe()
	}
	if c.Stdin != nil || c.stdinPipe != nil {
		return nil, errors.New("exec: Stdin already set")
	}
	if c.started {
		return nil, errors.New("exec: StdinPipe after process started")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c.stdinPipe = pr
	w := &closeOnce{File: pw}
	c.closeAfter = append(c.closeAfter, w)
	return w, nil
}

// StdoutPipe is exec.Cmd's StdoutPipe: the pipe from the command's
// standard output, closed by Wait once the process has exited, so that it
// is read to its end before Wait.
func (c *Cmd) StdoutPipe() (io.ReadCloser, error) {
	if !c.spawns() {
		c.forked = true
		return c.Cmd.StdoutPipe()
	}
	if c.Stdout != nil || c.stdoutPipe != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if c.started {
		return nil, errors.New("exec: StdoutPipe after process started")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c.stdoutPipe = pw
	c.closeAfter = append(c.closeAfter, pr)
	return pr, nil
}

// Start starts the command.
func (c *Cmd) Start() error {
	if !c.spawns() {
		c.forked = true
		return c.Cmd.Start()
	}
	if c.started {
		return errors.New("exec: already started")
	}
	c.started = true
	fail := func(err error) error {
		closeAll(c.closeAfter)
		c.closeAfter = nil
		c.goroutines = nil
		return err
	}
	if c.Err != nil {
		return fail(c.Err)
	}
	if c.Path == "" {
		return fail(errors.New("exec: no command"))
	}
	if c.ctx != nil {
		if err := c.ctx.Err(); err != nil {
			return fail(err)
		}
	}
	var child [3]*os.File
	var closeAfterStart []io.Closer
	defer func() { closeAll(closeAfterStart) }()
	var err error
	if child[0], err = c.childStdin(&closeAfterStart); err != nil {
		return fail(err)
	}
	if child[1], err = c.childOutput(c.Stdout, c.stdoutPipe, &closeAfterStart); err != nil {
		return fail(err)
	}
	if c.Stderr != nil && sameWriter(c.Stderr, c.Stdout) {
		child[2] = child[1]
	} else if child[2], err = c.childOutput(c.Stderr, nil, &closeAfterStart); err != nil {
		return fail(err)
	}
	group := c.SysProcAttr != nil && c.SysProcAttr.Setpgid
	pid, err := spawn(c.Path, c.Args, c.Environ(), c.Dir, child, group)
	if err != nil {
		return fail(&fs.PathError{Op: "fork/exec", Path: c.Path, Err: err})
	}
	if c.Process, err = os.FindProcess(pid); err != nil {
		return fail(err)
	}
	if len(c.goroutines) > 0 {
		c.copied = make(chan struct{})
		go func(fns []func() error) {
			var mu sync.Mutex
			var first error
			var wg sync.WaitGroup
			for _, fn := range fns {
				wg.Go(func() {
					if err := fn(); err != nil {
						mu.Lock()
						if first == nil {
							first = err
						}
						mu.Unlock()
					}
				})
			}
			wg.Wait()
			c.copyErr = first
			close(c.copied)
		}(c.goroutines)
	}
	if c.ctx != nil && c.ctx.Done() != nil {
		// Unbuffered: the watcher reports only once Wait asks.
		c.ctxResult = make(chan error)
		go c.watchCtx()
	}
	return nil
}

// childStdin is the file the process reads as its standard input.
func (c *Cmd) childStdin(closeAfterStart *[]io.Closer) (*os.File, error) {
	switch r := c.Stdin.(type) {
	case nil:
		if c.stdinPipe != nil {
			*closeAfterStart = append(*closeAfterStart, c.stdinPipe)
			return c.stdinPipe, nil
		}
		f, err := os.Open(os.DevNull)
		if err != nil {
			return nil, err
		}
		*closeAfterStart = append(*closeAfterStart, f)
		return f, nil
	case *os.File:
		return r, nil
	default:
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		*closeAfterStart = append(*closeAfterStart, pr)
		c.closeAfter = append(c.closeAfter, pw)
		c.goroutines = append(c.goroutines, func() error {
			_, err := io.Copy(pw, r)
			// The process may exit, or close its input, before it has read
			// everything: that is no failure of the copy.
			if errors.Is(err, syscall.EPIPE) {
				err = nil
			}
			if closeErr := pw.Close(); err == nil && !errors.Is(closeErr, os.ErrClosed) {
				err = closeErr
			}
			return err
		})
		return pr, nil
	}
}

// childOutput is the file the process writes w to: the child's end of a
// pipe from StdoutPipe, the null device for no writer, w itself for a file,
// and otherwise a pipe copied to w.
func (c *Cmd) childOutput(w io.Writer, pipe *os.File, closeAfterStart *[]io.Closer) (*os.File, error) {
	if pipe != nil {
		*closeAfterStart = append(*closeAfterStart, pipe)
		return pipe, nil
	}
	switch w := w.(type) {
	case nil:
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		*closeAfterStart = append(*closeAfterStart, f)
		return f, nil
	case *os.File:
		return w, nil
	default:
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		*closeAfterStart = append(*closeAfterStart, pw)
		c.closeAfter = append(c.closeAfter, pr)
		c.goroutines = append(c.goroutines, func() error {
			_, err := io.Copy(w, pr)
			_ = pr.Close()
			return err
		})
		return pw, nil
	}
}

// watchCtx cancels the process when its context ends before Wait has seen
// it exit, and reports what the cancel did.
func (c *Cmd) watchCtx() {
	select {
	case c.ctxResult <- nil:
		return
	case <-c.ctx.Done():
	}
	var err error
	if c.Cancel != nil {
		if cancelErr := c.Cancel(); cancelErr == nil {
			err = c.ctx.Err()
		} else if !errors.Is(cancelErr, os.ErrProcessDone) && !errors.Is(cancelErr, syscall.ESRCH) {
			err = cancelErr
		}
	} else if killErr := c.Process.Kill(); killErr == nil {
		err = c.ctx.Err()
	}
	c.ctxResult <- err
}

// Wait waits for the command to exit and for the copying of its input and
// output to end, as exec.Cmd's Wait does.
func (c *Cmd) Wait() error {
	if c.forked || !c.started {
		return c.Cmd.Wait()
	}
	if c.waited {
		return errors.New("exec: Wait was already called")
	}
	c.waited = true
	state, err := c.Process.Wait()
	if err == nil && !state.Success() {
		err = &exec.ExitError{ProcessState: state}
	}
	c.ProcessState = state
	if c.ctxResult != nil {
		if ctxErr := <-c.ctxResult; err == nil && ctxErr != nil {
			err = ctxErr
		}
	}
	if copyErr := c.awaitCopying(); err == nil {
		err = copyErr
	}
	closeAll(c.closeAfter)
	c.closeAfter = nil
	return err
}

// awaitCopying waits for the copying goroutines, at most WaitDelay when it
// is set; past it their pipes are closed and the error is
// exec.ErrWaitDelay.
func (c *Cmd) awaitCopying() error {
	if c.copied == nil {
		return nil
	}
	if c.WaitDelay == 0 {
		<-c.copied
		return c.copyErr
	}
	timer := time.NewTimer(c.WaitDelay)
	defer timer.Stop()
	select {
	case <-c.copied:
		return c.copyErr
	case <-timer.C:
		// What the copying fails with now is the pipes closed under it.
		closeAll(c.closeAfter)
		c.closeAfter = nil
		<-c.copied
		return exec.ErrWaitDelay
	}
}

// closeOnce is the parent's end of StdinPipe, which the caller and Wait
// may both close.
type closeOnce struct {
	*os.File
	once sync.Once
	err  error
}

func (c *closeOnce) Close() error {
	c.once.Do(func() { c.err = c.File.Close() })
	return c.err
}

func closeAll(cs []io.Closer) {
	for _, c := range cs {
		_ = c.Close()
	}
}

// sameWriter reports whether a and b are the same writer, false for a
// writer that cannot be compared.
func sameWriter(a, b io.Writer) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// limitedBuffer keeps the first limit bytes written to it.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
