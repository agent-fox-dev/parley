import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { localRepositoryStore } from '../platform/local/repostore.js';
import { movableClock } from '../platform/local/clock.js';
import { repositoryOf } from '../repo/repository.js';
import { buildCommit } from '../testutil/history.js';
import {
  type RecordedEvent,
  type Service,
  MOUNT,
  as,
  identityService,
  recordedEvents,
  withJson,
} from '../testutil/app.js';
import {
  SEED_NOW,
  freshStore,
  seedApiKey,
  seedBoundToken,
  seedOrg,
  seedUser,
  seedWorkspace,
} from '../testutil/identity.js';
import { HUB_EVENTS, shapeOf } from './events_table.js';
import { eventRecorder } from './recorder.js';

/**
 * What the service writes down when it does something, against the two
 * event tables of the API reference. Each case performs one operation
 * and reads back the one event it owes: the type, the verb and the
 * metadata keys, which are what a consumer filters on.
 */
const cleanups: Array<() => void> = [];
let service: Service;

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface World {
  service: Service;
  key: string;
  ownerId: string;
  slug: string;
  carried: string;
}

async function world(): Promise<World> {
  const dir = mkdtempSync(join(tmpdir(), 'parley-audit-'));
  cleanups.push(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const repos = localRepositoryStore(dir);
  service = await identityService([], { repos, jobs: 'store' });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  // A create needs an organization to put the workspace in.
  await seedOrg(service.store, owner);
  const key = (await seedApiKey(service.store, owner)).presented;
  const standard = await seedWorkspace(service.store, {
    slug: 'mine',
    owner_id: owner.id,
    status: 'ready',
    head_sha: 'a'.repeat(40),
  });
  const carried = await seedWorkspace(service.store, {
    slug: 'carried',
    owner_id: owner.id,
    status: 'ready',
    workspace_mode: 'carry_patch',
    integration_branch: 'integration',
    upstream_url: 'https://example.test/upstream.git',
  });
  for (const workspace of [standard, carried]) {
    await repos.create(repositoryOf(workspace));
    await buildCommit(repos, repositoryOf(workspace), { 'a.txt': 'hello\n' }, [], 'first');
  }

  return { service, key, ownerId: owner.id, slug: standard.slug, carried: carried.slug };
}

async function eventsOf(held: World, type: string): Promise<RecordedEvent[]> {
  return recordedEvents(held.service.store, type);
}

/** Performs one operation and answers with the one event it recorded. */
async function recordedBy(
  held: World,
  type: string,
  operation: () => Promise<Response>,
): Promise<{ event: RecordedEvent; keys: string[] }> {
  const before = (await eventsOf(held, type)).length;
  const response = await operation();
  expect(response.status, `${type} was not performed: ${await response.text()}`).toBeLessThan(400);

  const after = await eventsOf(held, type);
  expect(after.length - before, `${type} recorded ${String(after.length - before)} events`).toBe(1);
  const event = after.at(-1) as RecordedEvent;
  return { event, keys: Object.keys(event.metadata) };
}

describe('the events the reference says each operation records', () => {
  it.each([
    [
      'hub.workspace.create',
      ['git_url', 'branch'],
      (held: World) =>
        held.service.app.request(
          `${MOUNT}/workspaces`,
          withJson(
            'POST',
            { slug: 'made', git_url: 'https://example.test/r.git', branch: 'main' },
            held.key,
          ),
        ),
    ],
    [
      'hub.workspace.update',
      ['fields'],
      (held: World) =>
        held.service.app.request(
          `${MOUNT}/workspaces/mine`,
          withJson('PATCH', { display_name: 'A name' }, held.key),
        ),
    ],
    [
      'hub.workspace.archive',
      ['head_sha'],
      (held: World) =>
        held.service.app.request(
          `${MOUNT}/workspaces/mine/archive`,
          as(held.key, { method: 'POST' }),
        ),
    ],
    [
      'hub.secret.create',
      ['scope', 'key'],
      (held: World) =>
        held.service.app.request(
          `${MOUNT}/workspaces/mine/secrets`,
          withJson('POST', { entries: [{ key: 'A_SECRET', value: 'v' }] }, held.key),
        ),
    ],
    [
      'hub.secret.update',
      ['scope', 'key'],
      async (held: World) => {
        await held.service.app.request(
          `${MOUNT}/workspaces/mine/secrets`,
          withJson('POST', { entries: [{ key: 'TO_CHANGE', value: 'v' }] }, held.key),
        );
        return held.service.app.request(
          `${MOUNT}/workspaces/mine/secrets/TO_CHANGE`,
          withJson('PATCH', { value: 'w' }, held.key),
        );
      },
    ],
    [
      'hub.secret.delete',
      ['scope', 'key'],
      async (held: World) => {
        await held.service.app.request(
          `${MOUNT}/workspaces/mine/secrets`,
          withJson('POST', { entries: [{ key: 'TO_GO', value: 'v' }] }, held.key),
        );
        return held.service.app.request(
          `${MOUNT}/workspaces/mine/secrets/TO_GO`,
          as(held.key, { method: 'DELETE' }),
        );
      },
    ],
    [
      'hub.variable.create',
      ['scope', 'key'],
      (held: World) =>
        held.service.app.request(
          `${MOUNT}/workspaces/mine/vars`,
          withJson('POST', { entries: [{ key: 'A_VAR', value: 'v' }] }, held.key),
        ),
    ],
    [
      'hub.variable.update',
      ['scope', 'key'],
      async (held: World) => {
        await held.service.app.request(
          `${MOUNT}/workspaces/mine/vars`,
          withJson('POST', { entries: [{ key: 'VAR_TO_CHANGE', value: 'v' }] }, held.key),
        );
        return held.service.app.request(
          `${MOUNT}/workspaces/mine/vars/VAR_TO_CHANGE`,
          withJson('PATCH', { value: 'w' }, held.key),
        );
      },
    ],
    [
      'hub.variable.delete',
      ['scope', 'key'],
      async (held: World) => {
        await held.service.app.request(
          `${MOUNT}/workspaces/mine/vars`,
          withJson('POST', { entries: [{ key: 'VAR_TO_GO', value: 'v' }] }, held.key),
        );
        return held.service.app.request(
          `${MOUNT}/workspaces/mine/vars/VAR_TO_GO`,
          as(held.key, { method: 'DELETE' }),
        );
      },
    ],
    [
      'hub.token.bind',
      ['token_id', 'workspace', 'permissions', 'expires_at'],
      (held: World) =>
        held.service.app.request(
          `${MOUNT}/workspaces/mine/tokens`,
          withJson('POST', { name: 'a token', permissions: ['workspaces:read'] }, held.key),
        ),
    ],
    [
      'hub.patch.create',
      ['branch_name', 'position', 'branch_resolution'],
      (held: World) =>
        held.service.app.request(
          `${MOUNT}/workspaces/carried/patches`,
          withJson('POST', { branch_name: 'a-patch', skip_branch_check: true }, held.key),
        ),
    ],
    [
      'hub.patch.delete',
      ['branch_name'],
      async (held: World) => {
        const added = (await (
          await held.service.app.request(
            `${MOUNT}/workspaces/carried/patches`,
            withJson('POST', { branch_name: 'to-drop', skip_branch_check: true }, held.key),
          )
        ).json()) as { id: string };
        return held.service.app.request(
          `${MOUNT}/workspaces/carried/patches/${added.id}`,
          as(held.key, { method: 'DELETE' }),
        );
      },
    ],
    [
      'hub.rebuild.enqueue',
      ['job_id', 'patch_count'],
      async (held: World) => {
        await held.service.app.request(
          `${MOUNT}/workspaces/carried/patches`,
          withJson('POST', { branch_name: 'to-rebuild', skip_branch_check: true }, held.key),
        );
        return held.service.app.request(
          `${MOUNT}/workspaces/carried/rebuild`,
          withJson('POST', {}, held.key),
        );
      },
    ],
  ])('records %s with exactly its own metadata', async (type, keys, operation) => {
    const held = await world();

    const { event, keys: recorded } = await recordedBy(held, type, async () => operation(held));

    const declared = shapeOf(type);
    expect(declared, `${type} is not in the declaration`).toBeDefined();
    expect(event.action).toBe(declared?.action);
    expect(event.action).toBe(declared?.action);
    expect(recorded.toSorted()).toEqual([...keys].toSorted());
    // Nothing a call site invented: every key is one the reference lists.
    for (const key of recorded) {
      expect(declared?.metadata).toContain(key);
    }
    expect(event.actor_type).toBe('api_key');
  });

  it('records the delete with the bound tokens it revoked, and the sessions it ended', async () => {
    const held = await world();
    const minted = (await (
      await held.service.app.request(
        `${MOUNT}/workspaces/mine/tokens`,
        withJson('POST', { name: 'a token', permissions: ['workspaces:read'] }, held.key),
      )
    ).json()) as { token_id: string };
    const session = (await (
      await held.service.app.request(
        `${MOUNT}/sessions`,
        withJson('POST', { workspace_slug: 'mine' }, held.key),
      )
    ).json()) as { id: string };

    await held.service.app.request(
      `${MOUNT}/workspaces/mine/archive`,
      as(held.key, { method: 'POST' }),
    );
    await held.service.app.request(`${MOUNT}/workspaces/mine`, as(held.key, { method: 'DELETE' }));

    const [forced] = await eventsOf(held, 'hub.session.force_closed');
    expect(forced?.metadata).toEqual({ session_id: session.id, reason: 'workspace_archived' });
    expect(forced?.actor_type).toBe('system');

    const [deleted] = await eventsOf(held, 'hub.workspace.delete');
    expect(deleted?.metadata).toEqual({ revoked_tokens: [minted.token_id] });
  });

  it('records the reactivate, which carries no metadata at all', async () => {
    const held = await world();
    await held.service.app.request(
      `${MOUNT}/workspaces/mine/archive`,
      as(held.key, { method: 'POST' }),
    );

    const { keys } = await recordedBy(held, 'hub.workspace.reactivate', async () =>
      held.service.app.request(
        `${MOUNT}/workspaces/mine/reactivate`,
        as(held.key, { method: 'POST' }),
      ),
    );

    expect(keys).toEqual([]);
  });
});

describe('a bound token that keeps being refused', () => {
  it('is recorded once a minute, with the refusals held back counted into it', async () => {
    const held = await world();
    const owner = await seedUser(held.service.store);
    const bound = await seedBoundToken(held.service.store, owner, 'mine', ['workspaces:read']);

    for (let refusal = 0; refusal < 20; refusal += 1) {
      const answer = await held.service.app.request(
        `${MOUNT}/workspaces/elsewhere`,
        as(bound.presented),
      );
      expect(answer.status).toBe(404);
    }

    const recorded = await eventsOf(held, 'hub.token.bound_refused');
    expect(recorded).toHaveLength(1);
    expect(recorded[0]?.metadata).toMatchObject({
      token_id: bound.record.id,
      bound_workspace: 'mine',
      requested_workspace: 'elsewhere',
      status: 404,
      surface: 'api',
      suppressed: 19,
    });
  });

  it('records again once the minute has passed', async () => {
    const seeded = await freshStore();
    cleanups.push(seeded.dispose);
    const time = movableClock(SEED_NOW);
    const recorder = eventRecorder({ store: seeded.store, clock: time.clock });
    const refusal = {
      tokenId: 'a-token',
      boundWorkspace: 'mine',
      method: 'GET',
      route: '/api/v1/workspaces/elsewhere',
      status: 404,
      surface: 'api' as const,
    };

    await recorder.refusal(refusal);
    await recorder.refusal(refusal);
    time.advance(61_000);
    await recorder.refusal(refusal);

    const recorded = await seeded.store.all<{ metadata: string }>(
      'SELECT metadata FROM hub_events ORDER BY timestamp',
    );
    expect(recorded).toHaveLength(2);
    expect((JSON.parse(recorded[0]?.metadata ?? '{}') as { suppressed: number }).suppressed).toBe(
      1,
    );
    expect((JSON.parse(recorded[1]?.metadata ?? '{}') as { suppressed: number }).suppressed).toBe(
      0,
    );
  });
});

/**
 * No documented event left unemitted.
 *
 * The declaration is checked against the reference's own two tables,
 * so a type the reference gains and nobody declares fails here; and
 * every declared type whose operation this service performs is
 * checked to have a call site, so a type nobody records fails too.
 * The forge and sandbox types are the operations this service does
 * not perform: they land with packages 13 and 14 and with a
 * deployment that has a sandbox provider.
 */
/** The events whose operation belongs to a package not yet built. */
const awaitingItsPackage = (type: string) =>
  type.startsWith('hub.forge.') || type.startsWith('hub.sandbox.');

describe('the event types the reference lists', () => {
  const reference = readFileSync('docs/prd/api.md', 'utf8');
  const tables = reference.slice(
    reference.indexOf('## Hub Audit Events'),
    reference.indexOf('## Audit Ingestion Endpoints'),
  );

  /** The types the reference names, with its shorthand rows expanded. */
  function documented(): string[] {
    const named = new Set<string>();
    for (const line of tables.split('\n').filter((one) => one.startsWith('|'))) {
      const cell = line.split('|')[1] ?? '';
      const types = [...cell.matchAll(/`(hub\.[a-z_.]+)`/g)].map((match) => match[1] as string);
      for (const type of types) {
        named.add(type);
      }
      // A row may name one event and then its siblings by suffix
      // alone, as the secret and variable rows do.
      const [first] = types;
      if (first !== undefined) {
        const family = first.slice(0, first.lastIndexOf('.'));
        for (const suffix of [...cell.matchAll(/`(\.[a-z_]+)`/g)].map(
          (match) => match[1] as string,
        )) {
          named.add(`${family}${suffix}`);
        }
      }
    }
    return [...named];
  }

  it('are all declared, with nothing invented beside them', () => {
    const declared = HUB_EVENTS.map((shape) => shape.type);

    expect(documented().filter((type) => !declared.includes(type))).toEqual([]);
    expect(declared.filter((type) => !documented().includes(type))).toEqual([]);
  });

  it('are each recorded by a production path, bar the ones awaiting their package', () => {
    const sources = [
      'src/routes',
      'src/audit',
      'src/git',
      'src/auth',
      'src/workspace',
      'src/merge',
      'src/rebuild',
      'src/sync',
      'src/patch',
      'src/session',
    ]
      .flatMap((dir) =>
        execFileSync('git', ['ls-files', dir], { encoding: 'utf8' })
          .split('\n')
          .filter((path) => path.endsWith('.ts') && !path.includes('.test.')),
      )
      .map((path) => readFileSync(path, 'utf8'))
      .join('\n');

    const unrecorded = HUB_EVENTS.map((shape) => shape.type)
      .filter((type) => !awaitingItsPackage(type))
      .filter((type) => !sources.includes(type));

    expect(unrecorded).toEqual([]);
  });
});
