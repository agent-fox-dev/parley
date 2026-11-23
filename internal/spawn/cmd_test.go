//go:build unix

package spawn_test

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/spawn"
)

// A command that has exited while a process it started still holds its
// output is waited for no longer than WaitDelay: what it wrote is kept and
// the error says that the wait was cut.
func TestCmd_StopsWaitingForTheOutputAtTheWaitDelay(t *testing.T) {
	t.Parallel()
	cmd := spawn.CommandContext(context.Background(), "/bin/sh", "-c", "echo out; sleep 30 &")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 200 * time.Millisecond
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	err := cmd.Run()
	if cmd.Process != nil {
		// The sleep, in the command's group.
		t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	}
	if took := time.Since(start); !errors.Is(err, exec.ErrWaitDelay) || out.String() != "out\n" || took > 5*time.Second {
		t.Errorf("Run = %v with output %q after %s, want exec.ErrWaitDelay with %q within 5s", err, out.String(), took.Round(time.Millisecond), "out\n")
	}
}

// Output answers the command's standard output, and a failure's
// *exec.ExitError holds its standard error; CombinedOutput answers both.
func TestCmd_AnswersTheOutputOfTheCommand(t *testing.T) {
	t.Parallel()
	script := "echo out; echo err >&2; exit 3"
	out, err := spawn.Command("/bin/sh", "-c", script).Output()
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 3 || string(out) != "out\n" || string(exit.Stderr) != "err\n" {
		t.Errorf("Output = %q, %v, want %q and exit status 3 with %q", out, err, "out\n", "err\n")
	}
	both, err := spawn.Command("/bin/sh", "-c", "echo out; echo err >&2").CombinedOutput()
	if err != nil || string(both) != "out\nerr\n" {
		t.Errorf("CombinedOutput = %q, %v, want %q", both, err, "out\nerr\n")
	}
}
