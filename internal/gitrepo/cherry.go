package gitrepo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// CherryCommit is a commit of a branch as git cherry compares it with an
// upstream: Equivalent when the upstream holds a commit that makes the same
// change, the same patch id, whatever its id.
type CherryCommit struct {
	ID         string
	Equivalent bool
}

// Cherry lists, oldest first, the commits that the commit of the full id
// head reaches and that of upstream does not, each with whether upstream
// holds an equivalent, as git cherry compares them. Merge commits are left
// out, as git cherry leaves them out. Both ends must be full ids, so that no
// revision is ever read as an option; another value is ErrNotFound and
// runs no git.
func (r *Repo) Cherry(ctx context.Context, upstream, head string) ([]CherryCommit, error) {
	upstream, head, err := rangeEnds(upstream, head)
	if err != nil {
		return nil, err
	}
	out, err := r.git(ctx, "cherry", upstream, head)
	if err != nil {
		return nil, err
	}
	var commits []CherryCommit
	for line := range strings.Lines(string(out)) {
		mark, id, ok := strings.Cut(strings.TrimRight(line, "\n"), " ")
		if !ok || (mark != "+" && mark != "-") || !isHexID(id, idLen) {
			return nil, fmt.Errorf("cherry answered %q", line)
		}
		commits = append(commits, CherryCommit{ID: id, Equivalent: mark == "-"})
	}
	return commits, nil
}

// Subjects lists, newest first, the subject lines of the commits that the
// commit of the full id to reaches and that of from does not, every commit
// to reaches when from is ""; when limit is above 0, the newest limit of
// them. The ends must be full ids, so that no revision is ever read as an
// option; another value is ErrNotFound and runs no git.
func (r *Repo) Subjects(ctx context.Context, from, to string, limit int) ([]string, error) {
	start := from
	if from == "" {
		// Any full id stands in for the missing end while the ends are
		// checked; the listing is then of to alone.
		start = to
	}
	start, to, err := rangeEnds(start, to)
	if err != nil {
		return nil, err
	}
	revs := start + ".." + to
	if from == "" {
		revs = to
	}
	args := []string{"rev-list", "--no-commit-header", "--format=%s"}
	if limit > 0 {
		args = append(args, "--max-count="+strconv.Itoa(limit))
	}
	out, err := r.git(ctx, append(args, revs)...)
	if err != nil {
		return nil, err
	}
	var subjects []string
	for line := range strings.Lines(string(out)) {
		subjects = append(subjects, strings.TrimSuffix(line, "\n"))
	}
	return subjects, nil
}
