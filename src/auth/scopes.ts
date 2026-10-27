import { HttpError } from '../http/errors.js';

/**
 * One read and one write scope for every hub resource, exactly the API
 * reference's scope table.
 */
export const HUB_SCOPES: readonly string[] = [
  'workspaces:read',
  'workspaces:write',
  'patches:read',
  'patches:write',
  'rebuilds:read',
  'rebuilds:write',
  'merges:read',
  'merges:write',
  'git:read',
  'git:write',
  'secrets:read',
  'secrets:write',
  'vars:read',
  'vars:write',
  'sessions:read',
  'sessions:write',
  'audit:read',
  'audit:write',
  'sandboxes:read',
  'sandboxes:write',
  'forge:read',
  'forge:write',
];

/** The identity and access routes use their own scopes. */
export const APIKIT_SCOPES: readonly string[] = [
  'users:read',
  'keys:read',
  'keys:manage',
  'tokens:read',
  'tokens:write',
  'tokens:manage',
  'orgs:read',
  'orgs:write',
];

export const REGISTERED_SCOPES: readonly string[] = [...HUB_SCOPES, ...APIKIT_SCOPES];

/**
 * The names earlier releases used. A token that still holds one was
 * converted to the matching read or write scope, and no new token may ask
 * for one.
 */
export const RETIRED_SCOPE_NAMES: readonly string[] = [
  'workspaces:create',
  'workspaces:delete',
  'workspaces:sync',
  'secrets:list',
  'secrets:manage',
  'secrets:delete',
  'vars:manage',
  'vars:delete',
];

const registered = new Set(REGISTERED_SCOPES);
const retired = new Set(RETIRED_SCOPE_NAMES);

export function isRegisteredScope(name: string): boolean {
  return registered.has(name);
}

export function isRetiredScope(name: string): boolean {
  return retired.has(name);
}

/**
 * Whether a token holding these scopes reaches a route needing that one.
 * The write scope of a resource grants its read scope; nothing else is
 * implied, so a read scope never reaches a write route.
 */
export function grantsScope(held: readonly string[], needed: string): boolean {
  if (held.includes(needed)) {
    return true;
  }
  const [resource, action] = needed.split(':');
  return action === 'read' && resource !== undefined && held.includes(`${resource}:write`);
}

/**
 * The scopes a token may be granted, or a 400 naming the first that is
 * refused. Every route that takes a permission list passes it through here,
 * so a retired or invented name is refused in one place.
 */
export function checkedScopes(requested: unknown): string[] {
  if (!Array.isArray(requested)) {
    throw new HttpError(400, 'permissions must be an array of scope names');
  }

  const granted: string[] = [];
  for (const name of requested as unknown[]) {
    if (typeof name !== 'string') {
      throw new HttpError(400, 'permissions must be an array of scope names');
    }
    if (isRetiredScope(name)) {
      throw new HttpError(400, `the scope ${name} was retired and cannot be granted`);
    }
    if (!isRegisteredScope(name)) {
      throw new HttpError(400, `${name} is not a scope this service registers`);
    }
    if (!granted.includes(name)) {
      granted.push(name);
    }
  }
  return granted;
}
