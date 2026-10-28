import { HttpError } from '../http/errors.js';

/**
 * Where a forge route is about to send a workspace's credential.
 *
 * Eighteen routes ask this one question, and its answer decides which
 * host receives a token. It is worked out from the remote URL and one
 * setting, afresh every time: a rotated secret or a changed setting
 * applies to the next call, so nothing here is cached.
 */
export type ForgeName = 'github' | 'gitlab';

export interface Repository {
  host: string;
  /** The full namespace, so a GitLab subgroup path is kept whole. */
  owner: string;
  name: string;
}

export interface ForgeTarget {
  forge: ForgeName;
  apiUrl: string;
  repository: Repository;
}

const UNKNOWN = 'forge_unknown';
const INVALID = 'forge_api_url_invalid';

/** `git@host:owner/name`, which is no URL and has to be read as itself. */
const SCP = /^(?:(?<user>[^@/]+)@)?(?<host>[^:/]+):(?<path>.+)$/;

/**
 * The host, owner and name a remote URL names, or none where it names
 * no repository. The three shapes a person may have typed — https, ssh
 * and scp-style — are one repository, so they answer alike.
 */
export function repositoryFrom(remoteUrl: string): Repository | undefined {
  const given = remoteUrl.trim();
  if (given.length === 0) {
    return undefined;
  }

  const parsed = given.includes('://') ? urlParts(given) : SCP.exec(given)?.groups;
  if (parsed === undefined) {
    return undefined;
  }
  const host = String(parsed['host'] ?? '');
  const segments = String(parsed['path'] ?? '')
    .split('/')
    .filter((part) => part.length > 0);
  if (host.length === 0 || segments.length < 2) {
    return undefined;
  }

  const name = (segments.at(-1) as string).replace(/\.git$/, '');
  const owner = segments.slice(0, -1).join('/');
  return name.length === 0 || owner.length === 0 ? undefined : { host, owner, name };
}

function urlParts(given: string): Record<string, string> | undefined {
  try {
    // The userinfo goes here, with the rest of the URL: a credential
    // that leaked into a remote URL must not leave in an answer.
    const url = new URL(given);
    return { host: url.host, path: url.pathname };
  } catch {
    return undefined;
  }
}

/** The documented API URL of a host of each kind. */
export function defaultApiUrl(forge: ForgeName, host: string): string {
  if (forge === 'gitlab') {
    return `https://${host}/api/v4`;
  }
  return host === 'github.com' ? 'https://api.github.com' : `https://${host}/api/v3`;
}

/** Which forge a name belongs to, where its spelling says so. */
export function named(value: string): ForgeName | undefined {
  const lowered = value.toLowerCase();
  if (lowered.includes('github')) {
    return 'github';
  }
  return lowered.includes('gitlab') ? 'gitlab' : undefined;
}

/**
 * An API URL setting, checked as the reference requires. The value is
 * never in the refusal: it is a setting an operator typed, and the
 * rule is what they need told, not what they typed. The description
 * route checks a setting without resolving a forge, so this is
 * exported for it.
 */
export function checkedSetting(setting: string, settingName: string): URL {
  let url: URL;
  try {
    url = new URL(setting);
  } catch {
    throw new HttpError(422, `${settingName} must be an absolute https URL with a host`, INVALID);
  }
  if (url.protocol !== 'https:' || url.host.length === 0) {
    throw new HttpError(422, `${settingName} must be an absolute https URL with a host`, INVALID);
  }
  if (url.username.length > 0 || url.password.length > 0) {
    throw new HttpError(422, `${settingName} must carry no userinfo`, INVALID);
  }
  if (url.search.length > 0 || url.hash.length > 0) {
    throw new HttpError(422, `${settingName} must carry no query or fragment`, INVALID);
  }
  return url;
}

export interface TargetRequest {
  remoteUrl: string;
  /** The workspace's API URL setting for this target; '' where unset. */
  setting: string;
  /** Which setting it is, for the message of a refusal. */
  settingName?: string;
  /** The deployment's own API URLs, which reads may use and writes may not. */
  environment?: { github?: string; gitlab?: string };
}

/**
 * The forge, its API URL and its repository, or the refusal the
 * reference gives. Nothing is contacted: this is arithmetic over a URL
 * and a setting.
 */
export function forgeFor(request: TargetRequest): ForgeTarget {
  const settingName = request.settingName ?? 'forge_api_url';
  const setting = request.setting.trim().replace(/\/+$/, '');
  const repository = repositoryFrom(request.remoteUrl);

  const fromSetting = setting.length === 0 ? undefined : named(setting);
  const fromHost = repository === undefined ? undefined : named(repository.host);
  // A setting is the operator saying which forge this is, so it wins
  // over a host whose name suggests the other.
  const forge = fromSetting ?? fromHost;

  if (setting.length > 0) {
    checkedSetting(setting, settingName);
  }
  if (forge === undefined || repository === undefined) {
    throw new HttpError(
      422,
      repository === undefined
        ? 'the remote URL names no repository'
        : 'neither the host nor the API URL setting says which forge this is',
      UNKNOWN,
    );
  }

  const environment = request.environment?.[forge]?.trim().replace(/\/+$/, '') ?? '';
  const chosen =
    setting.length > 0
      ? setting
      : environment.length > 0
        ? environment
        : defaultApiUrl(forge, repository.host);

  // GitLab's API lives under /api/v4, and an operator who gave the
  // host alone meant the API.
  const apiUrl = forge === 'gitlab' && !/\/api\/v\d+$/.test(chosen) ? `${chosen}/api/v4` : chosen;

  return { forge, apiUrl, repository };
}
