package awareness

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// defaultSince is how far back the landed changes of an activity go when
// the query names no since.
const defaultSince = 24 * time.Hour

// maxLanded is the most landed changes an activity lists, and the most
// matching files of one it lists.
const maxLanded = 100

// ActivityQuery is what an activity asks for.
type ActivityQuery struct {
	// Paths are the globs, 1 to 50 of them, already validated.
	Paths []string
	// TargetBranch is the branch asked about; "" is the default branch of
	// the workspace.
	TargetBranch string
	// SinceCommit, a full commit id, selects the landed changes whose
	// landed commit it does not reach; "" for a since by time.
	SinceCommit string
	// Since selects the landed changes that landed after it, when
	// SinceCommit is ""; the zero time is 24 hours before the activity.
	Since time.Time
	// ExcludeSession, when not "", leaves out every change, claim and
	// landed change of that session.
	ExcludeSession string
	// ExcludeChange, when not 0, leaves out that change and every claim
	// linked to it.
	ExcludeChange int64
	// LeaveOutClaim, when not "", is a claim the activity does not list:
	// the one being created, whose activity it is.
	LeaveOutClaim string
}

// Activity is the answer to "what touches these paths" on a target branch:
// the in-flight changes, the live claims and the recently landed changes
// that match the globs. The three lists are never nil.
type Activity struct {
	TargetBranch string `json:"target_branch"`
	// TargetSHA is the head of the target branch, nil when the workspace
	// has no such branch or its repository cannot be read.
	TargetSHA *string       `json:"target_sha"`
	Changes   []Entry       `json:"changes"`
	Claims    []ClaimObject `json:"claims"`
	Landed    []LandedItem  `json:"landed"`
}

// LandedItem is a landed change of an activity, with its matching files.
type LandedItem struct {
	Number    int64        `json:"number"`
	Ref       string       `json:"ref"`
	Title     string       `json:"title"`
	LandedSHA string       `json:"landed_sha"`
	LandedAt  timefmt.Time `json:"landed_at"`
	SessionID *string      `json:"session_id"`
	// Paths are the matching files, in ascending byte order, the first 100.
	Paths []string `json:"paths"`
}

// Activity is the activity of the globs of q on the workspace the route
// acts on: the draft and open changes on the target branch with an indexed
// file matching a glob, each as an overlap entry over its matching files,
// the most first and then by number; the claims on the target branch that
// are active at the time of the call and have a glob able to match the
// same files as one of q's, newest first; and the changes landed on the
// target branch since q's since with a matching file, newest first and at
// most 100. A SinceCommit the repository does not hold is a 400
// invalid_request.
//
// It reads the repository for the target's head and the since commit, so
// it is called outside any write transaction.
func (c *Component) Activity(ctx context.Context, workspace string, q ActivityQuery) (Activity, error) {
	now := c.deps.Now()
	out := Activity{TargetBranch: q.TargetBranch, Changes: []Entry{}, Claims: []ClaimObject{}, Landed: []LandedItem{}}
	if out.TargetBranch == "" {
		branch, err := c.deps.DefaultBranch(ctx, workspace)
		if err != nil {
			return Activity{}, err
		}
		out.TargetBranch = branch
	}
	// The routes answer in every workspace status: a repository that
	// cannot be read only leaves the target's head unknown.
	repo, repoErr := c.deps.Repos(ctx, workspace)
	if repoErr == nil && out.TargetBranch != "" {
		if sha, found, err := repo.ResolveBranch(ctx, out.TargetBranch); err == nil && found {
			out.TargetSHA = &sha
		}
	}
	since := landedSince{after: q.Since}
	if q.SinceCommit != "" {
		if repoErr != nil {
			return Activity{}, server.InvalidRequest("since names commit " + q.SinceCommit + ", and the repository cannot be read")
		}
		id, err := repo.ResolveCommit(ctx, q.SinceCommit)
		if err != nil {
			if ctx.Err() != nil {
				return Activity{}, ctx.Err()
			}
			return Activity{}, server.InvalidRequest("since names commit " + q.SinceCommit + ", which the repository does not hold")
		}
		since = landedSince{commit: id, repo: repo}
	} else if since.after.IsZero() {
		since.after = now.Add(-defaultSince)
	}

	var err error
	db := c.deps.Store.Main()
	if out.Changes, err = c.activityChanges(ctx, db, workspace, out.TargetBranch, q); err != nil {
		return Activity{}, err
	}
	if out.Claims, err = activityClaims(ctx, db, workspace, out.TargetBranch, q, now); err != nil {
		return Activity{}, err
	}
	if out.Landed, err = c.activityLanded(ctx, db, workspace, out.TargetBranch, q, since); err != nil {
		return Activity{}, err
	}
	return out, nil
}

