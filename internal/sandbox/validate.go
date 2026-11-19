package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The bounds of a create body.
const (
	// MinTTL is the shortest ttl a sandbox takes.
	MinTTL         = time.Minute
	maxArguments   = 256
	maxEnv         = 64
	maxEnvEntry    = 8192
	maxLabels      = 16
	maxLabelValue  = 256
	maxPurposeRune = 200
)

// labelKeyPattern is a label key: 1 to 63 letters, digits, '.', '_' and
// '-', beginning and ending with a letter or a digit.
var labelKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

// reservedLabelPrefixes start the label keys the hub and the runtime keep
// for themselves.
var reservedLabelPrefixes = []string{"parley.", "openshell."}

// createRequest is a create body once its members are judged.
type createRequest struct {
	profile Profile
	ttl     time.Duration
	// command is nil for an idle sandbox.
	command     []string
	env, labels map[string]string
	// purpose is nil when the body names none.
	purpose  *string
	checkout checkoutRequest
	// restoreFrom is the sandbox whose checkpoint the new one starts from;
	// nil for none.
	restoreFrom *string
	// sessionID is the session the body names; nil for none.
	sessionID *string
}

// checkoutRequest is the checkout member of a create body.
type checkoutRequest struct {
	// off is set by checkout: false.
	off bool
	// ref and branch are nil when the body names none; sha is the commit
	// ref resolved to.
	ref, branch *string
	sha         string
}

// readBody reads raw, the body of a create, as a JSON object of members: no
// body, or one of white space only, is the empty object, and a member whose
// value is null is left out, so that a client may send back what it read.
func readBody(raw []byte) (map[string]json.RawMessage, error) {
	members := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return members, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		return nil, server.InvalidRequest("request body is not valid JSON")
	}
	if value[0] != '{' {
		return nil, server.InvalidRequest("request body must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, server.InvalidRequest("request body has data after the JSON object")
	}
	if err := json.Unmarshal(value, &members); err != nil {
		return nil, server.InvalidRequest("request body must be a JSON object")
	}
	for name, v := range members {
		if bytes.Equal(v, []byte("null")) {
			delete(members, name)
		}
	}
	return members, nil
}

// judge reads the members of a create body on the workspace w, reading
// stored sandboxes and the repository through q, in the order of the
// refusals, and answers the first member it refuses with 400
// invalid_request "<member>: <reason>". A message names a key and never
// quotes a value of env, which may be a secret.
func (s *Service) judge(ctx context.Context, q access.Querier, w workspace.Workspace, body map[string]json.RawMessage) (createRequest, error) {
	var req createRequest
	var err error
	if req.profile, err = s.pickProfile(body["profile"]); err != nil {
		return req, err
	}
	steps := []struct {
		member string
		read   func(raw json.RawMessage) error
	}{
		{"ttl", func(raw json.RawMessage) (err error) {
			req.ttl, err = readTTL(raw, req.profile.TTL)
			return err
		}},
		{"command", func(raw json.RawMessage) (err error) {
			req.command, err = readCommand(raw)
			return err
		}},
		{"env", func(raw json.RawMessage) (err error) {
			req.env, err = readEnv(raw, req.profile)
			return err
		}},
		{"labels", func(raw json.RawMessage) (err error) {
			req.labels, err = readLabels(raw)
			return err
		}},
		{"purpose", func(raw json.RawMessage) (err error) {
			req.purpose, err = readPurpose(raw)
			return err
		}},
		{"checkout", func(raw json.RawMessage) (err error) {
			req.checkout, err = s.readCheckout(ctx, q, w, raw)
			return err
		}},
		{"restore_from", func(raw json.RawMessage) (err error) {
			req.restoreFrom, err = readRestoreFrom(ctx, q, w, raw, req)
			return err
		}},
		{"session_id", func(raw json.RawMessage) (err error) {
			req.sessionID, err = readString(raw)
			return err
		}},
	}
	for _, step := range steps {
		if err := step.read(body[step.member]); err != nil {
			if answer, ok := errors.AsType[*server.Error](err); ok {
				return req, answer
			}
			if errors.Is(err, errInvalid) {
				return req, server.InvalidRequest(step.member + ": " + strings.TrimPrefix(err.Error(), errInvalid.Error()+": "))
			}
			return req, err
		}
	}
	return req, nil
}

