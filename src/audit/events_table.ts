/**
 * The hub's own events, transcribed from the two tables of the API
 * reference's Hub Audit Events section.
 *
 * It is one declaration rather than a shape remembered at each call
 * site, because the reference's promise is about the keys: a consumer
 * filters on them, so an event with an extra key or a missing one is
 * wrong in a way no caller notices until their filter quietly returns
 * nothing. Recording goes through this table, which means a call site
 * cannot invent a key and cannot forget one without the test seeing it.
 *
 * Every type the reference names is here, including those whose
 * operation this service does not perform yet: the packages that add
 * them then only call the recorder.
 */
export interface EventShape {
  /** The dot-separated event type, as the reference spells it. */
  type: string;
  /** What the event is about. */
  resource: string;
  /** The verb the reference gives the event. */
  action: string;
  /** Exactly the metadata keys the reference lists, in its order. */
  metadata: readonly string[];
  /** The actor where the reference names one other than the caller. */
  actor?: 'system' | 'lease' | 'forge';
}

/** The metadata every forge write event carries, before its own keys. */
const FORGE_WRITE = ['target', 'ok', 'error_type', 'number', 'url'] as const;

/** The metadata every event relayed from a forge delivery carries. */
const DELIVERY = ['forge', 'target', 'delivery_id', 'actor'] as const;

/** What a delivery event about an issue carries beyond the common keys. */
const ISSUE_DELIVERY = [...DELIVERY, 'number', 'title', 'url', 'labels'] as const;

/** What a delivery event about a pull or merge request carries. */
const PULL_DELIVERY = [...DELIVERY, 'number', 'url', 'head', 'base', 'merged'] as const;

