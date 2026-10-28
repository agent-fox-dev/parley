import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { systemClock } from '../../platform/local/clock.js';
import { sqliteStore } from '../../platform/local/store.js';
import { migrate } from '../../store/migrations/index.js';
import { insertUser } from '../../store/identity.js';
import { openSession, SESSION_COOKIE } from '../../../web/session.js';
import { buildLocalApp } from './main.js';

/**
 * The target as it is really assembled, rather than as a suite would
 * assemble it. What this is after is the wiring: a page of the web
 * interface reads the service's own API, so the target has to hand
 * the pages the application it just built.
 */
const made: string[] = [];

function freshDir(): string {
  const dir = mkdtempSync(join(tmpdir(), 'parley-target-'));
  made.push(dir);
  return dir;
}

afterEach(() => {
  for (const dir of made.splice(0)) {
    rmSync(dir, { recursive: true, force: true });
  }
});

async function readyApp(): Promise<{ app: ReturnType<typeof buildLocalApp>; cookie: string }> {
  const dir = freshDir();
  const path = join(dir, 'parley.db');
  const store = sqliteStore(path);
  await migrate(store);

  const now = new Date().toISOString();
  await insertUser(store, {
    id: 'user-of-the-target',
    username: 'dana',
    email: 'dana@example.test',
    full_name: 'Dana',
    status: 'active',
    role: 'user',
    provider: 'github',
    provider_id: '4242',
    created_at: now,
    updated_at: now,
  });
  const session = await openSession(store, systemClock(), 'user-of-the-target');

  return {
    app: buildLocalApp({ PARLEY_DB_PATH: path, PARLEY_REPO_PATH: join(dir, 'repositories') }),
    cookie: `${SESSION_COOKIE}=${session.value}`,
  };
}

describe('the target that runs the service on one host', () => {
  it('serves the landing page of the web interface', async () => {
    const { app } = await readyApp();
    const answer = await app.request('/');
    expect(answer.status).toBe(200);
    expect(answer.headers.get('content-type')).toContain('text/html');
    expect(await answer.text()).toContain('Sign in');
  });

  it('serves a signed-in person their dashboard, read through its own API', async () => {
    const { app, cookie } = await readyApp();
    const answer = await app.request('/dashboard', { headers: { cookie } });
    expect(answer.status).toBe(200);
    const shown = await answer.text();
    expect(shown).toContain('Your workspaces');
    expect(shown).toContain('dana');
  });
});
