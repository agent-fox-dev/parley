import type { RelationalStore } from '../platform/index.js';
import { workspaceScope } from '../platform/secrets.js';
import { isMember } from '../store/identity.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import {
  type EntryRecord,
  listEntries,
  orgScope,
  plainCipher,
  userScope,
} from '../entries/store.js';
import { foldKey } from '../entries/validate.js';

export type Origin = 'user' | 'org' | 'workspace';

export interface ResolvedVariable {
  key: string;
  value: string;
  origin: Origin;
  created_at: string;
  updated_at: string;
}

function resolved(entry: EntryRecord, origin: Origin): ResolvedVariable {
  return {
    key: entry.key,
    value: entry.value,
    origin,
    created_at: entry.created_at,
    updated_at: entry.updated_at,
  };
}

/**
 * The variables a workspace sees: the owner's, then their organization's,
 * then the workspace's, each tier overriding the one outside it.
 *
 * The user tier is the **workspace owner's**, never the caller's, so every
 * caller with read access gets the same answer — which is what makes the
 * view a property of the workspace rather than of who asked.
 */
export async function resolveVariables(
  store: RelationalStore,
  workspace: WorkspaceRecord,
): Promise<ResolvedVariable[]> {
  const tiers: Array<[Origin, EntryRecord[]]> = [
    ['user', await listEntries(store, userScope(workspace.owner_id), 'variable', plainCipher)],
  ];

  // An organization the owner does not belong to is not theirs to inherit
  // from, so it contributes nothing.
  if (workspace.org_id !== null && (await isMember(store, workspace.org_id, workspace.owner_id))) {
    tiers.push([
      'org',
      await listEntries(store, orgScope(workspace.org_id), 'variable', plainCipher),
    ]);
  }

  tiers.push([
    'workspace',
    await listEntries(store, workspaceScope(workspace.slug), 'variable', plainCipher),
  ]);

  const merged = new Map<string, ResolvedVariable>();
  for (const [origin, entries] of tiers) {
    for (const entry of entries) {
      merged.set(foldKey(entry.key), resolved(entry, origin));
    }
  }

  return [...merged.values()].toSorted((left, right) =>
    foldKey(left.key).localeCompare(foldKey(right.key)),
  );
}
