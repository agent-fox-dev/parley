/**
 * What the smoke suites share: where the deployment is, which credentials
 * were supplied, and how to say what is missing. A suite with something
 * missing skips naming it rather than passing on nothing.
 */
export const baseUrl = (process.env['PARLEY_BASE_URL'] ?? '').replace(/\/$/, '');
export const apiKey = process.env['PARLEY_SMOKE_API_KEY'] ?? '';
export const memberKey = process.env['PARLEY_SMOKE_MEMBER_KEY'] ?? '';
export const adminToken = process.env['PARLEY_ADMIN_TOKEN'] ?? '';

export function missing(...needs: Array<[string, string]>): string {
  return needs
    .filter(([, value]) => value.length === 0)
    .map(([name]) => name)
    .join(', ');
}

export function needed(reason: string): string {
  return reason.length > 0 ? ` (needs ${reason})` : '';
}

export function bearer(credential: string, init: RequestInit = {}): RequestInit {
  return {
    ...init,
    headers: {
      ...((init.headers ?? {}) as Record<string, string>),
      authorization: `Bearer ${credential}`,
    },
  };
}

export function asJson(method: string, body: unknown, credential: string): RequestInit {
  return bearer(credential, {
    method,
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
}

export async function call(path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(`${baseUrl}${path}`, init);
}

/** A slug of this run alone, so repeated runs do not collide. */
export function uniqueSlug(what: string): string {
  return `smoke-${what}-${Date.now().toString(36)}`;
}
