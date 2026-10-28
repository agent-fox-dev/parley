import { readPack } from '../platform/local/pack.js';

/**
 * Whether one commit descends from another, answered from a pack.
 *
 * A pack fetched or served with the older commit as a `have` carries
 * the commits between the two and names the older one as the parent of
 * the oldest of them; a pack that never names it is a history that
 * went another way. That makes the question answerable from bytes
 * already in hand, with no walk of the repository and no second
 * request.
 */
export function descendsFrom(pack: Uint8Array, head: string, ancestor: string): boolean {
  if (head === ancestor) {
    return true;
  }

  const parents = new Map<string, string[]>();
  for (const object of readPack(pack)) {
    if (object.type !== 'commit') {
      continue;
    }
    const text: string = Buffer.from(object.data).toString('latin1');
    const blank = text.indexOf('\n\n');
    const header: string = blank === -1 ? text : text.slice(0, blank);
    parents.set(
      object.oid,
      header
        .split('\n')
        .map((line) => /^parent ([0-9a-f]{40})$/.exec(line)?.[1])
        .filter((oid): oid is string => oid !== undefined),
    );
  }

  const seen = new Set<string>();
  const pending = [head];
  while (pending.length > 0) {
    const oid = pending.pop() ?? '';
    if (oid === ancestor) {
      return true;
    }
    if (seen.has(oid)) {
      continue;
    }
    seen.add(oid);
    pending.push(...(parents.get(oid) ?? []));
  }
  return false;
}
