import type { RelationalStore } from '../platform/index.js';
import { type CredentialRecord, toCredential } from './identity.js';

/** The workspace a token is bound to, or undefined when it is unbound. */
export async function bindingOf(
  store: RelationalStore,
  tokenId: string,
): Promise<string | undefined> {
  const rows = await store.all<{ workspace_slug: string }>(
    'SELECT workspace_slug FROM token_bindings WHERE token_id = ?',
    [tokenId],
  );
  return rows[0]?.workspace_slug;
}

export async function bindToken(
  store: RelationalStore,
  tokenId: string,
  slug: string,
  at: string,
): Promise<void> {
  await store.run(
    'INSERT INTO token_bindings (token_id, workspace_slug, created_at) VALUES (?, ?, ?)',
    [tokenId, slug, at],
  );
}

/**
 * The tokens bound to a workspace, revoked and expired ones included,
 * ordered as the reference asks. A binding whose token is gone is left
 * out, because the join finds nothing to show for it.
 */
export async function boundTokensOf(
  store: RelationalStore,
  slug: string,
): Promise<CredentialRecord[]> {
  const rows = await store.all(
    `SELECT credentials.* FROM token_bindings
     JOIN credentials ON credentials.id = token_bindings.token_id
     WHERE token_bindings.workspace_slug = ?
     ORDER BY credentials.created_at, credentials.id`,
    [slug],
  );
  return rows.map(toCredential);
}

/**
 * Deleting a workspace takes its bound tokens with it: every live one is
 * revoked and every binding removed, in one transaction, so a failure
 * leaves the workspace and its tokens both as they were.
 */
export function retireBindingsOf(slug: string, at: string) {
  return [
    {
      sql: `UPDATE credentials SET revoked_at = ?
            WHERE revoked_at IS NULL
              AND id IN (SELECT token_id FROM token_bindings WHERE workspace_slug = ?)`,
      params: [at, slug],
    },
    { sql: 'DELETE FROM token_bindings WHERE workspace_slug = ?', params: [slug] },
  ];
}

/**
 * Retires what deletions and revocations left behind: the live tokens of
 * bindings whose workspace is gone, and bindings whose token is gone. A
 * binding of a dead token of a workspace that still exists is kept, so the
 * workspace's token list still shows it.
 */
export async function sweepBindings(store: RelationalStore, at: string): Promise<void> {
  await store.batch([
    {
      sql: `UPDATE credentials SET revoked_at = ?
            WHERE revoked_at IS NULL AND id IN (
              SELECT token_id FROM token_bindings
              WHERE workspace_slug NOT IN (SELECT slug FROM workspaces)
            )`,
      params: [at],
    },
    {
      sql: 'DELETE FROM token_bindings WHERE workspace_slug NOT IN (SELECT slug FROM workspaces)',
    },
    {
      sql: 'DELETE FROM token_bindings WHERE token_id NOT IN (SELECT id FROM credentials)',
    },
  ]);
}
