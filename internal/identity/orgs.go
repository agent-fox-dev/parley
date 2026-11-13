package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

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
	var o Org
	var owner sql.NullString
	var created, updated int64
	err := q.QueryRowContext(ctx, `SELECT id, name, slug, url, owner_id, status, created_at, updated_at
FROM orgs WHERE `+where, args...).Scan(&o.ID, &o.Name, &o.Slug, &o.URL, &owner, &o.Status, &created, &updated)
	if err != nil {
		return Org{}, lookupErr("org", err)
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
