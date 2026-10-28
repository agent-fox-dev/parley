import type { Check } from './harness.js';

/**
 * The conformance checks of the sync and the branch push. They ask a
 * running deployment to do both directions and read what it recorded,
 * because a sync that answers 200 and leaves the workspace saying
 * nothing true about its remote has not worked.
 */
const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';
const workspace = () => process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';

function syncable(): string | undefined {
  if ((process.env['PARLEY_BASE_URL'] ?? '').length === 0) {
    return 'a deployment';
  }
  if (apiKey().length === 0) {
    return 'PARLEY_SMOKE_API_KEY';
  }
  return workspace().length === 0 ? 'PARLEY_SMOKE_WORKSPACE' : undefined;
}

async function send(
  baseUrl: string,
  method: string,
  path: string,
): Promise<{ status: number; body: Record<string, unknown> }> {
  const response = await fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    method,
    headers: { authorization: `Bearer ${apiKey()}` },
  });
  const body = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  return { status: response.status, body };
}

export function syncPushChecks(): Check[] {
  return [
    {
      name: 'a sync leaves the workspace saying where its remote stands',
      needs: syncable,
      run: async ({ baseUrl }) => {
        const synced = await send(baseUrl, 'POST', `/api/v1/workspaces/${workspace()}/sync`);
        if (synced.status !== 200) {
          return {
            outcome: 'fail',
            message: `the sync answered ${String(synced.status)}`,
          };
        }
        if (synced.body['sync_error'] !== null) {
          return { outcome: 'fail', message: 'the sync succeeded with an error recorded' };
        }
        if (!/^[0-9a-f]{40}$/.test(String(synced.body['head_sha']))) {
          return { outcome: 'fail', message: 'the workspace reports no head after a sync' };
        }
        return {
          outcome: 'pass',
          message: `head ${String(synced.body['head_sha']).slice(0, 8)}`,
        };
      },
    },
    {
      name: 'a branch push refuses a branch the workspace does not have',
      needs: syncable,
      run: async ({ baseUrl }) => {
        const refused = await send(
          baseUrl,
          'POST',
          `/api/v1/workspaces/${workspace()}/branches/conformance-no-such-branch/push`,
        );
        const error = (refused.body['error'] ?? {}) as { error_type?: string; message?: string };
        if (refused.status !== 404 || error.error_type !== 'branch_not_found') {
          return {
            outcome: 'fail',
            message: `expected 404 branch_not_found, got ${String(refused.status)} ${String(error.error_type)}`,
          };
        }
        // The CLI prints the message and nothing else, so the type has
        // to be in it.
        return (error.message ?? '').startsWith('branch_not_found: ')
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the message does not begin with its error type' };
      },
    },
    {
      name: 'a branch push answers no credential of its own',
      needs: syncable,
      run: async ({ baseUrl }) => {
        const answered = await send(
          baseUrl,
          'POST',
          `/api/v1/workspaces/${workspace()}/branches/conformance-no-such-branch/push`,
        );
        const said = JSON.stringify(answered.body);
        return said.includes(apiKey())
          ? { outcome: 'fail', message: 'the answer carried the calling credential' }
          : { outcome: 'pass' };
      },
    },
  ];
}
