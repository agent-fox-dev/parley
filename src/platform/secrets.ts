/**
 * Where the service keeps values nobody may read back out of the store: the
 * repository credentials, and later the user, organization and workspace
 * secrets. A value is sealed before it reaches whatever holds it.
 */
export interface SecretStore {
  put(scope: string, key: string, value: string): Promise<void>;
  get(scope: string, key: string): Promise<string | undefined>;
  delete(scope: string, key: string): Promise<void>;
  /** The names held in a scope, never the values. */
  names(scope: string): Promise<string[]>;
}

/** The scope a workspace's own secrets live in. */
export function workspaceScope(slug: string): string {
  return `workspace:${slug}`;
}
