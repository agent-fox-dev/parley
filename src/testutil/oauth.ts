import { HttpError } from '../http/errors.js';
import type { OAuthProvider, ProviderIdentity } from '../auth/oauth.js';
import { providerDirectory } from '../auth/oauth.js';

export interface StubProviderOptions {
  name?: string;
  /** The codes the provider accepts, each with the identity it yields. */
  codes: Record<string, ProviderIdentity>;
  redirectAllowlist?: readonly string[];
  clientId?: string;
  /** Where each exchange is written down, for a suite that checks one. */
  asked?: Array<{ code: string; redirectUri?: string }>;
}

/**
 * A provider that answers from a table instead of the network, so a test
 * drives sign-in without one.
 */
export function stubProvider(options: StubProviderOptions): OAuthProvider {
  const name = options.name ?? 'github';
  return {
    name,
    authorizeUrl: `https://provider.invalid/authorize?client_id=${options.clientId ?? 'test-client'}`,
    redirectAllowlist: options.redirectAllowlist ?? [],
    exchange: async (code: string, redirectUri?: string): Promise<ProviderIdentity> => {
      options.asked?.push({ code, ...(redirectUri === undefined ? {} : { redirectUri }) });
      const identity = options.codes[code];
      if (identity === undefined) {
        throw new HttpError(401, 'the authorization code is invalid or expired');
      }
      return Promise.resolve(identity);
    },
  };
}

export function stubDirectory(options: StubProviderOptions) {
  return providerDirectory([stubProvider(options)]);
}

export function providerIdentity(over: Partial<ProviderIdentity> = {}): ProviderIdentity {
  const username = over.username ?? 'alice';
  return {
    provider: 'github',
    provider_id: '12345',
    username,
    email: `${username}@example.com`,
    full_name: 'Alice Example',
    ...over,
  };
}
