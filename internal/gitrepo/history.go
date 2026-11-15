package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signature is who wrote or committed a commit, and when.
type Signature struct {
	Name, Email string
	Time        time.Time
}

// Commit is a commit as the repository holds it.
type Commit struct {
	// ID, Parents and Tree are full ids; Parents in their order, the first
	// parent first, none for a root commit.
	ID      string
	Parents []string
	Tree    string
	// Author and Committer are the commit's signatures.
	Author, Committer Signature
	// Message is the whole message, as written.
	Message string
}

// HistoryQuery chooses which of the commits reachable from a commit a
// history read returns.
type HistoryQuery struct {
	// FirstParent follows only the first parent of each merge.
	FirstParent bool
	// Path, when set, keeps only the commits that changed it, a file or
	// the files below a directory, taken literally.
	Path string
	// Follow follows Path across renames; Path must name one file.
	Follow bool
	// Since and Until, when set, keep only the commits whose committer time
	// is at or after Since and at or before Until.
	Since, Until *time.Time
	// Author, when set, keeps only the commits whose author's name or
	// e-mail contains it, without regard to case.
	Author string
	// Skip leaves out the first Skip commits the query keeps; Count, when
	// above 0, returns at most Count after them.
	Skip, Count int
}

// DiffStats are the numbers of a change between two trees.
type DiffStats struct {
	FilesChanged, Insertions, Deletions int
}

// History is the commits reachable from the commit of the full id from,
// newest first, that q keeps. A path that no commit changed keeps none.
func (r *Repo) History(ctx context.Context, from string, q HistoryQuery) ([]Commit, error) {
	if !isHexID(from, idLen) {
		return nil, fmt.Errorf("history of %q: %w", from, ErrNotFound)
	}
	if q.Follow && q.Path == "" {
		return nil, errors.New("history: following renames needs a path")
	}
	// Every pathspec is taken literally: a path such as :(glob)*.md is a
	// file's name, not pathspec magic.
	args := []string{"--literal-pathspecs", "log", "--format=%H"}
	if q.FirstParent {
		args = append(args, "--first-parent")
	}
	if q.Since != nil {
		// --since stops the walk at the first older commit, which would drop
		// the newer ones behind it; --since-as-filter looks at every commit.
		// A time within a second keeps the commits of the next one.
		since := q.Since.Unix()
		if q.Since.After(time.Unix(since, 0)) {
			since++
		}
		args = append(args, "--since-as-filter="+gitTime(max(since, 0)))
	}
	if q.Until != nil {
		if q.Until.Unix() < 0 {
			return nil, nil
		}
		args = append(args, "--until="+gitTime(q.Until.Unix()))
	}
	// git counts the commits it skips before --follow drops those that did
	// not change the file, so a followed history is counted here. git
	// matches --author against the whole "Name <email>" line, where a value
	// spanning the two, or a bare "<", would match too, so the author is
	// matched here, the name and the e-mail apart, and counted after it.
	skip, count := max(q.Skip, 0), q.Count
	if q.Follow {
		args = append(args, "--follow")
	}
	if !q.Follow && q.Author == "" {
		args = append(args, "--skip="+strconv.Itoa(skip))
		if count > 0 {
			args = append(args, "--max-count="+strconv.Itoa(count))
		}
		skip = 0
	}
	args = append(args, strings.ToLower(from), "--")
	if q.Path != "" {
		args = append(args, q.Path)
	}
	out, err := r.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if q.Author != "" {
		return r.authoredBy(ctx, ids, q.Author, skip, count)
	}
	ids = ids[min(skip, len(ids)):]
	if count > 0 {
		ids = ids[:min(count, len(ids))]
	}
	return r.ReadCommits(ctx, ids)
}

// authorBatch is how many commits a history kept by author reads at once.
const authorBatch = 256

