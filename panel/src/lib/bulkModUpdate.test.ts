import { describe, expect, it } from 'vitest';

import {
    updatableMods, summarise, runWasClean, emptyTally,
} from './bulkModUpdate';
import type { InstalledMod } from '@/lib/api/modrinth';

const mod = (over: Partial<InstalledMod> = {}): InstalledMod => ({
    id: 1, serverId: 7, subServerName: 'survival',
    modrinthProjectId: 'spark', modrinthProjectSlug: 'spark',
    modrinthVersionId: 'v2', title: 'Spark', fileName: 'spark-1.1.jar',
    sha512: '', installedAt: '2026-09-01T00:00:00Z', ...over,
});


// A bulk run must not act on a mod the server is not actually running. A queued
// install has not happened yet and a failed one never will, so updating either
// would be updating from a build that is not there.
describe('updatableMods', () => {
    it('skips a queued and a failed install', () => {
        const list = [
            mod({ id: 1, modrinthProjectId: 'a' }),
            mod({ id: 2, modrinthProjectId: 'b', status: 'installing' }),
            mod({ id: 3, modrinthProjectId: 'c', status: 'failed' }),
            mod({ id: 4, modrinthProjectId: 'd', status: 'installed' }),
        ];
        expect(updatableMods(list).map(m => m.modrinthProjectId)).toEqual(['a', 'd']);
    });

    it('leaves out a switched-off mod', () => {
        const list = [
            mod({ id: 1, modrinthProjectId: 'a', disabled: true }),
            mod({ id: 2, modrinthProjectId: 'b' }),
        ];
        expect(updatableMods(list).map(m => m.modrinthProjectId)).toEqual(['b']);
    });

    it('leaves out a mod its members paused', () => {
        const list = [
            mod({ id: 1, modrinthProjectId: 'a', updatesPaused: true }),
            mod({ id: 2, modrinthProjectId: 'b', updatesPaused: false }),
        ];
        expect(updatableMods(list).map(m => m.modrinthProjectId)).toEqual(['b']);
    });
});


// A bulk action that reports only its successes is how somebody concludes a run
// was clean when a third of it was not.
describe('summarise', () => {
    it('names every category that happened', () => {
        const t = { ...emptyTally(), updated: 12, current: 3, failed: 2, unknown: 1 };
        const got = summarise(t);
        expect(got).toContain('12 updated');
        expect(got).toContain('3 already current');
        expect(got).toContain('2 failed');
        expect(got).toContain('1 could not be checked');
    });

    it('leaves out the categories that did not happen', () => {
        expect(summarise({ ...emptyTally(), updated: 4 })).toBe('4 updated.');
    });

    it('says so when there was nothing to do', () => {
        expect(summarise(emptyTally())).toBe('Nothing to do.');
    });

    // "could not be checked" is not a success. A version lookup that failed
    // returns an empty list, which looks exactly like "no build matches" - so it
    // is counted apart, and it makes the run not clean.
    it('a run with an unchecked mod is not clean', () => {
        expect(runWasClean({ ...emptyTally(), updated: 9, unknown: 1 })).toBe(false);
        expect(runWasClean({ ...emptyTally(), updated: 9, failed: 1 })).toBe(false);
        expect(runWasClean({ ...emptyTally(), updated: 9, current: 1 })).toBe(true);
    });
});
