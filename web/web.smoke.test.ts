import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { apiKey, baseUrl, bearer, call, missing, needed } from '../src/testutil/smoke.js';

/**
 * The web interface in a real browser, against a deployment that is
 * really running.
 *
 * Everything else about the interface is proven by rendering it to a
 * string, which is what server-rendered pages make easy. Three things
 * cannot be: that a browser's session really survives a redirect
 * chain through a provider, that a page really lays out at a phone's
 * width, and that a link really goes where it says. So these drive a
 * browser the operator started — any Chromium with its debugging port
 * open — over the DevTools protocol, which is in the browser already
 * and costs the project no dependency.
 *
 *   chromium --headless --remote-debugging-port=9222
 *   PARLEY_SMOKE_BROWSER=http://127.0.0.1:9222 make test-smoke
 *
 * With no browser, no deployment or no fixtures, the suite says which
 * and skips: a smoke test that quietly passed on nothing would be
 * worse than no smoke test.
 */
const endpoint = (process.env['PARLEY_SMOKE_BROWSER'] ?? '').replace(/\/$/, '');
const provider = process.env['PARLEY_SMOKE_OAUTH_PROVIDER'] ?? 'github';
const ownWorkspace = process.env['PARLEY_SMOKE_WORKSPACE'] ?? '';
const patchWorkspace = process.env['PARLEY_SMOKE_PATCH_WORKSPACE'] ?? '';
const orgWorkspace = process.env['PARLEY_SMOKE_ORG_WORKSPACE'] ?? '';
const foreignWorkspace = process.env['PARLEY_SMOKE_FOREIGN_WORKSPACE'] ?? '';

const reason = missing(
  ['PARLEY_BASE_URL', baseUrl],
  ['PARLEY_SMOKE_BROWSER', endpoint],
  ['PARLEY_SMOKE_API_KEY', apiKey],
  ['PARLEY_SMOKE_WORKSPACE', ownWorkspace],
);

/** How long one protocol call, and one navigation, may take. */
const CALL_MS = 30_000;
const SETTLE_MS = 20_000;

interface Reply {
  id?: number;
  result?: unknown;
  error?: { message: string };
}

interface Connection {
  send(
    method: string,
    params?: Record<string, unknown>,
    sessionId?: string,
  ): Promise<Record<string, unknown>>;
  close(): void;
}

async function connect(address: string): Promise<Connection> {
  const socket = new WebSocket(address);
  await new Promise<void>((open, fail) => {
    socket.addEventListener('open', () => {
      open();
    });
    socket.addEventListener('error', () => {
      fail(new Error('the browser refused the connection'));
    });
  });

  const waiting = new Map<number, (reply: Reply) => void>();
  socket.addEventListener('message', (event: MessageEvent) => {
    const reply = JSON.parse(String(event.data)) as Reply;
    if (typeof reply.id === 'number') {
      const answer = waiting.get(reply.id);
      waiting.delete(reply.id);
      answer?.(reply);
    }
  });

  let last = 0;
  return {
    async send(method, params = {}, sessionId) {
      last += 1;
      const id = last;
      const answered = new Promise<Reply>((done, fail) => {
        const timer = setTimeout(() => {
          waiting.delete(id);
          fail(new Error(`the browser did not answer ${method}`));
        }, CALL_MS);
        waiting.set(id, (reply) => {
          clearTimeout(timer);
          done(reply);
        });
      });
      socket.send(
        JSON.stringify({ id, method, params, ...(sessionId === undefined ? {} : { sessionId }) }),
      );
      const reply = await answered;
      if (reply.error !== undefined) {
        throw new Error(`${method}: ${reply.error.message}`);
      }
      return (reply.result ?? {}) as Record<string, unknown>;
    },
    close() {
      socket.close();
    },
  };
}

/** One tab, and the few things these tests do with one. */
interface Page {
  go(path: string): Promise<void>;
  follow(selector: string): Promise<void>;
  at(): Promise<string>;
  read(): Promise<string>;
  html(): Promise<string>;
  links(): Promise<string[]>;
  has(selector: string): Promise<boolean>;
  overflows(): Promise<boolean>;
  phone(on: boolean): Promise<void>;
  scheme(which: 'dark' | 'light'): Promise<void>;
  close(): Promise<void>;
}

