import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('./api', () => ({ setRouteEdition: vi.fn() }));
import { setRouteEdition } from './api';
import {
    addBlockedReason, canAddBedrock, changeEdition, editionFlags, editionOf, editionOptions,
    floodgateLink, pausedNotice, showBedrock,
} from './bedrock';

const set = vi.mocked(setRouteEdition);
beforeEach(() => set.mockReset());

const routes = [
    { domain: 'both.example.com', java: true, bedrock: true },
    { domain: 'be.example.com', java: false, bedrock: true },
];

describe('editionOf', () => {
    it('reads a route without a row as Java only', () => {
        expect(editionOf({ routes }, 'java.example.com')).toBe('java');
        expect(editionOf(null, 'x')).toBe('java');
    });
    it('reads both and Bedrock only', () => {
        expect(editionOf({ routes }, 'both.example.com')).toBe('both');
        expect(editionOf({ routes }, 'be.example.com')).toBe('bedrock');
    });
});

describe('editionFlags', () => {
    it.each([['java', true, false], ['both', true, true], ['bedrock', false, true]] as const)('%s', (e, java, bedrock) => {
        expect(editionFlags(e)).toEqual({ java, bedrock });
    });
});

describe('showBedrock / canAddBedrock', () => {
    it('is hidden when Bedrock cannot be added and no route has it', () => {
        expect(showBedrock({ allowed: false, available: true, enabled: false })).toBe(false);
        expect(showBedrock({ allowed: true, available: false, enabled: false })).toBe(false);
    });
    it('stays shown while a route has it, so it can be taken away', () => {
        expect(showBedrock({ allowed: false, available: false, enabled: true })).toBe(true);
        expect(canAddBedrock({ allowed: false, available: true })).toBe(false);
    });
});

describe('editionOptions', () => {
    it('disables only the options that would add Bedrock', () => {
        const o = editionOptions('java', false, 'off');
        expect(o.map(x => [x.id, !!x.disabled])).toEqual([['java', false], ['both', true], ['bedrock', true]]);
        expect(o[1].hint).toBe('off');
    });
    it('never disables a route that already has Bedrock', () => {
        expect(editionOptions('both', false, 'off').some(x => x.disabled)).toBe(false);
    });
});

describe('addBlockedReason / pausedNotice', () => {
    it('names the platform switch first, then the region', () => {
        expect(addBlockedReason({ allowed: false, available: false, refusal: 'x' })).toBe('Bedrock is turned off on this platform.');
        expect(addBlockedReason({ allowed: true, available: false, refusal: 'No entry.' })).toBe('No entry.');
    });
    it('warns that a paused Bedrock-only address admits nobody', () => {
        expect(pausedNotice('bedrock', false)).toMatch(/nobody can join/);
        expect(pausedNotice('both', false)).toMatch(/Java players only/);
        expect(pausedNotice('both', true)).toBeNull();
        expect(pausedNotice('java', false)).toBeNull();
    });
});

describe('floodgateLink', () => {
    it('points plugins at Floodgate for Spigot/Paper and mods at Floodgate-Modded', () => {
        expect(floodgateLink('plugins').href).toBe('https://geysermc.org/download?project=floodgate');
        expect(floodgateLink('mods').href).toBe('https://modrinth.com/mod/floodgate');
    });
});

describe('changeEdition', () => {
    it('sends the flags for the edition', async () => {
        set.mockResolvedValue({ success: true });
        expect(await changeEdition(7, 'a.example.com', 'bedrock')).toBeNull();
        expect(set).toHaveBeenCalledWith(7, 'a.example.com', { java: false, bedrock: true });
    });
    it('returns the 409 message from Core', async () => {
        set.mockResolvedValue({ success: false, code: 'bedrock_disabled', message: 'Bedrock is turned off on this platform.' });
        expect(await changeEdition(7, 'a.example.com', 'both')).toBe('Bedrock is turned off on this platform.');
    });
});
