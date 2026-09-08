import { describe, it, expect } from 'vitest';
import { selectionProblem, summarise } from './PlatformBackupsTab';
import { EMPTY_SELECTION, type BackupTargetServer, type PlatformBackupSelection } from '@/lib/api/platformBackups';

const sel = (over: Partial<PlatformBackupSelection> = {}): PlatformBackupSelection => ({
    ...EMPTY_SELECTION,
    ...over,
    servers: { ...EMPTY_SELECTION.servers, ...(over.servers || {}) },
});

const fleet: BackupTargetServer[] = [
    { id: 1, uuid: 'u1', name: 'alpha', ownerId: 'alice', nodeId: 10, byon: false },
    { id: 2, uuid: 'u2', name: 'beta', ownerId: 'bob', nodeId: 11, byon: true },
    { id: 3, uuid: 'u3', name: 'gamma', ownerId: 'alice', nodeId: 11, byon: true },
];

describe('selectionProblem', () => {
    /**
     * The save button is inert for the same reasons the server refuses. Letting
     * it through means a round trip that ends in a red toast saying what the
     * form could have said before it was sent.
     */
    it('refuses a run that would archive nothing', () => {
        expect(selectionProblem(sel())).toBe('Choose at least one thing to back up.');
    });

    it('accepts the database on its own', () => {
        expect(selectionProblem(sel({ database: true }))).toBe('');
    });

    it('accepts servers on their own', () => {
        expect(selectionProblem(sel({ servers: { mode: 'all' } }))).toBe('');
    });

    /**
     * Selecting nobody must not read as selecting everybody. The whole platform
     * archived under a configuration that says "one owner" is the worst reading
     * of a missing field.
     */
    it('refuses by-owner with no owner picked', () => {
        expect(selectionProblem(sel({ database: true, servers: { mode: 'owner' } })))
            .toBe('Pick the user whose servers should be included.');
    });

    it('refuses an individual selection with nothing in it', () => {
        expect(selectionProblem(sel({ database: true, servers: { mode: 'list', serverIds: [] } })))
            .toBe('Pick at least one server.');
    });

    it('accepts an individual selection with one server', () => {
        expect(selectionProblem(sel({ servers: { mode: 'list', serverIds: [2] } }))).toBe('');
    });
});

describe('summarise', () => {
    it('names every selected component', () => {
        const got = summarise(sel({ database: true, library: true, modpacks: true, metricsDb: true }), fleet);
        expect(got).toContain('database');
        expect(got).toContain('library');
        expect(got).toContain('modpacks');
        expect(got).toContain('statistics');
    });

    it('counts what "all servers" currently means', () => {
        expect(summarise(sel({ servers: { mode: 'all' } }), fleet)).toBe('all servers (3)');
    });

    /** BYON is the node's ownership, so the count is not "servers of customers". */
    it('counts only servers on customer hardware for BYON', () => {
        expect(summarise(sel({ servers: { mode: 'byon' } }), fleet)).toBe('BYON servers (2)');
    });

    it('counts an individual selection', () => {
        expect(summarise(sel({ servers: { mode: 'list', serverIds: [1, 3] } }), fleet)).toBe('2 servers');
    });

    /**
     * A job row whose selection could not be read must not describe itself as
     * covering something. The store already reads an unparseable selection as
     * empty; the screen has to agree.
     */
    it('says nothing for a selection that is missing', () => {
        expect(summarise(undefined, fleet)).toBe('nothing');
        expect(summarise(sel(), fleet)).toBe('nothing');
    });
});
