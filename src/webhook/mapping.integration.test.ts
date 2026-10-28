import { afterEach, describe, expect, it } from 'vitest';
import { workspaceScope } from '../platform/secrets.js';
import { type Service, identityService, recordedEvents } from '../testutil/app.js';
import { seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * A delivery going all the way through: signed, claimed, answered
 * and relayed. What is measured here is the end of the path — the
 * outcome in the answer and in the log, and the events that reached
 * the recorder — rather than the mapping, which has its own suite.
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
    git_url: 'https://github.com/Acme/Widgets.git',
  });
  await seedWorkspace(service.store, {
    slug: 'carried',
    owner_id: owner.id,
    status: 'ready',
    git_url: 'https://github.com/acme/fork.git',
    workspace_mode: 'carry_patch',
    upstream_url: 'https://github.com/acme/upstream.git',
  });
  for (const slug of ['mine', 'carried']) {
    await service.secrets.put(workspaceScope(slug), 'FORGE_WEBHOOK_SECRET', SECRET);
  }
  return service;
}

async function signatureOf(body: string): Promise<string> {
  const key = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(SECRET),
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

/** Posts a delivery and waits for whatever it handed to the platform. */
async function deliver(
  held: Service,
  options: { slug?: string; id?: string; event?: string; payload: unknown },
): Promise<{ status: number; outcome: string }> {
  const body = JSON.stringify(options.payload);
  const kept: Array<Promise<unknown>> = [];

  const response = await held.app.request(
    `/hooks/forge/${options.slug ?? 'mine'}`,
    {
      method: 'POST',
      headers: {
        'x-hub-signature-256': await signatureOf(body),
        'x-github-delivery': options.id ?? `d-${Math.random().toString(36).slice(2)}`,
        'x-github-event': options.event ?? 'issues',
      },
      body,
    },
    {},
    {
      waitUntil: (work: Promise<unknown>) => {
        kept.push(work);
      },
      passThroughOnException: () => undefined,
    } as unknown as ExecutionContext,
  );

  const answered = (await response.json().catch(() => ({}))) as { outcome?: string };
  await Promise.all(kept);
  return { status: response.status, outcome: answered.outcome ?? '' };
}

const ISSUE_OPENED = {
  action: 'opened',
  repository: { full_name: 'acme/widgets' },
  issue: { number: 12, title: 'Widgets break', html_url: 'https://github.test/i/12' },
  sender: { login: 'alice' },
};

describe('what a delivery leaves behind', () => {
  it('relays its events and says so in the answer and the log', async () => {
    const held = await world();

    const { status, outcome } = await deliver(held, { id: 'd-relay', payload: ISSUE_OPENED });

    expect([status, outcome]).toEqual([202, 'relayed']);
    const events = await recordedEvents(held.store, 'hub.forge.issue.opened');
    expect(events).toHaveLength(1);
    expect(events[0]?.actor_type).toBe('forge');
    expect(events[0]?.metadata).toMatchObject({
      forge: 'github',
      target: 'origin',
      delivery_id: 'd-relay',
      actor: 'alice',
      number: 12,
    });

    const [row] = await held.store.all<{ outcome: string; relayed: string; target: string }>(
      'SELECT outcome, relayed, target FROM forge_deliveries WHERE delivery_id = ?',
      ['d-relay'],
    );
    expect(row?.outcome).toBe('relayed');
    expect(JSON.parse(row?.relayed ?? '[]')).toEqual(['hub.forge.issue.opened']);
    expect(row?.target).toBe('origin');
  });

  it.each([
    ['an event it does not relay', { event: 'star', payload: { ...ISSUE_OPENED } }, 'ignored'],
    [
      'a repository that is neither of the workspace’s',
      { payload: { ...ISSUE_OPENED, repository: { full_name: 'someone/else' } } },
      'repository_mismatch',
    ],
  ])('reports %s and emits nothing', async (_what, options, expected) => {
    const held = await world();

    const { outcome } = await deliver(held, { id: `d-${expected}`, ...options });

    expect(outcome).toBe(expected);
    expect(await recordedEvents(held.store)).toEqual([]);
    const [row] = await held.store.all<{ outcome: string }>(
      'SELECT outcome FROM forge_deliveries WHERE delivery_id = ?',
      [`d-${expected}`],
    );
    expect(row?.outcome).toBe(expected);
  });

  it('reports a duplicate without relaying it twice', async () => {
    const held = await world();

    const first = await deliver(held, { id: 'd-twice', payload: ISSUE_OPENED });
    const again = await deliver(held, { id: 'd-twice', payload: ISSUE_OPENED });

    expect([first.outcome, again.outcome]).toEqual(['relayed', 'duplicate']);
    expect(await recordedEvents(held.store, 'hub.forge.issue.opened')).toHaveLength(1);
  });
});

describe('which repository a delivery is about', () => {
  it.each([
    ['the origin, however it is spelled', 'mine', 'ACME/WIDGETS', 'origin'],
    ['the origin of a carry-patch workspace', 'carried', 'acme/fork', 'origin'],
    ['the upstream of a carry-patch workspace', 'carried', 'acme/upstream', 'upstream'],
  ])('is %s', async (_what, slug, repository, target) => {
    const held = await world();

    await deliver(held, {
      slug,
      id: `d-${target}-${slug}`,
      payload: { ...ISSUE_OPENED, repository: { full_name: repository } },
    });

    const [row] = await held.store.all<{ target: string }>(
      'SELECT target FROM forge_deliveries WHERE delivery_id = ?',
      [`d-${target}-${slug}`],
    );
    expect(row?.target).toBe(target);
  });

  it('is never the upstream of a workspace that carries no patches', async () => {
    const held = await world();

    const { outcome } = await deliver(held, {
      slug: 'mine',
      id: 'd-upstream-standard',
      payload: { ...ISSUE_OPENED, repository: { full_name: 'acme/upstream' } },
    });

    expect(outcome).toBe('repository_mismatch');
  });
});

describe('a delivery the hub cannot read', () => {
  it.each([
    ['a body that is not one JSON object', ['an', 'array']],
    ['a payload with no repository', { action: 'opened', issue: { number: 1 } }],
    [
      'a payload with no issue number',
      { action: 'opened', repository: { full_name: 'acme/widgets' }, issue: {} },
    ],
    [
      'a payload with no label name',
      {
        action: 'labeled',
        repository: { full_name: 'acme/widgets' },
        issue: { number: 1 },
        label: {},
      },
    ],
  ])('answers 400 to %s and records it as malformed', async (_what, payload) => {
    const held = await world();
    const id = `d-malformed-${Math.random().toString(36).slice(2)}`;

    const { status } = await deliver(held, { id, payload });

    expect(status).toBe(400);
    const [row] = await held.store.all<{ outcome: string }>(
      'SELECT outcome FROM forge_deliveries WHERE delivery_id = ?',
      [id],
    );
    expect(row?.outcome).toBe('malformed');
    expect(await recordedEvents(held.store)).toEqual([]);
  });

  it('takes one missing only the descriptive fields', async () => {
    const held = await world();

    const { outcome } = await deliver(held, {
      id: 'd-bare',
      payload: {
        action: 'opened',
        repository: { full_name: 'acme/widgets' },
        issue: { number: 12 },
      },
    });

    expect(outcome).toBe('relayed');
    const [event] = await recordedEvents(held.store, 'hub.forge.issue.opened');
    expect(event?.metadata).toMatchObject({ title: '', url: '', actor: '' });
  });
});

describe('what a delivery never carries into the hub', () => {
  it('keeps the payload out of the row, the events and the answer', async () => {
    const held = await world();
    const secretish = 'a-string-no-mapping-table-lists';

    const { status } = await deliver(held, {
      id: 'd-payload',
      payload: {
        ...ISSUE_OPENED,
        deep: { held: secretish },
        issue: { ...ISSUE_OPENED.issue, body: secretish },
      },
    });

    expect(status).toBe(202);
    const rows = JSON.stringify(await held.store.all('SELECT * FROM forge_deliveries'));
    const events = JSON.stringify(await recordedEvents(held.store));
    expect(rows).not.toContain(secretish);
    expect(events).not.toContain(secretish);
  });
});
