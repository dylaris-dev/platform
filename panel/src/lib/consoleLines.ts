// Console line buffer for ConsoleView. Pure so the merge rules can be tested.
// Live lines are only appended while the view follows the bottom; while the
// reader is scrolled up they collect in a second buffer built the same way.

import { computeLineLevels, type Level } from './consoleLog';

export interface ConsoleLine {
    // Redis stream ID when known, else a local sequence. Stable across
    // appends and prepends, so React keeps each row instead of re-rendering
    // every row whenever the array shifts.
    key: string;
    text: string;
    level: Level;
}

// Rows kept, in the shown buffer and in the held one.
export const CONSOLE_FOLLOW_CAP = 5000;

export function levelled<E extends { key: string; text: string }>(entries: E[], previous?: Level): (E & { level: Level })[] {
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
        const k = lines[i].key;
        if (isStreamId(k)) return k;
    }
    return '';
}

type Entry = { key: string; text: string };

// appendLive adds a batch of live lines, or the held buffer, with one copy of
// the buffer, and trims it to the cap. A line continues the level of the row
// before it. A line whose stream ID is not newer than the buffer's newest is
// already there: history fetched after the stream opened can hold it before
// its live event lands.
//
// Every line is its own row. Repeats used to be folded into the row above
// with a count, so the console no longer showed what the server printed line
// for line: a reader matching timestamps saw one row where there were forty.
// The batching and the row cap are what keep a spam cheap.
export function appendLive(prev: ConsoleLine[], entries: Entry[]): ConsoleLine[] {
    let next: ConsoleLine[] | null = null;
    for (const entry of entries) {
        const rows: ConsoleLine[] = next ?? prev;
        const last = rows[rows.length - 1];
        if (isStreamId(entry.key)) {
            const newest = newestStreamId(rows);
            if (newest && compareStreamIds(entry.key, newest) <= 0) continue;
        } else if (last && last.key === entry.key) {
            continue;
        }
        next ??= prev.slice();
        next.push(levelled([{ key: entry.key, text: entry.text }], last?.level)[0]);
    }
    if (!next) return prev;
    return next.length > CONSOLE_FOLLOW_CAP ? next.slice(-CONSOLE_FOLLOW_CAP) : next;
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
