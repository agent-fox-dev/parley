package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// PushResult is what a push did to origin's branch.
type PushResult string

// The results of a push: pushed when origin's branch was created or moved
// forward to the commit, forced when a forced push replaced it with a commit
// that does not descend from it, up_to_date when it was at the commit
// already.
const (
	PushPushed   PushResult = "pushed"
	PushForced   PushResult = "forced"
	PushUpToDate PushResult = "up_to_date"
)

// ErrNonFastForward is matched by the error of a push origin's branch did
// not move for, because the branch is not an ancestor of the commit: the
// two diverged or origin's branch is ahead of it.
var ErrNonFastForward = errors.New("origin's branch is not an ancestor of the commit")

// ErrOriginMoved is matched by the error of a forced push origin's branch
// did not move for, because the branch is not in the state the lease
// names: it is at another commit, it exists although the lease says origin
// lacks it, or it is gone although the lease names a commit.
var ErrOriginMoved = errors.New("origin's branch is not where the lease expects it")

// The classes of a push that failed for a reason other than the state of
// origin's branch. A PushError matches exactly one of them with errors.Is.
var (
	// ErrOriginRejected: origin itself refused the update of the branch,
	// by a branch rule or a server-side hook, for example.
	ErrOriginRejected = errors.New("origin refused the update of the branch")
	// ErrUnauthenticated: origin asked for a credential and the push had
	// none, or origin did not take the one presented.
	ErrUnauthenticated = errors.New("origin could not authenticate the hub")
	// ErrForbidden: origin took the credential but does not let it push to
	// the repository.
	ErrForbidden = errors.New("origin does not let the credential push to the repository")
	// ErrRepositoryNotFound: origin has no repository at the URL, or the
	// credential cannot see it.
	ErrRepositoryNotFound = errors.New("origin has no such repository, or the credential cannot see it")
	// ErrUnreachable: origin's name did not resolve, the connection failed,
	// or TLS (for ssh, the host key) did.
	ErrUnreachable = errors.New("origin cannot be reached")
	// ErrPushFailed: the push failed in any other way.
	ErrPushFailed = errors.New("the push failed")
)

// PushError is the error of a push that failed for one of the classes
// above, read from the status git reported for the ref when it reported
// one and from what the transport said otherwise.
type PushError struct {
	// Branch is the branch pushed to.
	Branch string
	// Class is the class of the failure, one of the errors above.
	Class error
	// Reason is what origin or the transport said: for ErrOriginRejected
	// origin's reason for the ref with the text its hooks wrote, otherwise
	// git's standard error without its boilerplate, one line per line.
	Reason string
	// Err is git's error.
	Err error
}

func (e *PushError) Error() string {
	return fmt.Sprintf("git push of %s: %v: %s", e.Branch, e.Class, e.Reason)
}

// Unwrap gives the class and git's error, so that errors.Is matches both.
func (e *PushError) Unwrap() []error { return []error{e.Class, e.Err} }

// PushRequest is one push: one commit of a copy to one branch of origin.
type PushRequest struct {
	// Dir is the git directory of the copy the commit is in.
	Dir string
	// URL is origin's URL and Credential what git presents to it when
	// origin asks for one.
	URL        string
	Credential Credential
	// Commit is the id of the commit pushed and Branch the name, without
	// refs/heads/, of origin's branch it is pushed to.
	Commit string
	Branch string
	// Force makes the push a forced one under Lease: origin's branch is
	// replaced only while it is still what Lease names, the id of the
	// commit origin's branch must have, or "" for a branch origin must not
	// have. Git is never handed a force that origin would apply whatever its
	// branch holds. Lease is ignored without Force.
	Force bool
	Lease string
}

