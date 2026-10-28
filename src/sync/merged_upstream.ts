import { readPack } from '../platform/local/pack.js';
import type { RepositoryStore } from '../platform/repostore.js';
import { descendsFrom } from '../git/ancestry.js';
import type { PatchRecord } from '../patch/list.js';

/**
 * Finding the patches the upstream has taken.
 *
 * A patch can land upstream in three shapes, and each needs its own
 * signal. Merged as it was, its commits are in the upstream's history
 * and ancestry finds it. Squashed, its commits are gone but their
 * content is in one upstream commit, so the trees are compared.
 * Merged through a pull request, the upstream's commit message ends
 * with the number, which is what a forge writes when it squashes.
 *
 * Which of the three are looked for is the workspace's own setting,
 * because the cost and the false-positive rate differ: ancestry is
 * exact and cheap, content is neither.
 */
export type Detection = 'ancestry_only' | 'content_based' | 'both';

export interface MergedDeps {
  repos: RepositoryStore;
  repository: string;
}

/** The commits of the upstream head, as far back as is worth reading. */
export interface UpstreamHistory {
  /** Every commit id upstream holds. */
  commits: Set<string>;
  /** Each commit's tree, for comparing content. */
  trees: Map<string, string>;
  /** The messages of the most recent commits, for a pull request number. */
  messages: string[];
}

/** Reads the upstream's history out of a pack the hub already has. */
export function historyOf(pack: Uint8Array, head: string): UpstreamHistory {
  const commits = new Set<string>();
  const trees = new Map<string, string>();
  const messages: string[] = [];

  // Nothing fetched is nothing to read: a sync that found the
  // upstream where it left it has no new history to look through.
  if (pack.byteLength === 0) {
    return { commits, trees, messages };
  }

  for (const object of readPack(pack)) {
    if (object.type !== 'commit') {
      continue;
    }
    const text: string = Buffer.from(object.data).toString('utf8');
    const blank = text.indexOf('\n\n');
    const header: string = blank === -1 ? text : text.slice(0, blank);
    commits.add(object.oid);
    const tree = /^tree ([0-9a-f]{40})$/m.exec(header)?.[1];
    if (tree !== undefined) {
      trees.set(object.oid, tree);
    }
    if (blank !== -1) {
      messages.push(text.slice(blank + 2).trim());
    }
  }
  void head;
  return { commits, trees, messages };
}

/**
 * Whether one patch has landed upstream, by the signals the setting
 * allows. Either the ancestry or a content signal is enough.
 */
export function landedUpstream(
  patch: PatchRecord,
  tip: string | undefined,
  tipTree: string | undefined,
  upstream: UpstreamHistory,
  detection: Detection,
): boolean {
  if (tip === undefined) {
    return false;
  }

  // Ancestry: the patch's own commits are in the upstream's history.
  if (upstream.commits.has(tip)) {
    return true;
  }
  if (detection === 'ancestry_only') {
    return false;
  }

  // Content: an upstream commit holds exactly what the patch does.
  if (tipTree !== undefined && [...upstream.trees.values()].includes(tipTree)) {
    return true;
  }

  // The number a forge writes into the message when it squashes.
  const number = /\/(\d+)\/?$/.exec(patch.upstream_pr_url ?? '')?.[1];
  if (number !== undefined) {
    return upstream.messages.some((message) => message.trimEnd().endsWith(`(#${number})`));
  }
  return false;
}

/** Whether this patch's branch is contained in the upstream's history. */
export function containedIn(pack: Uint8Array, upstreamHead: string, tip: string): boolean {
  return descendsFrom(pack, upstreamHead, tip);
}
