import type { FileEntry } from './types';

// Pure rules for the checkbox tree (SelectiveTree). Paths are relative to the
// tree's root; a checked folder stands for everything under it.

const join = (parent: string, name: string) => (parent ? `${parent}/${name}` : name);

/** Checked itself, under a checked folder, or everything is. */
export function isPathChecked(checked: Set<string>, selectAll: boolean, path: string): boolean {
  if (selectAll) return true;
  for (let p = path; p; p = p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '') {
    if (checked.has(p)) return true;
  }
  return false;
}

/**
 * The checked set after one click. Select-all becomes the explicit top level
 * first, so unticking one entry keeps the rest instead of leaving only it. A
 * path unticked under a checked folder splits that folder into its loaded
 * children; every folder on the way down is expanded, so they are loaded.
 */
export function toggleSelection(
  tree: Record<string, FileEntry[]>,
  checked: Set<string>,
  selectAll: boolean,
  path: string,
): Set<string> {
  const next = selectAll ? new Set((tree[''] ?? []).map(e => e.name)) : new Set(checked);
  if (!isPathChecked(next, false, path)) {
    for (const p of next) if (p.startsWith(path + '/')) next.delete(p);
    next.add(path);
    return next;
  }
  const parts = path.split('/');
  let i = parts.findIndex((_, k) => next.has(parts.slice(0, k + 1).join('/')));
  next.delete(parts.slice(0, i + 1).join('/'));
  for (; i < parts.length - 1; i++) {
    const level = parts.slice(0, i + 1).join('/');
    for (const e of tree[level] ?? []) {
      if (e.name !== parts[i + 1]) next.add(join(level, e.name));
    }
  }
  return next;
}

/** What to send: the fewest paths that cover the selection, never "". */
export function selectedPaths(tree: Record<string, FileEntry[]>, checked: Set<string>, selectAll: boolean): string[] {
  const all = selectAll ? (tree[''] ?? []).map(e => e.name) : [...checked];
  return all
    .filter(p => p !== '' && !isPathChecked(checked, false, p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : ''))
    .sort();
}

// A partial selective download sends every path as a query parameter of one
// GET, about 45 bytes each once encoded; 200 keeps the URL near 9 KB, under
// the request-line limits of common proxies.
export const MAX_DOWNLOAD_PATHS = 200;

export const TOO_MANY_PATHS_MESSAGE = 'Too many separate files selected. Tick whole folders instead.';

/** Unticking one file deep in a ticked folder splits it into every sibling,
 *  so a few clicks can produce thousands of paths. */
export const overPathLimit = (paths: string[], max: number): boolean => paths.length > max;
