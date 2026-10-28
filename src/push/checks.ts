import { HttpError } from '../http/errors.js';

import { patchBranchOf } from '../patch/list.js';
import type { WorkspaceRecord } from '../store/workspaces.js';

/**
 * The checks the branch push runs, in the order the reference fixes.
 *
 * The order is a requirement of its own: every refusal can be made to
 * pass with the checks in any order, and only a request where two
 * would both fail tells them apart. Getting it wrong gives the right
 * statuses for the wrong reasons.
 *
 * Each of this route's own refusals repeats its type at the start of
 * the message, because the CLI shows the message and nothing else.
 */
export function refusal(status: number, type: string, message: string): HttpError {
  return new HttpError(status, `${type}: ${message}`, type);
}

export interface PushBody {
  force: boolean;
}

/**
 * What the body asks for. No body, an empty object and unknown fields
 * are a push without force; anything that is not an object, and a
 * force that is not a boolean, is refused.
 */
export function readPushBody(raw: unknown): PushBody {
  if (raw === undefined || raw === null) {
    return { force: false };
  }
  if (typeof raw !== 'object' || Array.isArray(raw)) {
    return (() => {
      throw new HttpError(400, 'the request body must be a JSON object');
    })();
  }
  const force = (raw as Record<string, unknown>)['force'];
  if (force !== undefined && typeof force !== 'boolean') {
    throw new HttpError(400, 'force must be true or false');
  }
  return { force: force === true };
}

/**
 * The rules git itself applies to a branch name, in the order a person
 * would read them. The message names the rule broken, because a client
 * that is only told "invalid" cannot fix it.
 */
const NAME_RULES: Array<{ broken: (name: string) => boolean; rule: string }> = [
  { broken: (name) => name.length === 0, rule: 'a ref name must not be empty' },
  { broken: (name) => name.startsWith('-'), rule: "ref name must not start with '-'" },
  { broken: (name) => name.includes('..'), rule: "ref name must not contain '..'" },
  {
    // Measured by code point rather than by a pattern, because a
    // control character in a regular expression is a thing linters
    // rightly ask about.
    broken: (name) =>
      [...name].some((character) => {
        const code = character.codePointAt(0) ?? 0;
        return code < 0x20 || code === 0x7f;
      }),
    rule: 'ref name must not contain a control character',
  },
  {
    broken: (name) => /[ ~^:?*[\\]/.test(name),
    rule: 'ref name must not contain a space or any of ~ ^ : ? * [ \\',
  },
  { broken: (name) => name.endsWith('.lock'), rule: "ref name must not end with '.lock'" },
  { broken: (name) => name.includes('@{'), rule: "ref name must not contain '@{'" },
  {
    broken: (name) => name.startsWith('/') || name.endsWith('/') || name.includes('//'),
    rule: "ref name must not begin or end with '/' or contain '//'",
  },
  { broken: (name) => name.endsWith('.'), rule: "ref name must not end with '.'" },
];

export function branchNameRefusal(name: string): HttpError | undefined {
  const broken = NAME_RULES.find((rule) => rule.broken(name));
  return broken === undefined
    ? undefined
    : refusal(404, 'branch_not_found', `"${name}" is not a valid branch name: ${broken.rule}`);
}

/** A workspace that is not ready has nothing worth pushing. */
export function readinessRefusal(workspace: WorkspaceRecord): HttpError | undefined {
  return workspace.status === 'ready'
    ? undefined
    : new HttpError(400, 'workspace is not ready; push requires a ready workspace');
}

/**
 * The branches this route will not send, whatever else is true. They
 * are refused before the lock is taken, so a busy workspace gives the
 * same answer as a free one: which branches the hub owns is not a
 * matter of timing.
 */
export function unpushableRefusal(
  workspace: WorkspaceRecord,
  name: string,
  patches: readonly string[],
  force: boolean,
): HttpError | undefined {
  if (workspace.workspace_mode === 'carry_patch' && name === workspace.integration_branch) {
    return refusal(
      400,
      'integration_branch',
      'the integration branch reaches the fork through the rebuild_push_integration_branch setting, not this route',
    );
  }
  if (patchBranchOf(workspace, `refs/heads/${name}`, patches) !== undefined) {
    return refusal(
      400,
      'patch_branch',
      'a patch branch reaches the fork through the push_patches_to_origin setting, not this route',
    );
  }
  if (force && workspace.branch !== null && name === workspace.branch) {
    return refusal(
      400,
      'protected_branch',
      `"${name}" is this workspace's own branch and will not be force-pushed`,
    );
  }
  return undefined;
}

/**
 * The protections that need the repository, and so are applied once
 * the lock is held: a forced push of the branch HEAD follows where the
 * workspace names none, and a branch the repository does not have.
 */
export function inRepositoryRefusal(options: {
  workspace: WorkspaceRecord;
  name: string;
  force: boolean;
  head?: string;
  present: boolean;
}): HttpError | undefined {
  if (
    options.force &&
    (options.workspace.branch === null || options.workspace.branch.length === 0) &&
    options.head === options.name
  ) {
    return refusal(
      400,
      'protected_branch',
      `"${options.name}" is where this workspace's HEAD points and will not be force-pushed`,
    );
  }
  if (!options.present) {
    return refusal(
      404,
      'branch_not_found',
      `branch "${options.name}" does not exist in the workspace`,
    );
  }
  return undefined;
}
