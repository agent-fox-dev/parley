import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import {
  type Service,
  MOUNT,
  as,
  identityService,
  recordedEvents,
  withJson,
} from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What the webhook secret may be. It travels to the hub in a header,
 * so its alphabet is narrow; and a refusal names the rule it broke
 * and never the value, which may be most of a credential.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const owner = await seedUser(service.store);
  for (const [slug, status] of [
    ['mine', 'ready'],
    ['retired', 'archived'],
  ] as const) {
    await seedWorkspace(service.store, { slug, owner_id: owner.id, status });
  }
  return { ...service, owner, key: (await seedApiKey(service.store, owner)).presented };
}

const set = (held: Awaited<ReturnType<typeof world>>, body: unknown, slug = 'mine') =>
  held.app.request(`${MOUNT}/workspaces/${slug}/forge/webhook`, withJson('PUT', body, held.key));

describe('a webhook secret the service will not take', () => {
  it.each([
    ['a body that is not an object', ['secret'], 'JSON object'],
    ['a secret that is not a string', { secret: 7 }, 'secret must be a string'],
    ['a secret of fifteen characters', { secret: 'x'.repeat(15) }, '16 to 255'],
    ['a secret of 256 characters', { secret: 'x'.repeat(256) }, '16 to 255'],
    ['a secret holding a space', { secret: `x'.repeat(10) with a space` }, 'printable ASCII'],
    ['a secret holding a tab', { secret: `${'x'.repeat(16)}\t` }, 'printable ASCII'],
    ['a secret holding a newline', { secret: `${'x'.repeat(16)}\n` }, 'printable ASCII'],
    ['a secret holding an accent', { secret: `${'x'.repeat(16)}é` }, 'printable ASCII'],
  ])('refuses %s, naming the rule and not the value', async (_what, body, rule) => {
    const held = await world();

    const response = await set(held, body);
    const answered = (await response.json()) as { error: { message: string } };

    expect(response.status).toBe(400);
    expect(answered.error.message).toContain(rule);
    const value = (body as { secret?: unknown }).secret;
    if (typeof value === 'string') {
      expect(answered.error.message).not.toContain(value);
    }
    expect(await held.secrets.get(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET')).toBeUndefined();
  });

  it('refuses an archived workspace, leaving its secret alone', async () => {
    const held = await world();
    await held.secrets.put(workspaceScope('retired'), 'FORGE_WEBHOOK_SECRET', 'x'.repeat(20));

    const response = await set(held, {}, 'retired');
    const answered = (await response.json()) as { error: { message: string } };

    expect(response.status).toBe(400);
    expect(answered.error.message).toContain('archived');
    expect(await held.secrets.get(workspaceScope('retired'), 'FORGE_WEBHOOK_SECRET')).toBe(
      'x'.repeat(20),
    );
  });

  it('refuses a workspace whose secrets are full', async () => {
    const held = await world();
    for (let index = 0; index < 100; index += 1) {
      await held.secrets.put(workspaceScope('mine'), `SECRET_${String(index)}`, 'v');
    }

    const response = await set(held, {});
    const answered = (await response.json()) as { error: { message: string } };

    expect(response.status).toBe(400);
    expect(answered.error.message).toContain('100');
    expect(await held.secrets.get(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET')).toBeUndefined();
  });

  it('records no event for a refusal', async () => {
    const held = await world();

    await set(held, { secret: 'too-short' });

    expect(await recordedEvents(held.store, 'hub.forge.webhook.set')).toEqual([]);
  });
});

describe('removing the webhook secret', () => {
  it.each([
    ['one that was set', 'mine', true],
    ['one that was never set', 'mine', false],
    ['one of an archived workspace', 'retired', true],
  ])('answers 204 for %s', async (_what, slug, stored) => {
    const held = await world();
    if (stored) {
      await held.secrets.put(workspaceScope(slug), 'FORGE_WEBHOOK_SECRET', 'x'.repeat(20));
    }

    const response = await held.app.request(
      `${MOUNT}/workspaces/${slug}/forge/webhook`,
      as(held.key, { method: 'DELETE' }),
    );

    expect(response.status).toBe(204);
    expect(await held.secrets.get(workspaceScope(slug), 'FORGE_WEBHOOK_SECRET')).toBeUndefined();
    const [recorded] = await recordedEvents(held.store, 'hub.forge.webhook.unset');
    expect(recorded?.metadata).toEqual({ existed: stored });
  });
});