// errInvalid marks the reason a member is refused for, as against an
// error of the store or the repository.
var errInvalid = errors.New("invalid")

// invalid is the refusal of a member for reason.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalid, fmt.Sprintf(format, args...))
}

// pickProfile is the profile the member raw names, or the one marked
// default when it names none. With none marked, the refusal lists the
// configured names, so that the caller can pick one.
func (s *Service) pickProfile(raw json.RawMessage) (Profile, error) {
	if raw == nil {
		if p, ok := s.settings.DefaultProfile(); ok {
			return p, nil
		}
		return Profile{}, server.InvalidRequest("profile: no profile is marked default; name one of " + strings.Join(s.profileNames(), ", "))
	}
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return Profile{}, server.InvalidRequest("profile: must be a string")
	}
	p, ok := s.settings.Profile(name)
	if !ok {
		return Profile{}, server.InvalidRequest(fmt.Sprintf("profile: no profile %q; name one of %s", name, strings.Join(s.profileNames(), ", ")))
	}
	return p, nil
}

// profileNames are the names of the configured profiles, in name order.
func (s *Service) profileNames() []string {
	var names []string
	for _, p := range s.settings.Profiles() {
		names = append(names, p.Name)
	}
	return names
}

// ParseTTL reads value, a sandbox's ttl, in the hub's duration grammar (a
// whole number followed by s, m, h or d) and returns it when it is at least
// MinTTL and at most max, the profile's ttl; otherwise an error saying
// which rule it breaks.
func ParseTTL(value string, max time.Duration) (time.Duration, error) {
	ttl, err := timefmt.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("must be a whole number followed by s, m, h or d, such as 1h")
	}
	if ttl < MinTTL || ttl > max {
		return 0, fmt.Errorf("must be from %s to %s", formatDuration(MinTTL), formatDuration(max))
	}
	return ttl, nil
}

// ExpiresAt is when a sandbox of the workspace w with ttl, counted from
// now, expires: now plus ttl, but never after the expiry of a fork.
func ExpiresAt(now time.Time, ttl time.Duration, w workspace.Workspace) time.Time {
	at := now.Add(ttl)
	if w.Kind == access.KindFork && w.ExpiresAt != nil && w.ExpiresAt.Before(at) {
		at = w.ExpiresAt.UTC()
	}
	return at
}

// readTTL reads the ttl member, the profile's ttl max when it is absent.
func readTTL(raw json.RawMessage, max time.Duration) (time.Duration, error) {
	if raw == nil {
		return max, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return 0, invalid("must be a string")
	}
	ttl, err := ParseTTL(value, max)
	if err != nil {
		return 0, invalid("%s", err)
	}
	return ttl, nil
}

// readCommand reads the command member: 1 to 256 arguments, none empty.
func readCommand(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var args []string
	if json.Unmarshal(raw, &args) != nil || args == nil {
		return nil, invalid("must be a list of strings")
	}
	if len(args) == 0 || len(args) > maxArguments {
		return nil, invalid("must have 1 to %d arguments", maxArguments)
	}
	if slices.Contains(args, "") {
		return nil, invalid("must not have an empty argument")
	}
	return args, nil
}

