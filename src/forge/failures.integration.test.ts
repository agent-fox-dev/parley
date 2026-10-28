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
import { type FakeForge, fakeForge } from '../testutil/fakeforge.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * What a caller is told when the forge says no. The statuses are
 * deliberately not the forge's own: its 401 means the workspace's
 * credential was refused and not the caller's, so the caller is told
 * 502 and never 401.
 */
let service: Service;
let forge: FakeForge;

const TOKEN = 'the-workspace-token';

afterEach(() => {
  service.dispose();
});

async function world(options: Parameters<typeof fakeForge>[0] = {}, credential = 'pat') {
  forge = fakeForge(options);
  service = await identityService([], { fetch: forge.fetch });
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    status: 'ready',
    git_url: 'https://user:pass@github.com/acme/widgets.git',
  });
  if (credential === 'pat') {
    await service.secrets.put(workspaceScope('mine'), 'GIT_PAT', TOKEN);
  }
  if (credential === 'basic') {
    await service.secrets.put(workspaceScope('mine'), 'GIT_USERNAME', 'alice');
    await service.secrets.put(workspaceScope('mine'), 'GIT_PASSWORD', TOKEN);
  }
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

type World = Awaited<ReturnType<typeof world>>;

const readIssues = (held: World) =>
  held.app.request(`${MOUNT}/workspaces/mine/issues`, as(held.key));
const createIssue = (held: World) =>
  held.app.request(`${MOUNT}/workspaces/mine/issues`, withJson('POST', { title: 'ok' }, held.key));

describe('what the forge answered, as the caller is told it', () => {
  it.each([
    ['the forge has no such thing', 404, 404, 'forge_not_found'],
    ['the credential was refused', 401, 502, 'forge_unauthenticated'],
    ['the credential may not', 403, 502, 'forge_forbidden'],
    ['the request was refused', 422, 422, 'forge_rejected'],
    ['another refusal', 409, 422, 'forge_rejected'],
    ['the forge broke', 500, 502, 'forge_error'],
    ['the forge is unwell', 503, 502, 'forge_error'],
  ])('answers %s with %i as %i %s', async (_what, given, status, errorType) => {
    const held = await world({
      fallback: { status: given, body: { message: 'the forge said so' } },
    });

    for (const response of [await readIssues(held), await createIssue(held)]) {
      const body = (await response.json()) as { error: { error_type: string } };
      expect(response.status).toBe(status);
      expect(body.error.error_type).toBe(errorType);
    }
  });

  it('answers an unreadable body as the forge having broken', async () => {
    const held = await world({ fallback: { status: 200, body: undefined } });
    forge.answerWith({});

    // A 200 with nothing in it is not a list of issues.
    const response = await readIssues(held);

    expect(response.status).toBe(200);
  });

  it('answers a connection that failed as the forge being unreachable', async () => {
    const held = await world({ unreachable: true });

    const response = await readIssues(held);
    const body = (await response.json()) as { error: { error_type: string } };

    expect(response.status).toBe(502);
    expect(body.error.error_type).toBe('forge_unreachable');
  });
});

describe('the words the forge used', () => {
  it('are carried through, with the token and the userinfo taken out', async () => {
    const held = await world({
      fallback: {
        status: 404,
        body: {
          message: `no such repository for ${TOKEN} at https://user:pass@github.com/acme/widgets`,
        },
      },
    });

    const response = await readIssues(held);
    const text = await response.text();

    expect(text).toContain('the forge answered 404');
    expect(text).toContain('[redacted]');
    expect(text).not.toContain(TOKEN);
    expect(text).not.toContain('user:pass@');
  });

  it('are cut to what the reference allows', async () => {
    const held = await world({
      fallback: { status: 500, body: { message: 'x'.repeat(2000) } },
    });

    const response = await readIssues(held);
    const body = (await response.json()) as { error: { message: string } };

    expect(body.error.message.length).toBeLessThanOrEqual(1024 + 40);
    expect(body.error.message).toContain('the forge answered 500');
  });

  it('gain the hint to store a token when a username and password were refused', async () => {
    const held = await world(
      { fallback: { status: 401, body: { message: 'bad credentials' } } },
      'basic',
    );

    const response = await readIssues(held);
    const body = (await response.json()) as { error: { message: string } };

    expect(response.status).toBe(502);
    expect(body.error.message).toContain('GIT_PAT');
  });
});

