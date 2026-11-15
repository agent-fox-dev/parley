package gitrepo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rebase replays the commits that the commit of the full id head reaches
// and that of onto does not, oldest first, on top of onto, and returns the
// last commit it made, the new head. Each new commit keeps the author, with
// the author's time and zone, and the message of the commit it replays,
// and has the hub as committer at the time at (HubSignature). A commit
// that changes nothing on top of what it is replayed onto is left out; a
// commit whose parent already is what it is replayed onto is kept as it
// is, so that a head onto already is the base of comes back unchanged.
// With nothing to replay, or nothing left, the new head is onto.
//
// The first commit that does not apply ends the rebase: it returns no head
// and the same report TrialRebase gives. A merge commit among the commits
// is ErrMergeCommit, before anything is written.
//
// Like the trial, the rebase happens in the object store alone: no work
// tree, no index, no lock and no ref is used or changed, so nothing is
// left behind when it stops. The commits of a rebase that conflicts stay
// unreferenced, for git's garbage collection.
func (r *Repo) Rebase(ctx context.Context, onto, head string, at time.Time) (string, *ConflictReport, error) {
	return r.rebase(ctx, onto, head, onto, at)
}

// RebaseFrom is Rebase of the commits that head reaches and base does not,
// the commits of a branch made on base, on top of onto, which may share
// nothing with base: base's own commits are not replayed, also when onto
// does not have them. A base of "" replays every commit of head. The
// report of a commit that does not apply names base as its base.
func (r *Repo) RebaseFrom(ctx context.Context, base, head, onto string, at time.Time) (string, *ConflictReport, error) {
	newHead, report, err := r.rebase(ctx, base, head, onto, at)
	if report != nil {
		report.BaseSHA = base
	}
	return newHead, report, err
}

// rebase replays the commits from..head, every commit of head for a from
// of "", on top of onto, as Rebase says.
func (r *Repo) rebase(ctx context.Context, from, head, onto string, at time.Time) (string, *ConflictReport, error) {
	onto, head, err := rangeEnds(onto, head)
	if err != nil {
		return "", nil, err
	}
	commitRange := head
	if from != "" {
		if from, _, err = rangeEnds(from, head); err != nil {
			return "", nil, err
		}
		commitRange = from + ".." + head
	}
	commits, err := r.replayed(ctx, commitRange)
	switch {
	case err != nil:
		return "", nil, err
	case len(commits) == 0:
		return onto, nil, nil
	}
	ids := make([]string, len(commits))
	for i, c := range commits {
		ids[i] = c.ID
	}
	full, err := r.ReadCommits(ctx, ids)
	if err != nil {
		return "", nil, err
	}
	zones, err := r.authorZones(ctx, commitRange)
	if err != nil {
		return "", nil, err
	}
	i := 0
	newHead, report, err := r.replay(ctx, onto, head, commits, func(c Commit, ours, oursTree, tree string) (string, error) {
		orig := full[i]
		i++
		switch {
		case len(c.Parents) > 0 && c.Parents[0] == ours:
			return c.ID, nil
		case tree == oursTree:
			return ours, nil
		}
		author := orig.Author
		if zone, ok := zones[c.ID]; ok {
			author.Time = author.Time.In(zone)
		}
		return r.CreateCommit(ctx, CommitSpec{Tree: tree, Parents: []string{ours}, Message: orig.Message,
			Author: author, Committer: HubSignature(at)})
	})
	if err != nil || report != nil {
		return "", report, err
	}
	return newHead, nil, nil
}

// authorZones maps each commit of the rev-list range commitRange to the
// zone of its author's time, which ReadCommits gives in UTC.
func (r *Repo) authorZones(ctx context.Context, commitRange string) (map[string]*time.Location, error) {
	out, err := r.git(ctx, "rev-list", "--no-commit-header", "--date=raw", "--format=%H %ad", commitRange)
	if err != nil {
		return nil, err
	}
	zones := map[string]*time.Location{}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) != 3 || len(f[2]) != 5 || (f[2][0] != '+' && f[2][0] != '-') {
			return nil, fmt.Errorf("rev-list answered %q", line)
		}
		hours, herr := strconv.Atoi(f[2][1:3])
		minutes, merr := strconv.Atoi(f[2][3:])
		if herr != nil || merr != nil {
			return nil, fmt.Errorf("rev-list answered %q", line)
		}
		offset := (hours*60 + minutes) * 60
		if f[2][0] == '-' {
			offset = -offset
		}
		zones[f[0]] = time.FixedZone("", offset)
	}
	return zones, nil
}

// replay is the replay the rebases and the trial share: it replays
// commits, a range that replayed lists, which head ends, oldest first, on top of onto, each as a
// three-way merge with its parent as the merge base, and returns the commit
// the last one left to replay onto, or the report of the first that does
// not apply.
//
// keep is given each commit c that applies with what it was replayed onto,
// the commit ours ("" when only its tree oursTree is known) and the tree
// the replay of c makes, and returns the commit the next one is replayed
// onto: one whose tree is that tree, or "" when it makes none, for a replay
// that keeps no result.
func (r *Repo) replay(ctx context.Context, onto, head string, commits []Commit,
	keep func(c Commit, ours, oursTree, tree string) (string, error)) (string, *ConflictReport, error) {
	// Without merges, the commits form one line: each one's parent is the
	// one before it, and the first one's is outside the range, or none.
	ontoTree, parent, parentTree, err := r.trialStart(ctx, onto, commits[0])
	if err != nil {
		return "", nil, err
	}
	ours, oursTree := onto, ontoTree
	for _, c := range commits {
		// When the commit's own parent holds what it is replayed onto, it
		// applies as it is.
		tree := c.Tree
		if parentTree != oursTree {
			if ours == "" {
				if ours, err = r.trialCommit(ctx, oursTree); err != nil {
					return "", nil, err
				}
			}
			var files []ConflictFile
			if tree, files, err = r.mergeTree(ctx, parent, ours, c.ID); err != nil {
				return "", nil, err
			}
			if files != nil {
				base, err := r.MergeBase(ctx, onto, head)
				if err != nil {
					return "", nil, err
				}
				return "", &ConflictReport{TargetSHA: onto, HeadSHA: head, BaseSHA: base, CommitSHA: c.ID, Files: files}, nil
			}
		}
		if ours, err = keep(c, ours, oursTree, tree); err != nil {
			return "", nil, err
		}
		oursTree = tree
		parent, parentTree = c.ID, c.Tree
	}
	return ours, nil, nil
}
