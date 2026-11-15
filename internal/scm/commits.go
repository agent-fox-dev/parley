package scm

import (
	"context"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// signature is who wrote or committed a commit, and when, in UTC.
type signature struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Time  string `json:"time"`
}

// commitStats are a commit's numbers against its first parent.
type commitStats struct {
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
}

// commitObject is a commit as every read route answers it.
type commitObject struct {
	SHA       string        `json:"sha"`
	Parents   []string      `json:"parents"`
	TreeSHA   string        `json:"tree_sha"`
	Author    signature     `json:"author"`
	Committer signature     `json:"committer"`
	Message   string        `json:"message"`
	Change    *CommitChange `json:"change"`
	SessionID *string       `json:"session_id"`
	// Stats is present only where the caller asks for it.
	Stats *commitStats `json:"stats,omitempty"`
}

// fileChange is how one file of a commit differs from its first parent.
type fileChange struct {
	Path       string  `json:"path"`
	OldPath    *string `json:"old_path"`
	Status     string  `json:"status"`
	Insertions int     `json:"insertions"`
	Deletions  int     `json:"deletions"`
	Binary     bool    `json:"binary"`
	OldSHA     *string `json:"old_sha"`
	NewSHA     *string `json:"new_sha"`
}

// commitsPosition is where a commit listing continues: the commit its first
// page resolved, so that every page reads the same history when the ref
// moves, and how many commits the pages before held.
type commitsPosition struct {
	SHA    string `json:"sha"`
	Offset int    `json:"offset"`
}

// listCommits answers GET …/scm/commits: a page of the history of ref,
// newest first, that the filters keep, with the commit it was read from.
func (c *Component) listCommits(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var pos commitsPosition
	continuing, err := page.After(&pos)
	if err != nil {
		return err
	}
	if continuing && (!isCommitID(pos.SHA) || len(pos.SHA) != commitIDLen || pos.Offset < 1) {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	q := r.URL.Query()
	hq := gitrepo.HistoryQuery{Path: q.Get("path"), Author: q.Get("author"), Skip: pos.Offset, Count: page.Limit + 1}
	if hq.FirstParent, err = boolParam(q, "first_parent", true); err != nil {
		return err
	}
	if hq.Follow, err = boolParam(q, "follow", false); err != nil {
		return err
	}
	withStats, err := boolParam(q, "stats", false)
	if err != nil {
		return err
	}
	if hq.Since, err = timeParam(q, "since"); err != nil {
		return err
	}
	if hq.Until, err = timeParam(q, "until"); err != nil {
		return err
	}
	if hq.Follow && hq.Path == "" {
		return server.InvalidRequest("follow=true needs a path, the one file whose renames it follows")
	}

	rd, err := c.open(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	sha := pos.SHA
	if continuing {
		if _, err := rd.repo.PeelCommit(ctx, sha); err != nil {
			return notFound(sha, err)
		}
	} else {
		res, err := rd.resolver.ResolveOptional(ctx, q.Get("ref"))
		if err != nil {
			return err
		}
		sha = res.SHA
	}
	commits, err := rd.repo.History(ctx, sha, hq)
	if err != nil {
		return err
	}
	var stats []gitrepo.DiffStats
	if withStats {
		if stats, err = rd.repo.CommitStats(ctx, commits); err != nil {
			return err
		}
	}
	objs, err := c.commitObjects(ctx, rd, commits, stats)
	if err != nil {
		return err
	}
	next := commitsPosition{SHA: sha, Offset: pos.Offset + page.Limit}
	return server.WritePageWith(w, page, objs, func(commitObject) any { return next },
		server.Member{Name: "resolved_sha", Value: sha})
}

// getCommit answers GET …/scm/commits/:sha: the commit any revision names,
// with its numbers, and with files=true its file changes, against its first
// parent or, for a root commit, the empty tree.
func (c *Component) getCommit(w http.ResponseWriter, r *http.Request) error {
	withFiles, err := boolParam(r.URL.Query(), "files", false)
	if err != nil {
		return err
	}
	rd, err := c.open(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	res, err := rd.resolver.Resolve(ctx, server.Param(r, "sha"))
	if err != nil {
		return err
	}
	commits, err := rd.repo.ReadCommits(ctx, []string{res.SHA})
	if err != nil {
		return err
	}
	var from string
	if parents := commits[0].Parents; len(parents) > 0 {
		from = parents[0]
	}
	changes, err := rd.repo.DiffChanges(ctx, from, res.SHA)
	if err != nil {
		return err
	}
	objs, err := c.commitObjects(ctx, rd, commits, []gitrepo.DiffStats{gitrepo.Stats(changes)})
	if err != nil {
		return err
	}
	answer := struct {
		commitObject
		Files *[]fileChange `json:"files,omitempty"`
	}{commitObject: objs[0]}
	if withFiles {
		files := fileChanges(changes)
		answer.Files = &files
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// commitObjects are the objects of the commits of the read rd, each with
// the change and the session the lookups name for it, and with the numbers
// stats holds for it when stats is not nil. Every route that answers
// commits builds them here.
func (c *Component) commitObjects(ctx context.Context, rd *read, commits []gitrepo.Commit, stats []gitrepo.DiffStats) ([]commitObject, error) {
	objs := make([]commitObject, 0, len(commits))
	for i, cm := range commits {
		o := commitObject{
			SHA: cm.ID, Parents: append([]string{}, cm.Parents...), TreeSHA: cm.Tree,
			Author: newSignature(cm.Author), Committer: newSignature(cm.Committer), Message: cm.Message,
		}
		var err error
		if o.Change, err = c.deps.Changes.ChangeOfCommit(ctx, rd.ws.Slug, cm.ID); err != nil {
			return nil, err
		}
		if o.SessionID, err = c.deps.Sessions.SessionOf(ctx, rd.ws.Slug, cm.ID); err != nil {
			return nil, err
		}
		if stats != nil {
			s := stats[i]
			o.Stats = &commitStats{FilesChanged: s.FilesChanged, Insertions: s.Insertions, Deletions: s.Deletions}
		}
		objs = append(objs, o)
	}
	return objs, nil
}

func newSignature(s gitrepo.Signature) signature {
	return signature{Name: s.Name, Email: s.Email, Time: s.Time.UTC().Format(time.RFC3339)}
}

// fileChanges are changes as the read routes answer them: a path, id or
// old path a change has no value for is null.
func fileChanges(changes []gitrepo.FileChange) []fileChange {
	orNull := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	files := make([]fileChange, 0, len(changes))
	for _, ch := range changes {
		files = append(files, fileChange{
			Path: ch.Path, OldPath: orNull(ch.OldPath), Status: ch.Status, Insertions: ch.Insertions,
			Deletions: ch.Deletions, Binary: ch.Binary, OldSHA: orNull(ch.OldID), NewSHA: orNull(ch.NewID),
		})
	}
	return files
}
