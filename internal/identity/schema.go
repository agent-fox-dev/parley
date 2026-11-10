package identity

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/store"
)

func createTable(stmt string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, stmt)
		return err
	}
}

// Steps create the identity records a login writes and authentication reads:
// users, orgs, memberships, API keys and personal access tokens. Later
// packages read and write these tables and add none of their own for them.
// Times are Unix nanoseconds in UTC; provider and provider_id are null for a
// user created without a login, so the unique pair binds only real
// identities.
var Steps = []store.Step{
	{Name: "identity.users", Target: store.Main, Apply: createTable(`CREATE TABLE users (
	id          TEXT PRIMARY KEY,
	username    TEXT NOT NULL UNIQUE,
	email       TEXT NOT NULL UNIQUE,
	full_name   TEXT NOT NULL DEFAULT '',
	role        TEXT NOT NULL,
	status      TEXT NOT NULL,
	provider    TEXT,
	provider_id TEXT,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	UNIQUE (provider, provider_id)
)`)},
	{Name: "identity.orgs", Target: store.Main, Apply: createTable(`CREATE TABLE orgs (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	slug       TEXT NOT NULL UNIQUE,
	url        TEXT NOT NULL DEFAULT '',
	owner_id   TEXT,
	status     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
)`)},
	{Name: "identity.memberships", Target: store.Main, Apply: createTable(`CREATE TABLE memberships (
	org_id     TEXT NOT NULL,
	user_id    TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (org_id, user_id)
)`)},
	{Name: "identity.api_keys", Target: store.Main, Apply: createTable(`CREATE TABLE api_keys (
	key_id       TEXT PRIMARY KEY,
	user_id      TEXT NOT NULL,
	secret_hash  TEXT NOT NULL,
	expires_days INTEGER NOT NULL,
	expires_at   INTEGER,
	revoked_at   INTEGER,
	created_at   INTEGER NOT NULL
)`)},
	{Name: "identity.pats", Target: store.Main, Apply: createTable(`CREATE TABLE pats (
	token_id    TEXT PRIMARY KEY,
	user_id     TEXT NOT NULL,
	secret_hash TEXT NOT NULL,
	name        TEXT NOT NULL,
	permissions TEXT NOT NULL,
	paths       TEXT,
	workspace   TEXT,
	expires_at  INTEGER,
	revoked_at  INTEGER,
	created_at  INTEGER NOT NULL
)`)},
}
