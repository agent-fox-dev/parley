package gitrepo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// RefUpdate is one ref of a ref transaction: Ref, a full ref name, moves
// from Old to New. Old "" means the ref must not exist, so that the update
// creates it; New "" deletes it.
type RefUpdate struct {
	Ref, Old, New string
}

// RefError is the error of a ref transaction that one of its refs failed:
// Err is ErrRefMoved when Ref is not at its expected value, exists though
// it is to be created, is missing though it is to be updated or deleted,
// or is held by another writer; it is ErrRefNameConflict when another ref's
// name is in the way of Ref's.
type RefError struct {
	Ref string
	Err error
}

func (e *RefError) Error() string { return e.Ref + ": " + e.Err.Error() }

// Unwrap returns Err, so that errors.Is matches ErrRefMoved or
// ErrRefNameConflict.
func (e *RefError) Unwrap() error { return e.Err }

// RefTx is a ref transaction PrepareRefs has prepared: every ref of it is
// locked at its expected value and none has moved. Commit moves them all,
// Abort none; the one that comes first ends the transaction, and until then
// every other writer of those refs fails as for a moved ref. It is used
// from one goroutine.
type RefTx struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	stderr *limitedBuffer
	home   string
	ended  bool
}

// PrepareRefs runs the first phase of one ref transaction of git's over
// updates: it checks every expected value and locks every ref, and moves
// none. When an expectation fails, nothing is locked and the error is a
// *RefError naming the first such ref in the order of updates; any other
// failure is git's, such as a new value no object of the repository has.
// The repository's objects are read as the Repo reads them, so a Repo
// opened with a quarantine as an alternate object directory can point refs
// at the quarantine's objects.
//
// ctx bounds the preparation alone: once prepared, the transaction is
// ended by Commit or Abort, never by ctx, because a git process killed
// while it holds the locks would leave them behind.
func (r *Repo) PrepareRefs(ctx context.Context, updates []RefUpdate) (*RefTx, error) {
	script, err := refScript(updates)
	if err != nil {
		return nil, err
	}
	home, err := privateDir(false)
	if err != nil {
		return nil, err
	}
	// A transaction that deletes a packed ref holds the packed refs' lock
	// until it ends, and git waits one second for that lock by default:
	// waiting longer has a transaction deleting another packed ref wait for
	// the first one's commit rather than fail.
	cmd := r.g.command(context.WithoutCancel(ctx), home, access{},
		[]string{"-c", "core.packedRefsTimeout=" + packedRefsTimeout, "--git-dir=" + r.dir, "update-ref", "--stdin"})
	t := &RefTx{cmd: cmd, stderr: &limitedBuffer{limit: stderrLimit}, home: home}
	cmd.Stderr = t.stderr
	if t.stdin, err = cmd.StdinPipe(); err == nil {
		var out io.ReadCloser
		if out, err = cmd.StdoutPipe(); err == nil {
			t.out = bufio.NewReader(out)
			err = cmd.Start()
		}
	}
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("git update-ref: %w", err)
	}
	prepared := make(chan error, 1)
	go func() {
		if _, err := io.WriteString(t.stdin, script); err != nil {
			prepared <- err
			return
		}
		if err := t.expect("start"); err != nil {
			prepared <- err
			return
		}
		prepared <- t.expect("prepare")
	}()
	select {
	case err = <-prepared:
	case <-ctx.Done():
		// git aborts a transaction whose input ends before its commit.
		_ = t.stdin.Close()
		err = errors.Join(ctx.Err(), <-prepared)
	}
	if err == nil {
		return t, nil
	}
	waitErr := t.end()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("git update-ref: %w", ctxErr)
	}
	return nil, r.refTxFailure(ctx, updates, waitErr)
}

