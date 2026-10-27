import { afterEach, describe, expect, it } from 'vitest';
import type { Context } from 'hono';
import { buildApp } from '../http/router.js';
import type { LogSink, RouteDefinition } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import { SERVER_ROUTE_PATHS } from '../routes/server.js';
import { freshStore, seedApiKey, seedClock, seedPat, seedUser } from '../testutil/identity.js';
import { identityOf } from './middleware.js';

const ADMIN_TOKEN = 'af_admin_deployment_secret';
const MOUNT = '/api/v1';

interface Recording {
  calls: number;
  lastIdentity: ReturnType<typeof identityOf>;
}

const disposers: Array<() => void> = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) {
    dispose();
  }
});

async function store(): Promise<RelationalStore> {
  const seeded = await freshStore();
  disposers.push(seeded.dispose);
  return seeded.store;
}

function recordingSink(): { sink: LogSink; records: string[] } {
  const records: string[] = [];
  const write = (message: string, detail?: unknown) => {
    records.push(`${message} ${detail === undefined ? '' : JSON.stringify(detail)}`);
  };
  return {
    sink: {
      error: write,
      info: write,
    },
    records,
  };
}

function appWith(
  relational: RelationalStore,
  options: { log?: LogSink; extraRoutes?: RouteDefinition[] } = {},
) {
  const recording: Recording = { calls: 0, lastIdentity: undefined };
  const routes: RouteDefinition[] = [
    {
      path: `${MOUNT}/protected`,
      handler: (c: Context) => {
        recording.calls += 1;
        recording.lastIdentity = identityOf(c);
        return new Response('{}', { status: 200 });
      },
    },
    {
      path: `${MOUNT}/needs-workspaces-read`,
      scope: 'workspaces:read',
      handler: () => new Response('{}', { status: 200 }),
    },
    {
      path: `${MOUNT}/needs-workspaces-write`,
      scope: 'workspaces:write',
      handler: () => new Response('{}', { status: 200 }),
    },
    {
      path: `${MOUNT}/needs-tokens-manage`,
      scope: 'tokens:manage',
      handler: () => new Response('{}', { status: 200 }),
    },
    {
      path: `${MOUNT}/boom`,
      handler: () => {
        throw new Error('the handler failed');
      },
    },
    ...(options.extraRoutes ?? []),
  ];

  const app = buildApp({
    routes,
    store: relational,
    clock: seedClock(),
    ...(options.log === undefined ? {} : { log: options.log }),
    auth: {
      store: relational,
      clock: seedClock(),
      adminToken: ADMIN_TOKEN,
      mountPoint: MOUNT,
      ...(options.log === undefined ? {} : { log: options.log }),
    },
  });

  return { app, recording };
}

const bearer = (credential: string) => ({ headers: { authorization: `Bearer ${credential}` } });

describe('a request without a credential the service accepts', () => {
  it.each([
    ['no header at all', undefined],
    ['an empty bearer', ''],
    ['a bearer that is not a credential', 'not-a-credential'],
    ['a credential of an unknown identifier', 'af_unknown_0123456789abcdef'],
  ])('is refused 401 and never reaches the route: %s', async (_name, credential) => {
    const { app, recording } = appWith(await store());

    const response = await app.request(
      `${MOUNT}/protected`,
      credential === undefined ? {} : bearer(credential),
    );

    expect(response.status).toBe(401);
    expect(((await response.json()) as { error: { message: string } }).error.message).toBe(
      'authentication required',
    );
    expect(recording.calls).toBe(0);
  });

  it.each([
    ['a revoked key', { revoked_at: '2026-02-01T00:00:00Z' }],
    ['an expired key', { expires_at: '2026-02-01T00:00:00Z' }],
  ])('is refused 401 for %s, saying no more than for any other', async (_name, over) => {
    const relational = await store();
    const user = await seedUser(relational);
    const dead = await seedApiKey(relational, user, over);
    const { app, recording } = appWith(relational);

    const response = await app.request(`${MOUNT}/protected`, bearer(dead.presented));
    const unknown = await app.request(`${MOUNT}/protected`, bearer('af_nobody_deadbeef'));

    expect(response.status).toBe(401);
    expect(await response.json()).toEqual(await unknown.json());
    expect(recording.calls).toBe(0);
  });
});