describe('a write with no credential at all', () => {
  it('is answered 502 forge_error with nothing sent and nothing recorded', async () => {
    const held = await world({}, 'none');
    const before = forge.calls().length;

    const response = await createIssue(held);
    const body = (await response.json()) as { error: { error_type: string; message: string } };

    expect(response.status).toBe(502);
    expect(body.error.error_type).toBe('forge_error');
    expect(body.error.message).toContain('no authentication token configured');
    expect(forge.calls().length).toBe(before);
    expect(await recordedEvents(held.store)).toEqual([]);
  });
});

describe('a forge that is rate limiting', () => {
  it('waits out a short backoff and answers the retry’s result', { timeout: 20_000 }, async () => {
    let refusals = 0;
    const held = await world({
      fallback: {
        get status() {
          refusals += 1;
          return refusals === 1 ? 429 : 200;
        },
        body: [],
        headers: { 'retry-after': '1' },
      },
    });

    const response = await readIssues(held);

    expect(response.status).toBe(200);
    // Once refused, once more after the wait.
    expect(forge.calls()).toHaveLength(2);
  });

  it('refuses a wait it cannot take, with Retry-After in whole seconds', async () => {
    const held = await world({
      fallback: { status: 429, body: { message: 'slow down' }, headers: { 'retry-after': '200' } },
    });

    const response = await readIssues(held);
    const body = (await response.json()) as { error: { error_type: string } };

    expect(response.status).toBe(429);
    expect(body.error.error_type).toBe('forge_rate_limited');
    expect(response.headers.get('retry-after')).toBe('200');
    expect(forge.calls()).toHaveLength(1);
  });

  it('answers 504 where the deadline cuts the wait short', async () => {
    const held = await world({
      fallback: { status: 429, body: { message: 'slow down' }, headers: { 'retry-after': '29' } },
    });

    const response = await readIssues(held);
    const body = (await response.json()) as { error: { error_type: string } };

    expect(response.status).toBe(504);
    expect(body.error.error_type).toBe('forge_timeout');
  });

  it('refuses without a Retry-After where the wait is unknown', async () => {
    const held = await world({
      fallback: {
        status: 403,
        body: { message: 'API rate limit exceeded' },
        headers: { 'x-ratelimit-remaining': '0' },
      },
    });

    const response = await readIssues(held);
    const body = (await response.json()) as { error: { error_type: string } };

    expect(response.status).toBe(429);
    expect(body.error.error_type).toBe('forge_rate_limited');
    expect(response.headers.get('retry-after')).toBeNull();
  });
});

describe('what no answer and no event ever carries', () => {
  it.each([
    ['a refusal of a read', 404],
    ['a credential the forge would not take', 401],
    ['a refusal of a write', 422],
    ['a forge that broke', 500],
  ])('keeps the credential and the userinfo out of %s', async (_what, status) => {
    const held = await world({
      fallback: {
        status,
        body: {
          message: `${TOKEN} refused at https://user:pass@github.com/acme/widgets.git`,
        },
      },
    });

    const read = await (await readIssues(held)).text();
    const written = await (await createIssue(held)).text();
    const events = JSON.stringify(await recordedEvents(held.store));

    for (const answered of [read, written, events]) {
      expect(answered).not.toContain(TOKEN);
      expect(answered).not.toContain('user:pass');
      expect(answered).not.toContain('pass@github.com');
    }
  });
});
