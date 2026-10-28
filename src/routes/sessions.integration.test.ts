import { afterEach, describe, expect, it } from 'vitest';
import { ADMIN_TOKEN, type Service, identityService } from '../testutil/app.js';
import { addMember } from '../store/identity.js';
import { SEED_NOW, seedApiKey, seedOrg, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * Reading sessions, their usage and the pages they come in. The
 * access rule is the part that differs from the rest of the service:
 * a read needs an active workspace, and a member of an archived one
 * is told so rather than told nothing is there.
 */
const cleanups: Array<() => void> = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

interface World {
  service: Service;
  slug: string;
  other: string;
  archived: string;
  owner: string;
  member: string;
  stranger: string;
}

async function world(): Promise<World> {
  const service = await identityService([], { clock: 'ticking' });
  cleanups.push(service.dispose);

  const owner = await seedUser(service.store);
  const org = await seedOrg(service.store, owner);
  const member = await seedUser(service.store);
  await addMember(service.store, {
    org_id: org.id,
    user_id: member.id,
    role: 'member',
    created_at: SEED_NOW.toISOString(),
  });
  const stranger = await seedUser(service.store);

  const made = async (status: 'ready' | 'archived') =>
    (await seedWorkspace(service.store, { owner_id: owner.id, org_id: org.id, status })).slug;

  return {
    service,
    slug: await made('ready'),
    other: await made('ready'),
    archived: await made('archived'),
    owner: (await seedApiKey(service.store, owner)).presented,
    member: (await seedApiKey(service.store, member)).presented,
    stranger: (await seedApiKey(service.store, stranger)).presented,
  };
}

async function call(place: World, path: string, credential?: string): Promise<Response> {
  return place.service.app.request(`/api/v1${path}`, {
    headers: { authorization: `Bearer ${credential ?? place.owner}` },
  });
}

async function send(
  place: World,
  path: string,
  body: unknown,
  credential?: string,
): Promise<Response> {
  return place.service.app.request(`/api/v1${path}`, {
    method: 'POST',
    headers: {
      authorization: `Bearer ${credential ?? place.owner}`,
      'content-type': 'application/json',
    },
    body: JSON.stringify(body),
  });
}

/** A session with some usage against it. */
async function recorded(
  place: World,
  slug: string,
  over: Record<string, unknown> = {},
  usage: Array<Record<string, unknown>> = [],
): Promise<string> {
  const opened = await send(place, '/sessions', { workspace_slug: slug, ...over });
  const id = ((await opened.json()) as { id: string }).id;
  for (const one of usage) {
    await send(place, `/sessions/${id}/usage`, one);
  }
  return id;
}

describe('listing sessions', () => {
  it('answers newest first, each with what it spent', async () => {
    const place = await world();
    const first = await recorded(place, place.slug, { run_id: 'run-1' }, [
      { model: 'a-model', input_tokens: 100, output_tokens: 20 },
      { model: 'another', input_tokens: 50, cache_read_tokens: 5 },
    ]);
    const second = await recorded(place, place.slug, { run_id: 'run-2' });

    const listed = (await (await call(place, '/sessions')).json()) as {
      sessions: Array<Record<string, unknown>>;
      next_cursor: string | null;
      has_more: boolean;
    };

    expect(listed.sessions.map((one) => one['id'])).toEqual([second, first]);
    expect(listed.has_more).toBe(false);
    expect(listed.next_cursor).toBeNull();
    const summary = listed.sessions[1]?.['token_summary'] as Record<string, unknown>;
    expect(summary['total_input_tokens']).toBe(150);
    expect(summary['total_output_tokens']).toBe(20);
    expect(summary['total_cache_read_tokens']).toBe(5);
    expect(summary['models_used']).toEqual(['a-model', 'another']);
  });

  it.each([
    ['by workspace', (place: World) => `workspace_slug=${place.other}`, 1],
    ['by run', () => 'run_id=run-1', 1],
    ['by status', () => 'status=active', 3],
    ['by credential type', () => 'credential_type=api_key', 3],
  ])('narrows the listing %s', async (_what, query, expected) => {
    const place = await world();
    await recorded(place, place.slug, { run_id: 'run-1' });
    await recorded(place, place.slug, { run_id: 'run-2' });
    await recorded(place, place.other, { run_id: 'run-3' });

    const listed = (await (await call(place, `/sessions?${query(place)}`)).json()) as {
      sessions: unknown[];
    };

    expect(listed.sessions).toHaveLength(expected);
  });

  it('pages, and reading to the end gives each session once', async () => {
    const place = await world();
    const made: string[] = [];
    for (let index = 0; index < 5; index += 1) {
      made.push(await recorded(place, place.slug, { run_id: `run-${String(index)}` }));
    }

    const seen: string[] = [];
    let cursor: string | null = null;
    for (let round = 0; round < 6; round += 1) {
      const response: { sessions: Array<{ id: string }>; next_cursor: string | null } = (await (
        await call(
          place,
          `/sessions?order=asc&limit=2${cursor === null ? '' : `&cursor=${cursor}`}`,
        )
      ).json()) as { sessions: Array<{ id: string }>; next_cursor: string | null };
      seen.push(...response.sessions.map((one) => one.id));
      cursor = response.next_cursor;
      if (cursor === null) {
        break;
      }
    }

    expect(seen).toEqual(made);
    expect(new Set(seen).size).toBe(seen.length);
  });

  it('refuses a cursor it did not issue', async () => {
    const place = await world();

    expect((await call(place, '/sessions?cursor=not-ours')).status).toBe(400);
  });

  it('shows a caller only the workspaces they may read', async () => {
    const place = await world();
    await recorded(place, place.slug);

    const asStranger = (await (await call(place, '/sessions', place.stranger)).json()) as {
      sessions: unknown[];
    };
    const asMember = (await (await call(place, '/sessions', place.member)).json()) as {
      sessions: unknown[];
    };

    expect(asStranger.sessions).toEqual([]);
    // A member may read the workspace, so they see its sessions.
    expect(asMember.sessions).toHaveLength(1);
  });

  it('refuses a workspace the caller may not read, or an archived one', async () => {
    const place = await world();

    const hidden = await call(place, `/sessions?workspace_slug=${place.slug}`, place.stranger);
    const inactive = await call(place, `/sessions?workspace_slug=${place.archived}`);

    for (const response of [hidden, inactive]) {
      expect(response.status).toBe(403);
      expect(((await response.json()) as { error: { message: string } }).error.message).toContain(
        'workspace access denied',
      );
    }
  });
});

describe('reading one session and its usage', () => {
  it('answers the session with its summary, and its records with totals', async () => {
    const place = await world();
    const id = await recorded(place, place.slug, {}, [
      { model: 'a-model', input_tokens: 300, output_tokens: 120 },
      { model: 'a-model', input_tokens: 200, output_tokens: 80, cache_read_tokens: 30 },
    ]);

    const session = (await (await call(place, `/sessions/${id}`)).json()) as Record<
      string,
      unknown
    >;
    expect((session['token_summary'] as Record<string, unknown>)['total_input_tokens']).toBe(500);

    const usage = (await (await call(place, `/sessions/${id}/usage?limit=1`)).json()) as {
      records: unknown[];
      totals: Record<string, unknown>;
      has_more: boolean;
    };

    // One record on this page, and totals over both.
    expect(usage.records).toHaveLength(1);
    expect(usage.has_more).toBe(true);
    expect(usage.totals['total_input_tokens']).toBe(500);
    expect(usage.totals['total_output_tokens']).toBe(200);
    expect(usage.totals['models_used']).toEqual(['a-model']);
  });

  it('answers 404 for a session nobody has', async () => {
    const place = await world();

    expect((await call(place, '/sessions/nobody')).status).toBe(404);
  });

  it('refuses a session of an archived workspace, saying so', async () => {
    const place = await world();
    const id = await recorded(place, place.archived);

    const response = await call(place, `/sessions/${id}`);

    expect(response.status).toBe(403);
    expect(((await response.json()) as { error: { message: string } }).error.message).toContain(
      'workspace access denied',
    );
  });

  it('lets an admin token read any session', async () => {
    const place = await world();
    const id = await recorded(place, place.archived);

    expect((await call(place, `/sessions/${id}`, ADMIN_TOKEN)).status).toBe(200);
  });
});