async function pause(ms: number): Promise<void> {
  await new Promise((done) => setTimeout(done, ms));
}

async function openBrowser(): Promise<Page> {
  const found = await fetch(`${endpoint}/json/version`);
  if (!found.ok) {
    throw new Error(`the browser at ${endpoint} answered ${String(found.status)}`);
  }
  const { webSocketDebuggerUrl } = (await found.json()) as { webSocketDebuggerUrl?: string };
  if (webSocketDebuggerUrl === undefined) {
    throw new Error(`the browser at ${endpoint} named no debugger address`);
  }

  const connection = await connect(webSocketDebuggerUrl);
  const { targetId } = (await connection.send('Target.createTarget', {
    url: 'about:blank',
  })) as unknown as { targetId: string };
  const { sessionId } = (await connection.send('Target.attachToTarget', {
    targetId,
    flatten: true,
  })) as unknown as { sessionId: string };

  const send = async (method: string, params: Record<string, unknown> = {}) =>
    connection.send(method, params, sessionId);
  await send('Page.enable');
  await send('Runtime.enable');

  const evaluate = async <T>(expression: string): Promise<T> => {
    const { result } = (await send('Runtime.evaluate', {
      expression,
      returnByValue: true,
      awaitPromise: true,
    })) as unknown as { result: { value: T } };
    return result.value;
  };

  // A navigation is done when the document is, and a sign-in is
  // several navigations: polling the document is what survives a
  // redirect chain that no single load event covers.
  const settled = async (): Promise<void> => {
    const until = Date.now() + SETTLE_MS;
    while (Date.now() < until) {
      const ready = await evaluate<string>('document.readyState');
      if (ready === 'complete') {
        await pause(100);
        if ((await evaluate<string>('document.readyState')) === 'complete') {
          return;
        }
      }
      await pause(100);
    }
    throw new Error('the page never finished loading');
  };

  return {
    async go(path: string) {
      await send('Page.navigate', { url: `${baseUrl}${path}` });
      await settled();
    },
    async follow(selector: string) {
      const was = await evaluate<string>('location.href');
      const clicked = await evaluate<boolean>(
        `(() => { const found = document.querySelector(${JSON.stringify(selector)}); if (found === null) return false; found.click(); return true; })()`,
      );
      if (!clicked) {
        throw new Error(`there is no ${selector} on ${was} to follow`);
      }
      const until = Date.now() + SETTLE_MS;
      while (Date.now() < until) {
        if ((await evaluate<string>('location.href')) !== was) {
          await settled();
          return;
        }
        await pause(100);
      }
      throw new Error(`following ${selector} left the browser on ${was}`);
    },
    async at() {
      return evaluate<string>('location.href');
    },
    async read() {
      return evaluate<string>('document.body.innerText');
    },
    async html() {
      return evaluate<string>('document.documentElement.outerHTML');
    },
    async links() {
      return evaluate<string[]>(
        '[...document.querySelectorAll("a[href]")].map((link) => link.getAttribute("href"))',
      );
    },
    async has(selector: string) {
      return evaluate<boolean>(`document.querySelector(${JSON.stringify(selector)}) !== null`);
    },
    async overflows() {
      return evaluate<boolean>(
        'document.documentElement.scrollWidth > document.documentElement.clientWidth + 1',
      );
    },
    async phone(on: boolean) {
      await (on
        ? send('Emulation.setDeviceMetricsOverride', {
            width: 360,
            height: 740,
            deviceScaleFactor: 1,
            mobile: true,
          })
        : send('Emulation.clearDeviceMetricsOverride'));
    },
    async scheme(which: 'dark' | 'light') {
      await send('Emulation.setEmulatedMedia', {
        features: [{ name: 'prefers-color-scheme', value: which }],
      });
    },
    async close() {
      await connection.send('Target.closeTarget', { targetId });
      connection.close();
    },
  };
}

let page: Page | undefined;
let unreachable = '';

/** The tab, or a skip naming what was not there to drive. */
function browser(context: { skip: (note?: string) => never }): Page {
  if (page === undefined) {
    context.skip(unreachable);
  }
  return page;
}