// authoredBy is the commits of the full ids ids, in their order, whose
// author's name or e-mail contains author without regard to case, less the
// first skip of them and, when count is above 0, at most count after those.
func (r *Repo) authoredBy(ctx context.Context, ids []string, author string, skip, count int) ([]Commit, error) {
	author = strings.ToLower(author)
	var kept []Commit
	for len(ids) > 0 {
		batch := ids[:min(authorBatch, len(ids))]
		ids = ids[len(batch):]
		commits, err := r.ReadCommits(ctx, batch)
		if err != nil {
			return nil, err
		}
		for _, c := range commits {
			if !strings.Contains(strings.ToLower(c.Author.Name), author) &&
				!strings.Contains(strings.ToLower(c.Author.Email), author) {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			kept = append(kept, c)
			if count > 0 && len(kept) == count {
				return kept, nil
			}
		}
	}
	return kept, nil
}

// gitTime is the time of the Unix time t in the form git reads as that
// time; a bare @t is read as one only when it is large enough.
func gitTime(t int64) string { return "@" + strconv.FormatInt(t, 10) + " +0000" }

// ReadCommits reads the commits of the full ids ids, in their order. An id
// that is not a full id, or that names no object, is ErrNotFound, and one
// whose object is not a commit is ErrNotCommit.
func (r *Repo) ReadCommits(ctx context.Context, ids []string) ([]Commit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	for _, id := range ids {
		if !isHexID(id, idLen) {
			return nil, fmt.Errorf("commit %q: %w", id, ErrNotFound)
		}
	}
	// cat-file reads the ids from its standard input and answers each with
	// a header line and the object as it is stored, whose length the
	// header gives, so a message may hold any byte.
	out, err := r.g.runInput(ctx, access{}, []byte(strings.ToLower(strings.Join(ids, "\n"))+"\n"),
		"--git-dir="+r.dir, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	commits := make([]Commit, 0, len(ids))
	for _, id := range ids {
		header, rest, ok := bytes.Cut(out, []byte("\n"))
		if !ok {
			return nil, fmt.Errorf("read commit %s: cat-file answered %q", id, out)
		}
		f := strings.Fields(string(header))
		switch {
		case len(f) == 2 && f[1] == "missing":
			return nil, fmt.Errorf("commit %s: %w", id, ErrNotFound)
		case len(f) != 3:
			return nil, fmt.Errorf("read commit %s: cat-file answered %q", id, header)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size+1 > len(rest) {
			return nil, fmt.Errorf("read commit %s: cat-file answered %q", id, header)
		}
		if f[1] != ObjectCommit {
			return nil, fmt.Errorf("commit %s: %w", id, ErrNotCommit)
		}
		c, err := parseCommit(f[0], rest[:size])
		if err != nil {
			return nil, err
		}
		commits = append(commits, c)
		out = rest[size+1:]
	}
	return commits, nil
}

// parseCommit is the commit id whose object, as stored, is raw: header
// lines, of which a line that begins with a space continues the one before,
// an empty line, and the message.
func parseCommit(id string, raw []byte) (Commit, error) {
	c := Commit{ID: id}
	headers, message, _ := bytes.Cut(raw, []byte("\n\n"))
	c.Message = string(message)
	for line := range strings.SplitSeq(string(headers), "\n") {
		key, value, _ := strings.Cut(line, " ")
		var err error
		switch key {
		case "tree":
			c.Tree = value
		case "parent":
			c.Parents = append(c.Parents, value)
		case "author":
			c.Author, err = parseSignature(value)
		case "committer":
			c.Committer, err = parseSignature(value)
		}
		if err != nil {
			return Commit{}, fmt.Errorf("read commit %s: %w", id, err)
		}
	}
	if c.Tree == "" {
		return Commit{}, fmt.Errorf("read commit %s: it names no tree", id)
	}
	return c, nil
}

// parseSignature reads a signature as a commit stores it: a name, an
// e-mail address within <>, a Unix time and a zone offset.
func parseSignature(s string) (Signature, error) {
	open, end := strings.LastIndexByte(s, '<'), strings.LastIndexByte(s, '>')
	if open < 0 || end < open {
		return Signature{}, fmt.Errorf("signature %q has no e-mail address", s)
	}
	f := strings.Fields(s[end+1:])
	if len(f) == 0 {
		return Signature{}, fmt.Errorf("signature %q has no time", s)
	}
	at, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return Signature{}, fmt.Errorf("signature %q has no time", s)
	}
	return Signature{Name: strings.TrimSpace(s[:open]), Email: s[open+1 : end], Time: time.Unix(at, 0).UTC()}, nil
}

// CommitStats are the numbers of each of commits against its first parent,
// or against the empty tree for a commit without one, in their order.
// Renamed and copied files are found; a binary file counts as changed
// with no lines.
func (r *Repo) CommitStats(ctx context.Context, commits []Commit) ([]DiffStats, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	// One diff-tree reads every pair from its standard input: a commit and
	// its first parent, or a root commit alone, which --root compares with
	// the empty tree. --always names a commit that changed nothing too.
	var in strings.Builder
	for _, c := range commits {
		in.WriteString(c.ID)
		if len(c.Parents) > 0 {
			in.WriteString(" " + c.Parents[0])
		}
		in.WriteString("\n")
	}
	args := append(append([]string{"--git-dir=" + r.dir, "diff-tree", "--stdin", "--always", "--root", "-r", "-z"}, findRenames...), "--numstat")
	out, err := r.g.runInput(ctx, access{}, []byte(in.String()), args...)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	stats := make([]DiffStats, len(commits))
	i := -1
	for len(fields) > 0 && fields[0] != "" {
		if !strings.Contains(fields[0], "\t") {
			// The id of the next commit, which numstat lines follow.
			if i++; i >= len(commits) || fields[0] != commits[i].ID {
				return nil, fmt.Errorf("commit stats: diff-tree answered %q", fields[0])
			}
			fields = fields[1:]
			continue
		}
		if i < 0 {
			return nil, fmt.Errorf("commit stats: diff-tree answered %q", fields[0])
		}
		n, rest, err := parseNumstat(fields)
		if err != nil {
			return nil, err
		}
		stats[i].add(n)
		fields = rest
	}
	return stats, nil
}

// numstat is one file's line counts.
type numstat struct {
	Insertions, Deletions int
	binary                bool
}

// parseNumstat reads the numstat entry fields begin with, written with -z:
// "<insertions>\t<deletions>\t<path>", or, for a renamed or copied file,
// the counts with an empty path followed by the old and the new path; a
// binary file's counts are "-". It returns the fields after the entry.
func parseNumstat(fields []string) (numstat, []string, error) {
	if len(fields) == 0 {
		return numstat{}, nil, errors.New("diff: diff-tree answered fewer counts than files")
	}
	f := strings.SplitN(fields[0], "\t", 3)
	if len(f) != 3 {
		return numstat{}, nil, fmt.Errorf("diff: diff-tree answered %q", fields[0])
	}
	rest := fields[1:]
	if f[2] == "" {
		if len(rest) < 2 {
			return numstat{}, nil, fmt.Errorf("diff: diff-tree answered %q without its paths", fields[0])
		}
		rest = rest[2:]
	}
	if f[0] == "-" && f[1] == "-" {
		return numstat{binary: true}, rest, nil
	}
	ins, errIns := strconv.Atoi(f[0])
	del, errDel := strconv.Atoi(f[1])
	if errIns != nil || errDel != nil {
		return numstat{}, nil, fmt.Errorf("diff: diff-tree answered %q", fields[0])
	}
	return numstat{Insertions: ins, Deletions: del}, rest, nil
}

// add counts the file of n in s.
func (s *DiffStats) add(n numstat) {
	s.FilesChanged++
	s.Insertions += n.Insertions
	s.Deletions += n.Deletions
}

// AheadBehind counts the commits that the commit of the full id head
// reaches and that of base does not (ahead), and the reverse (behind).
func (r *Repo) AheadBehind(ctx context.Context, base, head string) (ahead, behind int, err error) {
	base, head, err = rangeEnds(base, head)
	if err != nil {
		return 0, 0, err
	}
	// The left side of base...head is what only base reaches.
	out, err := r.git(ctx, "rev-list", "--left-right", "--count", base+"..."+head)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("ahead and behind: rev-list answered %q", out)
	}
	if behind, err = strconv.Atoi(f[0]); err == nil {
		ahead, err = strconv.Atoi(f[1])
	}
	if err != nil {
		return 0, 0, fmt.Errorf("ahead and behind: rev-list answered %q", out)
	}
	return ahead, behind, nil
}

// CommitsBetween lists the full ids of the commits that the commit of the
// full id head reaches and that of base does not, every commit head
// reaches for base "", oldest first, every commit after its parents; when
// limit is above 0, the first limit of them.
func (r *Repo) CommitsBetween(ctx context.Context, base, head string, limit int) ([]string, error) {
	from := base
	if base == "" {
		// Any full id stands in for the missing base while the ends are
		// checked; the range is then head alone.
		from = head
	}
	from, head, err := rangeEnds(from, head)
	if err != nil {
		return nil, err
	}
	revs := from + ".." + head
	if base == "" {
		revs = head
	}
	// rev-list cuts a listing before it reverses it, so the oldest come
	// only from the whole list.
	out, err := r.git(ctx, "rev-list", "--reverse", "--topo-order", revs)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// rangeEnds are the full ids base and head as git takes them in a range.
func rangeEnds(base, head string) (string, string, error) {
	for _, id := range []string{base, head} {
		if !isHexID(id, idLen) {
			return "", "", fmt.Errorf("commits of %q: %w", id, ErrNotFound)
		}
	}
	return strings.ToLower(base), strings.ToLower(head), nil
}
