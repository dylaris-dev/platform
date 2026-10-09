import type { FileEntry } from './types';

export const formatBytes = (bytes: number, decimals = 2): string => {
  if (bytes === 0) return '0 Bytes';
  const k = 1024;
  const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  const dm = i < 3 ? 0 : decimals;
  return parseFloat((bytes / Math.pow(k, i)).toFixed(dm)) + ' ' + sizes[i];
};

export const validFilenameRegex = /^[a-zA-Z0-9._-]+$/;

export const editableExtensions = [
  '.txt', '.log', '.md', '.yml', '.yaml', '.json', '.json5', '.toml', '.xml',
  '.properties', '.config', '.cfg', '.conf', '.ini', '.js', '.sh',
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