export const HUB_EVENTS: readonly EventShape[] = [
  // The workspace's own life.
  {
    type: 'hub.workspace.create',
    resource: 'workspace',
    action: 'create',
    metadata: ['git_url', 'branch'],
  },
  { type: 'hub.workspace.update', resource: 'workspace', action: 'update', metadata: ['fields'] },
  {
    type: 'hub.workspace.archive',
    resource: 'workspace',
    action: 'archive',
    metadata: ['head_sha'],
  },
  {
    type: 'hub.workspace.reactivate',
    resource: 'workspace',
    action: 'reactivate',
    metadata: [],
  },
  {
    type: 'hub.workspace.delete',
    resource: 'workspace',
    action: 'delete',
    metadata: ['revoked_tokens'],
  },
  { type: 'hub.workspace.sync', resource: 'workspace', action: 'sync', metadata: ['result'] },
  {
    type: 'hub.git.push',
    resource: 'workspace',
    action: 'push',
    metadata: ['head_sha', 'refs_updated'],
  },
  {
    type: 'hub.session.force_closed',
    resource: 'session',
    action: 'terminate',
    metadata: ['session_id', 'reason'],
    actor: 'system',
  },

  // Secrets and variables, by scope and key and never by value.
  { type: 'hub.secret.create', resource: 'secret', action: 'create', metadata: ['scope', 'key'] },
  { type: 'hub.secret.update', resource: 'secret', action: 'update', metadata: ['scope', 'key'] },
  { type: 'hub.secret.delete', resource: 'secret', action: 'delete', metadata: ['scope', 'key'] },
  {
    type: 'hub.variable.create',
    resource: 'variable',
    action: 'create',
    metadata: ['scope', 'key'],
  },
  {
    type: 'hub.variable.update',
    resource: 'variable',
    action: 'update',
    metadata: ['scope', 'key'],
  },
  {
    type: 'hub.variable.delete',
    resource: 'variable',
    action: 'delete',
    metadata: ['scope', 'key'],
  },

  // Tokens bound to one workspace.
  {
    type: 'hub.token.bind',
    resource: 'token',
    action: 'bind',
    metadata: ['token_id', 'workspace', 'permissions', 'expires_at'],
  },
  {
    type: 'hub.token.bound_refused',
    resource: 'token',
    action: 'refuse',
    metadata: [
      'token_id',
      'bound_workspace',
      'requested_workspace',
      'method',
      'route',
      'status',
      'surface',
      'suppressed',
    ],
  },

  // The patch stack.
  {
    type: 'hub.patch.create',
    resource: 'patch',
    action: 'create',
    metadata: ['branch_name', 'position', 'branch_resolution'],
  },
  { type: 'hub.patch.delete', resource: 'patch', action: 'delete', metadata: ['branch_name'] },
  {
    type: 'hub.patch.sync',
    resource: 'patch',
    action: 'sync',
    metadata: [
      'origin_fetched',
      'created',
      'fast_forwarded',
      'replaced',
      'diverged',
      'missing_on_origin',
    ],
  },
  {
    type: 'hub.patch.replace',
    resource: 'patch',
    action: 'replace',
    metadata: ['branch_name', 'replaced_sha', 'origin_sha', 'trigger'],
  },
  {
    type: 'hub.patch.mirror_failed',
    resource: 'patch',
    action: 'mirror_failed',
    metadata: ['branch_name', 'error'],
  },
  {
    type: 'hub.patch.reset',
    resource: 'patch',
    action: 'reset',
    metadata: ['branch_name', 'action', 'local_sha', 'origin_sha', 'replaced_sha'],
  },

  // Rebuilds of the integration branch.
  {
    type: 'hub.rebuild.enqueue',
    resource: 'patch',
    action: 'enqueue',
    metadata: ['job_id', 'patch_count'],
  },
  {
    type: 'hub.rebuild.complete',
    resource: 'patch',
    action: 'complete',
    metadata: ['patches_applied'],
  },
  { type: 'hub.rebuild.fail', resource: 'patch', action: 'fail', metadata: ['reason'] },
  {
    type: 'hub.rebuild.push_failed',
    resource: 'patch',
    action: 'push_failed',
    metadata: ['integration_branch', 'reason'],
  },
  {
    type: 'hub.rebuild.followup',
    resource: 'rebuild',
    action: 'followup',
    metadata: ['submitted_by', 'workspace'],
  },

  // Merges.
  {
    type: 'hub.merge.enqueue',
    resource: 'merge',
    action: 'enqueue',
    metadata: ['target_branch', 'source_ref', 'job_id'],
  },
  {
    type: 'hub.merge.complete',
    resource: 'merge',
    action: 'complete',
    metadata: ['base_sha', 'merged_sha'],
  },
  { type: 'hub.merge.fail', resource: 'merge', action: 'fail', metadata: ['reason'] },

  // A push to a branch of the fork.
  {
    type: 'hub.branch.push',
    resource: 'branch',
    action: 'push',
    metadata: ['branch', 'sha', 'force', 'ok', 'remote', 'remote_url', 'result', 'error_type'],
  },

  // Sandboxes, which this deployment does not serve yet.
  {
    type: 'hub.sandbox.lease',
    resource: 'sandbox',
    action: 'lease',
    metadata: ['lease_id', 'profile', 'purpose', 'ttl', 'secret_names', 'optional_secret_names'],
  },
  {
    type: 'hub.sandbox.create',
    resource: 'sandbox',
    action: 'create',
    metadata: ['lease_id', 'sandbox_id', 'image', 'network'],
    actor: 'lease',
  },
  {
    type: 'hub.sandbox.failed',
    resource: 'sandbox',
    action: 'fail',
    metadata: ['lease_id', 'error'],
    actor: 'lease',
  },
  {
    type: 'hub.sandbox.destroy',
    resource: 'sandbox',
    action: 'destroy',
    metadata: ['lease_id', 'sandbox_id', 'reason'],
  },
  {
    type: 'hub.sandbox.expire',
    resource: 'sandbox',
    action: 'expire',
    metadata: ['lease_id', 'state', 'sandbox_id'],
    actor: 'system',
  },

  // Writes this service makes to a forge, which package 13 performs.
  {
    type: 'hub.forge.issue.create',
    resource: 'issue',
    action: 'create',
    metadata: [...FORGE_WRITE],
  },
  {
    type: 'hub.forge.issue.update',
    resource: 'issue',
    action: 'update',
    metadata: [...FORGE_WRITE, 'fields'],
  },
  {
    type: 'hub.forge.issue.close',
    resource: 'issue',
    action: 'close',
    metadata: [...FORGE_WRITE, 'comment_url'],
  },
  {
    type: 'hub.forge.comment.create',
    resource: 'comment',
    action: 'create',
    metadata: [...FORGE_WRITE],
  },
  {
    type: 'hub.forge.label.add',
    resource: 'label',
    action: 'add',
    metadata: [...FORGE_WRITE, 'labels'],
  },
  {
    type: 'hub.forge.label.remove',
    resource: 'label',
    action: 'remove',
    metadata: [...FORGE_WRITE, 'label'],
  },
  {
    type: 'hub.forge.label.create',
    resource: 'label',
    action: 'create',
    metadata: [...FORGE_WRITE, 'label', 'created'],
  },
  {
    type: 'hub.forge.pull_request.create',
    resource: 'pull_request',
    action: 'create',
    metadata: [...FORGE_WRITE, 'head', 'base', 'draft'],
  },
  {
    type: 'hub.forge.pull_request.comment',
    resource: 'pull_request',
    action: 'comment',
    metadata: [...FORGE_WRITE],
  },

  // What a forge reported, which package 14 relays.
  {
    type: 'hub.forge.issue.opened',
    resource: 'issue',
    action: 'opened',
    metadata: [...ISSUE_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.issue.edited',
    resource: 'issue',
    action: 'edited',
    metadata: [...ISSUE_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.issue.closed',
    resource: 'issue',
    action: 'closed',
    metadata: [...ISSUE_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.issue.reopened',
    resource: 'issue',
    action: 'reopened',
    metadata: [...ISSUE_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.issue.labeled',
    resource: 'issue',
    action: 'labeled',
    metadata: [...DELIVERY, 'number', 'label', 'labels', 'url'],
    actor: 'forge',
  },
  {
    type: 'hub.forge.issue.unlabeled',
    resource: 'issue',
    action: 'unlabeled',
    metadata: [...DELIVERY, 'number', 'label', 'labels', 'url'],
    actor: 'forge',
  },
  {
    type: 'hub.forge.comment.created',
    resource: 'comment',
    action: 'created',
    metadata: [...DELIVERY, 'number', 'comment_url', 'pull_request'],
    actor: 'forge',
  },
  {
    type: 'hub.forge.pull_request.opened',
    resource: 'pull_request',
    action: 'opened',
    metadata: [...PULL_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.pull_request.closed',
    resource: 'pull_request',
    action: 'closed',
    metadata: [...PULL_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.pull_request.merged',
    resource: 'pull_request',
    action: 'merged',
    metadata: [...PULL_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.pull_request.reopened',
    resource: 'pull_request',
    action: 'reopened',
    metadata: [...PULL_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.pull_request.synchronize',
    resource: 'pull_request',
    action: 'synchronize',
    metadata: [...PULL_DELIVERY],
    actor: 'forge',
  },
  {
    type: 'hub.forge.push',
    resource: 'repository',
    action: 'push',
    metadata: [...DELIVERY, 'ref', 'before', 'after'],
    actor: 'forge',
  },
  {
    type: 'hub.forge.webhook.set',
    resource: 'webhook',
    action: 'set',
    metadata: ['generated'],
  },
  {
    type: 'hub.forge.webhook.unset',
    resource: 'webhook',
    action: 'unset',
    metadata: ['existed'],
  },
];

const BY_TYPE = new Map(HUB_EVENTS.map((shape) => [shape.type, shape]));

/** The shape of one event type, or undefined for a type nobody declared. */
export function shapeOf(type: string): EventShape | undefined {
  return BY_TYPE.get(type);
}
