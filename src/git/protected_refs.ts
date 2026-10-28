import type { RefChange } from '../platform/repostore.js';
import type { Decider, RefDecision } from './push.js';

/**
 * The namespace the hub keeps for itself.
 *
 * A replaced patch tip lives here, and it is often the only copy of
 * work the hub set aside; a client that could overwrite it could
 * destroy that work. No push touches it, on any workspace, with any
 * credential — an admin token included, because the protection is
 * about what the hub owns rather than about who is asking.
 */
export const PROTECTED_NAMESPACE = 'refs/hub/replaced/';

export const PROTECTED_MESSAGE =
  'refs/hub/replaced/ is maintained by the hub and cannot be pushed to';

/** Whether a ref belongs to the namespace the hub maintains. */
export function isProtected(ref: string): boolean {
  return ref.startsWith(PROTECTED_NAMESPACE);
}

/**
 * Refuses every create, update and delete under the namespace, and
 * says nothing about any other ref. The rest of the push is processed
 * as usual, which is why this is a decision per ref rather than a
 * refusal of the request.
 */
export function protectedRefs(): Decider {
  return (command: RefChange): Promise<RefDecision> =>
    Promise.resolve(
      isProtected(command.name) ? { accept: false, reason: PROTECTED_MESSAGE } : { accept: true },
    );
}

/**
 * Reading the namespace needs nothing of its own. The advertisement
 * lists what the store holds, so a store that will not keep refs
 * outside branches and tags lists none of them and a client cannot ask
 * for what it was never offered. The refusal of a *push* to them
 * depends on no store and holds either way, which is why the two are
 * not decided together.
 */
