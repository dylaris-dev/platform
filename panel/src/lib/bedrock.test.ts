import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('./api', () => ({ setRouteEdition: vi.fn() }));
import { setRouteEdition } from './api';
import {
    addBlockedReason, canAddBedrock, changeEdition, editionFlags, editionOf, editionOptions,
    floodgateLink, linkEdition, pausedNotice, showBedrock, bedrockTakenElsewhere, saveEdition, isOwnDomainRoute,
    editionBlockedNote,
} from './bedrock';
import { OWN_DOMAIN_EXTRAS_TEXT } from './gatewayProvider';

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

describe('bedrockTakenElsewhere', () => {
    it('is null when no address has Bedrock', () => {
        expect(bedrockTakenElsewhere({ routes: [] }, 'a.example.com')).toBeNull();
        expect(bedrockTakenElsewhere({ routes: [{ domain: 'a.example.com', java: true, bedrock: false }] }, 'b.example.com')).toBeNull();
    });
    it('is null for the address that holds it', () => {
        expect(bedrockTakenElsewhere({ routes: [routes[0]] }, 'both.example.com')).toBeNull();
    });
    it('names another address that holds it', () => {
        expect(bedrockTakenElsewhere({ routes: [routes[0]] }, 'java.example.com')).toBe('both.example.com');
    });
});

describe('editionOptions on the managed gateway', () => {
    it('disables adding Bedrock with the address that holds it', () => {
        const o = editionOptions('java', true, '', 'both.example.com');
        expect(o.map(x => [x.id, !!x.disabled])).toEqual([['java', false], ['both', true], ['bedrock', true]]);
        expect(o[2].hint).toBe('This server already has a Bedrock address: both.example.com.');
    });
    it('never disables the address that has it', () => {
        expect(editionOptions('bedrock', true, '', null).some(x => x.disabled)).toBe(false);
    });
    // Local mode passes no fourth argument: today's options, byte for byte.
    it('is unchanged without it', () => {
        expect(editionOptions('java', true, '')).toEqual([
            { id: 'java', label: 'Java', hint: 'Java players only.' },
            { id: 'both', label: 'Java + Bedrock', hint: 'Java and Bedrock players on the same address.' },
            { id: 'bedrock', label: 'Bedrock', hint: 'Bedrock players only. Java players get a notice.' },
        ]);
        expect(editionOptions('java', false, 'off')).toEqual([
            { id: 'java', label: 'Java', hint: 'Java players only.' },
            { id: 'both', label: 'Java + Bedrock', hint: 'off', disabled: true },
            { id: 'bedrock', label: 'Bedrock', hint: 'off', disabled: true },
        ]);
    });
});

describe('saveEdition', () => {
    it('passes the restart and the warning on', async () => {
        set.mockResolvedValue({ success: true, restartRequired: true, warning: 'Key late.' });
        expect(await saveEdition(7, 'a.example.com', 'both')).toEqual({ error: null, restartRequired: true, warning: 'Key late.' });
    });
    it('is no restart when Core says none', async () => {
        set.mockResolvedValue({ success: true });
        expect(await saveEdition(7, 'a.example.com', 'java')).toEqual({ error: null, restartRequired: false, warning: '' });
    });
    it('returns the refusal', async () => {
        set.mockResolvedValue({ success: false, code: 'bedrock_one_address', message: 'Take Bedrock off b first.' });
        expect(await saveEdition(7, 'a.example.com', 'both')).toEqual({ error: 'Take Bedrock off b first.', restartRequired: false, warning: '' });
    });
    it('says when the request could not be sent', async () => {
        set.mockRejectedValueOnce(new Error('down'));
        expect((await saveEdition(7, 'a.example.com', 'both')).error).toBe('The request could not be sent.');
    });
});

describe('linkEdition', () => {
    it('reads a route-only address the way Core stores it', () => {
        expect(linkEdition(undefined)).toBe('java');
        expect(linkEdition({ java: true, bedrock: false })).toBe('java');
        expect(linkEdition({ java: true, bedrock: true })).toBe('both');
        expect(linkEdition({ java: false, bedrock: true })).toBe('bedrock');
    });
});

describe('own domains on the managed gateway', () => {
    const b = { routes: [
        { domain: 'play.acme.tld', java: true, bedrock: false, ownDomain: true },
        { domain: 'a.eu.p.com', java: true, bedrock: true },
    ] };

    it('isOwnDomainRoute reads the flag Core sends', () => {
        expect(isOwnDomainRoute(b, 'play.acme.tld')).toBe(true);
        expect(isOwnDomainRoute(b, 'a.eu.p.com')).toBe(false);
        expect(isOwnDomainRoute(b, 'unknown.example.com')).toBe(false);
        expect(isOwnDomainRoute(null, 'play.acme.tld')).toBe(false);
    });

    it('disables the options that add Bedrock and says why, over every other reason', () => {
        const opts = editionOptions('java', true, '', 'a.eu.p.com', true);
        expect(opts.map(o => !!o.disabled)).toEqual([false, true, true]);
        expect(opts[1].hint).toBe(OWN_DOMAIN_EXTRAS_TEXT);
    });

    it('leaves the options alone for an ordinary address', () => {
        expect(editionOptions('java', true, '', null, false)).toEqual(editionOptions('java', true, ''));
    });
});

describe('editionBlockedNote', () => {
    it('says visibly why a second address cannot get Bedrock', () => {
        expect(editionBlockedNote('java', 'both.example.com')).toMatch(/^This server already has a Bedrock address: both\.example\.com\. /);
    });
    it('is the own-domain sentence on an own domain', () => {
        expect(editionBlockedNote('java', null, true)).toBe(OWN_DOMAIN_EXTRAS_TEXT);
    });
    it('is nothing for the address that has it, and in local mode', () => {
        expect(editionBlockedNote('both', 'other.example.com')).toBeNull();
        expect(editionBlockedNote('java')).toBeNull();
    });
});
