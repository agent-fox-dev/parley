import type { RelationalStore, Row } from '../platform/index.js';

export interface UserRecord {
  id: string;
  username: string;
  email: string;
  full_name: string;
  status: 'active' | 'blocked';
  role: 'user' | 'admin';
  provider: string;
  provider_id: string;
  created_at: string;
  updated_at: string;
}

/** An API key or a personal access token. The secret is held only as a hash. */
export interface CredentialRecord {
  id: string;
  kind: 'api_key' | 'pat';
  user_id: string;
  name: string;
  secret_hash: string;
  /** The scopes a token was granted, empty for an API key. */
  permissions: string[];
  created_at: string;
  expires_at: string | null;
  revoked_at: string | null;
}

export interface OrgRecord {
  id: string;
  name: string;
  slug: string;
  url: string;
  owner_id: string;
  status: 'active' | 'blocked';
  created_at: string;
  updated_at: string;
}

export interface MembershipRecord {
  org_id: string;
  user_id: string;
  username: string;
  email: string;
  role: string;
  created_at: string;
}

const text = (value: unknown): string => (typeof value === 'string' ? value : '');
const nullableText = (value: unknown): string | null => (typeof value === 'string' ? value : null);

function toUser(row: Row): UserRecord {
  return {
    id: text(row['id']),
    username: text(row['username']),
    email: text(row['email']),
    full_name: text(row['full_name']),
    status: text(row['status']) === 'blocked' ? 'blocked' : 'active',
    role: text(row['role']) === 'admin' ? 'admin' : 'user',
    provider: text(row['provider']),
    provider_id: text(row['provider_id']),
    created_at: text(row['created_at']),
    updated_at: text(row['updated_at']),
  };
}

function toCredential(row: Row): CredentialRecord {
  const permissions = text(row['permissions']);
  return {
    id: text(row['id']),
    kind: text(row['kind']) === 'pat' ? 'pat' : 'api_key',
    user_id: text(row['user_id']),
    name: text(row['name']),
    secret_hash: text(row['secret_hash']),
    permissions: permissions.length === 0 ? [] : permissions.split(' '),
    created_at: text(row['created_at']),
    expires_at: nullableText(row['expires_at']),
    revoked_at: nullableText(row['revoked_at']),
  };
}

function toOrg(row: Row): OrgRecord {
  return {
    id: text(row['id']),
    name: text(row['name']),
    slug: text(row['slug']),
    url: text(row['url']),
    owner_id: text(row['owner_id']),
    status: text(row['status']) === 'blocked' ? 'blocked' : 'active',
    created_at: text(row['created_at']),
    updated_at: text(row['updated_at']),
  };
}

export async function findUser(
  store: RelationalStore,
  id: string,
): Promise<UserRecord | undefined> {
  const rows = await store.all('SELECT * FROM users WHERE id = ?', [id]);
  return rows[0] === undefined ? undefined : toUser(rows[0]);
}

/**
 * The credential with this identifier, whatever its state. Authentication
 * decides on revocation and expiry itself, so they are not filtered here.
 */
export async function findCredential(
  store: RelationalStore,
  kind: 'api_key' | 'pat',
  id: string,
): Promise<CredentialRecord | undefined> {
  const rows = await store.all('SELECT * FROM credentials WHERE id = ? AND kind = ?', [id, kind]);
  return rows[0] === undefined ? undefined : toCredential(rows[0]);
}

export async function findUserByProviderIdentity(
  store: RelationalStore,
  provider: string,
  providerId: string,
): Promise<UserRecord | undefined> {
  const rows = await store.all('SELECT * FROM users WHERE provider = ? AND provider_id = ?', [
    provider,
    providerId,
  ]);
  return rows[0] === undefined ? undefined : toUser(rows[0]);
}

export async function findUserByUsername(
  store: RelationalStore,
  username: string,
): Promise<UserRecord | undefined> {
  const rows = await store.all('SELECT * FROM users WHERE username = ?', [username]);
  return rows[0] === undefined ? undefined : toUser(rows[0]);
}

