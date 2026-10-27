export type CredentialKind = 'api_key' | 'pat' | 'admin_token';

export interface ParsedCredential {
  kind: 'api_key' | 'pat';
  id: string;
  secret: string;
}

const PREFIX = 'af_';
const PAT_PREFIX = 'af_pat_';
const HEX = '0123456789abcdef';

/** The credential a request presents, or undefined when it presents none. */
export function bearerCredential(header: string | undefined): string | undefined {
  if (header === undefined) {
    return undefined;
  }
  const [scheme, ...rest] = header.split(' ');
  if (scheme?.toLowerCase() !== 'bearer') {
    return undefined;
  }
  const credential = rest.join(' ').trim();
  return credential.length === 0 ? undefined : credential;
}

/**
 * The kind, identifier and secret a credential string carries. Identifiers
 * and secrets are hex, so the underscores separate them unambiguously.
 */
export function parseCredential(raw: string): ParsedCredential | undefined {
  const [kind, body] = raw.startsWith(PAT_PREFIX)
    ? (['pat', raw.slice(PAT_PREFIX.length)] as const)
    : raw.startsWith(PREFIX)
      ? (['api_key', raw.slice(PREFIX.length)] as const)
      : ([undefined, ''] as const);

  if (kind === undefined) {
    return undefined;
  }
  const separator = body.indexOf('_');
  if (separator <= 0 || separator === body.length - 1) {
    return undefined;
  }
  return { kind, id: body.slice(0, separator), secret: body.slice(separator + 1) };
}

export function formatApiKey(id: string, secret: string): string {
  return `${PREFIX}${id}_${secret}`;
}

export function formatPat(id: string, secret: string): string {
  return `${PAT_PREFIX}${id}_${secret}`;
}

function randomHex(bytes: number): string {
  const values = crypto.getRandomValues(new Uint8Array(bytes));
  let out = '';
  for (const value of values) {
    out += HEX[value >> 4];
    out += HEX[value & 15];
  }
  return out;
}

/** The identifier a credential is found by. Kept in clear in the store. */
export function newIdentifier(): string {
  return randomHex(8);
}

/** The secret half of a credential: 256 bits, so guessing it is hopeless. */
export function newSecret(): string {
  return randomHex(32);
}

/**
 * The stored form of a secret. A credential secret is random, not chosen by
 * a person, so one pass of SHA-256 puts it beyond guessing without spending
 * the per-request budget a password hash would (see ADR 3).
 */
export async function hashSecret(secret: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(secret));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, '0')).join('');
}

/** Whether two hex digests are the same, in time that does not depend on how far they agree. */
export function sameDigest(left: string, right: string): boolean {
  if (left.length !== right.length) {
    return false;
  }
  let difference = 0;
  for (let i = 0; i < left.length; i += 1) {
    difference |= left.charCodeAt(i) ^ right.charCodeAt(i);
  }
  return difference === 0;
}

const CREDENTIAL_PATTERN = /af_(?:pat_)?[A-Za-z0-9]+_[A-Za-z0-9]+/g;

/**
 * The text with every credential-shaped string replaced. Log records pass
 * through here, so a secret a failure carried into an error message never
 * reaches the log.
 */
export function redactCredentials(text: string): string {
  return text.replace(CREDENTIAL_PATTERN, 'af_[redacted]');
}
