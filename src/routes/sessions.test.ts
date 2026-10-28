import { afterEach, describe, expect, it } from 'vitest';
import { ADMIN_TOKEN, type Service, identityService } from '../testutil/app.js';
import { addMember } from '../store/identity.js';
import {
  SEED_NOW,
  seedApiKey,
  seedOrg,
  seedPat,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';

/**
 * Agent sessions. Two of the access rules here are deliberately
 * unlike the rest of the service, so most of what follows is about
 * who may do what rather than about what is recorded.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface World {
  service: Service;
  slug: string;
  archived: string;
  owner: string;
  member: string;
  stranger: string;
}

async function world(): Promise<World> {
  const service = await identityService();
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const member = await seedUser(service.store);
  await addMember(service.store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });
  const stranger = await seedUser(service.store);

  const workspace = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  const archived = await seedWorkspace(service.store, {
    owner_id: owner.id,
    org_id: org.id,
    status: 'archived',
  });

  return {
    service,
    slug: workspace.slug,
    archived: archived.slug,
    owner: (await seedApiKey(service.store, owner)).presented,
    member: (await seedApiKey(service.store, member)).presented,
    stranger: (await seedApiKey(service.store, stranger)).presented,
  };
}

async function open(place: World, body: unknown, credential?: string): Promise<Response> {
  return place.service.app.request('/api/v1/sessions', {
    method: 'POST',
    headers: {
      authorization: `Bearer ${credential ?? place.owner}`,
      'content-type': 'application/json',
    },
    body: JSON.stringify(body),
  });
}

describe('opening a session', () => {
  it('records the run and answers with the session and its credential', async () => {
    const place = await world();

    const response = await open(place, {
      workspace_slug: place.slug,
      run_id: 'run-1',
      node_id: 'node-1',
      archetype: 'coder',
      model: 'claude-3-5-sonnet',
      metadata: { why: 'a test' },
    });
    const session = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(201);
    expect(session['workspace_slug']).toBe(place.slug);
    expect(session['run_id']).toBe('run-1');
    expect(session['archetype']).toBe('coder');
    expect(session['metadata']).toEqual({ why: 'a test' });
    expect(session['status']).toBe('active');
    expect(session['credential_type']).toBe('api_key');
    expect(String(session['credential_id'])).not.toHaveLength(0);
    expect(String(session['started_at'])).not.toHaveLength(0);
  });

  it('opens one on an archived workspace, because the tokens were still spent', async () => {
    const place = await world();

    const response = await open(place, { workspace_slug: place.archived });

    expect(response.status).toBe(201);
  });

  it('answers with the existing session when the id is one it already has', async () => {
    const place = await world();
    const first = await open(place, { workspace_slug: place.slug, id: 'chosen-by-the-client' });
    expect(first.status).toBe(201);

    const again = await open(place, {
      workspace_slug: place.slug,
      id: 'chosen-by-the-client',
      run_id: 'a different run',
    });
    const session = (await again.json()) as Record<string, unknown>;

    expect(again.status).toBe(200);
    // The first open is what stands: a retry is not a second session.
    expect(session['run_id']).toBeNull();
  });

  it('hides a replayed id belonging to a workspace the caller may not read', async () => {
    const place = await world();
    await open(place, { workspace_slug: place.slug, id: 'the-owners-session' });

    const response = await open(
      place,
      { workspace_slug: place.slug, id: 'the-owners-session' },
      place.stranger,
    );

    expect(response.status).toBe(404);
  });

  it.each([
    ['no workspace at all', {}],
    ['an empty workspace slug', { workspace_slug: '' }],
    ['a body that is not an object', 'please'],
  ])('refuses %s', async (_what, body) => {
    const place = await world();

    expect((await open(place, body)).status).toBe(400);
  });

  it('needs write access, so a member is refused as though nothing were there', async () => {
    const place = await world();

    const asMember = await open(place, { workspace_slug: place.slug }, place.member);
    const asStranger = await open(place, { workspace_slug: place.slug }, place.stranger);

    expect(asMember.status).toBe(404);
    expect(asStranger.status).toBe(404);
  });

  it('lets an admin token open one', async () => {
    const place = await world();

    expect((await open(place, { workspace_slug: place.slug }, ADMIN_TOKEN)).status).toBe(201);
  });

  it('refuses a token without the scope', async () => {
    const place = await world();
    const owners = await place.service.store.all<{ owner_id: string }>(
      'SELECT owner_id FROM workspaces WHERE slug = ?',
      [place.slug],
    );
    const scopeless = await seedPat(
      place.service.store,
      { id: owners[0]?.owner_id ?? '' } as never,
      ['sessions:read'],
    );

    expect((await open(place, { workspace_slug: place.slug }, scopeless.presented)).status).toBe(
      403,
    );
  });
});
