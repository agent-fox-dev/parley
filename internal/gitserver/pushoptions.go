package gitserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
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
			return PushOptions{}, invalidOption(cutName(name), fmt.Sprintf("longer than %d bytes", maxPushOptionBytes))
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
// to the workspace slug: ParsePushOptions, with a parley.session checked by
// the session linker sessions, nil when none is registered, at its place in
// the order sent, so that an option at fault before it refuses the push
// without asking the linker and one after it is judged only once the linker
// accepted the session. The number of options is judged before any of
// them. It returns the options, with the session id as the linker gives it
// back, and "", or the zero options and the message the client is shown. A
// session the linker refuses, or that no linker can verify, is an unknown
// session; when the linker cannot tell, the push is refused as an internal
// error and the error is returned for the caller to log, since a session
// nothing verified is never accepted.
func ValidatePushOptions(ctx context.Context, raw []string, caller PushCaller, workspace string, sessions SessionLinker) (PushOptions, string, error) {
	at := slices.IndexFunc(raw, func(o string) bool {
		name, _, _ := strings.Cut(o, "=")
		return name == "parley.session"
	})
	if len(raw) > maxPushOptions || at < 0 {
		opts, refusal := ParsePushOptions(raw)
		return opts, refusal, nil
	}
	head, refusal := ParsePushOptions(raw[:at+1])
	if refusal != "" {
		return PushOptions{}, refusal, nil
	}
	unknown := invalidOption("parley.session", "unknown session")
	if sessions == nil {
		return PushOptions{}, unknown, nil
	}
	id, err := sessions.Check(ctx, caller, *head.SessionID, workspace)
	switch {
	case errors.Is(err, ErrUnknownSession):
		return PushOptions{}, unknown, nil
	case err != nil:
		return PushOptions{}, "internal error; try again", fmt.Errorf("check session %q: %w", *head.SessionID, err)
	}
	opts, refusal := ParsePushOptions(raw)
	if refusal != "" {
		return PushOptions{}, refusal, nil
	}
	opts.SessionID = &id
	return opts, "", nil
}

func invalidOption(name, reason string) string {
	return "invalid push option " + name + ": " + reason
}

// cutName is the name of an over-long option as its refusal shows it: the
// name, or when it is longer than one option may be, its first
// maxPushOptionBytes bytes at most, cut at a character boundary and marked
// with "...". A name as long as git can send would otherwise make the
// refusal's status and side-band lines longer than one pkt-line, and the
// client would report a protocol error instead of the refusal.
func cutName(name string) string {
	if len(name) <= maxPushOptionBytes {
		return name
	}
	n := maxPushOptionBytes
	for n > 0 && !utf8.RuneStart(name[n]) {
		n--
	}
	return name[:n] + "..."
}

// PushOptionsStage is the push processor's options stage: it judges the
// push options of a push with ValidatePushOptions, so that an invalid one
// refuses the push before its pack is read, and hands on the session of
// parley.session, which the push's version and events are written for.
//
// Its session linker is registered on the instance
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md); with
// none, every parley.session is refused as an unknown session. The zero
// value is ready to use and logs nothing.
type PushOptionsStage struct {
	mu       sync.RWMutex
	sessions SessionLinker
	logger   *slog.Logger
}

// Bind sets the logger of the stage's failures that are not the client's
// doing: a session linker that could not tell.
func (s *PushOptionsStage) Bind(logger *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logger = logger
}

// RegisterSessionLinker makes l the session linker, replacing the one
// registered before; nil removes it.
func (s *PushOptionsStage) RegisterSessionLinker(l SessionLinker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = l
}

// CheckOptions answers the push in with the refusal of its options, or with
// the session the push is written for and whether it opens a change: one
// that asks for a change, or any push to a task fork that opens changes on
// its own. A session linker that could not tell is answered as a refusal,
// not as an error, since the client is told only to try again; the error
// is logged.
func (s *PushOptionsStage) CheckOptions(ctx context.Context, in OptionsInput) (OptionsAnswer, error) {
	s.mu.RLock()
	sessions, logger := s.sessions, s.logger
	s.mu.RUnlock()
	opts, refusal, err := ValidatePushOptions(ctx, in.Options, in.Caller, in.Workspace.Slug, sessions)
	if err != nil && logger != nil {
		logger.LogAttrs(ctx, slog.LevelError, "git push options failed",
			slog.String("request_id", server.RequestID(ctx)),
			slog.String("workspace", in.Workspace.Slug),
			slog.String("error", err.Error()),
		)
	}
	if refusal != "" {
		return OptionsAnswer{Refusal: refusal}, nil
	}
	ws := in.Workspace
	autoChange := ws.Kind == access.KindFork && ws.AutoChange != nil && *ws.AutoChange
	return OptionsAnswer{SessionID: opts.SessionID, OpensChange: opts.Change || autoChange}, nil
}
