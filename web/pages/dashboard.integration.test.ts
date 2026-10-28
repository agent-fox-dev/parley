import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../../src/testutil/app.js';
import { SEED_NOW, seedOrg, seedUser, seedWorkspace } from '../../src/testutil/identity.js';
import { addMember } from '../../src/store/identity.js';
import { SESSION_COOKIE } from '../session.js';

/**
 * The dashboard against a deployment that really holds workspaces:
 * what it lists, what it does not, and whether its links go
 * anywhere.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const { store } = service;
  const alice = await seedUser(store, { username: 'alice', provider_id: '12345' });
  const org = await seedOrg(store, alice);
  const other = await seedUser(store, { username: 'bob', provider_id: '67890' });
  await addMember(store, {
    org_id: org.id,
    user_id: other.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });

  await seedWorkspace(store, {
    slug: 'widgets',
    owner_id: alice.id,
    status: 'ready',
    last_sync_at: '2026-10-21T09:12:03.000Z',
  });
  await seedWorkspace(store, { slug: 'sprockets', owner_id: alice.id, status: 'cloning' });
  await seedWorkspace(store, {
    slug: 'shared-with-bob',
    owner_id: alice.id,
    org_id: org.id,
    status: 'ready',
  });
  await seedWorkspace(store, { slug: 'nothing-to-do-with-her', owner_id: other.id });

  return service;
}

async function sessionFor(held: Service, code: string): Promise<string> {
  const response = await held.app.request(`/signin/github/callback?code=${code}`);
  return /parley_session=([^;]+)/.exec(response.headers.get('set-cookie') ?? '')?.[1] ?? '';
}

const withSession = (value: string) => ({ headers: { cookie: `${SESSION_COOKIE}=${value}` } });

describe('the dashboard of a person with workspaces', () => {
  it('lists each once, with its status, their access and its last sync', async () => {
    const held = await world();
    const session = await sessionFor(held, 'a-code');

    const html = await (await held.app.request('/dashboard', withSession(session))).text();

    for (const slug of ['widgets', 'sprockets', 'shared-with-bob']) {
      expect([...html.matchAll(new RegExp(`/workspaces/${slug}"`, 'g'))], slug).toHaveLength(1);
    }
    expect(html).toContain('ready');
    expect(html).toContain('cloning');
    expect(html).toContain('owner');
    expect(html).toContain('2026-10-21 09:12:03Z');
    // The one never synced says so rather than showing a gap.
    expect(html).toContain('synced never');
  });

  it('links each row to a page that answers', async () => {
    const held = await world();
    const session = await sessionFor(held, 'a-code');
    const html = await (await held.app.request('/dashboard', withSession(session))).text();

    const links = [...html.matchAll(/href="(\/workspaces\/[^"]+)"/g)].map((match) => match[1]);
    expect(links.length).toBeGreaterThan(0);

    for (const link of links) {
      const followed = await held.app.request(link as string, withSession(session));
      expect(followed.status, `${String(link)} answered`).toBe(200);
      expect(await followed.text()).not.toContain('Not available');
    }
  });

  it('lists exactly the workspaces the API lists for the same person', async () => {
    const held = await world();
    const session = await sessionFor(held, 'a-code');

    const fromApi = (await (
      await held.app.request(`${MOUNT}/workspaces`, as(session))
    ).json()) as Array<{ slug: string }>;
    const html = await (await held.app.request('/dashboard', withSession(session))).text();

    const shown = [...html.matchAll(/href="\/workspaces\/([^"]+)"/g)].map((match) => match[1]);
    expect(shown.toSorted()).toEqual(fromApi.map((one) => one.slug).toSorted());
    expect(html).not.toContain('nothing-to-do-with-her');
  });

  it('shows a member the workspace they share and their access to it', async () => {
    const held = await world();
    const session = await sessionFor(held, 'b-code');

    const html = await (await held.app.request('/dashboard', withSession(session))).text();

    expect(html).toContain('shared-with-bob');
    expect(html).toContain('member');
    expect(html).not.toContain('widgets');
  });
});
