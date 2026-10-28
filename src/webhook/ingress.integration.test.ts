import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import { type Service, identityService } from '../testutil/app.js';
import { seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * A delivery that really arrives, signed as GitHub signs one. The
 * point of these is the bytes: the signature is over what was sent,
 * so a body the hub re-serialised before checking would refuse
 * every genuine delivery whose spacing differed from its own.
 */
const SECRET = 'a-secret-the-forge-and-the-hub-share';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService();
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    status: 'ready',
    git_url: 'https://github.com/acme/widgets.git',
  });
  await service.secrets.put(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET', SECRET);
  return service;
}

async function signatureOf(body: string, secret = SECRET): Promise<string> {
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

/** A payload whose spacing no re-serialisation would reproduce. */
const SPACED = `{\n  "repository": {\n    "full_name": "acme/widgets"\n  },\n  "action": "opened"\n}`;

describe('a signed delivery', () => {
  it('is accepted when the signature is over the bytes as sent', async () => {
    const held = await world();

    const response = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers: {
        'x-hub-signature-256': await signatureOf(SPACED),
        'x-github-delivery': 'd-1',
        'x-github-event': 'issues',
        'content-type': 'application/json',
      },
      body: SPACED,
    });

    expect(response.status).toBe(202);
  });

  it('is refused when the signature is over the same JSON written out again', async () => {
    const held = await world();
    const rewritten = JSON.stringify(JSON.parse(SPACED) as unknown);
    expect(rewritten).not.toBe(SPACED);

    const response = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers: {
        'x-hub-signature-256': await signatureOf(rewritten),
        'x-github-delivery': 'd-2',
        'x-github-event': 'issues',
        'content-type': 'application/json',
      },
      body: SPACED,
    });

    expect(response.status).toBe(401);
  });

  it('is accepted from GitLab on its token alone', async () => {
    const held = await world();

    const response = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers: {
        'x-gitlab-token': SECRET,
        'x-gitlab-event': 'Issue Hook',
        'idempotency-key': 'd-3',
        'content-type': 'application/json',
      },
      body: SPACED,
    });

    expect(response.status).toBe(202);
  });

  it('is refused once the secret has been rotated out from under it', async () => {
    const held = await world();
    const before = await signatureOf(SPACED);
    await held.secrets.put(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET', 'a-new-secret-entirely');

    const stale = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers: {
        'x-hub-signature-256': before,
        'x-github-delivery': 'd-4',
        'x-github-event': 'issues',
      },
      body: SPACED,
    });
    const fresh = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers: {
        'x-hub-signature-256': await signatureOf(SPACED, 'a-new-secret-entirely'),
        'x-github-delivery': 'd-5',
        'x-github-event': 'issues',
      },
      body: SPACED,
    });

    // Read afresh for every delivery: no restart, no cache.
    expect(stale.status).toBe(401);
    expect(fresh.status).toBe(202);
  });

  it('is refused for a workspace whose secret has been removed', async () => {
    const held = await world();
    await held.secrets.delete(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET');

    const response = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers: {
        'x-hub-signature-256': await signatureOf(SPACED),
        'x-github-delivery': 'd-6',
        'x-github-event': 'issues',
      },
      body: SPACED,
    });

    expect(response.status).toBe(404);
  });
});
