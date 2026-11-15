package gitrepo

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// BlameRange is a run of adjacent lines of a file that one commit last
// changed, at adjacent lines of the file as that commit wrote it.
type BlameRange struct {
	// StartLine and EndLine are the run's first and last line, 1-based and
	// inclusive.
	StartLine, EndLine int
	// Commit is the full id of the commit that last changed the lines.
	Commit string
	// OriginalPath is the file's path in Commit, which a rename since makes
	// differ from the path blamed.
	OriginalPath string
	// OriginalStartLine is the line StartLine was in Commit.
	OriginalStartLine int
}

// Blame groups the lines first to last of the file path of the commit of
// the full id commit by the commit that last changed them, following the
// renames of the file, in line order. first 0 is the first line and last 0
// the last line; a file without lines has no ranges. path is a path of the
// commit's tree, given to git as a literal path after "--". A commit that is
// not a full id, or a span that ends before it starts, is ErrNotFound
// without running git; a path the commit does not hold, or a span past the
// file's end, is git's error.
func (r *Repo) Blame(ctx context.Context, commit, path string, first, last int) ([]BlameRange, error) {
	if len(commit) != idLen || !isHexID(commit, idLen) || path == "" || strings.ContainsAny(path, "\n\x00") ||
		first < 0 || last < 0 || (last > 0 && first > last) {
		return nil, fmt.Errorf("blame %q lines %d-%d: %w", commit+":"+path, first, last, ErrNotFound)
	}
	args := []string{"--literal-pathspecs", "blame", "--porcelain"}
	if first > 0 || last > 0 {
		span := "-L" + strconv.Itoa(max(first, 1)) + ","
		if last > 0 {
			span += strconv.Itoa(last)
		}
		args = append(args, span)
	}
	out, err := r.git(ctx, append(args, strings.ToLower(commit), "--", path)...)
	if err != nil {
		return nil, err
	}
	return parseBlame(string(out))
}

// parseBlame reads the output of git blame --porcelain into ranges in line
// order, adjacent groups of one commit at adjacent original lines of one
// path merged into one range.
func parseBlame(out string) ([]BlameRange, error) {
	var groups []BlameRange
	// git names a group's path only when it first shows its commit or when
	// the commit has lines under more than one path; any other group has
	// the path its commit was last shown with.
	pathOf := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" || line[0] == '\t' {
			// A line of the file, which may hold anything.
			continue
		}
		if name, ok := strings.CutPrefix(line, "filename "); ok {
			if len(groups) == 0 {
				return nil, fmt.Errorf("git blame: a filename before any group: %q", line)
			}
			if strings.HasPrefix(name, `"`) {
				unquoted, err := strconv.Unquote(name)
				if err != nil {
					return nil, fmt.Errorf("git blame: filename %s: %w", name, err)
				}
				name = unquoted
			}
			g := &groups[len(groups)-1]
			g.OriginalPath, pathOf[g.Commit] = name, name
			continue
		}
		// A group's first line is "<id> <original line> <line> <lines>";
		// every other header line starts with a word.
		f := strings.Fields(line)
		if len(f) != 4 || len(f[0]) != idLen || !isHexID(f[0], idLen) {
			continue
		}
		var n [3]int
		for i := range n {
			v, err := strconv.Atoi(f[i+1])
			if err != nil || v < 1 {
				return nil, fmt.Errorf("git blame: group header %q", line)
			}
			n[i] = v
		}
		groups = append(groups, BlameRange{StartLine: n[1], EndLine: n[1] + n[2] - 1, Commit: f[0], OriginalPath: pathOf[f[0]], OriginalStartLine: n[0]})
	}
	slices.SortFunc(groups, func(a, b BlameRange) int { return a.StartLine - b.StartLine })
	ranges := []BlameRange{}
	for _, g := range groups {
		if k := len(ranges) - 1; k >= 0 {
			prev := &ranges[k]
			if prev.Commit == g.Commit && prev.OriginalPath == g.OriginalPath && prev.EndLine+1 == g.StartLine &&
				prev.OriginalStartLine+(prev.EndLine-prev.StartLine)+1 == g.OriginalStartLine {
				prev.EndLine = g.EndLine
				continue
			}
		}
		ranges = append(ranges, g)
	}
	return ranges, nil
}
