import { describe, it, expect } from 'vitest';
import { clientSideNotice } from './modpackWarnings';

describe('clientSideNotice', () => {
    it('names the client-required mods on a modpack', () => {
        expect(clientSideNotice([
            { title: 'Sodium', client_side: 'required' },
            { title: 'Lithium', client_side: 'unsupported' },
            { title: 'JEI', client_side: 'required' },
        ], true)).toBe('Players need the same version of Sodium, JEI on their own client, or they cannot connect.');
    });

    it('is null on a server that is not a modpack', () => {
        expect(clientSideNotice([{ title: 'Sodium', client_side: 'required' }], false)).toBeNull();
    });

    it('is null when no mod is required on the client', () => {
        expect(clientSideNotice([{ title: 'Lithium', client_side: 'optional' }, { title: 'X' }], true)).toBeNull();
    });

    it('caps the list of names', () => {
        const many = Array.from({ length: 7 }, (_, i) => ({ title: `M${i}`, client_side: 'required' as const }));
        expect(clientSideNotice(many, true)).toBe('Players need the same version of M0, M1, M2, M3, M4 and 2 more on their own client, or they cannot connect.');
    });
});
