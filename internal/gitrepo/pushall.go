package gitrepo

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// PushBranchesRequest is one offer of several branches of a copy to the
// branches of the same names on origin.
type PushBranchesRequest struct {
	// Dir is the git directory of the copy the commits are in.
	Dir string
	// URL is origin's URL and Credential what git presents to it when
	// origin asks for one.
	URL        string
	Credential Credential
	// Heads maps every branch offered, without refs/heads/, to the id of
	// the commit offered to origin's branch of that name.
	Heads map[string]string
}

// BranchPush is what a push of several branches did to one of them.
type BranchPush struct {
	// Branch is the branch, without refs/heads/, and Commit the commit
	// offered to it.
	Branch string
	Commit string
	// Result is PushPushed or PushUpToDate when origin took the branch,
	// "" when it refused it.
	Result PushResult
	// Err is why origin refused the branch, nil when it took it: an error
	// matching ErrNonFastForward when origin's branch is not an ancestor
	// of the commit, a *PushError of class ErrOriginRejected when origin
	// itself refused the update, and of class ErrPushFailed otherwise.
	Err error
}

// PushBranches offers every branch of req.Heads to the branch of the same
// name on origin in one git push, each as the refspec
// <commit>:refs/heads/<branch>, so that git sends no other branch and no
// tag, forces nothing and deletes nothing, presenting req's credential when
// origin asks for one; git never prompts. Origin takes or refuses each
// branch on its own. The results, one for each branch git reported a status
// for, in the order of the branch names, are read from git's
// machine-readable output; a branch origin refused is a result, not an
// error. The error is not nil when git did not report every branch: a
// *PushError of the transport's class when it failed before any status,
// and, when ctx ends first, an error wrapping ctx's error, with git killed
// with every process it started and the statuses it reported before
// returned.
func (g Git) PushBranches(ctx context.Context, req PushBranchesRequest) ([]BranchPush, error) {
	branches := slices.Sorted(maps.Keys(req.Heads))
	if len(branches) == 0 {
		return nil, nil
	}
	args := []string{"-c", "credential.helper=", "--git-dir=" + req.Dir, "push", "--porcelain", "--no-follow-tags", "--", req.URL}
	for _, b := range branches {
		args = append(args, req.Heads[b]+":refs/heads/"+b)
	}
	out, stderr, err := g.runOutput(ctx, access{req.Credential, req.URL}, nil, args...)
	var results []BranchPush
	for _, b := range branches {
		flag, summary, reported := refStatus(out, "refs/heads/"+b)
		if !reported {
			continue
		}
		p := BranchPush{Branch: b, Commit: req.Heads[b]}
		switch {
		case flag == '=':
			p.Result = PushUpToDate
		case flag == ' ' || flag == '*':
			p.Result = PushPushed
		case flag == '!' && (strings.Contains(summary, "(non-fast-forward)") || strings.Contains(summary, "(fetch first)")):
			p.Err = fmt.Errorf("git push of %s: %w", b, ErrNonFastForward)
		case flag == '!' && strings.HasPrefix(summary, "[remote rejected]"):
			p.Err = &PushError{Branch: b, Class: ErrOriginRejected, Reason: rejectReason(summary, stderr), Err: err}
		default:
			p.Err = &PushError{Branch: b, Class: ErrPushFailed, Reason: fmt.Sprintf("git reported %q for the ref", summary), Err: err}
		}
		results = append(results, p)
	}
	switch {
	case err == nil || len(results) == len(branches):
		return results, nil
	case ctx.Err() != nil:
		return results, err
	}
	return results, &PushError{Branch: strings.Join(branches, ", "), Class: transportClass(stderr), Reason: transportReason(stderr), Err: err}
}
