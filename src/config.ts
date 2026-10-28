/** The mount point every versioned route is served under when none is configured. */
export const DEFAULT_MOUNT_POINT = '/api/v1';

/** The largest request body the service accepts when no limit is configured. */
export const DEFAULT_MAX_BODY_BYTES = 1_048_576;

/** How the service introduces itself to another: GitHub's API requires it. */
export const USER_AGENT = 'af-hub/parley';
