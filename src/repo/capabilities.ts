import { HttpError } from '../http/errors.js';

/**
 * The three things a repository store may or may not let Parley do with
 * refs. None is documented, so each is declared per implementation and
 * held against the real store by an integration test (see ADR 6).
 */
export interface StoreCapabilities {
  /** Accepts a ref outside refs/heads/ and refs/tags/. */
  arbitraryRefNamespaces: boolean;
  /** Accepts an update that is not a fast-forward, when forced. */
  nonFastForward: boolean;
  /** Accepts the deletion of a ref. */
  refDeletion: boolean;
}

export type Capability = keyof StoreCapabilities;

/**
 * What Cloudflare Artifacts does, established by experiment rather than
 * assumed: every one of the three is accepted (ADR 6).
 */
export const ARTIFACTS_CAPABILITIES: StoreCapabilities = {
  arbitraryRefNamespaces: true,
  nonFastForward: true,
  refDeletion: true,
};

const READABLE: Readonly<Record<Capability, string>> = {
  arbitraryRefNamespaces: 'refs outside refs/heads/ and refs/tags/',
  nonFastForward: 'non-fast-forward ref updates',
  refDeletion: 'ref deletion',
};

/**
 * Refuses an operation the store cannot perform, naming what is missing.
 * A caller that asks and is refused has changed nothing: there is no
 * substitute effect, because a silent substitute is worse than a refusal.
 */
export function requireCapability(capabilities: StoreCapabilities, capability: Capability): void {
  if (!capabilities[capability]) {
    throw new HttpError(501, `this repository store does not support ${READABLE[capability]}`);
  }
}
