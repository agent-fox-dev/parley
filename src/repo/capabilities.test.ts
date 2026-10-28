import { describe, expect, it } from 'vitest';
import type { HttpError } from '../http/errors.js';
import {
  type Capability,
  type StoreCapabilities,
  ARTIFACTS_CAPABILITIES,
  requireCapability,
} from './capabilities.js';

const NONE: StoreCapabilities = {
  arbitraryRefNamespaces: false,
  nonFastForward: false,
  refDeletion: false,
};

const CAPABILITIES: Capability[] = ['arbitraryRefNamespaces', 'nonFastForward', 'refDeletion'];

function refusal(capabilities: StoreCapabilities, capability: Capability): HttpError {
  try {
    requireCapability(capabilities, capability);
  } catch (err) {
    return err as HttpError;
  }
  throw new Error('the capability was granted when it should have been refused');
}

describe('asking a store for a capability it lacks', () => {
  it.each(CAPABILITIES)('refuses %s, naming it', (capability) => {
    const err = refusal(NONE, capability);

    expect(err.status).toBe(501);
    expect(err.message).toContain(
      capability === 'arbitraryRefNamespaces'
        ? 'refs outside'
        : capability === 'nonFastForward'
          ? 'non-fast-forward'
          : 'ref deletion',
    );
  });

  it.each(CAPABILITIES)(
    'says nothing and does nothing for %s when the store has it',
    (capability) => {
      expect(() => {
        requireCapability(ARTIFACTS_CAPABILITIES, capability);
      }).not.toThrow();
    },
  );

  it('refuses only the capability that is missing', () => {
    const partial: StoreCapabilities = { ...NONE, refDeletion: true };

    expect(() => {
      requireCapability(partial, 'refDeletion');
    }).not.toThrow();
    expect(refusal(partial, 'nonFastForward').status).toBe(501);
  });
});
