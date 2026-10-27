import { HttpError } from '../http/errors.js';

/** A create request as the route works with it, every default resolved. */
export interface CreateRequest {
  slug: string;
  git_url: string;
  branch: string | null;
  org_id: string | undefined;
  display_name: string;
  description: string;
  workspace_mode: 'standard' | 'carry_patch';
  upstream_url: string | null;
  integration_branch: string | null;
  settings: Record<string, unknown>;
  /** The secrets to store for the workspace, already named as they are stored. */
  credentials: Record<string, string>;
}

const MAX_DISPLAY_NAME = 128;
const MAX_DESCRIPTION = 1024;

/**
 * Globally unique, 3 to 64 characters, a lowercase letter first, a lowercase
 * letter or digit last, lowercase letters, digits and hyphens between, and
 * never two hyphens in a row. Uniqueness is the store's to decide.
 */
export function isValidSlug(slug: string): boolean {
  return /^[a-z][a-z0-9-]{1,62}[a-z0-9]$/.test(slug) && !slug.includes('--');
}

/** An https or ssh git URL, the two forms the reference accepts. */
export function isGitUrl(url: string): boolean {
  return (
    /^https:\/\/[^\s/@]+\/\S+$/.test(url) ||
    /^ssh:\/\/\S+@?[^\s/]+\/\S+$/.test(url) ||
    /^[^\s:@]+@[^\s:]+:\S+$/.test(url)
  );
}

function optionalText(
  body: Record<string, unknown>,
  field: string,
  limit: number,
  fallback: string,
): string {
  const value = body[field];
  if (value === undefined || value === null) {
    return fallback;
  }
  if (typeof value !== 'string' || value.length > limit) {
    throw new HttpError(400, `${field} must be a string of at most ${limit} characters`);
  }
  return value;
}

/** The three fields that decide what kind of workspace this is. */
function checkedMode(
  body: Record<string, unknown>,
): Pick<CreateRequest, 'workspace_mode' | 'upstream_url' | 'integration_branch'> {
  const mode = body['workspace_mode'] ?? 'standard';
  if (mode !== 'standard' && mode !== 'carry_patch') {
    throw new HttpError(400, 'workspace_mode must be standard or carry_patch');
  }
  const upstream = body['upstream_url'];
  const branch = body['integration_branch'];

  if (mode === 'standard') {
    if (upstream !== undefined && upstream !== null) {
      throw new HttpError(400, 'upstream_url is only for a carry_patch workspace');
    }
    if (branch !== undefined && branch !== null) {
      throw new HttpError(400, 'integration_branch is only for a carry_patch workspace');
    }
    return { workspace_mode: 'standard', upstream_url: null, integration_branch: null };
  }

  if (typeof upstream !== 'string' || !isGitUrl(upstream)) {
    throw new HttpError(400, 'upstream_url must be a git URL for a carry_patch workspace');
  }
  if (branch !== undefined && branch !== null && typeof branch !== 'string') {
    throw new HttpError(400, 'integration_branch must be a string');
  }
  return {
    workspace_mode: 'carry_patch',
    upstream_url: upstream,
    integration_branch: typeof branch === 'string' && branch.length > 0 ? branch : 'deploy',
  };
}

/**
 * The credentials, under the names they are stored as. A token and the basic
 * pair are alternatives, each half of the pair needs the other, and neither
 * travels over a URL that is not https.
 */
function checkedCredentials(body: Record<string, unknown>, gitUrl: string): Record<string, string> {
  const named = (field: string): string | undefined => {
    const value = body[field];
    if (value === undefined || value === null) {
      return undefined;
    }
    if (typeof value !== 'string' || value.length === 0) {
      throw new HttpError(400, `${field} must be a non-empty string`);
    }
    return value;
  };

  const pat = named('git_pat');
  const username = named('git_username');
  const password = named('git_password');

  if (pat !== undefined && (username !== undefined || password !== undefined)) {
    throw new HttpError(400, 'git_pat cannot be given with git_username or git_password');
  }
  if ((username === undefined) !== (password === undefined)) {
    throw new HttpError(400, 'git_username and git_password must be given together');
  }

  const credentials: Record<string, string> = {};
  if (pat !== undefined) {
    credentials['GIT_PAT'] = pat;
  } else if (username !== undefined && password !== undefined) {
    credentials['GIT_USERNAME'] = username;
    credentials['GIT_PASSWORD'] = password;
  }

  if (Object.keys(credentials).length > 0 && !gitUrl.startsWith('https://')) {
    throw new HttpError(400, 'credentials need an https git_url');
  }
  return credentials;
}

/** The create body, validated and defaulted, or the 400 that names the field. */
export function checkedCreate(body: Record<string, unknown>): CreateRequest {
  const slug = body['slug'];
  if (typeof slug !== 'string' || !isValidSlug(slug)) {
    throw new HttpError(
      400,
      'slug must be 3 to 64 lowercase letters, digits and single hyphens, starting with a letter',
    );
  }
  const gitUrl = body['git_url'];
  if (typeof gitUrl !== 'string' || !isGitUrl(gitUrl)) {
    throw new HttpError(400, 'git_url must be an https or ssh git URL');
  }

  const branch = body['branch'];
  if (branch !== undefined && branch !== null && typeof branch !== 'string') {
    throw new HttpError(400, 'branch must be a string');
  }
  const orgId = body['org_id'];
  if (orgId !== undefined && orgId !== null && typeof orgId !== 'string') {
    throw new HttpError(400, 'org_id must be a string');
  }
  const settings = body['settings'];
  if (settings !== undefined && settings !== null) {
    if (typeof settings !== 'object' || Array.isArray(settings)) {
      throw new HttpError(400, 'settings must be an object');
    }
  }

  return {
    slug,
    git_url: gitUrl,
    branch: typeof branch === 'string' && branch.length > 0 ? branch : null,
    org_id: typeof orgId === 'string' && orgId.length > 0 ? orgId : undefined,
    display_name: optionalText(body, 'display_name', MAX_DISPLAY_NAME, slug),
    description: optionalText(body, 'description', MAX_DESCRIPTION, ''),
    ...checkedMode(body),
    settings: (settings ?? {}) as Record<string, unknown>,
    credentials: checkedCredentials(body, gitUrl),
  };
}
