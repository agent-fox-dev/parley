/**
 * A forge of the suite's own: a fetch that answers canned replies and
 * remembers what it was sent. Four suites need one, so it is here
 * rather than written four times, and it is the forge equivalent of
 * `fakeremote.ts`.
 */
export interface ForgeCall {
  method: string;
  url: string;
  /** What the request carried, by header name in lower case. */
  headers: Record<string, string>;
  body: unknown;
}

export interface FakeForgeOptions {
  /** What to answer, by `METHOD /path` or by path alone. */
  answers?: Record<string, { status?: number; body?: unknown; headers?: Record<string, string> }>;
  /** Answers everything not named above. */
  fallback?: { status?: number; body?: unknown; headers?: Record<string, string> };
  /** Where to redirect every request, for the redirect rule. */
  redirectTo?: string;
  /** Never answers, for the deadline. */
  silent?: boolean;
  /** Fails the connection, for the unreachable case. */
  unreachable?: boolean;
}

export interface FakeForge {
  fetch: typeof fetch;
  calls: () => ForgeCall[];
  last: () => ForgeCall | undefined;
  /** The credential the forge was given, or undefined where none was. */
  lastToken: () => string | undefined;
  /** Changes what the forge answers from now on. */
  answerWith: (answers: FakeForgeOptions['answers']) => void;
}

function tokenOf(headers: Record<string, string>): string | undefined {
  const authorization = headers['authorization'];
  if (authorization === undefined) {
    return undefined;
  }
  return authorization.replace(/^(Bearer|token|Basic)\s+/i, '');
}

export function fakeForge(options: FakeForgeOptions = {}): FakeForge {
  const calls: ForgeCall[] = [];
  let answers = options.answers ?? {};

  const served: typeof fetch = async (input, init) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const method = (init?.method ?? 'GET').toUpperCase();
    const headers: Record<string, string> = {};
    for (const [name, value] of Object.entries((init?.headers ?? {}) as Record<string, string>)) {
      headers[name.toLowerCase()] = value;
    }
    const raw = init?.body;
    calls.push({
      method,
      url,
      headers,
      body: typeof raw === 'string' && raw.length > 0 ? JSON.parse(raw) : undefined,
    });

    if (options.unreachable === true) {
      throw new TypeError('fetch failed');
    }
    if (options.silent === true) {
      // Never answers, so only the caller's deadline ends the call —
      // which is what a real fetch does with an aborted signal.
      return new Promise<Response>((_, failed) => {
        init?.signal?.addEventListener('abort', () => {
          failed(new DOMException('the request was aborted', 'AbortError'));
        });
      });
    }
    if (options.redirectTo !== undefined) {
      return new Response(null, { status: 302, headers: { location: options.redirectTo } });
    }

    const path = new URL(url).pathname;
    const answer = answers[`${method} ${path}`] ??
      answers[path] ??
      options.fallback ?? { status: 200, body: {} };
    return new Response(answer.body === undefined ? null : JSON.stringify(answer.body), {
      status: answer.status ?? 200,
      headers: { 'content-type': 'application/json', ...answer.headers },
    });
  };

  return {
    fetch: served,
    calls: () => [...calls],
    last: () => calls.at(-1),
    lastToken: () =>
      calls.at(-1) === undefined ? undefined : tokenOf((calls.at(-1) as ForgeCall).headers),
    answerWith: (next) => {
      answers = next ?? {};
    },
  };
}
