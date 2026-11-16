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
	newHead, _, report, err := r.rebase(ctx, onto, head, onto, HubSignature(at), nil)
	return newHead, report, err
}

// RebaseFrom is Rebase of the commits that head reaches and base does not,
// the commits of a branch made on base, on top of onto, which may share
// nothing with base: base's own commits are not replayed, also when onto
// does not have them. A base of "" replays every commit of head. The
// report of a commit that does not apply names base as its base.
func (r *Repo) RebaseFrom(ctx context.Context, base, head, onto string, at time.Time) (string, *ConflictReport, error) {
	newHead, _, report, err := r.RebaseFromAs(ctx, base, head, onto, HubSignature(at))
	return newHead, report, err
}

// rebase replays the commits from..head, every commit of head for a from
// of "", on top of onto, as Rebase says, with committer as the committer
// of each new commit, and returns the new head with what each commit
// became, as RebaseFromAs says. A commit that does not apply is resolved
// with resolutions, by path, as RebaseFromResolving says; none leaves it
// a conflict.
func (r *Repo) rebase(ctx context.Context, from, head, onto string, committer Signature, resolutions map[string]Resolution) (string, []ReplayedCommit, *ConflictReport, error) {
	onto, head, err := rangeEnds(onto, head)
	if err != nil {
		return "", nil, nil, err
	}
	commitRange := head
	if from != "" {
		if from, _, err = rangeEnds(from, head); err != nil {
			return "", nil, nil, err
		}
		commitRange = from + ".." + head
	}
	commits, err := r.replayed(ctx, commitRange)
	switch {
	case err != nil:
		return "", nil, nil, err
	case len(commits) == 0:
		return onto, nil, nil, nil
	}
	ids := make([]string, len(commits))
	for i, c := range commits {
		ids[i] = c.ID
	}
	full, err := r.ReadCommits(ctx, ids)
	if err != nil {
		return "", nil, nil, err
	}
	zones, err := r.authorZones(ctx, commitRange)
	if err != nil {
		return "", nil, nil, err
	}
	var resolve resolveFunc
	if len(resolutions) > 0 {
		resolve = r.resolver(ctx, resolutions)
	}
	became := make([]ReplayedCommit, 0, len(commits))
	newHead, report, err := r.replay(ctx, onto, head, commits, resolve, func(c Commit, ours, oursTree, tree string) (string, error) {
		orig := full[len(became)]
		switch {
		case len(c.Parents) > 0 && c.Parents[0] == ours:
			became = append(became, ReplayedCommit{From: c.ID, To: c.ID})
			return c.ID, nil
		case tree == oursTree:
			became = append(became, ReplayedCommit{From: c.ID})
			return ours, nil
		}
		author := orig.Author
		if zone, ok := zones[c.ID]; ok {
			author.Time = author.Time.In(zone)
		}
		id, err := r.CreateCommit(ctx, CommitSpec{Tree: tree, Parents: []string{ours}, Message: orig.Message,
			Author: author, Committer: committer})
		became = append(became, ReplayedCommit{From: c.ID, To: id})
		return id, err
	})
	if err != nil || report != nil {
		return "", nil, report, err
	}
	return newHead, became, nil, nil
}

// AuthorZone is the zone of the author's time of the commit id, a full id,
// which ReadCommits gives in UTC, so that a commit made with that author
// keeps the zone the author wrote in.
func (r *Repo) AuthorZone(ctx context.Context, id string) (*time.Location, error) {
	if !isHexID(id, idLen) {
		return nil, fmt.Errorf("commit %q: %w", id, ErrNotFound)
	}
	zones, err := r.authorZones(ctx, "--no-walk", id)
	if err != nil {
		return nil, err
	}
	zone, ok := zones[strings.ToLower(id)]
	if !ok {
		return nil, fmt.Errorf("commit %s: %w", id, ErrNotFound)
	}
	return zone, nil
}

// authorZones maps each commit rev-list lists for revs to the zone of its
// author's time, which ReadCommits gives in UTC.
func (r *Repo) authorZones(ctx context.Context, revs ...string) (map[string]*time.Location, error) {
	out, err := r.git(ctx, append([]string{"rev-list", "--no-commit-header", "--date=raw", "--format=%H %ad"}, revs...)...)
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
// resolve, nil for none, is asked for the tree of each commit that does
// not apply; the commit is replayed as that tree when it gives one, and
// is the report otherwise.
//
// keep is given each commit c that applies with what it was replayed onto,
// the commit ours ("" when only its tree oursTree is known) and the tree
// the replay of c makes, and returns the commit the next one is replayed
// onto: one whose tree is that tree, or "" when it makes none, for a replay
// that keeps no result.
func (r *Repo) replay(ctx context.Context, onto, head string, commits []Commit, resolve resolveFunc,
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
			if files != nil && resolve != nil {
				resolved, ok, err := resolve(ours, c.ID, tree, files)
				if err != nil {
					return "", nil, err
				}
				if ok {
					tree, files = resolved, nil
				}
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
