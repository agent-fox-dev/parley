package identity

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/store"
)

// SchemaStep creates the identity records in the transactional store: the
// users, the orgs, the memberships, the API keys and the PATs. Timestamps are
// nanoseconds since the Unix epoch in UTC; a PAT's permissions and paths are
// JSON arrays, paths NULL when the token has no path limits. A credential's
// row holds the lowercase hex SHA-256 of its secret part, never the secret.
// Deleting a user deletes its keys, PATs and memberships and clears the
// owner of the orgs it owned.
var SchemaStep = store.Step{
	Name:   "identity_records",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE users (
	id          TEXT    PRIMARY KEY,
	username    TEXT    NOT NULL UNIQUE,
	email       TEXT    NOT NULL UNIQUE,
	full_name   TEXT    NOT NULL DEFAULT '',
	role        TEXT    NOT NULL CHECK (role IN ('user', 'admin')),
	status      TEXT    NOT NULL CHECK (status IN ('active', 'blocked')),
	provider    TEXT    NOT NULL,
	provider_id TEXT    NOT NULL,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	UNIQUE (provider, provider_id)
);
CREATE TABLE orgs (
	id         TEXT    PRIMARY KEY,
	name       TEXT    NOT NULL UNIQUE,
	slug       TEXT    NOT NULL UNIQUE,
	url        TEXT    NOT NULL DEFAULT '',
	owner_id   TEXT    REFERENCES users (id) ON DELETE SET NULL,
	status     TEXT    NOT NULL CHECK (status IN ('active', 'blocked')),
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE memberships (
	org_id     TEXT    NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
	user_id    TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id);
CREATE TABLE api_keys (
	key_id       TEXT    PRIMARY KEY,
	user_id      TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	secret_hash  TEXT    NOT NULL,
	expires_days INTEGER NOT NULL,
	expires_at   INTEGER,
	revoked_at   INTEGER,
	created_at   INTEGER NOT NULL
);
CREATE INDEX api_keys_user ON api_keys (user_id);
CREATE TABLE pats (
	token_id    TEXT    PRIMARY KEY,
	user_id     TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	secret_hash TEXT    NOT NULL,
	name        TEXT    NOT NULL,
	permissions TEXT    NOT NULL,
	paths       TEXT,
	workspace   TEXT,
	expires_at  INTEGER,
	revoked_at  INTEGER,
	created_at  INTEGER NOT NULL
);
CREATE INDEX pats_user ON pats (user_id);`)
		return err
	},
}

// PersonalOrgStep marks the personal org of each user: the org flagged
// personal and owned by the user. It is a step of its own, after
// SchemaStep, so that a store whose records exist already gains the flag;
// there it marks the org each user owns that is named after the user's
// username, which is how CreateUserAndPersonalOrg made it. Neither a later
// username nor a rename of the org moves the mark.
var PersonalOrgStep = store.Step{
	Name:   "identity_personal_orgs",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
ALTER TABLE orgs ADD COLUMN personal INTEGER NOT NULL DEFAULT 0 CHECK (personal IN (0, 1));
UPDATE orgs SET personal = 1
WHERE EXISTS (SELECT 1 FROM users u WHERE u.id = orgs.owner_id AND u.username = orgs.name);
CREATE UNIQUE INDEX orgs_personal_owner ON orgs (owner_id) WHERE personal = 1;`)
		return err
	},
}
