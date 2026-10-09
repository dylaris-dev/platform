// Console line buffer for ConsoleView. Pure so the merge rules can be tested:
// the view must stay still while the reader is scrolled up, which means no
// trimming at the top unless the view is following the bottom.

import { computeLineLevels, type Level } from './consoleLog';

export interface ConsoleLine {
    // Redis stream ID when known, else a local sequence. Stable across
    // appends and prepends, so React keeps each row instead of re-rendering
    // every row whenever the array shifts.
    key: string;
    text: string;
    level: Level;
}

// Lines kept while following. While scrolled up nothing is dropped, so the
// buffer may grow past this until the reader returns to the bottom.
export const CONSOLE_FOLLOW_CAP = 5000;

export function levelled(entries: { key: string; text: string }[], previous?: Level): ConsoleLine[] {
    const levels = computeLineLevels(entries.map(e => e.text), previous);
    return entries.map((e, i) => ({ ...e, level: levels[i] }));
}

const STREAM_ID = /^(\d+)-(\d+)$/;

export function isStreamId(key: string): boolean {
    return STREAM_ID.test(key);
}

// compareStreamIds orders two Redis stream IDs (ms-seq). Redis writes both
// parts without leading zeros, so length then text is numeric order and
// needs no BigInt for 20-digit parts.
export function compareStreamIds(a: string, b: string): number {
    const [, am, as] = STREAM_ID.exec(a)!;
    const [, bm, bs] = STREAM_ID.exec(b)!;
    const num = (x: string, y: string) => x.length - y.length || (x < y ? -1 : x > y ? 1 : 0);
    return num(am, bm) || num(as, bs);
}

// oldestStreamId is where paging back must start: the oldest line still in
// the buffer. A separate "oldest fetched" marker goes stale once following
// trims the top, and paging from it would skip the trimmed lines.
export function oldestStreamId(lines: ConsoleLine[]): string {
    return lines.find(l => isStreamId(l.key))?.key ?? '';
}

function newestStreamId(lines: ConsoleLine[]): string {
    for (let i = lines.length - 1; i >= 0; i--) {
        if (isStreamId(lines[i].key)) return lines[i].key;
    }
    return '';
}

// appendLive adds one live line. Its level continues from the last line, so
// only the new line is levelled. A line whose stream ID is not newer than the
// buffer's newest is already there: history fetched after the stream opened
// can hold it before its live event lands.
export function appendLive(prev: ConsoleLine[], entry: { key: string; text: string }, follow: boolean): ConsoleLine[] {
    if (isStreamId(entry.key)) {
        const newest = newestStreamId(prev);
        if (newest && compareStreamIds(entry.key, newest) <= 0) return prev;
    } else if (prev.length > 0 && prev[prev.length - 1].key === entry.key) {
        return prev;
    }
    const next = [...prev, ...levelled([entry], prev[prev.length - 1]?.level)];
    return follow && next.length > CONSOLE_FOLLOW_CAP ? next.slice(-CONSOLE_FOLLOW_CAP) : next;
}

// mergeOrdered joins an older block in front of a newer one, dropping lines
// of the older block the newer one already holds (history fetched after the
// live stream opened overlaps it), and re-levels the whole result because a
// stack trace may continue across the seam. Never trims.
export function mergeOrdered(older: ConsoleLine[], newer: ConsoleLine[]): ConsoleLine[] {
    const seen = new Set(newer.map(l => l.key));
    const kept = older.filter(l => !seen.has(l.key));
    return levelled([...kept, ...newer]);
}
