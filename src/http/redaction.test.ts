import { afterEach, describe, expect, it } from 'vitest';
import type { Context } from 'hono';
import { buildApp } from '../http/router.js';
import type { LogSink, RouteDefinition } from '../http/router.js';
import { type Service, MOUNT, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedUser } from '../testutil/identity.js';
import { protectValue } from './redaction.js';

const VALUE = 'SENTINEL-secret-9f3a';

let service: Service | undefined;

afterEach(() => {
  service?.dispose();
  service = undefined;
});

function recordingSink(): { sink: LogSink; records: string[] } {
  const records: string[] = [];
  const write = (message: string, detail?: unknown) => {
    records.push(
      `${message} ${detail === undefined ? '' : JSON.stringify(detail, Object.getOwnPropertyNames(detail ?? {}))}`,
    );
  };
  return { sink: { error: write, info: write }, records };
}

/** A route that handles a value as a secret route does and then fails. */
const failing: RouteDefinition = {
  path: `${MOUNT}/protected-then-fails`,
  methods: ['POST'],
  handler: async (c: Context) => {
    const body = (await c.req.json()) as { value: string };
    protectValue(c, body.value);
    throw new Error(`the store refused while writing ${body.value}`);
  },
};

async function world() {
  const { sink, records } = recordingSink();
  const fixture = await identityService([failing]);
  service = fixture;
  const user = await seedUser(fixture.store);
  const key = (await seedApiKey(fixture.store, user)).presented;
  return { ...fixture, sink, records, key };
}

describe('a value a request asked the service to protect', () => {
  it('is kept out of the log when the request then fails unhandled', async () => {
    const held = await world();
    const { sink, records } = recordingSink();
    const app = buildApp({
      routes: [failing],
      store: held.store,
      log: sink,
      auth: { store: held.store, clock: { now: () => new Date() }, mountPoint: MOUNT },
    });

    const response = await app.request(
      `${MOUNT}/protected-then-fails`,
      withJson('POST', { value: VALUE }, held.key),
    );

    expect(response.status).toBe(500);
    expect(await response.text()).not.toContain(VALUE);
    expect(records.join('\n')).not.toContain(VALUE);
    expect(records.join('\n')).toContain('the store refused while writing');
    expect(records.join('\n')).toContain(`${MOUNT}/protected-then-fails`);
  });
});

async function run(work: (app: ReturnType<typeof buildApp>, key: string) => Promise<unknown>) {
  const held = await world();
  const { sink, records } = recordingSink();
  const app = buildApp({
    routes: held.app.routes as never,
    store: held.store,
    log: sink,
    auth: { store: held.store, clock: { now: () => new Date() }, mountPoint: MOUNT },
  });
  await work(app, held.key);
  return records;
}

describe('what a secret request leaves in the log', () => {
  it.each([
    [
      'a create',
      async (app: ReturnType<typeof buildApp>, key: string) =>
        app.request(
          `${MOUNT}/user/secrets`,
          withJson('POST', { entries: [{ key: 'THE_KEY', value: VALUE }] }, key),
        ),
    ],
    [
      'a refused create',
      async (app: ReturnType<typeof buildApp>, key: string) =>
        app.request(
          `${MOUNT}/user/secrets`,
          withJson('POST', { entries: [{ key: '1BAD', value: VALUE }] }, key),
        ),
    ],
    [
      'a replace of a key that is not there',
      async (app: ReturnType<typeof buildApp>, key: string) =>
        app.request(`${MOUNT}/user/secrets/ABSENT`, withJson('PATCH', { value: VALUE }, key)),
    ],
  ])('carries no value: %s', async (_name, work) => {
    const records = await run(work);

    expect(records.join('\n')).not.toContain(VALUE);
  });

  it('still says which route it answered, so the scrubbing is of the value alone', async () => {
    const records = await run(async (app, key) =>
      app.request(
        `${MOUNT}/user/secrets`,
        withJson('POST', { entries: [{ key: 'THE_KEY', value: VALUE }] }, key),
      ),
    );

    expect(records.join('\n')).toContain('/user/secrets');
    expect(records.join('\n')).toContain('POST');
  });
});