// activityChanges are the in-flight changes of workspace on target with a
// file matching one of q's globs, each as an overlap entry over its
// matching files, count descending and then number ascending. The state
// and summary are read from each change's record, so a change that left
// flight since the index last followed it is left out.
func (c *Component) activityChanges(ctx context.Context, q Querier, workspace, target string, aq ActivityQuery) ([]Entry, error) {
	matched, err := matchingFiles(ctx, q, aq.Paths, `SELECT c.number, f.path FROM awareness_changes c
JOIN awareness_files f ON f.workspace_slug = c.workspace_slug AND f.number = c.number
WHERE c.workspace_slug = ? AND c.target_branch = ? AND c.phase = 'in_flight'
ORDER BY c.number, f.path`, workspace, target)
	if err != nil {
		return nil, fmt.Errorf("read the in-flight changes of %s on %s: %w", workspace, target, err)
	}
	out := []Entry{}
	for _, m := range matched {
		if m.number == aq.ExcludeChange {
			continue
		}
		ch, err := c.deps.Changes.ReadChange(ctx, q, workspace, m.number)
		if errors.Is(err, ErrChangeNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s#%d: %w", workspace, m.number, err)
		}
		if !inFlight(ch.State) || ofSession(ch.SessionID, aq.ExcludeSession) {
			continue
		}
		ch.Workspace, ch.Number = workspace, m.number
		out = append(out, EntryOf(ch, m.paths))
	}
	slices.SortStableFunc(out, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Change.Number, b.Change.Number))
	})
	return out, nil
}

// activityClaims are the claims of workspace on target that are active at
// now and have a glob able to match the same files as one of q's, newest
// first, as a read at now shows them.
func activityClaims(ctx context.Context, q Querier, workspace, target string, aq ActivityQuery, now time.Time) ([]ClaimObject, error) {
	claims, err := liveClaims(ctx, q, workspace, target, now)
	if err != nil {
		return nil, err
	}
	out := []ClaimObject{}
	for _, cl := range claims {
		switch {
		case cl.ID == aq.LeaveOutClaim,
			ofSession(cl.SessionID, aq.ExcludeSession),
			aq.ExcludeChange != 0 && cl.Change != nil && *cl.Change == aq.ExcludeChange,
			!overlapsAny(cl.Paths, aq.Paths):
			continue
		}
		out = append(out, objectOf(cl, now))
	}
	return out, nil
}

// landedSince is the since of the landed changes: a commit, in the
// repository repo, whose reachable commits are not new, or else a time
// after which a landing is new.
type landedSince struct {
	commit string
	repo   *gitrepo.Repo
	after  time.Time
}

// activityLanded are the changes of workspace landed on target since since
// with a file matching one of q's globs, landed_at descending and then
// number descending, at most 100.
func (c *Component) activityLanded(ctx context.Context, q Querier, workspace, target string, aq ActivityQuery, since landedSince) ([]LandedItem, error) {
	query := `SELECT c.number, f.path, c.landed_sha, c.landed_at, c.session_id FROM awareness_changes c
JOIN awareness_files f ON f.workspace_slug = c.workspace_slug AND f.number = c.number
WHERE c.workspace_slug = ? AND c.target_branch = ? AND c.phase = 'landed'`
	args := []any{workspace, target}
	if since.commit == "" {
		query += ` AND c.landed_at > ?`
		args = append(args, since.after.UnixNano())
	}
	matched, err := matchingFiles(ctx, q, aq.Paths, query+` ORDER BY c.landed_at DESC, c.number DESC, f.path`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the landed changes of %s on %s: %w", workspace, target, err)
	}
	out := []LandedItem{}
	for _, m := range matched {
		if len(out) == maxLanded {
			break
		}
		if m.number == aq.ExcludeChange || ofSession(m.session, aq.ExcludeSession) {
			continue
		}
		if since.commit != "" && m.sha != "" {
			// A landed commit the repository does not hold cannot be
			// reachable from since.
			reached, err := since.repo.IsAncestor(ctx, m.sha, since.commit)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err == nil && reached {
				continue
			}
		}
		item := LandedItem{
			Number: m.number, Ref: workspace + "#" + fmt.Sprint(m.number), LandedSHA: m.sha, LandedAt: timefmt.Time(m.at),
			SessionID: m.session, Paths: m.paths[:min(len(m.paths), maxLanded)],
		}
		ch, err := c.deps.Changes.ReadChange(ctx, q, workspace, m.number)
		switch {
		case err == nil:
			ch.Workspace, ch.Number = workspace, m.number
			item.Ref, item.Title = refOf(ch), ch.Title
		case !errors.Is(err, ErrChangeNotFound):
			return nil, fmt.Errorf("read %s#%d: %w", workspace, m.number, err)
		}
		out = append(out, item)
	}
	return out, nil
}

