import type { ConfigSource } from '../config.js';

/** The settings a host process is started with, read from its environment. */
export function processConfig(env: Record<string, string | undefined>): ConfigSource {
  return {
    get(key: string): string | undefined {
      const value = env[key];
      return value !== undefined && value.length > 0 ? value : undefined;
    },
  };
}
