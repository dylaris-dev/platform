import { describe, expect, it } from 'vitest';
import {
    selectionTotals, toggleAll, orphanAge, truncateKey, summarizeDelete,
    supportsStorageMaintenance, lifecycleUpToDate, describeLifecycleRule, dropDeleted,
} from '@/lib/storageOrphans';
import type { LifecycleRule, OrphanCandidate } from '@/lib/api/storageMaintenance';

const c = (key: string, size: number): OrphanCandidate => ({ key, size, lastModified: '2026-10-01T00:00:00Z', kind: 'server-backup' });
const cands = [c('a', 10), c('b', 20), c('c', 30)];

describe('selection', () => {
    it('totals only the selected keys', () => {
        expect(selectionTotals(cands, new Set(['a', 'c', 'gone']))).toEqual({ count: 2, bytes: 40 });
    });
    it('selects all, then clears', () => {
        const all = toggleAll(cands, new Set(['a']));
        expect([...all].sort()).toEqual(['a', 'b', 'c']);
        expect(toggleAll(cands, all).size).toBe(0);
        expect(toggleAll([], new Set()).size).toBe(0);
    });
});

describe('formatting', () => {
    it('ages in hours, then days', () => {
        const now = Date.parse('2026-10-03T00:00:00Z');
        expect(orphanAge('2026-10-02T00:00:00Z', now)).toBe('24 h');
        expect(orphanAge('2026-10-01T00:00:00Z', now)).toBe('2 d');
        expect(orphanAge('not a date', now)).toBe('-');
    });
    it('truncates in the middle and keeps short keys', () => {
        expect(truncateKey('short')).toBe('short');
        const long = 'backups/0f8fad5b-d9cb-469f-a165-70867728950e/job-12/20261001-020304-deadbeef.tar.gz';
        const t = truncateKey(long, 30);
        expect(t).toHaveLength(30);
        expect(t.startsWith('backups/')).toBe(true);
        expect(t.endsWith('.tar.gz')).toBe(true);
    });
});

describe('delete summary', () => {
    it('counts deleted bytes and keeps the first refusal reason', () => {
        expect(summarizeDelete([
            { key: 'a', deleted: true, size: 5 },
            { key: 'b', deleted: false, reason: 'a backup now refers to it' },
            { key: 'c', deleted: false, reason: 'already gone' },
            { key: 'd', deleted: true, size: 7 },
        ])).toEqual({ deleted: 2, bytes: 12, refused: 2, firstReason: 'a backup now refers to it' });
    });
});

describe('lifecycle', () => {
    const abort: LifecycleRule = { id: 'dylaris-abort-mpu', status: 'Enabled', prefix: '', abortMultipartDays: 1, ours: true };
    it('is up to date only when every planned rule matches', () => {
        expect(lifecycleUpToDate([abort], [abort])).toBe(true);
        expect(lifecycleUpToDate([{ ...abort, abortMultipartDays: 7 }], [abort])).toBe(false);
        expect(lifecycleUpToDate([], [abort])).toBe(false);
    });
    it('describes a rule', () => {
        expect(describeLifecycleRule(abort)).toBe('abort unfinished uploads after 1 d (whole bucket)');
        expect(describeLifecycleRule({ id: 'x', status: 'Enabled', prefix: 'p/', expirationDays: 2, ours: false }))
            .toBe('delete files after 2 d (under p/)');
    });
    it('only offers object storage', () => {
        expect(['s3', 'connection', 'core-storage', 'local', 'node-local'].map(supportsStorageMaintenance))
            .toEqual([true, true, true, false, false]);
    });
});

describe('dropDeleted', () => {
    it('removes deleted files from both lists and recomputes the totals', () => {
        const scan = {
            candidates: [c('a', 10), c('b', 20)], candidateBytes: 30,
            live: [c('l1', 5), c('l2', 7)], liveBytes: 12,
            referencedCount: 0, referencedBytes: 0, recentCount: 0, recentBytes: 0,
            unclassifiedCount: 0, unclassifiedBytes: 0, scannedAt: '',
        };
        const next = dropDeleted(scan, [
            { key: 'a', deleted: true, size: 10 },
            { key: 'l2', deleted: true, size: 7 },
            { key: 'b', deleted: false, reason: 'a backup now refers to it' },
        ]);
        expect(next.candidates.map(x => x.key)).toEqual(['b']);
        expect(next.candidateBytes).toBe(20);
        expect(next.live.map(x => x.key)).toEqual(['l1']);
        expect(next.liveBytes).toBe(5);
    });
});
