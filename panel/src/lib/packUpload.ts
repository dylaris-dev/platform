const CONTENT_ROOTS = new Set(['config', 'defaultconfigs', 'kubejs', 'scripts']);

export function packTargetPathError(path: string): string | null {
    if (!path || path.startsWith('/') || path.includes('\\') || /^[a-z]:/i.test(path) || /[\x00-\x1f\x7f]/.test(path)) {
        return 'Use a relative Minecraft path with forward slashes.';
    }
    if (path.split('/').some(part => !part || part === '.' || part === '..')) return 'Target paths cannot contain empty, dot or parent segments.';
    return null;
}

/** Folder selection keeps known Minecraft roots and drops an optional enclosing pack folder. */
export function packUploadTarget(file: { name: string; webkitRelativePath?: string }, prefix: string): string {
    const relative = file.webkitRelativePath || '';
    const segments = relative.split('/');
    if (CONTENT_ROOTS.has(segments[0])) return relative;
    if (segments.length > 2 && CONTENT_ROOTS.has(segments[1])) return segments.slice(1).join('/');
    return `${prefix.replace(/\/$/, '')}/${relative || file.name}`;
}
