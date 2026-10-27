import { HttpError } from '../http/errors.js';
import type { ConfigSource } from '../platform/index.js';

/** Who the provider says the person signing in is. */
export interface ProviderIdentity {
  provider: string;
  provider_id: string;
  username: string;
  email: string;
  full_name: string;
}

export interface OAuthProvider {
  name: string;
  /** Where a client sends the person to authorise, client id included. */
  authorizeUrl: string;
  /** The redirect URIs this provider accepts; empty allows any. */
  redirectAllowlist: readonly string[];
  /**
   * The identity behind an authorization code. Throws a 401 when the code
   * is one the provider does not accept.
   */
  exchange(code: string, redirectUri?: string): Promise<ProviderIdentity>;
}

export interface ProviderDirectory {
  list(): readonly OAuthProvider[];
  find(name: string): OAuthProvider | undefined;
}

export function providerDirectory(providers: readonly OAuthProvider[]): ProviderDirectory {
  return {
    list: () => providers,
    find: (name) => providers.find((provider) => provider.name === name),
  };
}

interface GithubSettings {
  clientId: string;
  clientSecret: string;
  redirectAllowlist: readonly string[];
  tokenUrl: string;
  profileUrl: string;
  authorizeBase: string;
}

const GITHUB_DEFAULTS = {
  authorizeBase: 'https://github.com/login/oauth/authorize',
  tokenUrl: 'https://github.com/login/oauth/access_token',
  profileUrl: 'https://api.github.com/user',
};

function asText(value: unknown): string {
  return typeof value === 'string' ? value : typeof value === 'number' ? String(value) : '';
}

/** GitHub's OAuth flow: a code for an access token, the token for a profile. */
export function githubProvider(settings: GithubSettings): OAuthProvider {
  return {
    name: 'github',
    authorizeUrl: `${settings.authorizeBase}?client_id=${encodeURIComponent(settings.clientId)}`,
    redirectAllowlist: settings.redirectAllowlist,

    async exchange(code: string, redirectUri?: string): Promise<ProviderIdentity> {
      const exchanged = await fetch(settings.tokenUrl, {
        method: 'POST',
        headers: { accept: 'application/json', 'content-type': 'application/json' },
        body: JSON.stringify({
          client_id: settings.clientId,
          client_secret: settings.clientSecret,
          code,
          ...(redirectUri === undefined ? {} : { redirect_uri: redirectUri }),
        }),
      });

      const granted = (await exchanged.json().catch(() => ({}))) as { access_token?: unknown };
      if (!exchanged.ok || typeof granted.access_token !== 'string') {
        throw new HttpError(401, 'the authorization code is invalid or expired');
      }

      const profiled = await fetch(settings.profileUrl, {
        headers: { accept: 'application/json', authorization: `Bearer ${granted.access_token}` },
      });
      if (!profiled.ok) {
        throw new HttpError(401, 'the authorization code is invalid or expired');
      }

      const profile = (await profiled.json()) as Record<string, unknown>;
      const username = asText(profile['login']);
      return {
        provider: 'github',
        provider_id: asText(profile['id']),
        username,
        email: asText(profile['email']),
        full_name: asText(profile['name']) || username,
      };
    },
  };
}

/**
 * The providers a deployment configured. A deployment that configured none
 * serves an empty list rather than failing: sign-in is unavailable, every
 * other route still works.
 */
export function configuredProviders(config: ConfigSource): ProviderDirectory {
  const clientId = config.get('PARLEY_OAUTH_GITHUB_CLIENT_ID');
  const clientSecret = config.get('PARLEY_OAUTH_GITHUB_CLIENT_SECRET');
  if (clientId === undefined || clientSecret === undefined) {
    return providerDirectory([]);
  }

  const allowlist = config.get('PARLEY_OAUTH_GITHUB_REDIRECT_URIS');
  return providerDirectory([
    githubProvider({
      clientId,
      clientSecret,
      redirectAllowlist: allowlist === undefined ? [] : allowlist.split(',').map((u) => u.trim()),
      authorizeBase:
        config.get('PARLEY_OAUTH_GITHUB_AUTHORIZE_URL') ?? GITHUB_DEFAULTS.authorizeBase,
      tokenUrl: config.get('PARLEY_OAUTH_GITHUB_TOKEN_URL') ?? GITHUB_DEFAULTS.tokenUrl,
      profileUrl: config.get('PARLEY_OAUTH_GITHUB_PROFILE_URL') ?? GITHUB_DEFAULTS.profileUrl,
    }),
  ]);
}
