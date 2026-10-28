import type { LogSink } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import type { RefChange } from '../platform/repostore.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { authorizationFor } from '../repo/copy_in.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { patchBranchOf, registeredPatches } from '../patches/registry.js';
import { effectiveSettings } from '../workspace/settings.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import { ZERO_OID, pushPack } from './transport.js';
import type { Decider, RefDecision, RefOutcome } from './push.js';

/**
 * What the hub does with a push to a patch branch.
 *
 * The API reference describes this as a pre-receive hook, and the store
 * has none: its only push signal fires afterwards. So the decision is
 * made here, in front of the store, one ref at a time — which is also
 * what lets a push carrying a refused patch branch and an ordinary
 * branch apply the one and refuse the other.
 */
export interface PushControlDeps {
  store: RelationalStore;
  workspace: WorkspaceRecord;
  /** The pack the client sent, which is what a forward carries. */
  pack: Uint8Array;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
}

const SYNCED = (gitUrl: string) => `branch is synced from origin; push to ${gitUrl} instead`;
const SYNCED_DELETE = 'branch is synced from origin; delete it on the fork instead';
const NO_CREDENTIAL = 'could not resolve the credentials for origin';

/** A remote's address with any username and password taken out of it. */
export function withoutUserinfo(gitUrl: string): string {
  try {
    const parsed = new URL(gitUrl);
    parsed.username = '';
    parsed.password = '';
    return parsed.toString().replace(/\/$/, '');
  } catch {
    // Not a URL the parser knows; an scp-style address carries its
    // userinfo before an @, and nothing after it is secret.
    return gitUrl.replace(/^[^@/]*@/, '');
  }
}

/** The fork, as something to push to, or why it cannot be reached. */
async function fork(
  deps: PushControlDeps,
): Promise<{ url: string; headers?: Record<string, string>; fetch?: typeof fetch } | undefined> {
  if (deps.secrets === undefined) {
    return undefined;
  }
  const credential = await resolveGitCredential(
    deps.secrets,
    workspaceScope(deps.workspace.slug),
    'origin',
    deps.log,
  );
  const authorization = authorizationFor(credential);
  if (authorization === undefined) {
    return undefined;
  }
  return {
    url: deps.workspace.git_url.replace(/\/$/, ''),
    headers: { authorization },
    ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
  };
}

/** Sends one ref to the fork and says what the fork made of it. */
async function sendToFork(
  deps: PushControlDeps,
  command: RefChange,
  forced: boolean,
): Promise<{ ok: true } | { ok: false; reason: string }> {
  const remote = await fork(deps);
  if (remote === undefined) {
    return { ok: false, reason: NO_CREDENTIAL };
  }

  try {
    const report = await pushPack(remote, {
      commands: [
        {
          name: command.name,
          // A forced push offers no previous value, so the fork takes
          // the new one whatever it held; a forward offers the value
          // the client had, so the fork refuses what is not a
          // fast-forward.
          oldOid: forced ? ZERO_OID : (command.oldOid ?? ZERO_OID),
          newOid: command.newOid ?? ZERO_OID,
        },
      ],
      pack: deps.pack,
    });
    const refused = report.refs.find((ref) => !ref.ok);
    if (!report.unpacked) {
      return { ok: false, reason: 'origin rejected push: the remote could not unpack it' };
    }
    if (refused !== undefined) {
      return { ok: false, reason: `origin rejected push: ${refused.reason ?? 'refused'}` };
    }
    return { ok: true };
  } catch (err) {
    const why = err instanceof Error ? err.message : 'the remote could not be reached';
    return { ok: false, reason: `origin rejected push: ${why}` };
  }
}

/**
 * The decision for each ref of a push, from the workspace's own
 * settings. Everything that is not a registered patch branch of a
 * carry-patch workspace is accepted under the ordinary access rules.
 */
export function pushControl(deps: PushControlDeps): Decider {
  const settings = effectiveSettings(deps.workspace.settings);
  const fromOrigin = settings['patch_branch_source'] === 'origin';
  const toOrigin = settings['push_patches_to_origin'] === true;

  return async (command: RefChange): Promise<RefDecision> => {
    const patches = await registeredPatches(deps.store, deps.workspace.slug);
    if (patchBranchOf(deps.workspace, command.name, patches) === undefined) {
      return { accept: true };
    }

    if (!fromOrigin) {
      // The hub is where these branches live. Mirroring, where it is
      // asked for, happens after the store has taken the push.
      return { accept: true };
    }

    // A branch that comes from the fork is deleted on the fork.
    if (command.newOid === null) {
      return { accept: false, reason: SYNCED_DELETE };
    }
    if (!toOrigin) {
      return { accept: false, reason: SYNCED(withoutUserinfo(deps.workspace.git_url)) };
    }

    // Forwarded first, and taken here only if the fork took it.
    const sent = await sendToFork(deps, command, false);
    return sent.ok ? { accept: true } : { accept: false, reason: sent.reason };
  };
}

/**
 * What happens once the store has taken a push: a patch branch the hub
 * owns is mirrored to the fork, best effort. A mirror that fails fails
 * neither the push nor anything after it, because the hub's copy is
 * the one that counts.
 */
export function mirrorAfterPush(
  deps: PushControlDeps,
  commands: readonly RefChange[],
): (outcomes: readonly RefOutcome[]) => Promise<void> {
  const settings = effectiveSettings(deps.workspace.settings);
  const mirroring =
    settings['patch_branch_source'] === 'hub' && settings['push_patches_to_origin'] === true;

  return async (outcomes: readonly RefOutcome[]) => {
    if (!mirroring) {
      return;
    }
    const patches = await registeredPatches(deps.store, deps.workspace.slug);
    for (const outcome of outcomes.filter((one) => one.ok)) {
      const command = commands.find((one) => one.name === outcome.name);
      if (
        command === undefined ||
        patchBranchOf(deps.workspace, command.name, patches) === undefined
      ) {
        continue;
      }
      const sent = await sendToFork(deps, command, true);
      if (!sent.ok) {
        deps.log?.info?.('a patch branch was not mirrored to origin', {
          workspace: deps.workspace.slug,
          ref: command.name,
          reason: sent.reason,
        });
      }
    }
  };
}
