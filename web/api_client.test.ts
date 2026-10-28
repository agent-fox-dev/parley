import { describe, expect, it } from 'vitest';
import { apiReader } from './api_client.js';

/**
 * How a page reads. It goes through the service's own API as the
 * person signed in, so the access rules live in one place and the
 * interface shows exactly what that person's credential is given.
 */
function recordingApp(answers: Record<string, { status?: number; body?: unknown }>) {
  const asked: Array<{ path: string; authorization: string | null }> = [];
  return {
    asked,
    app: {
      request: (path: string, init?: RequestInit) => {
        const headers = new Headers((init?.headers ?? {}) as Record<string, string>);
        asked.push({ path, authorization: headers.get('authorization') });
        const answer = answers[path] ?? { status: 404, body: { error: { message: 'no' } } };
        return Promise.resolve(
          new Response(JSON.stringify(answer.body ?? {}), {
            status: answer.status ?? 200,
            headers: { 'content-type': 'application/json' },
          }),
        );
      },
    },
  };
}

describe('a page reading the API', () => {
  it('asks the API, as the person, under the mount point', async () => {
    const recording = recordingApp({ '/api/v1/workspaces': { body: [{ slug: 'mine' }] } });
    const reader = apiReader({
      app: recording.app as never,
      mountPoint: '/api/v1',
      credential: 'the-session',
    });

    const read = await reader.read<Array<{ slug: string }>>('/workspaces');

    expect(read).toEqual({ ok: true, value: [{ slug: 'mine' }] });
    expect(recording.asked).toEqual([
      { path: '/api/v1/workspaces', authorization: 'Bearer the-session' },
    ]);
  });

  it('reports a refusal as a refusal rather than throwing', async () => {
    const recording = recordingApp({ '/api/v1/workspaces/theirs': { status: 404 } });
    const reader = apiReader({
      app: recording.app as never,
      mountPoint: '/api/v1',
      credential: 'the-session',
    });

    const read = await reader.read('/workspaces/theirs');

    expect(read).toEqual({ ok: false, status: 404, what: '/workspaces/theirs' });
  });

  it('reads nothing but the API: it is given no store at all', () => {
    // The reader's dependencies are the app, the mount point and
    // the credential. A page cannot reach the store through it.
    const reader = apiReader({
      app: recordingApp({}).app as never,
      mountPoint: '/api/v1',
      credential: 'the-session',
    });

    expect(Object.keys(reader)).toEqual(['read']);
  });
});
