package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The oldest git the hub runs with.
const (
	minMajor = 2
	minMinor = 38
)

// versionLineLimit is how much of an unreadable first line an error quotes.
const versionLineLimit = 100

// Version is the version of git that git --version reports.
type Version struct {
	// Major and Minor are the first two numbers of the version.
	Major, Minor int
	// Text is the version as git printed it, up to the first space, such as
	// 2.39.3 or 2.45.1.windows.1.
	Text string
}

// ParseVersion reads the first line of the output of git --version: the
// two numbers after "git version ", separated by a dot. Whatever follows the
// minor number is ignored, since vendors append their own suffixes.
func ParseVersion(line string) (Version, error) {
	text, ok := strings.CutPrefix(line, "git version ")
	if ok {
		text, _, _ = strings.Cut(text, " ")
		majorText, rest, _ := strings.Cut(text, ".")
		major, err1 := strconv.Atoi(majorText)
		minor, err2 := strconv.Atoi(leadingDigits(rest))
		if err1 == nil && err2 == nil && isDigits(majorText) {
			return Version{Major: major, Minor: minor, Text: text}, nil
		}
	}
	if len(line) > versionLineLimit {
		line = line[:versionLineLimit]
	}
	return Version{}, fmt.Errorf("cannot read the git version from %q", line)
}

// leadingDigits is the decimal digits s starts with.
func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// isDigits reports whether s is a non-empty run of decimal digits, which
// strconv.Atoi alone does not ensure: it takes a sign.
func isDigits(s string) bool { return s != "" && leadingDigits(s) == s }

// AtLeast reports whether v is major.minor or later.
func (v Version) AtLeast(major, minor int) bool {
	return v.Major > major || v.Major == major && v.Minor >= minor
}

// CheckVersion finds git with lookPath, runs git --version and returns the
// version when it is 2.38 or later. A git that has not answered within
// limit is killed, with every process it started. The error says what was
// wrong: git not found, too old, an unreadable answer, or a failed run.
func CheckVersion(ctx context.Context, lookPath func(string) (string, error), limit time.Duration) (Version, error) {
	path, err := lookPath("git")
	if err != nil {
		return Version{}, errors.New("git not found on PATH; parleyd needs git 2.38 or later")
	}
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(runCtx, path, "--version")
	// The PATH git is run with later (Git.command), so that a git that is a
	// wrapper finds what it calls.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	var out bytes.Buffer
	cmd.Stdout = &out
	// Less than waitDelay: a killed git's output pipe, held open by a process
	// that left its group, must not keep the check waiting long past limit.
	cmd.WaitDelay = time.Second
	killGroupOnCancel(cmd)
	if err := cmd.Run(); err != nil {
		switch {
		case ctx.Err() != nil:
			return Version{}, fmt.Errorf("git --version: %w", ctx.Err())
		case runCtx.Err() != nil:
			return Version{}, fmt.Errorf("git --version failed: no answer within %s", limit)
		}
		return Version{}, fmt.Errorf("git --version failed: %w", err)
	}
	line, _, _ := strings.Cut(out.String(), "\n")
	v, err := ParseVersion(strings.TrimSuffix(line, "\r"))
	if err != nil {
		return Version{}, err
	}
	if !v.AtLeast(minMajor, minMinor) {
		return Version{}, fmt.Errorf("git %s is too old; parleyd needs git %d.%d or later", v.Text, minMajor, minMinor)
	}
	return v, nil
}
