import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('./api', () => ({ setRouteEdition: vi.fn() }));
import { setRouteEdition } from './api';
import {
    addBlockedReason, canAddBedrock, changeEdition, editionFlags, editionOf, editionOptions,
    floodgateLink, linkEdition, pausedNotice, showBedrock, bedrockTakenElsewhere, saveEdition,
    editionBlockedNote, canAddBedrockTo, bedrockDNSNote,
} from './bedrock';
import { fmtDate, fmtDateTime } from './time';

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

describe('per-address adds', () => {
    const mixed = { allowed: true, available: false, address: 'a.eu.example.com', enabled: false };

    it('blocks only the address the summary is about', () => {
        expect(canAddBedrockTo(mixed, 'a.eu.example.com')).toBe(false);
        expect(canAddBedrockTo(mixed, 'b.us.example.com')).toBe(true);
    });
    it('blocks every address without one to speak of, or for a caller who may not', () => {
        expect(canAddBedrockTo({ ...mixed, address: '' }, 'b.us.example.com')).toBe(false);
        expect(canAddBedrockTo({ ...mixed, allowed: false }, 'b.us.example.com')).toBe(false);
        expect(canAddBedrockTo({ ...mixed, available: true }, 'a.eu.example.com')).toBe(true);
    });
    it('shows the controls when another address may take it, as before for a single address', () => {
        expect(showBedrock(mixed, ['a.eu.example.com', 'b.us.example.com'])).toBe(true);
        expect(showBedrock(mixed, ['a.eu.example.com'])).toBe(false);
        expect(showBedrock(mixed)).toBe(false);
    });
});

describe('editionBlockedNote', () => {
    it('says visibly why a second address cannot get Bedrock', () => {
        expect(editionBlockedNote('java', 'both.example.com')).toMatch(/^This server already has a Bedrock address: both\.example\.com\. /);
    });
    it('shows the refusal the caller found applies to this address', () => {
        expect(editionBlockedNote('java', null, 'No entry.')).toBe('No entry.');
    });
    it('is nothing for the address that has it, and in local mode', () => {
        expect(editionBlockedNote('both', 'other.example.com')).toBeNull();
        expect(editionBlockedNote('both', null, 'No entry.')).toBeNull();
        expect(editionBlockedNote('java')).toBeNull();
    });
});

describe('editionOptions has no own-domain block', () => {
    it('offers Bedrock on any address the caller may add it to', () => {
        expect(editionOptions('java', true, '', null).some(x => x.disabled)).toBe(false);
    });
});

describe('bedrockDNSNote', () => {
    const at = '2026-11-15T09:30:00Z';
    const since = '2026-11-14T09:00:00Z';
    const missing = (d: string) => `No longer points at the gateway since ${fmtDate(d)}. Bedrock players cannot reach this address until the record is back.`;
    it.each([
        ['hosted ok', { domain: 'play.acme.tld', java: true, bedrock: true, ownDomain: true, dnsStatus: 'ok', dnsCheckedAt: at },
            `DNS checked ${fmtDateTime(at)}: points at the gateway.`],
        ['hosted failing', { domain: 'play.acme.tld', java: true, bedrock: true, ownDomain: true, dnsStatus: 'elsewhere', dnsCheckedAt: at, dnsFailingSince: since },
            missing(since)],
        ['hosted never checked, with Bedrock', { domain: 'play.acme.tld', java: true, bedrock: true, ownDomain: true }, 'Not checked yet.'],
        ['hosted never checked, without Bedrock', { domain: 'play.acme.tld', java: true, bedrock: false, ownDomain: true }, null],
        ['hosted regional', { domain: 'a.eu.example.com', java: true, bedrock: true }, null],
        ['hosted Java only with a stored miss', { domain: 'play.acme.tld', java: true, bedrock: false, ownDomain: true, dnsStatus: 'elsewhere', dnsCheckedAt: at, dnsFailingSince: since }, null],
        ['route-only Java only with a stored miss', { java: true, bedrock: false, has_key: false, dns_status: 'elsewhere', dns_checked_at: at, dns_failing_since: since }, null],
        ['route-only ok', { java: true, bedrock: true, has_key: true, dns_status: 'ok', dns_checked_at: at, dns_failing_since: null },
            `DNS checked ${fmtDateTime(at)}: points at the gateway.`],
        ['route-only failing', { java: true, bedrock: true, has_key: true, dns_status: 'srv_only', dns_checked_at: at, dns_failing_since: since },
            missing(since)],
        ['route-only regional or never checked', { java: true, bedrock: false, has_key: false, dns_status: '', dns_checked_at: null, dns_failing_since: null }, null],
        ['route-only from an older Core', { java: true, bedrock: true, has_key: true }, null],
    ] as const)('%s', (_name, r, want) => {
        expect(bedrockDNSNote(r)).toBe(want);
    });
});
