import type { Check } from './harness.js';

/**
 * The conformance checks of the web interface. A deployment that
 * claims to serve it has to serve the pages, the stylesheet they all
 * reference, and — for a visitor carrying no session — the landing
 * page rather than an error or anything about what it holds.
 */
const workspace = () => process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';

function deployed(): string | undefined {
  return (process.env['PARLEY_BASE_URL'] ?? '').length === 0 ? 'a deployment' : undefined;
}

function withWorkspace(): string | undefined {
  return deployed() ?? (workspace().length === 0 ? 'PARLEY_SMOKE_WORKSPACE' : undefined);
}

async function get(baseUrl: string, path: string): Promise<{ response: Response; body: string }> {
  const response = await fetch(`${baseUrl.replace(/\/$/, '')}${path}`);
  return { response, body: await response.text() };
}

export function webChecks(): Check[] {
  return [
    {
      name: 'the landing page is served as a page',
      needs: deployed,
      run: async ({ baseUrl }) => {
        const { response, body } = await get(baseUrl, '/');
        if (response.status !== 200) {
          return { outcome: 'fail', message: `expected 200, got ${String(response.status)}` };
        }
        if (!(response.headers.get('content-type') ?? '').startsWith('text/html')) {
          return {
            outcome: 'fail',
            message: `expected HTML, got ${response.headers.get('content-type') ?? 'nothing'}`,
          };
        }
        return body.includes('<main class="page">') && body.includes('Parley')
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the landing page has no main landmark' };
      },
    },
    {
      name: 'every page carries the viewport and the one stylesheet',
      needs: deployed,
      run: async ({ baseUrl }) => {
        for (const path of ['/', '/dashboard']) {
          const { body } = await get(baseUrl, path);
          if (!body.includes('name="viewport"') || !body.includes('/web/tokens.css')) {
            return {
              outcome: 'fail',
              message: `${path} is missing the viewport or the stylesheet`,
            };
          }
        }
        return { outcome: 'pass' };
      },
    },
    {
      name: 'the stylesheet defines both themes',
      needs: deployed,
      run: async ({ baseUrl }) => {
        const { response, body } = await get(baseUrl, '/web/tokens.css');
        if (response.status !== 200) {
          return { outcome: 'fail', message: `expected 200, got ${String(response.status)}` };
        }
        if (!(response.headers.get('content-type') ?? '').startsWith('text/css')) {
          return {
            outcome: 'fail',
            message: `expected CSS, got ${response.headers.get('content-type') ?? 'nothing'}`,
          };
        }
        return body.includes('prefers-color-scheme: dark') && body.includes("[data-theme='dark']")
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the stylesheet carries only one theme' };
      },
    },
    {
      name: 'a visitor with no session is shown the way in, not an error',
      needs: deployed,
      run: async ({ baseUrl }) => {
        const { response, body } = await get(baseUrl, '/dashboard');
        if (response.status !== 200) {
          return { outcome: 'fail', message: `expected 200, got ${String(response.status)}` };
        }
        return body.includes('Sign in')
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'the dashboard answered a stranger with something else' };
      },
    },
    {
      name: 'a workspace address tells a stranger nothing about it',
      needs: withWorkspace,
      run: async ({ baseUrl }) => {
        const slug = workspace();
        const known = await get(baseUrl, `/workspaces/${slug}`);
        const unknown = await get(baseUrl, '/workspaces/no-such-workspace-here');
        if (known.response.status !== unknown.response.status) {
          return {
            outcome: 'fail',
            message: `a real slug answered ${String(known.response.status)} and an invented one ${String(unknown.response.status)}`,
          };
        }
        return known.body.includes(slug)
          ? { outcome: 'fail', message: 'the page named the workspace to a stranger' }
          : { outcome: 'pass', message: `both answered ${String(known.response.status)}` };
      },
    },
  ];
}
