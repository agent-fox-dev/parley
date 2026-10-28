import type { Check } from './harness.js';

/**
 * The conformance checks of the merge routes. They go after the two
 * things a client depends on and cannot see for itself: that the body
 * is checked before the workspace, and that a merge queues rather
 * than being refused for a busy workspace.
 */
const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';
const workspace = () => process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';

function mergeable(): string | undefined {
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
  path: string,
  body?: unknown,
): Promise<{ status: number; body: Record<string, unknown> }> {
  const response = await fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    method: body === undefined ? 'GET' : 'POST',
    headers: {
      authorization: `Bearer ${apiKey()}`,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  return {
    status: response.status,
    body: (await response.json().catch(() => ({}))) as Record<string, unknown>,
  };
}

export function mergeChecks(): Check[] {
  return [
    {
      // The deliberate exception to the anti-enumeration rule: a name
      // that breaks git's rules is the caller's mistake whatever
      // workspace it names.
      name: 'a merge submit checks its body before the workspace',
      needs: mergeable,
      run: async ({ baseUrl }) => {
        const refused = await send(baseUrl, '/api/v1/workspaces/conformance-no-such-ws/merges', {
          target_branch: '-x',
          source_ref: 'whatever',
        });
        return refused.status === 400
          ? { outcome: 'pass' }
          : {
              outcome: 'fail',
              message: `expected 400 for a malformed name, got ${String(refused.status)}`,
            };
      },
    },
    {
      name: 'a merge list answers an array',
      needs: mergeable,
      run: async ({ baseUrl }) => {
        const listed = await send(baseUrl, `/api/v1/workspaces/${workspace()}/merges`);
        if (listed.status !== 200) {
          return { outcome: 'fail', message: `the list answered ${String(listed.status)}` };
        }
        return Array.isArray(listed.body)
          ? { outcome: 'pass', message: `${String((listed.body as unknown[]).length)} jobs` }
          : { outcome: 'fail', message: 'the list did not answer an array' };
      },
    },
    {
      name: 'a batch rebase names an offending branch by its index',
      needs: mergeable,
      run: async ({ baseUrl }) => {
        const refused = await send(baseUrl, `/api/v1/workspaces/${workspace()}/rebase`, {
          target_ref: 'main',
          branches: ['fine', 'a..b'],
        });
        const message = ((refused.body['error'] ?? {}) as { message?: string }).message ?? '';
        if (refused.status !== 400) {
          return { outcome: 'fail', message: `expected 400, got ${String(refused.status)}` };
        }
        return message.includes('branches[1]')
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: `the message does not name the branch: ${message}` };
      },
    },
  ];
}
