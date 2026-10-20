import { describe, it, expect } from 'vitest';
import { appendLive, levelled, mergeOrdered, oldestStreamId, compareStreamIds, CONSOLE_FOLLOW_CAP, type ConsoleLine } from './consoleLines';

const block = (from: number, to: number): ConsoleLine[] =>
    levelled(Array.from({ length: to - from + 1 }, (_, i) => ({ key: `${from + i}-0`, text: `line ${from + i}` })));

describe('appendLive', () => {
    it('trims to the cap while following', () => {
        const next = appendLive(block(1, CONSOLE_FOLLOW_CAP), [{ key: 'new', text: 'new' }]);
        expect(next).toHaveLength(CONSOLE_FOLLOW_CAP);
        expect(next[0].key).toBe('2-0');
        expect(next[next.length - 1].key).toBe('new');
    });

    it('keeps the existing line objects, so memoised rows do not re-render', () => {
        const prev = block(1, 3);
        const next = appendLive(prev, [{ key: '4-0', text: 'x' }]);
        expect(next.slice(0, 3)).toEqual(prev);
        next.slice(0, 3).forEach((l, i) => expect(l).toBe(prev[i]));
    });

    it('drops a repeat of the last line', () => {
        const prev = block(1, 3);
        expect(appendLive(prev, [{ key: '3-0', text: 'line 3' }])).toBe(prev);
    });

    it('drops a live line the history already holds, even when it is not the last', () => {
        // History fetched after the stream opened holds L and L+1; the live event for L lands later.
        const prev = block(1, 3);
        expect(appendLive(prev, [{ key: '2-0', text: 'line 2' }])).toBe(prev);
        expect(appendLive(prev, [{ key: '1-0', text: 'line 1' }])).toBe(prev);
    });

    it('compares stream ids as numbers, not text', () => {
        const prev = block(9, 9);
        expect(appendLive(prev, [{ key: '10-0', text: 'line 10' }])).toHaveLength(2);
        expect(appendLive(levelled([{ key: '5-9', text: 'x' }]), [{ key: '5-10', text: 'y' }])).toHaveLength(2);
        expect(compareStreamIds('99999999999999999999-1', '99999999999999999999-0')).toBeGreaterThan(0);
    });

    it('shows every repeat as its own row, in the order printed', () => {
        // A spam from two worker threads, the shape folding used to collapse into one row.
        const spam = (k: number, thread: number, time = '20:51:01') =>
            ({ key: `${k}-0`, text: `[${time}] [DH-World Gen Thread[${thread}]/WARN] [co.iz.by.BygoneNetherMod/]: broken basalt` });
        const prev = block(1, 2);
        const next = appendLive(prev, [spam(3, 0), spam(4, 1), spam(5, 0), spam(6, 1, '20:51:02')]);
        expect(next.map(l => l.key)).toEqual(['1-0', '2-0', '3-0', '4-0', '5-0', '6-0']);
        expect(next[3]).toEqual({ key: '4-0', text: spam(4, 1).text, level: 'warn' });
        // Identical text, Paper format, one after the other: still two rows.
        const paper = appendLive([], [
            { key: '1-0', text: "[20:51:01 WARN]: Can't keep up!" }, { key: '2-0', text: "[20:51:01 WARN]: Can't keep up!" },
        ]);
        expect(paper).toHaveLength(2);
        // A replay is still recognised as already shown.
        expect(appendLive(next, [spam(6, 1)])).toBe(next);
        expect(appendLive(next, [spam(5, 0)])).toBe(next);
    });

    it('releases a held buffer onto the shown one, line for line', () => {
        const spam = (k: number) => ({ key: `${k}-0`, text: `[20:00:00] [Server thread/WARN]: spam` });
        const shown = appendLive(block(1, 2), [spam(3), spam(4)]);
        const held = appendLive([], [spam(5), spam(6), { key: '7-0', text: '[20:00:00] [Server thread/ERROR]: boom' }, spam(8)]);
        expect(held).toHaveLength(4);
        const next = appendLive(shown, held);
        expect(next.map(l => l.key)).toEqual(['1-0', '2-0', '3-0', '4-0', '5-0', '6-0', '7-0', '8-0']);
        expect(next[6].level).toBe('error');
    });

    it('caps the held buffer by rows, keeping the newest', () => {
        const spam = Array.from({ length: CONSOLE_FOLLOW_CAP + 10 }, (_, i) => ({ key: `${i + 1}-0`, text: '[20:00:00] [T/WARN]: spam' }));
        const held = appendLive([], spam);
        expect(held).toHaveLength(CONSOLE_FOLLOW_CAP);
        expect(held[0].key).toBe('11-0');
        expect(held[held.length - 1].key).toBe(`${CONSOLE_FOLLOW_CAP + 10}-0`);
    });

    it('appends a batch in order and keeps untouched rows', () => {
        const prev = block(1, 3);
        const next = appendLive(prev, [{ key: '4-0', text: 'x' }, { key: '5-0', text: 'y' }]);
        expect(next.map(l => l.key)).toEqual(['1-0', '2-0', '3-0', '4-0', '5-0']);
        expect(prev).toHaveLength(3);
        next.slice(0, 3).forEach((l, i) => expect(l).toBe(prev[i]));
    });

    it('levels a continuation line from the line before it', () => {
        const prev = levelled([{ key: 'a', text: '[12:00:00] [Server thread/ERROR]: boom' }]);
        const next = appendLive(prev, [{ key: 'b', text: '\tat com.foo.Bar(Bar.java:1)' }]);
        expect(next[1].level).toBe('error');
    });
});

describe('mergeOrdered', () => {
    it('prepends an older page in order without trimming', () => {
        const current = block(1001, 1000 + CONSOLE_FOLLOW_CAP);
        const merged = mergeOrdered(block(1, 1000), current);
        expect(merged).toHaveLength(1000 + CONSOLE_FOLLOW_CAP);
        expect(merged[0].key).toBe('1-0');
        expect(merged[1000].key).toBe('1001-0');
    });

    it('drops older lines the newer block already holds', () => {
        // History fetched after the live stream opened repeats its first lines.
        const merged = mergeOrdered(block(1, 5), block(4, 6));
        expect(merged.map(l => l.key)).toEqual(['1-0', '2-0', '3-0', '4-0', '5-0', '6-0']);
    });

    it('re-levels across the seam', () => {
        const older = levelled([{ key: 'a', text: '[Server thread/WARN]: careful' }]);
        const newer = levelled([{ key: 'b', text: '\tat x.y(Z.java:1)' }]);
        expect(newer[0].level).toBe('info');
        expect(mergeOrdered(older, newer)[1].level).toBe('warn');
    });
});

describe('oldestStreamId', () => {
    it('follows the follow-cap trim, so paging back does not skip the trimmed lines', () => {
        // History page started at 1-0; following then trimmed the top.
        let lines = block(1, CONSOLE_FOLLOW_CAP);
        expect(oldestStreamId(lines)).toBe('1-0');
        for (let i = 1; i <= 10; i++) {
            lines = appendLive(lines, [{ key: `${CONSOLE_FOLLOW_CAP + i}-0`, text: `x${i}` }]);
        }
        expect(oldestStreamId(lines)).toBe('11-0');
    });

    it('skips local keys and is empty without stream ids', () => {
        const lines = levelled([{ key: 'local-1', text: 'a' }, { key: '7-0', text: 'b' }]);
        expect(oldestStreamId(lines)).toBe('7-0');
        expect(oldestStreamId(levelled([{ key: 'local-1', text: 'a' }]))).toBe('');
    });
});