// readEnv reads the env member against the profile p: at most 64 entries,
// each an environment variable's name outside the reserved prefixes and
// the secrets p exposes, its key and value together at most 8,192 bytes.
func readEnv(raw json.RawMessage, p Profile) (map[string]string, error) {
	env, err := readStringMap(raw)
	if err != nil || env == nil {
		return env, err
	}
	if len(env) > maxEnv {
		return nil, invalid("must have at most %d entries", maxEnv)
	}
	for _, k := range sortedKeys(env) {
		switch {
		case !variablePattern.MatchString(k):
			return nil, invalid("key %q must match %s", k, variablePattern)
		case reservedPrefix(k):
			return nil, invalid("key %q must not start with PARLEY_ or OPENSHELL_", k)
		case slices.Contains(p.ExposeSecrets, k) || slices.Contains(p.OptionalExposeSecrets, k):
			return nil, invalid("key %q is a secret the profile %s exposes", k, p.Name)
		case len(k)+len(env[k]) > maxEnvEntry:
			return nil, invalid("the entry %q must have at most %d bytes of key and value", k, maxEnvEntry)
		}
	}
	return env, nil
}

// readLabels reads the labels member: at most 16 entries, each key a label
// key outside the reserved prefixes, each value at most 256 bytes without a
// control character.
func readLabels(raw json.RawMessage) (map[string]string, error) {
	labels, err := readStringMap(raw)
	if err != nil || labels == nil {
		return labels, err
	}
	if len(labels) > maxLabels {
		return nil, invalid("must have at most %d entries", maxLabels)
	}
	for _, k := range sortedKeys(labels) {
		v := labels[k]
		switch {
		case !labelKeyPattern.MatchString(k):
			return nil, invalid("key %q must be 1 to 63 letters, digits, '.', '_' and '-', beginning and ending with a letter or a digit", k)
		case slices.ContainsFunc(reservedLabelPrefixes, func(p string) bool { return strings.HasPrefix(k, p) }):
			return nil, invalid("key %q must not start with parley. or openshell.", k)
		case len(v) > maxLabelValue:
			return nil, invalid("the value of %q must have at most %d bytes", k, maxLabelValue)
		case strings.ContainsFunc(v, unicode.IsControl):
			return nil, invalid("the value of %q must not have a control character", k)
		}
	}
	return labels, nil
}

// readPurpose reads the purpose member: at most 200 characters.
func readPurpose(raw json.RawMessage) (*string, error) {
	p, err := readString(raw)
	if err != nil || p == nil {
		return p, err
	}
	if utf8.RuneCountInString(*p) > maxPurposeRune {
		return nil, invalid("must be at most %d characters long", maxPurposeRune)
	}
	return p, nil
}

// readCheckout reads the checkout member on the workspace w: false, or an
// object whose ref names a commit of w's repository and whose branch, on a
// fork only, is a valid branch name.
func (s *Service) readCheckout(ctx context.Context, q access.Querier, w workspace.Workspace, raw json.RawMessage) (checkoutRequest, error) {
	var c checkoutRequest
	if raw == nil {
		return c, nil
	}
	if bytes.Equal(raw, []byte("false")) {
		c.off = true
		return c, nil
	}
	var members map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &members) != nil {
		return c, invalid("must be false or an object")
	}
	var err error
	if c.ref, err = readString(nullAsAbsent(members["ref"])); err != nil {
		return c, invalid("ref must be a string")
	}
	if c.branch, err = readString(nullAsAbsent(members["branch"])); err != nil {
		return c, invalid("branch must be a string")
	}
	if c.branch != nil {
		switch {
		case w.Kind != access.KindFork:
			return c, invalid("branch is taken on a task fork only")
		case !gitrepo.ValidRefName("refs/heads/" + *c.branch):
			return c, invalid("branch %q is not a valid branch name", *c.branch)
		}
	}
	if c.ref != nil {
		sha, found, err := s.resolveRef(ctx, q, w, *c.ref)
		if err != nil {
			return c, err
		}
		if !found {
			return c, invalid("ref %q names no commit of workspace %q", *c.ref, w.Slug)
		}
		c.sha = sha
	}
	return c, nil
}

