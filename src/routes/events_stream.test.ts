import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService } from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What a stream is told before it becomes a stream. Both refusals
 * are answered as an ordinary response, because a client that is
 * handed a half-open stream and an error frame has to parse the
 * stream to find out it was refused.
 */
let service: Service;

afterEach(() => {
  service.dispose();
});

async function world(options: { streamLimit?: number } = {}) {
  service = await identityService([], options);
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id, status: 'ready' });
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

describe('a stream that is refused', () => {
  it('answers 404 for a workspace the caller may not read, before any frame', async () => {
    const held = await world();
    const other = await seedUser(held.store);
    await seedWorkspace(held.store, { slug: 'theirs', owner_id: other.id, status: 'ready' });

    const response = await held.app.request(`${MOUNT}/events?workspace=theirs`, as(held.key));

    expect(response.status).toBe(404);
    expect(response.headers.get('content-type')).not.toContain('text/event-stream');
  });

  it('answers 404 for a workspace that is not there', async () => {
    const held = await world();

    expect((await held.app.request(`${MOUNT}/events?workspace=nowhere`, as(held.key))).status).toBe(
      404,
    );
  });

  it('answers 503 once the deployment is at its limit of streams', async () => {
    const held = await world({ streamLimit: 1 });

    const first = await held.app.request(`${MOUNT}/events`, as(held.key));
    const second = await held.app.request(`${MOUNT}/events`, as(held.key));

    expect(first.status).toBe(200);
    expect(second.status).toBe(503);

    // The refused one left nothing half open: once the first is let
    // go, another may open.
    await first.body?.cancel();
    await new Promise((settled) => setTimeout(settled, 50));
    const third = await held.app.request(`${MOUNT}/events`, as(held.key));
    expect(third.status).toBe(200);
    await third.body?.cancel();
  });
});

describe('a stream that opens', () => {
  it('is a server-sent event stream with a heartbeat already in it', async () => {
    const held = await world();

    const response = await held.app.request(`${MOUNT}/events`, as(held.key));
    const reader = response.body?.getReader();
    const first = await reader?.read();

    expect(response.status).toBe(200);
    expect(response.headers.get('content-type')).toContain('text/event-stream');
    expect(new TextDecoder().decode(first?.value)).toContain('event: heartbeat');
    await reader?.cancel();
  });
});
