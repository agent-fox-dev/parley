import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fixedClock } from '../platform/local/clock.js';
import { sqliteStore } from '../platform/local/store.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import {
  formatApiKey,
  formatPat,
  hashSecret,
  newIdentifier,
  newSecret,
} from '../auth/credentials.js';
import {
  type CredentialRecord,
  type OrgRecord,
  type UserRecord,
  addMember,
  insertCredential,
  insertOrg,
  insertUser,
} from '../store/identity.js';
import { type WorkspaceRecord, insertWorkspace } from '../store/workspaces.js';
import { migrate } from '../store/migrations/index.js';

/** The instant every seeded record is dated from, so expiry is decidable. */
export const SEED_NOW = new Date('2026-03-01T12:00:00Z');

export function seedClock(): Clock {
  return fixedClock(SEED_NOW);
}

export interface Seeded {
  store: RelationalStore;
  dispose: () => void;
}

/** A migrated store on its own temporary file, removed by dispose. */
export async function freshStore(): Promise<Seeded> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-identity-'));
  const store = sqliteStore(join(dir, 'parley.db'));
  await migrate(store, SEED_NOW);
  return { store, dispose: () => rmSync(dir, { recursive: true, force: true }) };
}

let counter = 0;
const unique = (prefix: string) => `${prefix}-${String(++counter)}`;

export async function seedUser(
  store: RelationalStore,
  over: Partial<UserRecord> = {},
): Promise<UserRecord> {
  const name = unique('user');
  const user: UserRecord = {
    id: unique('u'),
    username: name,
    email: `${name}@example.com`,
    full_name: 'Seeded User',
    status: 'active',
    role: 'user',
    provider: 'github',
    provider_id: unique('p'),
    created_at: SEED_NOW.toISOString(),
    updated_at: SEED_NOW.toISOString(),
    ...over,
  };
  await insertUser(store, user);
  return user;
}

export interface SeededCredential {
  record: CredentialRecord;
  /** The credential string a client presents. Held nowhere but here. */
  presented: string;
}

async function seedCredential(
  store: RelationalStore,
  kind: 'api_key' | 'pat',
  user: UserRecord,
  over: Partial<CredentialRecord>,
): Promise<SeededCredential> {
  const id = newIdentifier();
  const secret = newSecret();
  const record: CredentialRecord = {
    id,
    kind,
    user_id: user.id,
    name: kind === 'pat' ? 'seeded token' : '',
    secret_hash: await hashSecret(secret),
    permissions: [],
    created_at: SEED_NOW.toISOString(),
    expires_at: null,
    revoked_at: null,
    ...over,
  };
  await insertCredential(store, record);
  return {
    record,
    presented: kind === 'pat' ? formatPat(record.id, secret) : formatApiKey(record.id, secret),
  };
}

export async function seedApiKey(
  store: RelationalStore,
  user: UserRecord,
  over: Partial<CredentialRecord> = {},
): Promise<SeededCredential> {
  return seedCredential(store, 'api_key', user, over);
}

export async function seedPat(
  store: RelationalStore,
  user: UserRecord,
  permissions: readonly string[],
  over: Partial<CredentialRecord> = {},
): Promise<SeededCredential> {
  return seedCredential(store, 'pat', user, { permissions: [...permissions], ...over });
}

/** A workspace row, for suites that need one without posting a create. */
export async function seedWorkspace(
  store: RelationalStore,
  over: Partial<WorkspaceRecord> & { owner_id: string },
): Promise<WorkspaceRecord> {
  const slug = over.slug ?? unique('ws');
  const workspace: WorkspaceRecord = {
    slug,
    git_url: 'https://github.com/u/r.git',
    branch: null,
    org_id: null,
    status: 'pending',
    display_name: slug,
    description: '',
    head_sha: null,
    clone_error: null,
    upstream_head_sha: null,
    last_sync_at: null,
    sync_error: null,
    workspace_mode: 'standard',
    upstream_url: null,
    integration_branch: null,
    settings: {},
    created_at: SEED_NOW.toISOString(),
    updated_at: SEED_NOW.toISOString(),
    ...over,
  };
  await insertWorkspace(store, workspace);
  return workspace;
}

export async function seedOrg(
  store: RelationalStore,
  owner: UserRecord,
  over: Partial<OrgRecord> = {},
): Promise<OrgRecord> {
  const slug = unique('org');
  const org: OrgRecord = {
    id: unique('o'),
    name: slug,
    slug,
    url: '',
    owner_id: owner.id,
    status: 'active',
    created_at: SEED_NOW.toISOString(),
    updated_at: SEED_NOW.toISOString(),
    ...over,
  };
  await insertOrg(store, org);
  await addMember(store, {
    org_id: org.id,
    user_id: owner.id,
    role: 'owner',
    created_at: SEED_NOW.toISOString(),
  });
  return org;
}
