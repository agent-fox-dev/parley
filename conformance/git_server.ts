import { execFile } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import { prepareAfc } from './afc.js';
import type { Check } from './harness.js';

/**
 * The conformance checks of the git server. They ask a running
 * deployment to serve a real git client, because the only honest
 * question about a git server is whether git can use it.
 */
const run = promisify(execFile);

const apiKey = () => process.env['PARLEY_SMOKE_API_KEY'] ?? '';
const workspace = () => process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';

const GIT_ENV: NodeJS.ProcessEnv = {
  ...process.env,
  GIT_TERMINAL_PROMPT: '0',
  GIT_ASKPASS: '/usr/bin/true',
  GIT_AUTHOR_NAME: 'Parley Conformance',
  GIT_AUTHOR_EMAIL: 'conformance@parley.test',
  GIT_COMMITTER_NAME: 'Parley Conformance',
  GIT_COMMITTER_EMAIL: 'conformance@parley.test',
};

function hasGit(): boolean {
  try {
    return process.env['PATH'] !== undefined;
  } catch {
    return false;
  }
}

function servable(): string | undefined {
  if ((process.env['PARLEY_BASE_URL'] ?? '').length === 0) {
    return 'a deployment';
  }
  if (apiKey().length === 0) {
    return 'PARLEY_SMOKE_API_KEY';
  }
  if (workspace().length === 0) {
    return 'PARLEY_SMOKE_WORKSPACE';
  }
  return hasGit() ? undefined : 'a git binary';
}

/** The workspace's own clone URL, which is the address under test. */
async function hubUrl(baseUrl: string): Promise<string | undefined> {
  const response = await fetch(`${baseUrl.replace(/\/$/, '')}/api/v1/workspaces/${workspace()}`, {
    headers: { authorization: `Bearer ${apiKey()}` },
  });
  if (response.status !== 200) {
    return undefined;
  }
  const body = (await response.json()) as { hub_url: string | null };
  return body.hub_url ?? undefined;
}

function withCredential(url: string): string {
  return url.replace('://', `://parley:${encodeURIComponent(apiKey())}@`);
}

async function git(
  ...args: string[]
): Promise<{ stdout: string; stderr: string; failed: boolean }> {
  return run('git', args, { env: GIT_ENV, timeout: 120_000 })
    .then((done) => ({ ...done, failed: false }))
    .catch((err: { stdout?: string; stderr?: string }) => ({
      stdout: err.stdout ?? '',
      stderr: err.stderr ?? '',
      failed: true,
    }));
}

export function gitServerChecks(): Check[] {
  return [
    {
      // The Quick Tour of the CLI reference: no token in the URL, the
      // helper supplies it, and git clones. It is the only check that
      // proves the hub asks for credentials in the way a helper can
      // answer, which a URL carrying its own credential never does.
      name: 'a plain clone works through the afc credential helper',
      needs: servable,
      run: async ({ baseUrl }) => {
        const afc = await prepareAfc();
        if (afc.path === undefined) {
          return { outcome: 'skip', message: afc.skipped ?? 'afc is not available' };
        }
        const url = await hubUrl(baseUrl);
        if (url === undefined) {
          return { outcome: 'fail', message: 'the workspace reports no hub_url' };
        }

        const origin = new URL(url).origin;
        const dir = mkdtempSync(join(tmpdir(), 'parley-conformance-helper-'));
        try {
          const cloned = await run(
            'git',
            [
              '-c',
              `credential.${origin}.helper=!${afc.path} credential-helper`,
              'clone',
              '--quiet',
              url,
              dir,
            ],
            {
              env: {
                ...GIT_ENV,
                ENDPOINT_URL: baseUrl.replace(/\/$/, ''),
                API_KEY: apiKey(),
              },
              timeout: 120_000,
            },
          )
            .then(() => ({ failed: false, stderr: '' }))
            .catch((err: { stderr?: string }) => ({ failed: true, stderr: err.stderr ?? '' }));

          return cloned.failed
            ? {
                outcome: 'fail',
                message: cloned.stderr.trim().split('\n').at(-1) ?? 'the clone failed',
              }
            : { outcome: 'pass' };
        } finally {
          rmSync(dir, { recursive: true, force: true });
        }
      },
    },
    {
      name: 'a real git client clones the workspace from its hub_url',
      needs: servable,
      run: async ({ baseUrl }) => {
        const url = await hubUrl(baseUrl);
        if (url === undefined) {
          return { outcome: 'fail', message: 'the workspace reports no hub_url' };
        }
        const dir = mkdtempSync(join(tmpdir(), 'parley-conformance-git-'));
        try {
          const cloned = await git('clone', '--quiet', withCredential(url), dir);
          if (cloned.failed) {
            return {
              outcome: 'fail',
              message: cloned.stderr.trim().split('\n').at(-1) ?? 'the clone failed',
            };
          }
          const head = await git('-C', dir, 'rev-parse', 'HEAD');
          return /^[0-9a-f]{40}$/.test(head.stdout.trim())
            ? { outcome: 'pass', message: `head ${head.stdout.trim().slice(0, 8)}` }
            : { outcome: 'fail', message: 'the clone carried no commit' };
        } finally {
          rmSync(dir, { recursive: true, force: true });
        }
      },
    },
    {
      name: 'the namespace the hub keeps refuses a push and says so',
      needs: servable,
      run: async ({ baseUrl }) => {
        const url = await hubUrl(baseUrl);
        if (url === undefined) {
          return { outcome: 'fail', message: 'the workspace reports no hub_url' };
        }
        const dir = mkdtempSync(join(tmpdir(), 'parley-conformance-held-'));
        try {
          await git('clone', '--quiet', withCredential(url), dir);
          const pushed = await git(
            '-C',
            dir,
            'push',
            'origin',
            'HEAD:refs/hub/replaced/conformance',
          );
          const said = `${pushed.stdout}${pushed.stderr}`;
          return said.includes('maintained by the hub')
            ? { outcome: 'pass' }
            : {
                outcome: 'fail',
                message: `expected the hub's refusal, got ${said.trim().split('\n').at(-1) ?? 'nothing'}`,
              };
        } finally {
          rmSync(dir, { recursive: true, force: true });
        }
      },
    },
    {
      name: 'a credential the deployment does not know is refused',
      needs: servable,
      run: async ({ baseUrl }) => {
        const url = await hubUrl(baseUrl);
        if (url === undefined) {
          return { outcome: 'fail', message: 'the workspace reports no hub_url' };
        }
        const dir = join(tmpdir(), `parley-conformance-denied-${Date.now().toString(36)}`);
        const refused = await git(
          'clone',
          '--quiet',
          url.replace('://', '://parley:af_key_0000.not-a-credential@'),
          dir,
        );
        rmSync(dir, { recursive: true, force: true });
        return refused.failed
          ? { outcome: 'pass' }
          : { outcome: 'fail', message: 'a clone with an unknown credential succeeded' };
      },
    },
  ];
}
