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
const SPACED = `{\n  "repository": {\n    "full_name": "acme/widgets"\n  },\n  "action": "opened",\n  "issue": {\n    "number": 12\n  }\n}`;

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
      body: JSON.stringify({
        project: { path_with_namespace: 'acme/widgets' },
        object_attributes: { iid: 12, action: 'open' },
      }),
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

/** The headers GitHub would send for this payload and this id. */
const headersFor = async (id: string) => ({
  'x-hub-signature-256': await signatureOf(SPACED),
  'x-github-delivery': id,
  'x-github-event': 'issues',
});

describe('a delivery that arrives more than once', () => {
  it('is recorded once and answered as a duplicate thereafter', async () => {
    const held = await world();

    const first = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers: await headersFor('d-once'),
      body: SPACED,
    });
    const again = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers: await headersFor('d-once'),
      body: SPACED,
    });

    expect(first.status).toBe(202);
    expect(again.status).toBe(202);
    expect((await again.json()) as { outcome: string }).toEqual({ outcome: 'duplicate' });
    expect(
      await held.store.all('SELECT delivery_id FROM forge_deliveries WHERE delivery_id = ?', [
        'd-once',
      ]),
    ).toHaveLength(1);
  });

  it('is processed once however many copies arrive together', async () => {
    const held = await world();
    const headers = await headersFor('d-together');

    const answers = await Promise.all(
      Array.from({ length: 5 }, async () =>
        held.app.request('/hooks/forge/mine', { method: 'POST', headers, body: SPACED }),
      ),
    );
    const outcomes = await Promise.all(
      answers.map(async (one) => ((await one.json()) as { outcome: string }).outcome),
    );

    expect(outcomes.filter((outcome) => outcome === 'duplicate')).toHaveLength(4);
    expect(
      await held.store.all('SELECT delivery_id FROM forge_deliveries WHERE delivery_id = ?', [
        'd-together',
      ]),
    ).toHaveLength(1);
  });

  it('is its own delivery for another workspace', async () => {
    const held = await world();
    await seedWorkspace(held.store, {
      slug: 'theirs',
      owner_id: 'u-1',
      status: 'ready',
      git_url: 'https://github.com/acme/other.git',
    });
    await held.secrets.put(workspaceScope('theirs'), 'FORGE_WEBHOOK_SECRET', SECRET);
    const headers = await headersFor('d-shared');

    const mine = await held.app.request('/hooks/forge/mine', {
      method: 'POST',
      headers,
      body: SPACED,
    });
    const theirs = await held.app.request('/hooks/forge/theirs', {
      method: 'POST',
      headers,
      body: SPACED,
    });

    for (const response of [mine, theirs]) {
      expect(((await response.json()) as { outcome: string }).outcome).not.toBe('duplicate');
    }
  });
});

/** Stands in until the promise below hands over its own release. */
const unblocked = () => undefined;

describe('the order of the answer and the relay', () => {
  it('answers while the recorder is still blocked, and relays once it is not', async () => {
    // A recorder that does not finish until the suite lets it: if
    // the answer waited for the relay, nothing below would return.
    let release: () => void = unblocked;
    const blocked = new Promise<void>((settled) => {
      release = settled;
    });
    const seen: string[] = [];
    const recorder = {
      record: async (event: { type: string }) => {
        await blocked;
        seen.push(event.type);
      },
      refusal: async () => undefined,
    };

    service = await identityService([], { events: recorder });
    const owner = await seedUser(service.store);
    await seedWorkspace(service.store, {
      slug: 'mine',
      owner_id: owner.id,
      status: 'ready',
      git_url: 'https://github.com/acme/widgets.git',
    });
    await service.secrets.put(workspaceScope('mine'), 'FORGE_WEBHOOK_SECRET', SECRET);

    const kept: Array<Promise<unknown>> = [];
    const response = await service.app.request(
      '/hooks/forge/mine',
      {
        method: 'POST',
        headers: await headersFor('d-ordering'),
        body: SPACED,
      },
      {},
      {
        waitUntil: (work: Promise<unknown>) => {
          kept.push(work);
        },
        passThroughOnException: () => undefined,
      } as unknown as ExecutionContext,
    );

    expect(response.status).toBe(202);
    expect(seen, 'nothing was recorded before the forge was answered').toEqual([]);
    expect(kept, 'the relay was handed to the platform').toHaveLength(1);

    // The forge has its answer and may hang up; the relay finishes
    // regardless, which is what the platform was handed it for.
    release();
    await Promise.all(kept);

    expect(seen).toEqual(['hub.forge.issue.opened']);
  });
});