/** The slugs the dashboard lists, read from its one list of workspaces. */
async function listed(tab: Page): Promise<string[]> {
  const links = await tab.links();
  return links
    .filter((href) => href.startsWith('/workspaces/'))
    .map((href) => href.slice('/workspaces/'.length));
}

async function keysOf(): Promise<string[]> {
  const answer = await call('/api/v1/user/keys', bearer(apiKey));
  expect(answer.status).toBe(200);
  const keys = (await answer.json()) as Array<{ key_id: string }>;
  return keys.map((key) => key.key_id).toSorted();
}

describe.skipIf(reason.length > 0)(`the web interface in a browser${needed(reason)}`, () => {
  beforeAll(async () => {
    try {
      page = await openBrowser();
    } catch (err) {
      unreachable = `skipped: ${err instanceof Error ? err.message : 'no browser answered'}`;
    }
  }, 60_000);

  afterAll(async () => {
    await page?.close();
    page = undefined;
  });

  it('signs a developer in while their agent keeps using its key', async (context) => {
    const tab = browser(context);
    const before = await keysOf();

    await tab.go('/');
    expect(await tab.read()).toContain('Parley');
    expect(await tab.has(`a[href="/signin/${provider}"]`)).toBe(true);

    await tab.follow(`a[href="/signin/${provider}"]`);
    expect(await tab.at()).toContain('/dashboard');

    const shown = await tab.read();
    expect(shown).toContain('Your workspaces');
    expect(await listed(tab)).toContain(ownWorkspace);

    // The key the agent is running with was not touched by any of that.
    const stillWorks = await call('/api/v1/workspaces', bearer(apiKey));
    expect(stillWorks.status).toBe(200);
    expect(await keysOf()).toEqual(before);
  });

  it.skipIf(patchWorkspace.length === 0)(
    'shows a patch stack’s health, and shows it at a phone’s width',
    async (context) => {
      const tab = browser(context);
      await tab.go('/dashboard');
      await tab.follow(`a[href="/workspaces/${patchWorkspace}"]`);
      expect(await tab.at()).toContain(`/workspaces/${patchWorkspace}`);

      const shown = await tab.read();
      expect(shown).toContain('Patches');
      expect(shown).toContain('Latest rebuild');
      expect(shown).toContain('Recent events');

      // What the page says is what the API says, for this person.
      const status = (await (
        await call(`/api/v1/workspaces/${patchWorkspace}/patch-status`, bearer(apiKey))
      ).json()) as {
        patches: Array<{ branch_name: string }>;
        last_rebuild: { id: string } | null;
      };
      for (const patch of status.patches) {
        expect(shown, `${patch.branch_name} is on the page`).toContain(patch.branch_name);
      }
      if (status.last_rebuild !== null) {
        expect(shown).toContain(status.last_rebuild.id);
      }

      // And it reads at a phone's width, in either theme.
      try {
        await tab.phone(true);
        for (const which of ['light', 'dark'] as const) {
          await tab.scheme(which);
          await tab.go(`/workspaces/${patchWorkspace}`);
          expect(await tab.overflows(), `${which} fits 360px`).toBe(false);
        }
      } finally {
        await tab.scheme('light');
        await tab.phone(false);
      }
    },
  );

  it.skipIf(orgWorkspace.length === 0 || foreignWorkspace.length === 0)(
    'lists a member’s own and shared workspaces, and nothing else',
    async (context) => {
      const tab = browser(context);
      await tab.go('/dashboard');

      const slugs = await listed(tab);
      expect(slugs).toContain(ownWorkspace);
      expect(slugs).toContain(orgWorkspace);
      expect(slugs).not.toContain(foreignWorkspace);
      // The access the dashboard reports is the API's own word for it.
      expect(await tab.read()).toContain('member');

      await tab.follow(`a[href="/workspaces/${orgWorkspace}"]`);
      expect(await tab.read()).toContain(orgWorkspace);

      // The one a person may not read is told nothing apart.
      await tab.go(`/workspaces/${foreignWorkspace}`);
      const refused = await tab.read();
      expect(refused).toContain('Not available');
      expect(refused).not.toContain(foreignWorkspace);
      expect(await tab.has('dl.fields')).toBe(false);
    },
  );
});
