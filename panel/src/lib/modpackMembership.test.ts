import { describe, it, expect } from 'vitest';
import { isPackMember } from './modpackMembership';

const contents = [
    { modrinthProjectId: 'P1', fileName: 'sodium.jar' },
    { modrinthProjectId: 'P2', fileName: 'lithium.jar' },
];

describe('isPackMember', () => {
    it('matches by project id', () => {
        expect(isPackMember({ modrinthProjectId: 'P1', fileName: 'renamed.jar' }, contents)).toBe(true);
    });

    it('matches by file name when the project differs', () => {
        expect(isPackMember({ modrinthProjectId: 'X9', fileName: 'lithium.jar' }, contents)).toBe(true);
    });

    it('does not match a mod the pack does not ship', () => {
        expect(isPackMember({ modrinthProjectId: 'X9', fileName: 'extra.jar' }, contents)).toBe(false);
    });

    it('never matches on two empty fields', () => {
        expect(isPackMember({ modrinthProjectId: '', fileName: '' }, [{ modrinthProjectId: '', fileName: '' }])).toBe(false);
    });

    it('is false without a snapshot', () => {
        expect(isPackMember({ modrinthProjectId: 'P1', fileName: 'sodium.jar' }, [])).toBe(false);
    });
});
