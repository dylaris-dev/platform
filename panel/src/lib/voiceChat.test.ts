import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { linkRouteVoiceConfig, showVoiceSwitch, validVoicePort, voiceAddress, voiceErrorMessage } from './voiceChat';

/** Every 409 code Core's enable can answer, read from the handler rather than
 *  restated, so a new refusal without a panel sentence turns this red. */
function coreCodes(): string[] {
    const src = readFileSync(join(__dirname, '../../../core/handlers/gateway/gateway_voice.go'), 'utf8');
    // handlers/gateway calls it as handlers.SendJSONErrorCode.
    const codes = [...src.matchAll(/[sS]endJSONErrorCode\([^)]*?"([a-z_]+)", http\.StatusConflict\)/g)].map(m => m[1]);
    expect(codes.length, 'no codes found in gateway_voice.go - this test stopped reading it').toBeGreaterThan(5);
    return codes;
}

describe('voiceErrorMessage', () => {
    it('has its own sentence for every refusal Core sends', () => {
        for (const code of coreCodes()) {
            expect(voiceErrorMessage(code, 'FALLBACK'), code).not.toBe('FALLBACK');
        }
    });

    it('tells the owner what to do for each refusal', () => {
        expect(voiceErrorMessage('voice_not_allowed', 'x')).toBe('Voice chat is not enabled for this server. Ask an admin.');
        expect(voiceErrorMessage('node_too_old', 'x')).toBe("Update this server's node, then try again.");
        expect(voiceErrorMessage('no_routes', 'x')).toContain('Add a route');
        expect(voiceErrorMessage('no_relay', 'x')).toContain('No voice relay');
        expect(voiceErrorMessage('no_sub_server', 'x')).toContain('Install the server first');
        expect(voiceErrorMessage('voice_mod_missing', 'x')).toContain('Install Simple Voice Chat or Plasmo Voice');
        expect(voiceErrorMessage('voice_mod_ambiguous', 'x')).toContain('Remove one');
        expect(voiceErrorMessage('voice_pool_full', 'x')).toContain('Every voice port');
    });

    it("falls back to Core's message for an unknown or absent code", () => {
        expect(voiceErrorMessage('something_new', 'Core said this.')).toBe('Core said this.');
        expect(voiceErrorMessage(undefined, 'Core said this.')).toBe('Core said this.');
    });
});

describe('voiceAddress', () => {
    it('joins host and port', () => {
        expect(voiceAddress({ host: 'a.eu.dylaris.com', port: 26000 })).toBe('a.eu.dylaris.com:26000');
    });
});

describe('showVoiceSwitch', () => {
    it('is hidden while the server may not turn voice on', () => {
        expect(showVoiceSwitch(false, false)).toBe(false);
    });

    it('shows when the server may turn voice on', () => {
        expect(showVoiceSwitch(true, false)).toBe(true);
    });

    it('stays visible while voice is on, so it can always be turned off', () => {
        expect(showVoiceSwitch(false, true)).toBe(true);
    });
});

describe('linkRouteVoiceConfig', () => {
    it('names the mod port and the public address, as Core writes for a hosted server', () => {
        const c = linkRouteVoiceConfig('a.eu.dylaris.com', 26003, 24454);
        expect(c.svc).toBe('port=24454\nvoice_host=a.eu.dylaris.com:26003');
        expect(c.plasmo).toBe('[host]\nport = 24454\n\n[host.public]\nip = "a.eu.dylaris.com"\nport = 26003');
    });
});

describe('validVoicePort', () => {
    it('accepts 1-65535 integers only', () => {
        expect(validVoicePort(24454)).toBe(true);
        expect([0, 65536, 1.5, NaN, -1].some(validVoicePort)).toBe(false);
    });
});

describe('voiceErrorMessage for route-only', () => {
    it('explains an old link', () => {
        expect(voiceErrorMessage('link_without_voice', 'x')).toContain('up to date');
    });
});
