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
// forward to the commit, up_to_date when it was at the commit already.
const (
	PushPushed   PushResult = "pushed"
	PushUpToDate PushResult = "up_to_date"
)

// ErrNonFastForward is matched by the error of a push origin's branch did
// not move for, because the branch is not an ancestor of the commit: the
// two diverged or origin's branch is ahead of it.
var ErrNonFastForward = errors.New("origin's branch is not an ancestor of the commit")

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
	// Force makes the push a forced one under a lease: origin's branch is
	// replaced only while origin does not have it. Git is never handed a
	// force that origin would apply whatever its branch holds.
	Force bool
}

// Push pushes req's commit to req's branch on origin in one git push of
// the single refspec <commit>:refs/heads/<branch>, so that no other branch
// and no tag is sent, presenting req's credential when origin asks for one;
// git never prompts. The result is read from the status git reports for the
// ref in its machine-readable output: a ref origin refused because its
// branch is not an ancestor of the commit gives an error matching
// ErrNonFastForward. Any other failure is an error holding what git wrote.
func (g Git) Push(ctx context.Context, req PushRequest) (PushResult, error) {
	ref := "refs/heads/" + req.Branch
	args := []string{"-c", "credential.helper=", "--git-dir=" + req.Dir, "push", "--porcelain", "--no-follow-tags"}
	if req.Force {
		// An empty expected value is git's lease that origin must not have
		// the ref.
		args = append(args, "--force-with-lease="+ref+":")
	}
	out, err := g.runOutput(ctx, access{req.Credential, req.URL}, nil, append(args, "--", req.URL, req.Commit+":"+ref)...)
	flag, summary, reported := refStatus(out, ref)
	switch {
	case reported && flag == '=' && err == nil:
		return PushUpToDate, nil
	case reported && (flag == ' ' || flag == '*') && err == nil:
		return PushPushed, nil
	case reported && flag == '!' && (strings.Contains(summary, "(non-fast-forward)") || strings.Contains(summary, "(fetch first)")):
		return "", fmt.Errorf("git push of %s: %w", req.Branch, ErrNonFastForward)
	case err != nil:
		return "", err
	}
	return "", fmt.Errorf("git push of %s reported %q for the ref", req.Branch, summary)
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
