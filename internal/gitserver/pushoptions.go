package gitserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// The limits of the push options of one push. They count every option, not
// only the parley ones, and the length of one counts its name and value.
const (
	maxPushOptions     = 16
	maxPushOptionBytes = 1024
	maxTitleCharacters = 200
)

// ErrUnknownSession is the answer of a SessionLinker's check that refuses a
// session id: the push is refused with the reason unknown session.
var ErrUnknownSession = errors.New("unknown session")

// SessionLinker is the session-linking rule as a push asks it for the
// value of parley.session, the check the session feature hands to every
// surface that takes a session id. A push only checks: it becomes part of
// a session through the change version recorded for it, so the link is
// recorded there and never here.
type SessionLinker interface {
	// Check judges the session id the caller names for a push to the
	// workspace slug. It returns the id in lower case when it accepts it,
	// an error matching ErrUnknownSession when it refuses it, and any
	// other error when it cannot tell.
	Check(ctx context.Context, caller PushCaller, sessionID, workspace string) (string, error)
}

// PushOptions is what the parley push options of a push ask for. Change is
// true when the push asks for a change, with parley.change or
// parley.target. Target, Title, Intent and SessionID are nil when their
// option is absent.
type PushOptions struct {
	Change    bool
	Target    *string
	Title     *string
	Intent    *string
	Draft     bool
	SessionID *string
}

// parleyOptions are the parley push options a push may send, each with
// whether it is a flag, which takes no value, or takes a value that may
// not be empty.
var parleyOptions = map[string]struct{ flag bool }{
	"parley.change":  {flag: true},
	"parley.draft":   {flag: true},
	"parley.target":  {},
	"parley.title":   {},
	"parley.intent":  {},
	"parley.session": {},
}

// ParsePushOptions judges the push options of a push as the client sent
// them, without asking anything outside: their number first, then each
// option in the order sent, so that the refusal names the first one at
// fault. It returns the options and "", or the zero options and the
// message the client is shown. Options whose name does not begin with
// parley. are held only to the limits. A parley.session it returns is
// still to be checked, which ValidatePushOptions does.
func ParsePushOptions(raw []string) (PushOptions, string) {
	if len(raw) > maxPushOptions {
		return PushOptions{}, fmt.Sprintf("too many push options: at most %d are accepted", maxPushOptions)
	}
	var opts PushOptions
	seen := map[string]bool{}
	for _, o := range raw {
		name, value, hasValue := strings.Cut(o, "=")
		if len(o) > maxPushOptionBytes {
			return PushOptions{}, invalidOption(name, fmt.Sprintf("longer than %d bytes", maxPushOptionBytes))
		}
		if !strings.HasPrefix(name, "parley.") {
			continue
		}
		known, ok := parleyOptions[name]
		switch {
		case !ok:
			return PushOptions{}, invalidOption(name, "unknown option")
		case seen[name]:
			return PushOptions{}, invalidOption(name, "given more than once")
		case known.flag && hasValue:
			return PushOptions{}, invalidOption(name, "takes no value")
		case !known.flag && value == "":
			return PushOptions{}, invalidOption(name, "value required")
		}
		seen[name] = true
		switch name {
		case "parley.change":
			opts.Change = true
		case "parley.draft":
			opts.Draft = true
		case "parley.target":
			// Only the name is judged here: a branch that does not exist
			// costs the push its change, never the push.
			if !gitrepo.ValidRefName("refs/heads/" + value) {
				return PushOptions{}, invalidOption(name, "not a valid branch name")
			}
			opts.Change, opts.Target = true, &value
		case "parley.title":
			// Counted in characters, as the change's title is.
			if utf8.RuneCountInString(value) > maxTitleCharacters {
				return PushOptions{}, invalidOption(name, fmt.Sprintf("longer than %d characters", maxTitleCharacters))
			}
			opts.Title = &value
		case "parley.intent":
			opts.Intent = &value
		case "parley.session":
			opts.SessionID = &value
		}
	}
	return opts, ""
}

// ValidatePushOptions judges the push options raw of a push by the caller
// to the workspace slug: ParsePushOptions, then, for well-formed options
// with a parley.session, the session linker sessions, nil when none is
// registered. It returns the options, with the session id as the linker
// gives it back, and "", or the zero options and the message the client is
// shown. A session the linker refuses, or that no linker can verify, is an
// unknown session; when the linker cannot tell, the push is refused as an
// internal error and the error is returned for the caller to log, since a
// session nothing verified is never accepted.
func ValidatePushOptions(ctx context.Context, raw []string, caller PushCaller, workspace string, sessions SessionLinker) (PushOptions, string, error) {
	opts, refusal := ParsePushOptions(raw)
	if refusal != "" || opts.SessionID == nil {
		return opts, refusal, nil
	}
	unknown := invalidOption("parley.session", "unknown session")
	if sessions == nil {
		return PushOptions{}, unknown, nil
	}
	id, err := sessions.Check(ctx, caller, *opts.SessionID, workspace)
	switch {
	case errors.Is(err, ErrUnknownSession):
		return PushOptions{}, unknown, nil
	case err != nil:
		return PushOptions{}, "internal error; try again", fmt.Errorf("check session %q: %w", *opts.SessionID, err)
	}
	opts.SessionID = &id
	return opts, "", nil
}

func invalidOption(name, reason string) string {
	return "invalid push option " + name + ": " + reason
}
