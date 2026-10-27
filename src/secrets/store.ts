import type { RelationalStore } from '../platform/index.js';
import type { SecretStore } from '../platform/secrets.js';

const KEY_BYTES = 32;
const IV_BYTES = 12;

function toBase64(bytes: Uint8Array): string {
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}

function fromBase64(text: string): Uint8Array {
  const binary = atob(text);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

/**
 * The key a deployment seals its secrets with, read from a base64 value of
 * exactly 32 bytes. A deployment that configured none has no secret store,
 * and the routes that would use one say so rather than storing in clear.
 */
export async function deploymentKey(
  configured: string | undefined,
): Promise<CryptoKey | undefined> {
  if (configured === undefined || configured.length === 0) {
    return undefined;
  }
  let material: Uint8Array;
  try {
    material = fromBase64(configured);
  } catch {
    return undefined;
  }
  if (material.length !== KEY_BYTES) {
    return undefined;
  }
  return crypto.subtle.importKey('raw', material as BufferSource, 'AES-GCM', false, [
    'encrypt',
    'decrypt',
  ]);
}

/**
 * Secrets over the relational store, each value sealed with AES-GCM under
 * the deployment's key before it is written. A fresh nonce per write means
 * two equal secrets do not look equal in the store, and the tag means a
 * value changed in the store is refused rather than returned (see ADR 4).
 */
/** Seals a value under the deployment's key, with a fresh nonce each time. */
export function sealWith(key: CryptoKey): (value: string) => Promise<string> {
  return async (value: string) => {
    const iv = crypto.getRandomValues(new Uint8Array(IV_BYTES));
    const sealed = await crypto.subtle.encrypt(
      { name: 'AES-GCM', iv: iv as BufferSource },
      key,
      new TextEncoder().encode(value),
    );
    return `${toBase64(iv)}.${toBase64(new Uint8Array(sealed))}`;
  };
}

/** Opens what sealWith sealed, refusing anything the tag does not vouch for. */
export function openWith(key: CryptoKey): (stored: string) => Promise<string> {
  return async (stored: string) => {
    const [iv, sealed] = stored.split('.');
    if (iv === undefined || sealed === undefined) {
      throw new Error('the stored secret is not in the sealed form');
    }
    const opened = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: fromBase64(iv) as BufferSource },
      key,
      fromBase64(sealed) as BufferSource,
    );
    return new TextDecoder().decode(opened);
  };
}

export function sealedSecrets(store: RelationalStore, key: CryptoKey): SecretStore {
  async function seal(value: string): Promise<string> {
    const iv = crypto.getRandomValues(new Uint8Array(IV_BYTES));
    const sealed = await crypto.subtle.encrypt(
      { name: 'AES-GCM', iv: iv as BufferSource },
      key,
      new TextEncoder().encode(value),
    );
    return `${toBase64(iv)}.${toBase64(new Uint8Array(sealed))}`;
  }

  async function open(stored: string): Promise<string> {
    const [iv, sealed] = stored.split('.');
    if (iv === undefined || sealed === undefined) {
      throw new Error('the stored secret is not in the sealed form');
    }
    const opened = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: fromBase64(iv) as BufferSource },
      key,
      fromBase64(sealed) as BufferSource,
    );
    return new TextDecoder().decode(opened);
  }

  return {
    async put(scope: string, name: string, value: string): Promise<void> {
      const at = new Date().toISOString();
      await store.run(
        `INSERT INTO entries (scope, kind, key, key_folded, value, created_at, updated_at)
         VALUES (?, 'secret', ?, ?, ?, ?, ?)
         ON CONFLICT (scope, kind, key_folded) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
        [scope, name, name.toLowerCase(), await seal(value), at, at],
      );
    },

    async get(scope: string, name: string): Promise<string | undefined> {
      const rows = await store.all<{ value: string }>(
        "SELECT value FROM entries WHERE scope = ? AND kind = 'secret' AND key_folded = ?",
        [scope, name.toLowerCase()],
      );
      const row = rows[0];
      return row === undefined ? undefined : open(row.value);
    },

    async delete(scope: string, name: string): Promise<void> {
      await store.run(
        "DELETE FROM entries WHERE scope = ? AND kind = 'secret' AND key_folded = ?",
        [scope, name.toLowerCase()],
      );
    },

    async names(scope: string): Promise<string[]> {
      const rows = await store.all<{ key: string }>(
        "SELECT key FROM entries WHERE scope = ? AND kind = 'secret' ORDER BY key_folded",
        [scope],
      );
      return rows.map((row) => row.key);
    },
  };
}
