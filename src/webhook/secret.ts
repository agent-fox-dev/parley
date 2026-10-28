import { HttpError } from '../http/errors.js';

/**
 * The webhook secret: what it may be, and how one is made.
 *
 * GitLab sends the secret itself in a header, which is why the
 * alphabet is printable ASCII from `!` to `~` — a space or a control
 * character in a header is at best ignored and at worst an injection.
 * A refusal names the rule and never the value, because the value is
 * a credential whoever typed it may have meant for somewhere else.
 */
export const SECRET_NAME = 'FORGE_WEBHOOK_SECRET';

/** How long a supplied secret may be. */
const SHORTEST = 16;
const LONGEST = 255;

/** How many bytes the hub's own secret has, shown as hex. */
const GENERATED_BYTES = 32;

const ALLOWED = /^[!-~]+$/;

/** A secret of the hub's own making: 32 random bytes as hex. */
export function generatedSecret(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(GENERATED_BYTES));
  return [...bytes].map((byte) => byte.toString(16).padStart(2, '0')).join('');
}

/**
 * The secret a request carries, or none where the caller asked the
 * hub to make one. Everything that can be wrong with it is refused
 * here, by its rule.
 */
export function suppliedSecret(raw: unknown): string | undefined {
  if (raw === undefined || raw === null) {
    return undefined;
  }
  if (typeof raw !== 'object' || Array.isArray(raw)) {
    throw new HttpError(
      400,
      'the request body must be a JSON object with an optional "secret" string',
    );
  }

  const given = (raw as Record<string, unknown>)['secret'];
  if (given === undefined || given === null) {
    return undefined;
  }
  if (typeof given !== 'string') {
    throw new HttpError(400, 'secret must be a string');
  }
  if (given.length < SHORTEST || given.length > LONGEST) {
    throw new HttpError(
      400,
      `secret must be ${String(SHORTEST)} to ${String(LONGEST)} characters long`,
    );
  }
  if (!ALLOWED.test(given)) {
    throw new HttpError(
      400,
      'secret must only have the printable ASCII characters from ! to ~, so no space and no control character',
    );
  }
  return given;
}

/** Where the forge is to post for this workspace. */
export function ingressUrl(externalUrl: string | undefined, slug: string): string | null {
  if (externalUrl === undefined || externalUrl.length === 0) {
    return null;
  }
  return `${externalUrl.replace(/\/$/, '')}/hooks/forge/${slug}`;
}