// Push pushes req's commit to req's branch on origin in one git push of
// the single refspec <commit>:refs/heads/<branch>, so that no other branch
// and no tag is sent, presenting req's credential when origin asks for one;
// git never prompts. A forced push hands git the lease for that one ref
// explicitly. The result is read from the status git reports for the ref in
// its machine-readable output: a ref origin refused because its branch is
// not an ancestor of the commit gives an error matching ErrNonFastForward,
// one refused because the lease is stale an error matching ErrOriginMoved.
// Git reports a branch already at the commit as up to date before it judges
// the lease. Every other failure is a *PushError of its class; when ctx ends
// first git is killed with every process it started and the error wraps
// ctx's error instead.
func (g Git) Push(ctx context.Context, req PushRequest) (PushResult, error) {
	ref := "refs/heads/" + req.Branch
	args := []string{"-c", "credential.helper=", "--git-dir=" + req.Dir, "push", "--porcelain", "--no-follow-tags"}
	if req.Force {
		// An empty expected value is git's lease that origin must not have
		// the ref.
		args = append(args, "--force-with-lease="+ref+":"+req.Lease)
	}
	out, stderr, err := g.runOutput(ctx, access{req.Credential, req.URL}, nil, append(args, "--", req.URL, req.Commit+":"+ref)...)
	flag, summary, reported := refStatus(out, ref)
	switch {
	case reported && flag == '=' && err == nil:
		return PushUpToDate, nil
	case reported && (flag == ' ' || flag == '*') && err == nil:
		return PushPushed, nil
	case reported && flag == '+' && err == nil:
		return PushForced, nil
	case reported && flag == '!' && strings.Contains(summary, "(stale info)"):
		return "", fmt.Errorf("git push of %s: %w", req.Branch, ErrOriginMoved)
	case reported && flag == '!' && (strings.Contains(summary, "(non-fast-forward)") || strings.Contains(summary, "(fetch first)")):
		return "", fmt.Errorf("git push of %s: %w", req.Branch, ErrNonFastForward)
	case reported && flag == '!' && strings.HasPrefix(summary, "[remote rejected]"):
		return "", &PushError{Branch: req.Branch, Class: ErrOriginRejected, Reason: rejectReason(summary, stderr), Err: err}
	case err != nil && ctx.Err() != nil:
		return "", err
	case err != nil && !reported:
		return "", &PushError{Branch: req.Branch, Class: transportClass(stderr), Reason: transportReason(stderr), Err: err}
	}
	if err == nil {
		err = errors.New("git push reported no failure")
	}
	return "", &PushError{Branch: req.Branch, Class: ErrPushFailed, Reason: fmt.Sprintf("git reported %q for the ref", summary), Err: err}
}

// rejectReason is origin's reason for refusing the ref: the reason of the
// ref's status, "[remote rejected] (<reason>)", followed by the text
// origin's hooks wrote, which git shows on the lines starting with remote:.
func rejectReason(summary, stderr string) string {
	reason := strings.TrimPrefix(summary, "[remote rejected]")
	reason = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(reason), "("), ")")
	var said []string
	for line := range strings.Lines(stderr) {
		if text, ok := strings.CutPrefix(line, "remote:"); ok && strings.TrimSpace(text) != "" {
			said = append(said, strings.TrimSpace(text))
		}
	}
	if len(said) == 0 {
		return reason
	}
	return reason + ": " + strings.Join(said, "\n")
}

// transportClasses map what git's HTTPS transport (curl) and its SSH
// transport (ssh, and the forge's message behind it) write about a failure
// to its class, tried in this order. Reachability is judged before the
// repository's absence, since the text of a failed name resolution,
// connection, proxy or TLS handshake may hold a plain "not found" of its
// own.
var transportClasses = []struct {
	class error
	says  []string
}{
	{ErrUnauthenticated, []string{
		"Authentication failed", "could not read Username", "could not read Password",
		"returned error: 401", "Permission denied (publickey", "Permission denied (password",
	}},
	{ErrForbidden, []string{"returned error: 403", "not allowed to push", "Permission to "}},
	{ErrUnreachable, []string{
		"Could not resolve host", "Could not resolve proxy", "Failed to connect", "Could not connect",
		"Couldn't connect", "Connection refused", "Connection timed out", "Connection reset", "Connection closed",
		"timed out", "No route to host", "Network is unreachable", "SSL", "TLS", "certificate",
		"Host key verification failed",
	}},
	{ErrRepositoryNotFound, []string{
		"returned error: 404", "not found", "could not be found", "does not appear to be a git repository",
	}},
}

// transportClass is the class of a push that failed before git reported a
// status for the ref, read from its standard error.
func transportClass(stderr string) error {
	for _, c := range transportClasses {
		for _, says := range c.says {
			if strings.Contains(stderr, says) {
				return c.class
			}
		}
	}
	return ErrPushFailed
}

// transportReason is git's standard error without its blank lines, the
// fatal: and remote: prefixes, and the advice it gives after any failure
// of the SSH transport.
func transportReason(stderr string) string {
	var lines []string
	for line := range strings.Lines(stderr) {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "fatal:"), "remote:"))
		switch line {
		case "", "Please make sure you have the correct access rights", "and the repository exists.":
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// refStatus is the status line git push --porcelain reports for ref in out,
// "<flag>\t<source>:<ref>\t<summary>": its flag and its summary; reported is
// false when out has no line for ref.
func refStatus(out []byte, ref string) (flag byte, summary string, reported bool) {
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(fields) != 3 || len(fields[0]) != 1 {
			continue
		}
		if _, dst, ok := strings.Cut(fields[1], ":"); ok && dst == ref {
			return fields[0][0], fields[2], true
		}
	}
	return 0, "", false
}
