/**
 * What a conflict is, and what names it. Git's rerere keys a resolution
 * by a hash of the normalised conflict kept in a directory; there is no
 * directory here, so the engine computes its own identity. It need not
 * equal git's — only be the same for the same conflict, here and after a
 * restart (ADR 8, decision 2 of the package).
 */
export interface Conflict {
  path: string;
  /** The content of the path in each of the three inputs. */
  base: string;
  ours: string;
  theirs: string;
}

/**
 * Normalised so that what does not change the conflict does not change
 * its identity: line endings, and whitespace at the end of a line.
 */
function normalise(content: string): string {
  return content
    .replaceAll('\r\n', '\n')
    .split('\n')
    .map((line) => line.replace(/[ \t]+$/, ''))
    .join('\n');
}

const encoder = new TextEncoder();

/**
 * The identity of a conflict, computed from its content and the path it
 * is at. The same conflict in two repositories has the same identity,
 * so a resolution recorded once is found again.
 */
export async function conflictIdentity(conflict: Conflict): Promise<string> {
  const parts = [
    conflict.path,
    normalise(conflict.base),
    normalise(conflict.ours),
    normalise(conflict.theirs),
  ];
  // Each part is prefixed with its length, so no two different
  // conflicts can run together into the same bytes.
  const material = parts.map((part) => `${String(part.length)}\u0000${part}`).join('\u0000');
  const digest = await crypto.subtle.digest('SHA-256', encoder.encode(material));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, '0')).join('');
}