// refScript is the input of git update-ref --stdin up to the prepare.
func refScript(updates []RefUpdate) (string, error) {
	if len(updates) == 0 {
		return "", errors.New("a ref transaction needs at least one ref")
	}
	var b strings.Builder
	b.WriteString("start\n")
	for _, u := range updates {
		// A space or a line break would end a field of git's input early.
		if u.Ref == "" || strings.ContainsFunc(u.Ref+u.Old+u.New, func(c rune) bool { return c <= ' ' || c == 0x7f }) {
			return "", fmt.Errorf("ref update %q cannot be written to git", u.Ref)
		}
		switch {
		case u.Old == "" && u.New == "":
			return "", fmt.Errorf("ref update %s has neither an old nor a new value", u.Ref)
		case u.Old == "":
			fmt.Fprintf(&b, "create %s %s\n", u.Ref, u.New)
		case u.New == "":
			fmt.Fprintf(&b, "delete %s %s\n", u.Ref, u.Old)
		default:
			fmt.Fprintf(&b, "update %s %s %s\n", u.Ref, u.New, u.Old)
		}
	}
	b.WriteString("prepare\n")
	return b.String(), nil
}

// packedRefsTimeout is how long, in milliseconds, a ref transaction waits
// for the lock of the packed refs that another transaction holds.
const packedRefsTimeout = "10000"

// lockedRef finds the ref a failed prepare names in git's message.
var lockedRef = regexp.MustCompile(`cannot lock ref '([^']+)'`)

// refTxFailure is the error of a prepare that failed with err. The refs are
// read again rather than git's message matched, whose wording changes
// between versions: the first update whose ref is not as it expects moved.
// A ref no expectation fails was held by another writer when git's message
// says that ref's lock exists, and in the way of another ref's name when
// the update creates it and such a ref exists. A lock held on no ref of
// the transaction, such as that of the packed refs past the wait, moved no
// ref: it is git's error.
func (r *Repo) refTxFailure(ctx context.Context, updates []RefUpdate, err error) error {
	refs := make([]string, len(updates))
	for i, u := range updates {
		refs[i] = u.Ref
	}
	current, readErr := r.RefValues(ctx, refs)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	for _, u := range updates {
		if id, found := current[u.Ref]; found != (u.Old != "") || id != u.Old {
			return &RefError{Ref: u.Ref, Err: ErrRefMoved}
		}
	}
	if strings.Contains(err.Error(), ".lock': File exists") {
		if m := lockedRef.FindStringSubmatch(err.Error()); m != nil && slices.Contains(refs, m[1]) {
			return &RefError{Ref: m[1], Err: ErrRefMoved}
		}
		return err
	}
	for _, u := range updates {
		if u.Old != "" {
			continue
		}
		if blocked, blockErr := r.nameBlocked(ctx, u.Ref); blockErr == nil && blocked {
			return &RefError{Ref: u.Ref, Err: ErrRefNameConflict}
		}
	}
	return err
}

// Commit moves every ref of the transaction to its new value. It fails
// once the transaction has ended.
func (t *RefTx) Commit() error {
	if t.ended {
		return errors.New("git update-ref: the transaction has ended")
	}
	_, err := io.WriteString(t.stdin, "commit\n")
	if err == nil {
		err = t.expect("commit")
	}
	if waitErr := t.end(); err == nil {
		err = waitErr
	}
	if err != nil {
		return fmt.Errorf("git update-ref: commit: %w", err)
	}
	return nil
}

// Abort ends the transaction with no ref moved and every lock released. It
// does nothing once the transaction has ended, so that it can be deferred.
func (t *RefTx) Abort() error {
	if t.ended {
		return nil
	}
	_, _ = io.WriteString(t.stdin, "abort\n")
	if err := t.end(); err != nil {
		return fmt.Errorf("git update-ref: abort: %w", err)
	}
	return nil
}

// expect reads git's answer to the command cmd, "<cmd>: ok".
func (t *RefTx) expect(cmd string) error {
	line, err := t.out.ReadString('\n')
	if err != nil {
		return fmt.Errorf("%s: no answer from git", cmd)
	}
	if strings.TrimSpace(line) != cmd+": ok" {
		return fmt.Errorf("%s: git answered %q", cmd, strings.TrimSpace(line))
	}
	return nil
}

// end closes git's input, which ends a transaction that is still open with
// nothing moved, waits for git and removes its private directory. The
// error carries git's standard error.
func (t *RefTx) end() error {
	t.ended = true
	_ = t.stdin.Close()
	_, _ = io.Copy(io.Discard, t.out)
	err := t.cmd.Wait()
	_ = os.RemoveAll(t.home)
	if err != nil {
		return fmt.Errorf("git update-ref: %w: %s", err, strings.TrimSpace(t.stderr.String()))
	}
	return nil
}
