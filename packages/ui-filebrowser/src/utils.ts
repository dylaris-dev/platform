import type { FileEntry } from './types';

export const formatBytes = (bytes: number, decimals = 2): string => {
  if (bytes === 0) return '0 Bytes';
  const k = 1024;
  const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  const dm = i < 3 ? 0 : decimals;
  return parseFloat((bytes / Math.pow(k, i)).toFixed(dm)) + ' ' + sizes[i];
};

// A name, not a path: anything but a separator or a control character, and
// not "." or "..". It used to allow only [a-zA-Z0-9._-], so "New World" and
// "fabric-api+1.20.1.jar" (names Minecraft and mod authors write) could not be
// renamed, created or uploaded. Core and the node refuse separators and the
// dot names too (validate.IsPlainFileName); control characters only here.
export const validFilenameRegex = /^(?!\.{1,2}$)[^/\\\x00-\x1f\x7f]{1,255}$/;
export const INVALID_NAME_MESSAGE = 'Invalid name. It cannot contain / or \\, or be "." or "..".';

// Files sort the way people count: world2 before world10, case ignored.
export const compareNames = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' }).compare;

export const editableExtensions = [
  '.txt', '.log', '.md', '.yml', '.yaml', '.json', '.json5', '.toml', '.xml',
  '.properties', '.config', '.cfg', '.conf', '.ini', '.js', '.sh',
  '.snbt', '.zs', '.mcmeta', '.lang', '.csv', '.bat',
];

// Mirrors Core's maxOpenFileBytes: the most a file may hold to be opened.
export const OPEN_MAX_BYTES = 10 * 1024 * 1024;

export const isGzipName = (name: string): boolean => /\.gz$/i.test(name);

// A .gz is opened decompressed by Core, so its type is the name inside it.
export const stripGz = (name: string): string => (isGzipName(name) ? name.slice(0, -3) : name);

// Whether a file opens in the viewer: case-insensitive (Latest.LOG is still a
// log), and a .gz only when what it holds is text (latest.log.gz, not a.tar.gz).
export function isOpenableFile(name: string): boolean {
  const lower = stripGz(name).toLowerCase();
  return editableExtensions.some(ext => lower.endsWith(ext));
}

// Literal, case-insensitive match offsets. Compiling user input as a RegExp
// risked a SyntaxError on invalid patterns and ReDoS on large files.
export function findMatches(text: string, term: string): number[] {
  if (!term) return [];
  const haystack = text.toLowerCase();
  const needle = term.toLowerCase();
  const matches: number[] = [];
  let i = haystack.indexOf(needle);
  while (i !== -1) {
    matches.push(i);
    i = haystack.indexOf(needle, i + needle.length);
  }
  return matches;
}

// Shorten a deep path for display: keep first + last segment, elide the
// middle. Used for the secondary path line on global-search result rows.
export function getTruncatedPath(fullPath: string): string {
  if (!fullPath) return '';
  const parts = fullPath.split('/');
  if (parts.length <= 3) {
    return `/${fullPath}`;
  }
  return `/${parts[0]}/.../${parts[parts.length - 1]}`;
}

export function getCopyName(name: string, isDir: boolean, existingFiles: FileEntry[]): string {
  const existingNames = new Set(existingFiles.map(f => f.name));
  const lastDot = isDir ? -1 : name.lastIndexOf('.');
  const base = lastDot > 0 ? name.substring(0, lastDot) : name;
  const ext = lastDot > 0 ? name.substring(lastDot + 1) : '';
  const makeName = (suffix: string) => ext ? `${base}${suffix}.${ext}` : `${base}${suffix}`;

  let candidate = makeName('_copy');
  if (!existingNames.has(candidate)) return candidate;

  for (let i = 1; ; i++) {
    candidate = makeName(`_copy-${i}`);
    if (!existingNames.has(candidate)) return candidate;
  }
}
/** Creating uses the currently open folder; copy/rename use the source parent. */
export function fileActionDestination(currentPath: string, oldPath: string, name: string, creating: boolean): string {
  const parent = creating ? currentPath.replace(/\/+$/, '') : oldPath.slice(0, Math.max(0, oldPath.lastIndexOf('/')));
  return parent ? `${parent}/${name}` : name;
}
