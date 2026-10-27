import { afterEach, describe, expect, it } from 'vitest';
import {
  type Service,
  ADMIN_TOKEN,
  MOUNT,
  as,
  identityService,
  withJson,
} from '../testutil/app.js';
import {
  SEED_NOW,
  seedApiKey,
  seedBoundToken,
  seedOrg,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { sweepBindings } from '../store/bindings.js';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function ownerWithWorkspace() {
  service = await identityService();
  const { store } = service;
  const owner = await seedUser(store);
  const org = await seedOrg(store, owner);
  const workspace = await seedWorkspace(store, {
    slug: 'my-ws',
    owner_id: owner.id,
    org_id: org.id,
  });
  return { ...service, owner, workspace, key: (await seedApiKey(store, owner)).presented };
}

interface MintedBody {
  token_id: string;
  token: string;
  name: string;
  permissions: string[];
  workspace: string;
  expires_at: string | null;
  created_at: string;
}

describe('minting a token bound to a workspace', () => {
  it('returns it once, says which workspace it is for, and the token reaches that workspace', async () => {
    const held = await ownerWithWorkspace();

    const response = await held.app.request(
      `${MOUNT}/workspaces/my-ws/tokens`,
      withJson(
        'POST',
        {
          name: 'agent-fox service',
          permissions: ['workspaces:read', 'patches:read'],
          expires: 30,
        },
        held.key,
      ),
    );
    const minted = (await response.json()) as MintedBody;

    expect(response.status).toBe(201);
    expect(response.headers.get('cache-control')).toBe('no-store');
    expect(minted.token).toMatch(/^af_pat_[0-9a-f]+_[0-9a-f]+$/);
    expect(minted.workspace).toBe('my-ws');
    expect(minted.permissions).toEqual(['workspaces:read', 'patches:read']);
    expect(minted.expires_at).toBe(
      new Date(SEED_NOW.getTime() + 30 * 24 * 60 * 60 * 1000).toISOString(),
    );

    expect((await held.app.request(`${MOUNT}/workspaces/my-ws`, as(minted.token))).status).toBe(
      200,
    );

    const listed = await (
      await held.app.request(`${MOUNT}/workspaces/my-ws/tokens`, as(held.key))
    ).text();
    expect(listed).not.toContain(minted.token);
  });
});

describe("a workspace's bound tokens", () => {
  it('list the dead with the living, in order, and never a secret', async () => {
    const held = await ownerWithWorkspace();
    const { store } = held;
    const live = await seedBoundToken(store, held.owner, 'my-ws', ['workspaces:read'], {
      created_at: '2026-01-01T00:00:00.000Z',
      name: 'live',
    });
    const revoked = await seedBoundToken(store, held.owner, 'my-ws', ['workspaces:read'], {
      created_at: '2026-01-02T00:00:00.000Z',
      name: 'revoked',
      revoked_at: '2026-02-01T00:00:00.000Z',
    });
    const expired = await seedBoundToken(store, held.owner, 'my-ws', ['workspaces:read'], {
      created_at: '2026-01-03T00:00:00.000Z',
      name: 'expired',
      expires_at: '2026-02-01T00:00:00.000Z',
    });
    // A binding whose token is gone has nothing to show.
    await store.run(
      'INSERT INTO token_bindings (token_id, workspace_slug, created_at) VALUES (?, ?, ?)',
      ['vanished', 'my-ws', SEED_NOW.toISOString()],
    );

    const byOwner = await held.app.request(`${MOUNT}/workspaces/my-ws/tokens`, as(held.key));
    const byAdmin = await held.app.request(`${MOUNT}/workspaces/my-ws/tokens`, as(ADMIN_TOKEN));
    const listed = (await byOwner.json()) as Array<Record<string, unknown>>;

    expect(byOwner.status).toBe(200);
    expect(byAdmin.status).toBe(200);
    expect(listed.map((entry) => entry['name'])).toEqual(['live', 'revoked', 'expired']);
    expect(listed.every((entry) => entry['workspace'] === 'my-ws')).toBe(true);

    const text = JSON.stringify(listed);
    for (const seeded of [live, revoked, expired]) {
      expect(text).not.toContain(seeded.presented);
      expect(text).not.toContain(seeded.record.secret_hash);
    }
  });

  it('are still listed once the workspace is archived', async () => {
    const held = await ownerWithWorkspace();
    await seedBoundToken(held.store, held.owner, 'my-ws', ['workspaces:read']);
    await held.store.run('UPDATE workspaces SET status = ? WHERE slug = ?', ['archived', 'my-ws']);

    const response = await held.app.request(`${MOUNT}/workspaces/my-ws/tokens`, as(held.key));

    expect(response.status).toBe(200);
    expect((await response.json()) as unknown[]).toHaveLength(1);
  });
});

describe('the sweep over bindings', () => {
  it('retires what a deletion left behind and keeps what is still in use', async () => {
    const held = await ownerWithWorkspace();
    const { store } = held;

    const orphaned = await seedBoundToken(store, held.owner, 'gone-workspace', ['workspaces:read']);
    const deadButHere = await seedBoundToken(store, held.owner, 'my-ws', ['workspaces:read'], {
      revoked_at: '2026-02-01T00:00:00.000Z',
    });
    const live = await seedBoundToken(store, held.owner, 'my-ws', ['workspaces:read']);
    await store.run(
      'INSERT INTO token_bindings (token_id, workspace_slug, created_at) VALUES (?, ?, ?)',
      ['vanished-token', 'my-ws', SEED_NOW.toISOString()],
    );

    await sweepBindings(store, SEED_NOW.toISOString());

    const bindings = (
      await store.all<{ token_id: string }>('SELECT token_id FROM token_bindings')
    ).map((row) => row.token_id);
    const revokedAt = async (id: string) =>
      (
        await store.all<{ revoked_at: string | null }>(
          'SELECT revoked_at FROM credentials WHERE id = ?',
          [id],
        )
      )[0]?.revoked_at;

    expect(bindings).not.toContain(orphaned.record.id);
    expect(bindings).not.toContain('vanished-token');
    expect(bindings).toContain(deadButHere.record.id);
    expect(bindings).toContain(live.record.id);
    expect(await revokedAt(orphaned.record.id)).not.toBeNull();
    expect(await revokedAt(live.record.id)).toBeNull();

    const listed = (await (
      await held.app.request(`${MOUNT}/workspaces/my-ws/tokens`, as(held.key))
    ).json()) as Array<{ token_id: string }>;
    expect(listed.map((entry) => entry.token_id)).toContain(deadButHere.record.id);
  });
});
