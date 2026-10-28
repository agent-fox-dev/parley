import { HttpError } from '../http/errors.js';

/**
 * Proving who sent a delivery.
 *
 * The ingress is the one route served without a bearer credential,
 * so this is the whole of its security. Two rules shape it. The
 * sender is established before a byte of the body is read, because
 * an unsigned request from anyone on the internet should cost
 * nothing to refuse. And every way of failing answers the same
 * thing: a caller learning *which* part of their forgery was wrong
 * is a caller being helped.
 */
export type ForgeKind = 'github' | 'gitlab';

/** The one answer every bad credential gets. */
export const REFUSED = 'delivery signature missing or invalid';

/** The one answer an unknown workspace and one with no secret share. */
export const NOT_FOUND = 'not found';

/** How much of a body the ingress will read. */
export const MOST_BODY_BYTES = 1_048_576;

/** How long a delivery id may be. */
const LONGEST_ID = 255;

export interface DeliveryIdentity {
  forge: ForgeKind;
  id: string;
  event: string;
}

/** One header value, where the sender gave it exactly once. */
function header(headers: Headers, name: string): string | undefined {
  const value = headers.get(name);
  if (value === null) {
    return undefined;
  }
  // A header given twice arrives joined by a comma, and a signature
  // given twice is not a signature.
  return value.includes(',') && name.toLowerCase().includes('signature') ? undefined : value;
}

/**
 * Which forge sent this, from the headers alone. GitHub's signature
 * header decides; failing that, GitLab's token header.
 */
export function schemeOf(headers: Headers): ForgeKind {
  if (headers.get('x-hub-signature-256') !== null) {
    return 'github';
  }
  if (headers.get('x-gitlab-token') !== null) {
    return 'gitlab';
  }
  throw new HttpError(401, REFUSED);
}

/** Two strings compared without telling how far they matched. */
export function sameSecret(given: string, expected: string): boolean {
  const a = new TextEncoder().encode(given);
  const b = new TextEncoder().encode(expected);
  // The lengths are compared by folding them into the result rather
  // than by returning early, so the comparison takes one time.
  let difference = a.length ^ b.length;
  const longest = Math.max(a.length, b.length);
  for (let index = 0; index < longest; index += 1) {
    difference |= (a[index % a.length] ?? 0) ^ (b[index % b.length] ?? 0);
  }
  return difference === 0 && a.length === b.length;
}

/** GitLab proves itself by sending the secret back. */
export function checkGitlabToken(headers: Headers, secret: string): void {
  const given = header(headers, 'x-gitlab-token');
  if (given === undefined || !sameSecret(given, secret)) {
    throw new HttpError(401, REFUSED);
  }
}

/**
 * GitHub proves itself with an HMAC over the body as it sent it.
 * The bytes are what is signed, never a body parsed and written out
 * again: two JSON documents that mean the same thing have different
 * bytes, and re-serialising would refuse every genuine delivery
 * whose spacing differs from ours (ADR 12).
 */
export async function checkGithubSignature(
  headers: Headers,
  body: Uint8Array,
  secret: string,
): Promise<void> {
  const given = header(headers, 'x-hub-signature-256');
  if (given === undefined || !/^sha256=[0-9a-f]{64}$/.test(given)) {
    throw new HttpError(401, REFUSED);
  }

  const key = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  );
  const signed = new Uint8Array(
    await crypto.subtle.sign('HMAC', key, body as unknown as BufferSource),
  );
  const expected = `sha256=${[...signed].map((byte) => byte.toString(16).padStart(2, '0')).join('')}`;

  if (!sameSecret(given, expected)) {
    throw new HttpError(401, REFUSED);
  }
}

/**
 * The delivery's own id and event name, from each forge's headers
 * (ADR 12). An id that is too long or holds anything but printable
 * ASCII is refused: it is a primary key and a log field, and the
 * forge has no business sending either.
 */
export function identityOf(headers: Headers, forge: ForgeKind): DeliveryIdentity {
  const id =
    forge === 'github'
      ? headers.get('x-github-delivery')
      : (headers.get('idempotency-key') ?? headers.get('x-gitlab-event-uuid'));
  const event = forge === 'github' ? headers.get('x-github-event') : headers.get('x-gitlab-event');

  if (id === null || id.length === 0 || event === null || event.length === 0) {
    throw new HttpError(400, 'the delivery has no id or no event name');
  }
  if (id.length > LONGEST_ID) {
    throw new HttpError(400, `the delivery id is longer than ${String(LONGEST_ID)} characters`);
  }
  if (!/^[ -~]+$/.test(id)) {
    throw new HttpError(400, 'the delivery id has a character outside printable ASCII');
  }

  return { forge, id, event };
}
