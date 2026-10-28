import type { Check } from './harness.js';

/**
 * The conformance checks of the repository store. They ask the running
 * deployment what it did with a workspace's repository, which needs a
 * credential and a remote to copy from.
 */
const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';
const gitUrl = () => process.env['PARLEY_SMOKE_GIT_URL'] ?? '';

function deployed(): string | undefined {
  return (process.env['PARLEY_BASE_URL'] ?? '').length === 0 ? 'a deployment' : undefined;
}

function copyable(): string | undefined {
  return (
    deployed() ??
    (apiKey().length === 0
      ? 'PARLEY_SMOKE_API_KEY'
      : gitUrl().length === 0
        ? 'PARLEY_SMOKE_GIT_URL'
        : undefined)
  );
}

async function send(
  baseUrl: string,
  method: string,
  path: string,
  body?: unknown,
): Promise<Response> {
  return fetch(`${baseUrl.replace(/\/$/, '')}${path}`, {
    method,
    headers: {
      authorization: `Bearer ${apiKey()}`,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

async function retire(baseUrl: string, slug: string): Promise<void> {
  await send(baseUrl, 'POST', `/api/v1/workspaces/${slug}/archive`);
  await send(baseUrl, 'DELETE', `/api/v1/workspaces/${slug}`);
}

export function repositoryChecks(): Check[] {
  return [
    {
      name: 'a registered workspace copies its repository in and reaches ready',
      needs: copyable,
      run: async ({ baseUrl }) => {
        const slug = `conformance-copy-${Date.now().toString(36)}`;
        try {
          const created = await send(baseUrl, 'POST', '/api/v1/workspaces', {
            slug,
            git_url: gitUrl(),
          });
          if (created.status !== 201) {
            return { outcome: 'fail', message: `the create answered ${String(created.status)}` };
          }

          const deadline = Date.now() + 120_000;
          let status = 'pending';
          let headSha: string | null = null;
          while (Date.now() < deadline && (status === 'pending' || status === 'cloning')) {
            await new Promise((resolve) => setTimeout(resolve, 2000));
            const read = await send(baseUrl, 'GET', `/api/v1/workspaces/${slug}`);
            const body = (await read.json()) as { status: string; head_sha: string | null };
            status = body.status;
            headSha = body.head_sha;
          }

          if (status !== 'ready') {
            return { outcome: 'fail', message: `the workspace settled at ${status}` };
          }
          return /^[0-9a-f]{40}$/.test(headSha ?? '')
            ? { outcome: 'pass', message: `head ${(headSha ?? '').slice(0, 8)}` }
            : { outcome: 'fail', message: 'the workspace is ready with no head commit' };
        } finally {
          await retire(baseUrl, slug);
        }
      },
    },
    {
      name: 'a workspace whose remote does not exist records why',
      needs: copyable,
      run: async ({ baseUrl }) => {
        const slug = `conformance-fail-${Date.now().toString(36)}`;
        try {
          const created = await send(baseUrl, 'POST', '/api/v1/workspaces', {
            slug,
            git_url: 'https://nothing.invalid/owner/repo.git',
          });
          if (created.status !== 201) {
            return {
              outcome: 'fail',
              message: `an unreachable remote answered ${String(created.status)}, not 201`,
            };
          }

          const deadline = Date.now() + 120_000;
          let body = { status: 'pending', clone_error: null as string | null };
          while (
            Date.now() < deadline &&
            (body.status === 'pending' || body.status === 'cloning')
          ) {
            await new Promise((resolve) => setTimeout(resolve, 2000));
            const read = await send(baseUrl, 'GET', `/api/v1/workspaces/${slug}`);
            body = (await read.json()) as { status: string; clone_error: string | null };
          }

          return body.status === 'failed' && (body.clone_error ?? '').length > 0
            ? { outcome: 'pass' }
            : {
                outcome: 'fail',
                message: `settled at ${body.status} with clone_error ${String(body.clone_error)}`,
              };
        } finally {
          await retire(baseUrl, slug);
        }
      },
    },
  ];
}