export async function insertUser(store: RelationalStore, user: UserRecord): Promise<void> {
  await store.run(
    `INSERT INTO users (id, username, email, full_name, status, role, provider, provider_id, created_at, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    [
      user.id,
      user.username,
      user.email,
      user.full_name,
      user.status,
      user.role,
      user.provider,
      user.provider_id,
      user.created_at,
      user.updated_at,
    ],
  );
}

export async function insertCredential(
  store: RelationalStore,
  credential: CredentialRecord,
): Promise<void> {
  await store.run(
    `INSERT INTO credentials (id, kind, user_id, name, secret_hash, permissions, created_at, expires_at, revoked_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    [
      credential.id,
      credential.kind,
      credential.user_id,
      credential.name,
      credential.secret_hash,
      credential.permissions.join(' '),
      credential.created_at,
      credential.expires_at,
      credential.revoked_at,
    ],
  );
}

export async function insertOrg(store: RelationalStore, org: OrgRecord): Promise<void> {
  await store.run(
    `INSERT INTO orgs (id, name, slug, url, owner_id, status, created_at, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
    [org.id, org.name, org.slug, org.url, org.owner_id, org.status, org.created_at, org.updated_at],
  );
}

export async function addMember(
  store: RelationalStore,
  member: { org_id: string; user_id: string; role: string; created_at: string },
): Promise<void> {
  await store.run(
    `INSERT OR REPLACE INTO org_members (org_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
    [member.org_id, member.user_id, member.role, member.created_at],
  );
}

export async function updateFullName(
  store: RelationalStore,
  id: string,
  fullName: string,
  at: string,
): Promise<void> {
  await store.run('UPDATE users SET full_name = ?, updated_at = ? WHERE id = ?', [
    fullName,
    at,
    id,
  ]);
}

/** Every credential of one kind a user holds, revoked and expired included. */
export async function credentialsOfUser(
  store: RelationalStore,
  userId: string,
  kind: 'api_key' | 'pat',
): Promise<CredentialRecord[]> {
  const rows = await store.all(
    'SELECT * FROM credentials WHERE user_id = ? AND kind = ? ORDER BY created_at, id',
    [userId, kind],
  );
  return rows.map(toCredential);
}

export async function rotateSecret(
  store: RelationalStore,
  id: string,
  secretHash: string,
  expiresAt: string | null,
): Promise<void> {
  await store.run('UPDATE credentials SET secret_hash = ?, expires_at = ? WHERE id = ?', [
    secretHash,
    expiresAt,
    id,
  ]);
}

export async function revokeCredential(
  store: RelationalStore,
  id: string,
  at: string,
): Promise<void> {
  await store.run('UPDATE credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL', [
    at,
    id,
  ]);
}

/**
 * Retires every API key the user holds and issues the one given, so that a
 * sign-in never leaves two live keys or, on a failure, none.
 */
export async function replaceApiKeys(
  store: RelationalStore,
  issued: CredentialRecord,
  at: string,
): Promise<void> {
  await store.batch([
    {
      sql: 'UPDATE credentials SET revoked_at = ? WHERE user_id = ? AND kind = ? AND revoked_at IS NULL',
      params: [at, issued.user_id, 'api_key'],
    },
    {
      sql: `INSERT INTO credentials (id, kind, user_id, name, secret_hash, permissions, created_at, expires_at, revoked_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      params: [
        issued.id,
        issued.kind,
        issued.user_id,
        issued.name,
        issued.secret_hash,
        issued.permissions.join(' '),
        issued.created_at,
        issued.expires_at,
        issued.revoked_at,
      ],
    },
  ]);
}

/** The organizations a user belongs to, read on every workspace decision. */
export async function orgsOfUser(store: RelationalStore, userId: string): Promise<OrgRecord[]> {
  const rows = await store.all(
    `SELECT orgs.* FROM orgs JOIN org_members ON org_members.org_id = orgs.id
     WHERE org_members.user_id = ? ORDER BY orgs.created_at`,
    [userId],
  );
  return rows.map(toOrg);
}

export { toUser, toCredential, toOrg };
