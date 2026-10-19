import { describe, it, expect } from 'vitest';
import { matchesSide } from './modSide';

const lithium = { client_side: 'optional', server_side: 'optional' };
const chunky = { client_side: 'unsupported', server_side: 'required' };
const create = { client_side: 'required', server_side: 'required' };
const sodium = { client_side: 'required', server_side: 'unsupported' };
const shaderish = { client_side: 'optional', server_side: 'unsupported' };
const unknown = { client_side: 'unknown', server_side: 'unknown' };

describe('matchesSide', () => {
    it('server keeps mods players do not need', () => {
        expect(matchesSide('server', lithium)).toBe(true);
        expect(matchesSide('server', chunky)).toBe(true);
    });

    it('server hides mods players need and client-only mods', () => {
        expect(matchesSide('server', create)).toBe(false);
        expect(matchesSide('server', sodium)).toBe(false);
        expect(matchesSide('server', shaderish)).toBe(false);
        expect(matchesSide('server', unknown)).toBe(false);
    });

    it('client keeps mods players need and client-only mods', () => {
        expect(matchesSide('client', create)).toBe(true);
        expect(matchesSide('client', sodium)).toBe(true);
        expect(matchesSide('client', shaderish)).toBe(true);
    });

    it('client hides server-only and unknown mods', () => {
        expect(matchesSide('client', lithium)).toBe(false);
        expect(matchesSide('client', chunky)).toBe(false);
        expect(matchesSide('client', unknown)).toBe(false);
    });

    it('all keeps everything, including no side data', () => {
        expect(matchesSide('all', {})).toBe(true);
        expect(matchesSide('all', create)).toBe(true);
    });
});