describe('an accepted credential', () => {
  it('is identified as an API key with its user', async () => {
    const relational = await store();
    const user = await seedUser(relational);
    const key = await seedApiKey(relational, user);
    const { app, recording } = appWith(relational);

    await app.request(`${MOUNT}/protected`, bearer(key.presented));

    expect(recording.lastIdentity?.kind).toBe('api_key');
    expect(recording.lastIdentity?.userId).toBe(user.id);
  });

  it('is identified as a personal access token with its user and its scopes', async () => {
    const relational = await store();
    const user = await seedUser(relational);
    const pat = await seedPat(relational, user, ['workspaces:read']);
    const { app, recording } = appWith(relational);

    await app.request(`${MOUNT}/protected`, bearer(pat.presented));

    expect(recording.lastIdentity?.kind).toBe('pat');
    expect(recording.lastIdentity?.userId).toBe(user.id);
    expect(recording.lastIdentity?.permissions).toEqual(['workspaces:read']);
  });

  it('is identified as an admin token with no user of its own', async () => {
    const { app, recording } = appWith(await store());

    await app.request(`${MOUNT}/protected`, bearer(ADMIN_TOKEN));

    expect(recording.lastIdentity?.kind).toBe('admin_token');
    expect(recording.lastIdentity?.userId).toBeUndefined();
  });
});

const sentinel = 'SENTINEL9f3a';

async function answerWithLog(path: string, credential: string) {
  const relational = await store();
  const user = await seedUser(relational);
  const key = await seedApiKey(relational, user);
  const { sink, records } = recordingSink();
  const { app } = appWith(relational, { log: sink });

  const response = await app.request(
    path,
    bearer(credential === 'live' ? key.presented : credential),
  );
  return { records, body: await response.text(), status: response.status };
}

describe('what reaches the log', () => {
  it.each([
    ['an answered request', `${MOUNT}/protected`, 'live'],
    ['a refused request', `${MOUNT}/protected`, `af_k1_${sentinel}`],
    ['a request whose handler failed', `${MOUNT}/boom`, `af_k1_${sentinel}`],
  ])('carries no credential secret: %s', async (_name, path, credential) => {
    const { records, body } = await answerWithLog(path, credential);

    expect(records.join('\n')).not.toContain(sentinel);
    expect(body).not.toContain(sentinel);
  });

  it('still records the method and the route of the request it answered', async () => {
    const { records } = await answerWithLog(`${MOUNT}/protected`, 'live');

    expect(records.join('\n')).toContain('GET');
    expect(records.join('\n')).toContain(`${MOUNT}/protected`);
  });
});

describe('the public routes', () => {
  it.each([
    ...SERVER_ROUTE_PATHS,
    `${MOUNT}/auth/providers`,
    `${MOUNT}/auth/callback`,
    '/hooks/forge/demo',
  ])('answer %s without a credential', async (path) => {
    const { app } = appWith(await store());

    expect((await app.request(path)).status).not.toBe(401);
  });

  it('still refuse a route that is not public', async () => {
    const { app } = appWith(await store());

    expect((await app.request(`${MOUNT}/protected`)).status).toBe(401);
  });
});

async function heldCredentials(relational: RelationalStore) {
  const user = await seedUser(relational);
  return {
    key: (await seedApiKey(relational, user)).presented,
    admin: ADMIN_TOKEN,
    writer: (await seedPat(relational, user, ['workspaces:write'])).presented,
    reader: (await seedPat(relational, user, ['workspaces:read'])).presented,
  };
}

describe('the scope a route needs', () => {
  it.each([
    ['key', 'needs-workspaces-read', 200],
    ['key', 'needs-workspaces-write', 200],
    ['admin', 'needs-workspaces-read', 200],
    ['admin', 'needs-workspaces-write', 200],
    ['writer', 'needs-workspaces-read', 200],
    ['writer', 'needs-workspaces-write', 200],
    ['reader', 'needs-workspaces-read', 200],
    ['reader', 'needs-workspaces-write', 403],
  ] as const)('answers %s on %s with %i', async (who, route, expected) => {
    const relational = await store();
    const held = await heldCredentials(relational);
    const { app } = appWith(relational);

    expect((await app.request(`${MOUNT}/${route}`, bearer(held[who]))).status).toBe(expected);
  });

  it('refuses a token outside its scopes with no error_type, before the handler runs', async () => {
    const relational = await store();
    const user = await seedUser(relational);
    const pat = await seedPat(relational, user, ['workspaces:read']);
    const { app, recording } = appWith(relational);

    const response = await app.request(`${MOUNT}/needs-tokens-manage`, bearer(pat.presented));
    const body = (await response.json()) as { error: Record<string, unknown> };

    expect(response.status).toBe(403);
    expect(Object.keys(body.error)).not.toContain('error_type');
    expect(recording.calls).toBe(0);
  });
});
