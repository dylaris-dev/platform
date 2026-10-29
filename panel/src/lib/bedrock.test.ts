import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('./api', () => ({ setServerBedrock: vi.fn() }));
import { setServerBedrock } from './api';
import { changeBedrock, floodgateLink, showBedrockSwitch } from './bedrock';

const set = vi.mocked(setServerBedrock);
beforeEach(() => set.mockReset());

describe('showBedrockSwitch', () => {
    it('is hidden when unavailable and off', () => {
        expect(showBedrockSwitch({ available: false, enabled: false })).toBe(false);
    });
    it('is shown when available, or when on but no longer available', () => {
        expect(showBedrockSwitch({ available: true, enabled: false })).toBe(true);
        expect(showBedrockSwitch({ available: false, enabled: true })).toBe(true);
    });
});

describe('floodgateLink', () => {
    it('points plugins at Floodgate for Spigot/Paper and mods at Floodgate-Modded', () => {
        expect(floodgateLink('plugins').href).toBe('https://geysermc.org/download?project=floodgate');
        expect(floodgateLink('mods').href).toBe('https://modrinth.com/mod/floodgate');
    });
});

describe('changeBedrock', () => {
    it.each([true, false])('calls setServerBedrock with %s', async next => {
        set.mockResolvedValue({ success: true });
        expect(await changeBedrock(7, next)).toBeNull();
        expect(set).toHaveBeenCalledWith(7, next);
    });
    it('returns the 409 message from Core', async () => {
        set.mockResolvedValue({ success: false, code: 'bedrock_unavailable', message: 'Bedrock needs the gateway.' });
        expect(await changeBedrock(7, true)).toBe('Bedrock needs the gateway.');
    });
});
