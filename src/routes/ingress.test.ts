import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import { type Service, identityService, recordedEvents } from '../testutil/app.js';
import { seedUser, seedWorkspace } from '../testutil/identity.js';
import { MOST_BODY_BYTES } from '../webhook/verify.js';

/**
 * What the ingress refuses, and in what order. It is open to anyone
 * on the internet, so the two things that matter are that a refusal
 * says nothing a forger could use, and that nothing expensive
 * happens before the sender is proved.
 */
const SECRET = 'a-secret-the-forge-and-the-hub-share';

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
    ['secretless', 'ready'],
    ['empty-secret', 'ready'],
  ] as const) {
    await seedWorkspace(service.store, { slug, owner_id: owner.id, status });
  }
  await service.secrets.put(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET', SECRET);
  await service.secrets.put(workspaceScope('retired'), 'FORGE_WEBHOOK_SECRET', SECRET);
  await service.secrets.put(workspaceScope('empty-secret'), 'FORGE_WEBHOOK_SECRET', '');
  return service;
}

/** The signature GitHub would send for these bytes. */
async function signed(body: string, secret = SECRET): Promise<string> {
  const key = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  );
  const mac = new Uint8Array(
    await crypto.subtle.sign(
      'HMAC',
      key,
      new TextEncoder().encode(body) as unknown as BufferSource,
    ),
  );
  return `sha256=${[...mac].map((byte) => byte.toString(16).padStart(2, '0')).join('')}`;
}

const PAYLOAD = JSON.stringify({
  action: 'opened',
  repository: { full_name: 'acme/widgets' },
  issue: { number: 12 },
});

async function post(
  held: Service,
  slug: string,
  headers: Record<string, string>,
  body = PAYLOAD,
): Promise<Response> {
  return held.app.request(`/hooks/forge/${slug}`, { method: 'POST', headers, body });
}

describe('a workspace the ingress will not serve', () => {
  it.each([
    ['a slug naming nothing', 'nowhere'],
    ['a workspace with no secret', 'secretless'],
    ['a workspace whose secret is empty', 'empty-secret'],
  ])('answers %s with one identical 404', async (_what, slug) => {
    const held = await world();

    const response = await post(held, slug, {
      'x-hub-signature-256': await signed(PAYLOAD),
      'x-github-delivery': 'd-1',
      'x-github-event': 'issues',
    });

    expect(response.status).toBe(404);
    expect(await response.text()).toContain('not found');
  });

  it('serves an archived workspace that has a secret', async () => {
    const held = await world();

    const response = await post(held, 'retired', {
      'x-hub-signature-256': await signed(PAYLOAD),
      'x-github-delivery': 'd-1',
      'x-github-event': 'issues',
    });

    expect(response.status).toBe(202);
  });
});

describe('a delivery the ingress will not believe', () => {
  it.each([
    ['neither scheme header', {}],
    ['a missing signature with a GitHub delivery id', { 'x-github-delivery': 'd-1' }],
    ['a wrong GitHub signature', { 'x-hub-signature-256': `sha256=${'a'.repeat(64)}` }],
    [
      'a GitHub signature with the SHA-1 prefix',
      { 'x-hub-signature-256': `sha1=${'a'.repeat(40)}` },
    ],
    ['a wrong GitLab token', { 'x-gitlab-token': 'not-the-secret' }],
    ['an empty GitLab token', { 'x-gitlab-token': '' }],
  ])('refuses %s, recording and emitting nothing', async (_what, headers) => {
    const held = await world();

    const response = await post(held, 'mine', {
      ...headers,
      'x-github-event': 'issues',
      'x-gitlab-event': 'Issue Hook',
    });

    expect(response.status).toBe(401);
    expect(await response.text()).toContain('delivery signature missing or invalid');
    expect(await held.store.all('SELECT delivery_id FROM forge_deliveries')).toEqual([]);
    expect(await recordedEvents(held.store)).toEqual([]);
  });
});

describe('what the ingress does before it reads the body', () => {
  it.each([
    ['an unknown slug with no signature', 'nowhere', {}, 404],
    ['a workspace with no secret', 'secretless', { 'x-gitlab-token': SECRET }, 404],
    ['a wrong GitLab token', 'mine', { 'x-gitlab-token': 'wrong' }, 401],
  ])('answers %s without reading an oversized body', async (_what, slug, headers, status) => {
    const held = await world();
    const enormous = 'x'.repeat(MOST_BODY_BYTES + 1000);

    const response = await post(
      held,
      slug,
      { ...headers, 'x-gitlab-event': 'Issue Hook', 'idempotency-key': 'd-1' },
      enormous,
    );

    // The body would be 413 if it were read; the earlier check is
    // what answers.
    expect(response.status).toBe(status);
  });
});

/**
 * A delivery of exactly this many bytes: a payload the hub can read,
 * padded in a field no mapping table looks at.
 */
const filler = (bytes: number) => {
  const envelope = JSON.stringify({
    action: 'opened',
    repository: { full_name: 'acme/widgets' },
    issue: { number: 12 },
    pad: '',
  });
  return JSON.stringify({
    action: 'opened',
    repository: { full_name: 'acme/widgets' },
    issue: { number: 12 },
    pad: 'x'.repeat(bytes - envelope.length),
  });
};

describe('the size of a delivery', () => {
  it('takes one exactly at the limit and refuses one over it', async () => {
    const held = await world();

    const exact = filler(MOST_BODY_BYTES);
    const atLimit = await post(
      held,
      'mine',
      {
        'x-hub-signature-256': await signed(exact),
        'x-github-delivery': 'd-at-limit',
        'x-github-event': 'issues',
      },
      exact,
    );

    const over = filler(MOST_BODY_BYTES + 1);
    const past = await post(
      held,
      'mine',
      {
        'x-hub-signature-256': await signed(over),
        'x-github-delivery': 'd-over',
        'x-github-event': 'issues',
      },
      over,
    );

    expect(new TextEncoder().encode(exact).length).toBe(MOST_BODY_BYTES);
    expect(new TextEncoder().encode(over).length).toBe(MOST_BODY_BYTES + 1);
    expect(atLimit.status).toBe(202);
    expect(past.status).toBe(413);
    expect(await past.text()).toContain('body_too_large');
  });
});

describe('a delivery with no identity', () => {
  it.each([
    ['no delivery id', { 'x-github-event': 'issues' }],
    ['no event name', { 'x-github-delivery': 'd-1' }],
    [
      'an id of 256 characters',
      { 'x-github-delivery': 'd'.repeat(256), 'x-github-event': 'issues' },
    ],
    [
      'an id outside printable ASCII',
      { 'x-github-delivery': 'd-\u0007', 'x-github-event': 'issues' },
    ],
  ])('refuses %s, recording nothing', async (_what, headers) => {
    const held = await world();

    const response = await post(held, 'mine', {
      ...headers,
      'x-hub-signature-256': await signed(PAYLOAD),
    });

    expect(response.status).toBe(400);
    expect(await held.store.all('SELECT delivery_id FROM forge_deliveries')).toEqual([]);
  });
});
