/**
 * Container RAM headroom ("padding"): memory a server's container gets on top
 * of its booked RAM. Resolved server -> node -> global -> 512, where null at a
 * level means "inherit the next one" and 0 is a real value (no headroom). It is
 * not a cap, so there is no "unlimited" and LimitField does not fit it.
 */
export const DEFAULT_RAM_PADDING_MB = 512;
export const MAX_RAM_PADDING_MB = 16384;

export const RAM_PADDING_HELP =
    'Memory the server gets on top of its RAM for what Java needs outside the heap (metaspace, threads, native libraries). Too little gets large modpacks killed without a log line.';

export type RamPaddingSource = 'server' | 'node' | 'global';

type Level = number | null | undefined;

const isSet = (v: Level): v is number => typeof v === 'number';

export function resolveRamPadding(server: Level, node: Level, global: Level): { value: number; source: RamPaddingSource } {
    if (isSet(server)) return { value: server, source: 'server' };
    return inheritedRamPadding(node, global);
}

/** What a server gets when it has no override of its own. */
export function inheritedRamPadding(node: Level, global: Level): { value: number; source: 'node' | 'global' } {
    if (isSet(node)) return { value: node, source: 'node' };
    return { value: isSet(global) ? global : DEFAULT_RAM_PADDING_MB, source: 'global' };
}

export function resetLabel(source: 'node' | 'global', value: number): string {
    return `Use ${source} default (${value} MB)`;
}

export function isValidRamPadding(n: number): boolean {
    return Number.isInteger(n) && n >= 0 && n <= MAX_RAM_PADDING_MB;
}

/** An input's text -> override: empty means inherit. NaN marks invalid text. */
export function parseRamPaddingInput(text: string): number | null {
    const t = text.trim();
    if (t === '') return null;
    const n = Number(t);
    return isValidRamPadding(n) ? n : NaN;
}

/** The global default has no level to inherit from, so empty is invalid too. */
export function parseRequiredRamPadding(text: string): number {
    return parseRamPaddingInput(text) ?? NaN;
}

/**
 * The server resources PATCH fields for an override edit. Core reads absent as
 * unchanged, ramPaddingMb as set, resetRamPadding as clear, and refuses both.
 */
export function ramPaddingPatch(current: number | null, edited: number | null):
    Record<string, never> | { ramPaddingMb: number } | { resetRamPadding: true } {
    if (edited === current) return {};
    if (edited === null) return { resetRamPadding: true };
    return { ramPaddingMb: edited };
}