// matched is an indexed change with the files of it that match the globs
// of an activity, in ascending byte order, and, for a landed change, its
// landed commit and time and its session.
type matched struct {
	number  int64
	paths   []string
	sha     string
	at      time.Time
	session *string
}

// matchingFiles runs query, whose rows are a change's number and one of
// its files, grouped by change, and then for a landed change its landed
// commit, landed time and session, and returns, in the order of the rows,
// the changes with at least one file matching one of globs.
func matchingFiles(ctx context.Context, q Querier, globs []string, query string, args ...any) ([]matched, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []matched
	for rows.Next() {
		var m matched
		var path string
		var sha, session sql.NullString
		var at sql.NullInt64
		dest := []any{&m.number, &path, &sha, &at, &session}
		if err := rows.Scan(dest[:len(cols)]...); err != nil {
			return nil, err
		}
		if !matchesAny(globs, path) {
			continue
		}
		if n := len(out); n > 0 && out[n-1].number == m.number {
			out[n-1].paths = append(out[n-1].paths, path)
			continue
		}
		m.paths, m.sha, m.at = []string{path}, sha.String, time.Unix(0, at.Int64).UTC()
		if session.Valid {
			m.session = &session.String
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ofSession reports whether session is the excluded session excluded, ""
// excluding none.
func ofSession(session *string, excluded string) bool {
	return excluded != "" && session != nil && *session == excluded
}

// activityRoute answers the activity of the paths of the query on the
// workspace the route acts on.
func (c *Component) activityRoute(w http.ResponseWriter, r *http.Request) error {
	q, err := activityQueryOf(r.URL.Query(), c.deps.Now())
	if err != nil {
		return err
	}
	a, err := c.Activity(r.Context(), actedOn(r), q)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, a)
	return nil
}

// activityQueryOf reads the activity query of the query parameters v: the
// globs of every paths value, each a comma list; target; since, a commit id
// of exactly 40 hexadecimal characters or an RFC 3339 timestamp, 24 hours
// before now when absent; exclude_session; and exclude_change, a positive
// integer. Each violation is a 400 invalid_request.
func activityQueryOf(v url.Values, now time.Time) (ActivityQuery, error) {
	var globs []string
	for _, value := range v["paths"] {
		globs = append(globs, strings.Split(value, ",")...)
	}
	if strings.Join(globs, "") == "" {
		return ActivityQuery{}, server.InvalidRequest("paths must name at least one glob")
	}
	globs, err := identity.ValidatePaths(globs)
	if err != nil {
		return ActivityQuery{}, server.InvalidRequest(err.Error())
	}
	q := ActivityQuery{Paths: globs, TargetBranch: v.Get("target"), ExcludeSession: v.Get("exclude_session"),
		Since: now.Add(-defaultSince)}
	if v.Has("since") {
		s := v.Get("since")
		if isCommitID(s) {
			q.SinceCommit = strings.ToLower(s)
		} else if q.Since, err = time.Parse(time.RFC3339, s); err != nil {
			return ActivityQuery{}, server.InvalidRequest(
				"since must be a commit id of 40 hexadecimal characters or an RFC 3339 timestamp, not " + s)
		}
	}
	if v.Has("exclude_change") {
		n, ok := positive(v.Get("exclude_change"))
		if !ok {
			return ActivityQuery{}, server.InvalidRequest("exclude_change must be a positive integer")
		}
		q.ExcludeChange = n
	}
	return q, nil
}

// isCommitID reports whether s is a full commit id: exactly 40 hexadecimal
// characters.
func isCommitID(s string) bool {
	return len(s) == 40 && strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}
