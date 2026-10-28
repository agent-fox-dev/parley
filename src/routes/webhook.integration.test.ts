import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import {
  type Service,
  EXTERNAL_URL,
  MOUNT,
  as,
  identityService,
  recordedEvents,
  withJson,
} from '../testutil/app.js';
import { seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * Setting up the webhook. The secret the hub generates is shown
 * once, in the answer that generated it, and the value is the
 * ordinary workspace secret, so the secrets routes and this one are
 * two doors to one room.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world(options: { externalUrl?: string; clock?: 'ticking' } = {}) {
  service = await identityService([], {
    ...(options.externalUrl === undefined ? {} : { externalUrl: options.externalUrl }),
    ...(options.clock === undefined ? {} : { clock: options.clock }),
  });
  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    org_id: org.id,
    status: 'ready',
  });
  return { ...service, owner, org, key: (await seedApiKey(service.store, owner)).presented };
}

const webhookAt = `${MOUNT}/workspaces/mine/forge/webhook`;

describe('the secret the hub generates', () => {
  it.each([
    ['no body at all', undefined],
    ['an empty object', {}],
    ['a null secret', { secret: null }],
  ])('is made for %s and shown once', async (_what, body) => {
    const held = await world();

    const response =
      body === undefined
        ? await held.app.request(webhookAt, as(held.key, { method: 'PUT' }))
        : await held.app.request(webhookAt, withJson('PUT', body, held.key));
    const answered = (await response.json()) as {
      configured: boolean;
      url: string;
      secret: string;
    };

    expect(response.status).toBe(200);
    expect(answered.configured).toBe(true);
    expect(answered.url).toBe(`${EXTERNAL_URL}/hooks/forge/mine`);
    expect(answered.secret).toMatch(/^[0-9a-f]{64}$/);
    expect(await held.secrets.get(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET')).toBe(
      answered.secret,
    );

    // Shown once: no later answer, event or record carries it.
    const described = await (
      await held.app.request(`${MOUNT}/workspaces/mine/forge`, as(held.key))
    ).text();
    const listed = await (
      await held.app.request(`${MOUNT}/workspaces/mine/secrets`, as(held.key))
    ).text();
    const events = JSON.stringify(await recordedEvents(held.store));
    for (const text of [described, listed, events]) {
      expect(text).not.toContain(answered.secret);
    }
  });

  it('records that it generated one, and that it did not', async () => {
    // A clock that moves, so the two events are in the order they
    // were recorded rather than in whatever order one instant holds.
    const held = await world({ clock: 'ticking' });

    await held.app.request(webhookAt, withJson('PUT', {}, held.key));
    await held.app.request(webhookAt, withJson('PUT', { secret: 'x'.repeat(20) }, held.key));

    expect(
      (await recordedEvents(held.store, 'hub.forge.webhook.set')).map((one) => one.metadata),
    ).toEqual([{ generated: true }, { generated: false }]);
  });

  it('answers a null URL where the deployment has no address of its own', async () => {
    const held = await world({ externalUrl: '' });

    const response = await held.app.request(webhookAt, withJson('PUT', {}, held.key));
    const answered = (await response.json()) as { url: string | null; secret: string };

    expect(answered.url).toBeNull();
    // Stored all the same.
    expect(await held.secrets.get(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET')).toBe(
      answered.secret,
    );
  });
});

describe('the secret a caller supplies', () => {
  it('is stored as given and answered without a secret field', async () => {
    const held = await world();

    const response = await held.app.request(
      webhookAt,
      withJson('PUT', { secret: 'a-perfectly-good-secret' }, held.key),
    );
    const answered = (await response.json()) as Record<string, unknown>;

    expect(response.status).toBe(200);
    expect(Object.keys(answered).toSorted()).toEqual(['configured', 'url']);
    expect(await held.secrets.get(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET')).toBe(
      'a-perfectly-good-secret',
    );
  });

  it('replaces one that was there', async () => {
    const held = await world();
    await held.app.request(webhookAt, withJson('PUT', { secret: 'x'.repeat(20) }, held.key));

    await held.app.request(webhookAt, withJson('PUT', { secret: 'y'.repeat(20) }, held.key));

    expect(await held.secrets.get(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET')).toBe(
      'y'.repeat(20),
    );
  });
});

describe('the webhook secret and the secrets routes', () => {
  it('are two doors to one value', async () => {
    const held = await world();

    // Set through the secrets route, read by the webhook route.
    await held.app.request(
      `${MOUNT}/workspaces/mine/secrets`,
      withJson(
        'POST',
        { entries: [{ key: 'FORGE_WEBHOOK_SECRET', value: 'from-the-secrets-route' }] },
        held.key,
      ),
    );
    const described = (await (
      await held.app.request(`${MOUNT}/workspaces/mine/forge`, as(held.key))
    ).json()) as { webhook: { configured: boolean } };
    expect(described.webhook.configured).toBe(true);

    // Set through the webhook route, listed by the secrets route.
    await held.app.request(webhookAt, withJson('PUT', { secret: 'x'.repeat(20) }, held.key));
    const listed = (await (
      await held.app.request(`${MOUNT}/workspaces/mine/secrets`, as(held.key))
    ).json()) as Array<{ key: string }>;

    expect(listed.map((one) => one.key)).toContain('FORGE_WEBHOOK_SECRET');
  });

  it('are a workspace’s alone: the same name elsewhere is a different secret', async () => {
    const held = await world();
    await held.app.request(
      `${MOUNT}/user/secrets`,
      withJson(
        'POST',
        { entries: [{ key: 'FORGE_WEBHOOK_SECRET', value: 'the-user-one' }] },
        held.key,
      ),
    );
    await held.app.request(
      `${MOUNT}/orgs/${held.org.slug}/secrets`,
      withJson(
        'POST',
        { entries: [{ key: 'FORGE_WEBHOOK_SECRET', value: 'the-org-one' }] },
        held.key,
      ),
    );

    const described = (await (
      await held.app.request(`${MOUNT}/workspaces/mine/forge`, as(held.key))
    ).json()) as { webhook: { configured: boolean } };

    // The workspace has none of its own, whatever the user or the
    // organization holds under that name.
    expect(described.webhook.configured).toBe(false);
  });
});
