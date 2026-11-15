package gitrepo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ErrInvalidPattern is the error of a search pattern git cannot take: an
// extended regular expression that does not compile, or a pattern that is
// empty or holds a NUL byte, which no argument of a process can.
var ErrInvalidPattern = errors.New("the pattern is not one git can search for")

// GrepQuery is what a content search looks for.
type GrepQuery struct {
	// Pattern is matched against each line.
	Pattern string
	// Regexp takes Pattern as an extended regular expression; otherwise it
	// is a fixed string.
	Regexp bool
	// IgnoreCase matches without regard to case.
	IgnoreCase bool
}

// GrepMatch is a line of a file that a content search matched.
type GrepMatch struct {
	// Path is the file's path in the commit's tree.
	Path string
	// Line is the line's number, from 1.
	Line int
	// Text is the line without its line ending.
	Text string
}

// Grep searches the files of the tree of the commit of the full id commit
// for the lines q matches, skipping every file git treats as binary, and
// calls each with each match in path order, the paths compared byte by
// byte, then in line order, until each returns false; git is stopped as
// soon as it does. A pattern holding a newline matches no line, since no
// line holds one, and runs no git. A commit that is not a full id is
// ErrNotFound and a pattern git cannot take ErrInvalidPattern, both without
// a match.
func (r *Repo) Grep(ctx context.Context, commit string, q GrepQuery, each func(GrepMatch) bool) error {
	if len(commit) != idLen || !isHexID(commit, idLen) {
		return fmt.Errorf("grep %q: %w", commit, ErrNotFound)
	}
	if q.Pattern == "" || strings.ContainsRune(q.Pattern, 0) {
		return fmt.Errorf("grep %q: %w", q.Pattern, ErrInvalidPattern)
	}
	if strings.ContainsRune(q.Pattern, '\n') {
		// git would take each line of the pattern as a pattern of its own.
		return nil
	}
	commit = strings.ToLower(commit)
	args := grepArgs(q, commit)

	dir, err := privateDir(false)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	gitCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := r.g.command(gitCtx, dir, access{}, append([]string{"--git-dir=" + r.dir}, args...))
	errOut := &limitedBuffer{limit: stderrLimit}
	cmd.Stderr = errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git grep: %w", err)
	}
	stopped, readErr := readGrep(out, commit+":", each)
	if stopped || readErr != nil {
		stop()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("git grep: %w", ctx.Err())
	case readErr != nil:
		return fmt.Errorf("git grep: read the matches: %w", readErr)
	case stopped, waitErr == nil, exitCode(waitErr) == 1:
		// git exits with 1 when nothing matched.
		return nil
	case q.Regexp && r.patternRefused(ctx, q):
		return fmt.Errorf("grep %q: %w: %s", q.Pattern, ErrInvalidPattern, strings.TrimSpace(errOut.String()))
	}
	return fmt.Errorf("git grep: %w: %s", waitErr, strings.TrimSpace(errOut.String()))
}

// grepArgs are the arguments of git grep searching the tree treeish for
// q: line numbers, NUL after each name, no binary file, no colour, and
// the pattern after -e, so that it cannot be read as an option.
func grepArgs(q GrepQuery, treeish string) []string {
	args := []string{"grep", "-n", "-z", "-I", "--no-color"}
	pattern := q.Pattern
	if q.Regexp {
		args = append(args, "-E")
		pattern = portableERE(pattern)
	} else {
		args = append(args, "-F")
	}
	if q.IgnoreCase {
		args = append(args, "-i")
	}
	return append(args, "-e", pattern, treeish, "--")
}

// patternRefused reports whether git refuses the regular expression of q
// itself, by searching the empty tree for it: a failure there owes nothing
// to the commit's files.
func (r *Repo) patternRefused(ctx context.Context, q GrepQuery) bool {
	empty, err := r.emptyTree(ctx)
	if err != nil {
		return false
	}
	_, err = r.git(ctx, grepArgs(q, empty)...)
	return err != nil && exitCode(err) != 1
}

// readGrep reads the output of git grep -n -z on a tree, each match as
// "<prefix><path>\0<line>\0<text>\n", and calls each with the matches
// until it returns false; stopped says that it did. The text is read to
// its newline, which no line holds, so that it may hold a NUL byte.
func readGrep(in io.Reader, prefix string, each func(GrepMatch) bool) (stopped bool, err error) {
	br := bufio.NewReaderSize(in, 64<<10)
	for {
		name, err := br.ReadString(0)
		if errors.Is(err, io.EOF) && name == "" {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		number, err := br.ReadString(0)
		if err != nil {
			return false, err
		}
		text, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		path, ok := strings.CutPrefix(strings.TrimSuffix(name, "\x00"), prefix)
		line, numErr := strconv.Atoi(strings.TrimSuffix(number, "\x00"))
		if !ok || numErr != nil || line < 1 {
			return false, fmt.Errorf("unexpected match %q", name+number)
		}
		text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
		if !each(GrepMatch{Path: path, Line: line, Text: text}) {
			return true, nil
		}
	}
}

// portableERE is the extended regular expression re with GNU's escapes
// \w, \W, \s and \S outside bracket expressions written as the POSIX
// classes they stand for, so that they mean the same with every C library
// git is built on; macOS's, among others, does not know them.
func portableERE(re string) string {
	classes := map[byte]string{'w': "[[:alnum:]_]", 'W': "[^[:alnum:]_]", 's': "[[:space:]]", 'S': "[^[:space:]]"}
	var b strings.Builder
	for i := 0; i < len(re); i++ {
		switch c := re[i]; {
		case c == '\\' && i+1 < len(re):
			if class, ok := classes[re[i+1]]; ok {
				b.WriteString(class)
			} else {
				b.WriteString(re[i : i+2])
			}
			i++
		case c == '[':
			end := bracketEnd(re, i)
			b.WriteString(re[i:end])
			i = end - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// bracketEnd is the index just after the bracket expression that begins at
// re[start], or len(re) when it does not end: a ] first in the list, after
// an optional ^, is a member, and so is every character of a [:class:],
// [.symbol.] or [=equivalent=] within it. A backslash in a bracket
// expression is itself.
func bracketEnd(re string, start int) int {
	i := start + 1
	if i < len(re) && re[i] == '^' {
		i++
	}
	if i < len(re) && re[i] == ']' {
		i++
	}
	for i < len(re) {
		switch {
		case re[i] == ']':
			return i + 1
		case re[i] == '[' && i+1 < len(re) && strings.IndexByte(":.=", re[i+1]) >= 0:
			if end := strings.Index(re[i+2:], string(re[i+1])+"]"); end >= 0 {
				i += 2 + end + 2
				continue
			}
		}
		i++
	}
	return len(re)
}
