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

export async function listUsers(store: RelationalStore): Promise<UserRecord[]> {
  const rows = await store.all('SELECT * FROM users ORDER BY created_at, id');
  return rows.map(toUser);
}

export async function deleteUser(store: RelationalStore, id: string): Promise<void> {
  await store.batch([
    { sql: 'DELETE FROM org_members WHERE user_id = ?', params: [id] },
    { sql: 'DELETE FROM credentials WHERE user_id = ?', params: [id] },
    { sql: 'DELETE FROM users WHERE id = ?', params: [id] },
  ]);
}

export async function setUserRole(
  store: RelationalStore,
  id: string,
  role: 'user' | 'admin',
  at: string,
): Promise<void> {
  await store.run('UPDATE users SET role = ?, updated_at = ? WHERE id = ?', [role, at, id]);
}

export async function setUserStatus(
  store: RelationalStore,
  id: string,
  status: 'active' | 'blocked',
  at: string,
): Promise<void> {
  await store.run('UPDATE users SET status = ?, updated_at = ? WHERE id = ?', [status, at, id]);
}

/**
 * How many admins could still administer the deployment. A blocked admin
 * cannot, so a deployment is not allowed to rely on one.
 */
export async function countActiveAdmins(store: RelationalStore): Promise<number> {
  const rows = await store.all<{ total: number }>(
    "SELECT COUNT(*) AS total FROM users WHERE role = 'admin' AND status = 'active'",
  );
  return Number(rows[0]?.total ?? 0);
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

/**
 * The scopes a token holds from now on. Passing a revocation instant with
 * an empty set is how a token is emptied and retired at once.
 */
export async function setPermissions(
  store: RelationalStore,
  id: string,
  permissions: readonly string[],
  revokedAt: string | null,
): Promise<void> {
  await store.run(
    revokedAt === null
      ? 'UPDATE credentials SET permissions = ? WHERE id = ?'
      : 'UPDATE credentials SET permissions = ?, revoked_at = ? WHERE id = ?',
    revokedAt === null ? [permissions.join(' '), id] : [permissions.join(' '), revokedAt, id],
  );
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

export async function findOrg(store: RelationalStore, id: string): Promise<OrgRecord | undefined> {
  const rows = await store.all('SELECT * FROM orgs WHERE id = ?', [id]);
  return rows[0] === undefined ? undefined : toOrg(rows[0]);
}

/**
 * Whether a user belongs to an organization. The workspace access check of
 * the later packages reads this on every request that names a workspace.
 */
export async function isMember(
  store: RelationalStore,
  orgId: string,
  userId: string,
): Promise<boolean> {
  const rows = await store.all(
    'SELECT 1 AS present FROM org_members WHERE org_id = ? AND user_id = ?',
    [orgId, userId],
  );
  return rows.length > 0;
}

export async function listOrgs(store: RelationalStore): Promise<OrgRecord[]> {
  const rows = await store.all('SELECT * FROM orgs ORDER BY created_at, id');
  return rows.map(toOrg);
}

export async function findOrgByNameOrSlug(
  store: RelationalStore,
  name: string,
  slug: string,
): Promise<OrgRecord | undefined> {
  const rows = await store.all('SELECT * FROM orgs WHERE name = ? OR slug = ?', [name, slug]);
  return rows[0] === undefined ? undefined : toOrg(rows[0]);
}

export async function updateOrg(
  store: RelationalStore,
  id: string,
  fields: { name?: string; url?: string; status?: 'active' | 'blocked' },
  at: string,
): Promise<void> {
  const assignments: string[] = [];
  const params: unknown[] = [];
  for (const [column, value] of Object.entries(fields)) {
    if (value !== undefined) {
      assignments.push(`${column} = ?`);
      params.push(value);
    }
  }
  assignments.push('updated_at = ?');
  params.push(at, id);
  await store.run(`UPDATE orgs SET ${assignments.join(', ')} WHERE id = ?`, params);
}

export async function deleteOrg(store: RelationalStore, id: string): Promise<void> {
  await store.batch([
    { sql: 'DELETE FROM org_members WHERE org_id = ?', params: [id] },
    { sql: 'DELETE FROM orgs WHERE id = ?', params: [id] },
  ]);
}

export async function membersOfOrg(
  store: RelationalStore,
  orgId: string,
): Promise<MembershipRecord[]> {
  const rows = await store.all(
    `SELECT org_members.org_id, org_members.user_id, users.username, users.email,
            org_members.role, org_members.created_at
     FROM org_members JOIN users ON users.id = org_members.user_id
     WHERE org_members.org_id = ? ORDER BY org_members.created_at, org_members.user_id`,
    [orgId],
  );
  return rows.map((row) => ({
    org_id: text(row['org_id']),
    user_id: text(row['user_id']),
    username: text(row['username']),
    email: text(row['email']),
    role: text(row['role']),
    created_at: text(row['created_at']),
  }));
}

export async function removeMember(
  store: RelationalStore,
  orgId: string,
  userId: string,
): Promise<void> {
  await store.run('DELETE FROM org_members WHERE org_id = ? AND user_id = ?', [orgId, userId]);
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
