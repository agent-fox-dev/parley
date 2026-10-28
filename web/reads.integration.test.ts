import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../src/testutil/app.js';
import {
  SEED_NOW,
  seedApiKey,
  seedOrg,
  seedUser,
  seedWorkspace,
} from '../src/testutil/identity.js';
import { addMember } from '../src/store/identity.js';
import { SESSION_COOKIE } from './session.js';

/**
 * Where the pages get what they show. Every one of them reads the
 * service's own API as the person signed in, so what the interface
 * shows and what that person's credential is given are the same
 * thing by construction rather than by two rules kept in step.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const { store } = service;
  const owner = await seedUser(store, { username: 'alice', provider_id: '12345' });
  const org = await seedOrg(store, owner);
  const member = await seedUser(store, { username: 'bob', provider_id: '67890' });
  await addMember(store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });
  const stranger = await seedUser(store, { username: 'carol', provider_id: '99999' });

  await seedWorkspace(store, {
    slug: 'shared',
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  await seedWorkspace(store, { slug: 'alices-own', owner_id: owner.id, status: 'ready' });
  await seedWorkspace(store, { slug: 'carols-own', owner_id: stranger.id, status: 'ready' });
  await seedWorkspace(store, {
    slug: 'retired',
    owner_id: stranger.id,
    status: 'archived',
  });

  return { ...service, owner, member, stranger, org };
}

/** Signs in as the person the given code names and answers the cookie. */
async function sessionFor(held: Service, code: string): Promise<string> {
  const response = await held.app.request(`/signin/github/callback?code=${code}`);
  return /parley_session=([^;]+)/.exec(response.headers.get('set-cookie') ?? '')?.[1] ?? '';
}

const withSession = (value: string) => ({ headers: { cookie: `${SESSION_COOKIE}=${value}` } });

describe('what a page may read', () => {
  it('is what the API gives that person’s own credential, and no more', async () => {
    const held = await world();
    const session = await sessionFor(held, 'a-code');

    // The same question, asked by the API with her key and by the
    // interface with her session.
    const key = (await seedApiKey(held.store, held.owner)).presented;
    const throughTheApi = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(key))
    ).json()) as Array<{ slug: string }>;

    const page = await (await held.app.request('/dashboard', withSession(session))).text();

    for (const workspace of throughTheApi) {
      expect(page, `${workspace.slug} is shown`).toContain(workspace.slug);
    }
    expect(page).not.toContain('carols-own');
    expect(page).not.toContain('retired');
  });

  it('is read with the session as the credential, so the API decides', async () => {
    const held = await world();
    const session = await sessionFor(held, 'a-code');

    // The session value is accepted by the API itself, as that
    // person: that is how the pages read.
    const asHer = await held.app.request(`${MOUNT}/workspaces`, as(session));
    const read = (await asHer.json()) as Array<{ slug: string; access: string }>;

    expect(asHer.status).toBe(200);
    expect(read.map((one) => one.slug).toSorted()).toEqual(['alices-own', 'shared']);
  });
});

describe('a workspace a person cannot open', () => {
  it.each([
    ['one belonging to somebody else', 'carols-own'],
    ['an archived one they may not see', 'retired'],
    ['an address naming nothing', 'no-such-workspace'],
  ])('answers %s with the same not-available page', async (_what, slug) => {
    const held = await world();
    const session = await sessionFor(held, 'a-code');

    const response = await held.app.request(`/workspaces/${slug}`, withSession(session));
    const html = await response.text();

    expect(html).toContain('Not available');
    // Nothing of the workspace, not even that it exists.
    expect(html).not.toContain('archived');
    expect(html).not.toContain('carol');
  });

  it('answers all three identically', async () => {
    const held = await world();
    const session = await sessionFor(held, 'a-code');

    const pages = await Promise.all(
      ['carols-own', 'retired', 'no-such-workspace'].map(async (slug) =>
        (await held.app.request(`/workspaces/${slug}`, withSession(session))).text(),
      ),
    );

    // The slug appears nowhere, so the three pages are the same page.
    expect(new Set(pages).size).toBe(1);
  });
});

describe('a member of the workspace’s organization', () => {
  it('is shown what the API returns a member, and their access as member', async () => {
    const held = await world();
    const session = await sessionFor(held, 'b-code');

    const listed = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(session))
    ).json()) as Array<{ slug: string; access: string }>;
    const page = await (await held.app.request('/dashboard', withSession(session))).text();

    expect(listed.map((one) => one.slug)).toEqual(['shared']);
    expect(listed[0]?.access).toBe('member');
    expect(page).toContain('shared');
    expect(page).toContain('member');
    expect(page).not.toContain('alices-own');
  });
});
