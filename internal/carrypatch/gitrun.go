package carrypatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// DefaultGitDeadline is the deadline of every git operation of carry-patch
// work that has no shorter one of its own: an upstream fetch of a large
// project may take minutes, and one that hangs must not hold the
// workspace for longer than this.
const DefaultGitDeadline = 10 * time.Minute

// ErrGitTimeout is matched by the error of a git operation that did not end
// before its deadline. Such an error matches gitrepo.ErrDeadline too, the
// deadline error of internal/gitrepo, so that a caller may test for either.
var ErrGitTimeout error = gitTimeout{}

type gitTimeout struct{}

func (gitTimeout) Error() string { return "git did not finish within its deadline" }

func (gitTimeout) Is(target error) bool { return target == gitrepo.ErrDeadline }

// RunGit runs op, one git operation of internal/gitrepo, the way every git
// operation of carry-patch work runs, so that none sets a deadline or
// cleans a failure's text of its own:
//
//   - op gets a deadline of its own, limit when it is positive and shorter
//     than DefaultGitDeadline, else DefaultGitDeadline. Its context ends
//     then, which kills git with every process it started (internal/gitrepo
//     runs each in a process group of its own), and op receives the
//     deadline too, for a gitrepo operation that takes one. The error of an
//     operation cut off by it matches ErrGitTimeout; one cut off by ctx
//     wraps ctx's error instead.
//   - The text of any other error is cleaned with gitrepo.SanitizeGitText:
//     a URL's user information reads ***@ and the user name and password of
//     cred, the credential the operation presents, are removed. The error
//     still matches what op's error matches (errors.Is, errors.As).
func RunGit(ctx context.Context, limit time.Duration, cred gitrepo.Credential, op func(ctx context.Context, deadline time.Duration) error) error {
	deadline := gitDeadline(limit)
	opCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	err := op(opCtx, deadline)
	if err == nil {
		return nil
	}
	if ctx.Err() == nil && (errors.Is(opCtx.Err(), context.DeadlineExceeded) || errors.Is(err, gitrepo.ErrDeadline)) {
		return fmt.Errorf("%w (%v)", ErrGitTimeout, deadline)
	}
	return &cleanedError{text: gitrepo.SanitizeGitText(err.Error(), cred.Username, cred.Password), err: err}
}

// gitDeadline is the deadline a git operation given limit runs under:
// limit when it is positive and shorter than DefaultGitDeadline, else
// DefaultGitDeadline.
func gitDeadline(limit time.Duration) time.Duration {
	if limit > 0 && limit < DefaultGitDeadline {
		return limit
	}
	return DefaultGitDeadline
}

// cleanedError is an error of git whose text was cleaned of credentials:
// it shows the cleaned text and matches what the original matches.
type cleanedError struct {
	text string
	err  error
}

func (e *cleanedError) Error() string { return e.text }

func (e *cleanedError) Unwrap() error { return e.err }
