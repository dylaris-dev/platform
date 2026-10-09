import { describe, it, expect } from 'vitest';
import { appendLive, levelled, mergeOrdered, oldestStreamId, compareStreamIds, CONSOLE_FOLLOW_CAP, type ConsoleLine } from './consoleLines';

const block = (from: number, to: number): ConsoleLine[] =>
    levelled(Array.from({ length: to - from + 1 }, (_, i) => ({ key: `${from + i}-0`, text: `line ${from + i}` })));

describe('appendLive', () => {
    it('trims to the cap while following', () => {
        const next = appendLive(block(1, CONSOLE_FOLLOW_CAP), { key: 'new', text: 'new' }, true);
        expect(next).toHaveLength(CONSOLE_FOLLOW_CAP);
        expect(next[0].key).toBe('2-0');
        expect(next[next.length - 1].key).toBe('new');
    });

    it('never trims while the reader is scrolled up, so the text stays still', () => {
        const prev = block(1, CONSOLE_FOLLOW_CAP);
        const next = appendLive(prev, { key: 'new', text: 'new' }, false);
        expect(next).toHaveLength(CONSOLE_FOLLOW_CAP + 1);
        expect(next[0]).toBe(prev[0]);
    });

    it('keeps the existing line objects, so memoised rows do not re-render', () => {
        const prev = block(1, 3);
        const next = appendLive(prev, { key: '4-0', text: 'x' }, true);
        expect(next.slice(0, 3)).toEqual(prev);
        next.slice(0, 3).forEach((l, i) => expect(l).toBe(prev[i]));
    });

    it('drops a repeat of the last line', () => {
        const prev = block(1, 3);
        expect(appendLive(prev, { key: '3-0', text: 'line 3' }, true)).toBe(prev);
    });

    it('drops a live line the history already holds, even when it is not the last', () => {
        // History fetched after the stream opened holds L and L+1; the live event for L lands later.
        const prev = block(1, 3);
        expect(appendLive(prev, { key: '2-0', text: 'line 2' }, true)).toBe(prev);
        expect(appendLive(prev, { key: '1-0', text: 'line 1' }, false)).toBe(prev);
    });

    it('compares stream ids as numbers, not text', () => {
        const prev = block(9, 9);
        expect(appendLive(prev, { key: '10-0', text: 'line 10' }, true)).toHaveLength(2);
        expect(appendLive(levelled([{ key: '5-9', text: 'x' }]), { key: '5-10', text: 'y' }, true)).toHaveLength(2);
        expect(compareStreamIds('99999999999999999999-1', '99999999999999999999-0')).toBeGreaterThan(0);
    });

    it('levels a continuation line from the line before it', () => {
        const prev = levelled([{ key: 'a', text: '[12:00:00] [Server thread/ERROR]: boom' }]);
        const next = appendLive(prev, { key: 'b', text: '\tat com.foo.Bar(Bar.java:1)' }, true);
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
            lines = appendLive(lines, { key: `${CONSOLE_FOLLOW_CAP + i}-0`, text: 'x' }, true);
        }
        expect(oldestStreamId(lines)).toBe('11-0');
    });

    it('skips local keys and is empty without stream ids', () => {
        const lines = levelled([{ key: 'local-1', text: 'a' }, { key: '7-0', text: 'b' }]);
        expect(oldestStreamId(lines)).toBe('7-0');
        expect(oldestStreamId(levelled([{ key: 'local-1', text: 'a' }]))).toBe('');
    });
});
