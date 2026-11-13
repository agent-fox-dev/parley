package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// RowsQuerier is what a read of several rows goes through: the store's
// *sql.DB or an open *sql.Tx.
type RowsQuerier interface {
	Querier
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Membership is a user's membership of an org.
type Membership struct {
	OrgID     string
	UserID    string
	CreatedAt time.Time
}

// MarshalJSON writes o as the REST API's Org object, with owner_id null
// when the org has no owner.
func (o Org) MarshalJSON() ([]byte, error) {
	var owner *string
	if o.OwnerID != "" {
		owner = &o.OwnerID
	}
	return json.Marshal(struct {
		ID        string       `json:"id"`
		Name      string       `json:"name"`
		Slug      string       `json:"slug"`
		URL       string       `json:"url"`
		OwnerID   *string      `json:"owner_id"`
		Status    string       `json:"status"`
		CreatedAt timefmt.Time `json:"created_at"`
		UpdatedAt timefmt.Time `json:"updated_at"`
	}{o.ID, o.Name, o.Slug, o.URL, owner, o.Status, timefmt.Time(o.CreatedAt), timefmt.Time(o.UpdatedAt)})
}

// OrgByID returns the org with the id id, or ErrNotFound.
func OrgByID(ctx context.Context, q Querier, id string) (Org, error) {
	return orgWhere(ctx, q, `id = ?`, id)
}

// OrgBySlug returns the org with the slug slug, compared as stored, letter
// case included, or ErrNotFound.
func OrgBySlug(ctx context.Context, q Querier, slug string) (Org, error) {
	return orgWhere(ctx, q, `slug = ?`, slug)
}

// orgWhere returns the org the condition where, with the arguments args,
// selects.
func orgWhere(ctx context.Context, q Querier, where string, args ...any) (Org, error) {
	o, err := scanOrg(q.QueryRowContext(ctx, `SELECT `+orgColumns+` FROM orgs WHERE `+where, args...))
	if err != nil {
		return Org{}, lookupErr("org", err)
	}
	return o, nil
}

// orgColumns are the columns scanOrg reads, in its order.
const orgColumns = `id, name, slug, url, owner_id, status, created_at, updated_at`

// scanOrg reads an org from a row of orgColumns.
func scanOrg(row interface{ Scan(dest ...any) error }) (Org, error) {
	var o Org
	var owner sql.NullString
	var created, updated int64
	if err := row.Scan(&o.ID, &o.Name, &o.Slug, &o.URL, &owner, &o.Status, &created, &updated); err != nil {
		return Org{}, err
	}
	o.OwnerID, o.CreatedAt, o.UpdatedAt = owner.String, fromNanos(created), fromNanos(updated)
	return o, nil
}

// Memberships returns the memberships of the org orgID, oldest first, then
// by user id; none when the org has no member or does not exist.
func Memberships(ctx context.Context, q RowsQuerier, orgID string) ([]Membership, error) {
	rows, err := q.QueryContext(ctx, `SELECT org_id, user_id, created_at FROM memberships WHERE org_id = ?
ORDER BY created_at, user_id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("read the memberships of an org: %w", err)
	}
	defer rows.Close()
	ms := []Membership{}
	for rows.Next() {
		var m Membership
		var created int64
		if err := rows.Scan(&m.OrgID, &m.UserID, &created); err != nil {
			return nil, fmt.Errorf("read a membership: %w", err)
		}
		m.CreatedAt = fromNanos(created)
		ms = append(ms, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the memberships of an org: %w", err)
	}
	return ms, nil
}

// OrgBySelector returns the org the selector sel names, or ErrNotFound.
// The shape of sel decides what it is compared with, and nothing else is
// tried: a value in hyphenated UUID form is an id, every other value a
// slug. The comparison is exact, letter case included, so an org's name
// never names it.
func OrgBySelector(ctx context.Context, q Querier, sel string) (Org, error) {
	if isUUID(sel) {
		return OrgByID(ctx, q, sel)
	}
	return OrgBySlug(ctx, q, sel)
}

// slugPattern is the form of an org slug: 1 to 128 characters of a-z, 0-9,
// underscore and hyphen that start and end with a letter or digit.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,126}[a-z0-9])?$`)

// validSlug reports whether s has the form of an org slug.
func validSlug(s string) bool { return slugPattern.MatchString(s) }

// The conflicts of a create or an update of an org.
var (
	errOrgNameTaken = conflictError{"an org has the name"}
	errOrgSlugTaken = conflictError{"an org has the slug"}
)

// orgNameTaken reports whether an org other than the org exceptID ("" for
// none) has the name name, compared exactly.
func orgNameTaken(ctx context.Context, q Querier, name, exceptID string) (bool, error) {
	return exists(ctx, q, `SELECT count(*) FROM orgs WHERE name = ? AND id <> ?`, name, exceptID)
}

// createOrg stores o, with a new id, in tx and returns it as stored. Its
// name and slug must be free, else it returns errOrgNameTaken or
// errOrgSlugTaken and writes nothing. It creates no membership, also when o
// has an owner.
func createOrg(ctx context.Context, tx *sql.Tx, o Org) (Org, error) {
	taken, err := orgNameTaken(ctx, tx, o.Name, "")
	if err != nil {
		return Org{}, err
	}
	if taken {
		return Org{}, errOrgNameTaken
	}
	taken, err = exists(ctx, tx, `SELECT count(*) FROM orgs WHERE slug = ?`, o.Slug)
	if err != nil {
		return Org{}, err
	}
	if taken {
		return Org{}, errOrgSlugTaken
	}
	o.ID = uuid.NewV4().String()
	owner := sql.NullString{String: o.OwnerID, Valid: o.OwnerID != ""}
	at := o.CreatedAt.UTC().UnixNano()
	_, err = tx.ExecContext(ctx, `INSERT INTO orgs (id, name, slug, url, owner_id, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, o.ID, o.Name, o.Slug, o.URL, owner, o.Status, at, at)
	if isUniqueViolation(err) {
		// The checks above leave this only to an id drawn twice.
		return Org{}, conflictError{"the org is taken"}
	}
	if err != nil {
		return Org{}, fmt.Errorf("store the org: %w", err)
	}
	return OrgByID(ctx, tx, o.ID)
}

// updateOrg stores the name and the url of the org id in tx, each when it
// is not nil, with updated_at now, and returns the stored org, or
// ErrNotFound when there is no such org. A name another org has is
// errOrgNameTaken, and nothing is written; the org's own name is not.
func updateOrg(ctx context.Context, tx *sql.Tx, id string, name, url *string, now time.Time) (Org, error) {
	if name != nil {
		taken, err := orgNameTaken(ctx, tx, *name, id)
		if err != nil {
			return Org{}, err
		}
		if taken {
			return Org{}, errOrgNameTaken
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE orgs SET name = coalesce(?, name), url = coalesce(?, url), updated_at = ?
WHERE id = ?`, orNull(name), orNull(url), now.UTC().UnixNano(), id)
	if err != nil {
		return Org{}, fmt.Errorf("store the org: %w", err)
	}
	return OrgByID(ctx, tx, id)
}

// orNull is the value of p for a statement: SQL NULL when p is nil.
func orNull(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

// orgPosition is the position of an org in the list of orgs: by name,
// ascending.
type orgPosition struct {
	Name string `json:"name"`
}

// listOrgs returns up to limit orgs in the order of the list of orgs, by
// name compared byte-wise, after the position after when it is not nil,
// the blocked ones only when includeBlocked is true.
func listOrgs(ctx context.Context, q RowsQuerier, includeBlocked bool, after *orgPosition, limit int) ([]Org, error) {
	query := `SELECT ` + orgColumns + ` FROM orgs WHERE 1 = 1`
	var args []any
	if !includeBlocked {
		query += ` AND status = ?`
		args = append(args, StatusActive)
	}
	if after != nil {
		query += ` AND name > ?`
		args = append(args, after.Name)
	}
	query += ` ORDER BY name LIMIT ?`
	rows, err := q.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list the orgs: %w", err)
	}
	defer rows.Close()
	var orgs []Org
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, fmt.Errorf("list the orgs: %w", err)
		}
		orgs = append(orgs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the orgs: %w", err)
	}
	return orgs, nil
}

// isMember reports whether the user userID is a member of the org orgID.
func isMember(ctx context.Context, q Querier, orgID, userID string) (bool, error) {
	return exists(ctx, q, `SELECT count(*) FROM memberships WHERE org_id = ? AND user_id = ?`, orgID, userID)
}
