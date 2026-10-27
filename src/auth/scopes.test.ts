import { describe, expect, it } from 'vitest';
import { HttpError } from '../http/errors.js';
import { checkedScopes, grantsScope } from './scopes.js';

/**
 * The names are written out here rather than read from the registry: the
 * reference fixes them, and a test that iterated the registry would agree
 * with it however wrong it was.
 */
const HUB = [
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

const APIKIT = [
  'users:read',
  'keys:read',
  'keys:manage',
  'tokens:read',
  'tokens:write',
  'tokens:manage',
  'orgs:read',
  'orgs:write',
];

const RETIRED = [
  'workspaces:create',
  'workspaces:delete',
  'workspaces:sync',
  'secrets:list',
  'secrets:manage',
  'secrets:delete',
  'vars:manage',
  'vars:delete',
];

function refusal(requested: unknown): HttpError {
  try {
    checkedScopes(requested);
  } catch (err) {
    return err as HttpError;
  }
  throw new Error('the registry accepted what it should have refused');
}

describe('the scopes a token may be granted', () => {
  it.each([...HUB, ...APIKIT])('accepts %s', (name) => {
    expect(checkedScopes([name])).toEqual([name]);
  });

  it.each(RETIRED)('refuses the retired name %s, naming it', (name) => {
    const err = refusal([name]);

    expect(err.status).toBe(400);
    expect(err.message).toContain(name);
  });

  it.each(['workspaces:admin', 'nonsense', 'workspaces', '', 'WORKSPACES:READ'])(
    'refuses the unregistered name %s',
    (name) => {
      expect(refusal([name]).status).toBe(400);
    },
  );

  it.each([
    ['a list that is not an array', 'workspaces:read'],
    ['an entry that is not a string', [1]],
    ['a missing list', undefined],
    ['a null list', null],
  ])('refuses %s', (_name, requested) => {
    expect(refusal(requested).status).toBe(400);
  });

  it('keeps a granted list in the order it was asked for, without duplicates', () => {
    expect(checkedScopes(['patches:read', 'workspaces:read', 'patches:read'])).toEqual([
      'patches:read',
      'workspaces:read',
    ]);
  });
});

describe('what a held scope grants', () => {
  it.each([
    ['workspaces:write', 'workspaces:read', true],
    ['workspaces:write', 'workspaces:write', true],
    ['workspaces:read', 'workspaces:read', true],
    ['workspaces:read', 'workspaces:write', false],
    ['patches:write', 'workspaces:read', false],
    ['tokens:write', 'tokens:read', true],
    ['tokens:manage', 'tokens:read', false],
    ['tokens:read', 'tokens:manage', false],
  ])('holding %s towards %s is %s', (held, needed, expected) => {
    expect(grantsScope([held], needed)).toBe(expected);
  });

  it('grants nothing when the token holds nothing', () => {
    expect(grantsScope([], 'workspaces:read')).toBe(false);
  });
});
