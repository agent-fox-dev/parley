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
	"unicode/utf8"
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
	// AfterPath and AfterLine, when AfterPath is not "", leave out every
	// match up to the line AfterLine of the file AfterPath in the order of
	// the matches, and git searches no file before AfterPath.
	AfterPath string
	AfterLine int
	// MaxText is the most characters of a matching line a match keeps, 0
	// for the whole line; git's output is read no further than that.
	MaxText int
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
	args := append(grepArgs(q, commit), afterPathspecs(q.AfterPath)...)

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
	keep := each
	if q.AfterPath != "" {
		// A pathspec does not tell lines apart, and a cursor too long for
		// pathspecs leaves git to search from the first file.
		keep = func(m GrepMatch) bool {
			if m.Path < q.AfterPath || (m.Path == q.AfterPath && m.Line <= q.AfterLine) {
				return true
			}
			return each(m)
		}
	}
	stopped, readErr := readGrep(out, commit+":", q.MaxText, keep)
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

// maxAfterPathspecs is the most bytes the pathspecs of a search that
// continues after a path may take on git's command line.
const maxAfterPathspecs = 64 << 10

// afterPathspecs are the pathspecs of git grep that leave out every file
// before the path after in byte order: the file itself, then for each byte
// of it a wildcard pattern of the paths that share the bytes before it and
// have a greater one there, and the paths it begins. A wildcard of a
// pathspec without magic matches "/" too, and its ranges compare bytes.
// They are none, so that git searches every file, for no path, for one git
// would read otherwise than literally (a NUL byte, an empty, "." or ".."
// segment, a leading or trailing "/") and for one whose patterns would not
// fit on the command line.
func afterPathspecs(after string) []string {
	if after == "" || strings.ContainsRune(after, 0) {
		return nil
	}
	for seg := range strings.SplitSeq(after, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil
		}
	}
	specs := []string{":(literal)" + after}
	size := len(specs[0])
	// ":()" is an empty list of magic, so that a path that begins with ":"
	// is not read as magic; the start of a range is escaped, so that it may
	// be any byte.
	for i := 0; i <= len(after); i++ {
		var spec string
		switch {
		case i == len(after):
			spec = ":()" + globEscape(after) + "?*"
		case after[i] == 0xff:
			continue
		default:
			spec = ":()" + globEscape(after[:i]) + "[\\" + string([]byte{after[i] + 1}) + "-\xff]*"
		}
		if size += len(spec); size > maxAfterPathspecs {
			return nil
		}
		specs = append(specs, spec)
	}
	return specs
}

// globEscape is s with every character special to a wildcard pattern
// escaped, so that the pattern matches s itself.
func globEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(`\*?[`, s[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
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
// its newline, which no line holds, so that it may hold a NUL byte, and
// with maxText above 0 only its first maxText characters are kept.
func readGrep(in io.Reader, prefix string, maxText int, each func(GrepMatch) bool) (stopped bool, err error) {
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
		text, err := readLine(br, maxText)
		if err != nil {
			return false, err
		}
		path, ok := strings.CutPrefix(strings.TrimSuffix(name, "\x00"), prefix)
		line, numErr := strconv.Atoi(strings.TrimSuffix(number, "\x00"))
		if !ok || numErr != nil || line < 1 {
			return false, fmt.Errorf("unexpected match %q", name+number)
		}
		if !each(GrepMatch{Path: path, Line: line, Text: text}) {
			return true, nil
		}
	}
}

// readLine reads a line to its newline or the end of br, and returns it
// without its line ending; with maxChars above 0, it keeps the line's
// first maxChars characters and does not hold the rest in memory.
func readLine(br *bufio.Reader, maxChars int) (string, error) {
	// A character takes at most utf8.UTFMax bytes, and one byte more tells
	// the CR that ends a line from one within it.
	limit := maxChars*utf8.UTFMax + 1
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if maxChars <= 0 {
			line = append(line, chunk...)
		} else if room := limit - len(line); room > 0 {
			line = append(line, chunk[:min(len(chunk), room)]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		break
	}
	text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
	if maxChars > 0 {
		text = firstRunes(text, maxChars)
	}
	return text, nil
}

// firstRunes is s cut to its first n characters, so that no character is
// split.
func firstRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
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