// readRestoreFrom reads the restore_from member: a sandbox of the workspace
// w whose latest checkpoint succeeded, for a sandbox with a checkout and
// hub access, on which the restore is done.
func readRestoreFrom(ctx context.Context, q access.Querier, w workspace.Workspace, raw json.RawMessage, req createRequest) (*string, error) {
	id, err := readString(raw)
	if err != nil || id == nil {
		return id, err
	}
	switch {
	case req.checkout.off:
		return nil, invalid("needs a checkout; it is not taken with checkout false")
	case req.profile.HubAccess == HubAccessNone:
		return nil, invalid("needs hub access; the profile %s has none", req.profile.Name)
	}
	rec, err := GetIn(ctx, q, w.Slug, *id)
	if errors.Is(err, ErrNotFound) {
		return nil, invalid("no sandbox %q in workspace %q", *id, w.Slug)
	}
	if err != nil {
		return nil, err
	}
	var cp struct {
		OK bool `json:"ok"`
	}
	if rec.Checkpoint == nil || json.Unmarshal(rec.Checkpoint, &cp) != nil || !cp.OK {
		return nil, invalid("the sandbox %q has no checkpoint to restore", *id)
	}
	return id, nil
}

// readString reads raw, a string member; nil when it is absent.
func readString(raw json.RawMessage) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return nil, invalid("must be a string")
	}
	return &v, nil
}

// readStringMap reads raw, an object of strings; nil when it is absent.
func readStringMap(raw json.RawMessage) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	var m map[string]string
	if raw[0] != '{' || json.Unmarshal(raw, &m) != nil {
		return nil, invalid("must be an object of strings")
	}
	return m, nil
}

// nullAsAbsent is raw, or nil for a JSON null.
func nullAsAbsent(raw json.RawMessage) json.RawMessage {
	if bytes.Equal(raw, []byte("null")) {
		return nil
	}
	return raw
}

// sortedKeys are the keys of m in order, so that the first refused is the
// same on every request.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// resolveRef is the commit ref names in the repository of the workspace w,
// found false when it names none. On a repository ref is, in order, a
// branch, a tag or a commit id. On a fork it is an own branch, a branch or
// a tag the fork tracks, at the parent's head of it, or a commit id: the
// refs the fork serves.
func (s *Service) resolveRef(ctx context.Context, q access.Querier, w workspace.Workspace, ref string) (string, bool, error) {
	if ref == "" {
		return "", false, nil
	}
	repo, err := s.deps.Repos(ctx, w.Slug)
	if err != nil {
		return "", false, err
	}
	var tracking forkTracking
	if w.Kind == access.KindFork {
		if tracking, err = s.trackingOf(ctx, q, w); err != nil {
			return "", false, err
		}
	}
	branchRepo, tagRepo := repo, repo
	if tracking.parent != "" {
		parent, err := s.deps.Repos(ctx, tracking.parent)
		if err != nil {
			return "", false, err
		}
		if slices.Contains(tracking.branches, ref) {
			branchRepo = parent
		}
		tagRepo = nil
		if slices.Contains(tracking.tags, ref) {
			tagRepo = parent
		}
	}
	if id, found, err := branchRepo.ResolveBranch(ctx, ref); err != nil || found {
		return id, found, err
	}
	if tagRepo != nil {
		id, err := tagRepo.ResolveRef(ctx, "refs/tags/"+ref)
		if err == nil || !notFound(err) {
			return id, err == nil, err
		}
	}
	id, err := repo.ResolveCommit(ctx, ref)
	if err != nil && notFound(err) {
		return "", false, nil
	}
	return id, err == nil, err
}

// notFound reports whether err of a resolution means that nothing of the
// name exists, as against a failure to read.
func notFound(err error) bool {
	return errors.Is(err, gitrepo.ErrNotFound) || errors.Is(err, gitrepo.ErrNotCommit) ||
		errors.Is(err, gitrepo.ErrAmbiguous) || errors.Is(err, gitrepo.ErrInvalidArgument)
}
