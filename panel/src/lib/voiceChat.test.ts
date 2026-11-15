import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fallenBackAt, linkRouteVoiceConfig, managedVoiceHelp, showVoiceName, showVoiceSwitch, validVoicePort, voiceAccessDescription, voiceAddress, voiceErrorMessage, voiceNameState, voiceTargetHost, type VoiceNameInput } from './voiceChat';
import { fmtDate, fmtDateTime } from './time';

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
        // no_routes is Core's sentence for a server without any address; its
        // own-domain sentences are passed through (below).
        const generic = "Voice chat needs an address on one of the platform's domains. Add a route first.";
        for (const code of coreCodes()) {
            const fallback = code === 'no_routes' ? generic : 'FALLBACK';
            expect(voiceErrorMessage(code, fallback), code).not.toBe(fallback);
        }
    });

    it('tells the owner what to do for each refusal', () => {
        expect(voiceErrorMessage('voice_not_allowed', 'x')).toBe('Voice chat is not enabled for this server. Ask an admin.');
        expect(voiceErrorMessage('node_too_old', 'x')).toBe("Update this server's node, then try again.");
        expect(voiceErrorMessage('no_routes', "Voice chat needs an address on one of the platform's domains.")).toContain('Add a route');
        expect(voiceErrorMessage('no_relay', 'x')).toContain('No voice relay');
        expect(voiceErrorMessage('no_sub_server', 'x')).toContain('Install the server first');
        expect(voiceErrorMessage('voice_mod_missing', 'x')).toContain('Install Simple Voice Chat or Plasmo Voice');
        expect(voiceErrorMessage('voice_mod_ambiguous', 'x')).toContain('Remove one');
        expect(voiceErrorMessage('voice_pool_full', 'x')).toContain('Every voice port');
    });

    it("shows Core's sentence for an own domain that does not point at the gateway yet, and for a DNS check that could not run", () => {
        const dns = "Voice chat needs to know which region this server's players arrive in. play.example.com does not point at the gateway yet: add a CNAME from play.example.com to the address shown under Your own domains, or add an address on one of the gateway's domains.";
        expect(voiceErrorMessage('no_routes', dns)).toBe(dns);
        const retry = 'The lookup of play.example.com failed right now. Nothing was changed. Try again in a moment.';
        expect(voiceErrorMessage('dns_lookup_failed', retry)).toBe(retry);
        expect(voiceErrorMessage('rate_limited', 'Too many DNS checks. Wait a minute.')).toBe('Too many DNS checks. Wait a minute.');
        // A server with no address at all keeps the sentence it had.
        expect(voiceErrorMessage('no_routes', "Voice chat needs an address on one of the platform's domains. Add a route first."))
            .toBe("Add a route on one of the platform's domains first, then try again.");
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

describe('voiceTargetHost', () => {
    // The name the voice mod is told is the one Core answers: our regional
    // voice name for an own domain. Only an older Core, which sends no host,
    // falls back to the address.
    it.each([
        ['own domain, host present', 'play.example.com', { host: 'voice.eu.dylaris.com' }, 'voice.eu.dylaris.com'],
        ['own domain, host absent (older Core)', 'play.example.com', {}, 'play.example.com'],
        ['regional domain, host present', 'a.eu.dylaris.com', { host: 'a.eu.dylaris.com' }, 'a.eu.dylaris.com'],
        ['regional domain, host absent', 'a.eu.dylaris.com', {}, 'a.eu.dylaris.com'],
        ['empty host', 'play.example.com', { host: '' }, 'play.example.com'],
        ['no voice at all', 'play.example.com', undefined, 'play.example.com'],
        ['white-label name active: mod_host wins', 'play.example.com', { host: 'voice.eu.dylaris.com', mod_host: 'voice.play.example.com' }, 'voice.play.example.com'],
        ['pending or fallen back: mod_host is ours again', 'play.example.com', { host: 'voice.eu.dylaris.com', mod_host: 'voice.eu.dylaris.com' }, 'voice.eu.dylaris.com'],
        ['regional address: mod_host is the address', 'a.eu.dylaris.com', { mod_host: 'a.eu.dylaris.com' }, 'a.eu.dylaris.com'],
    ])('%s', (_name, domain, voice, want) => {
        expect(voiceTargetHost(voice, domain)).toBe(want);
    });

    it('is what the route-only config lines name', () => {
        const c = linkRouteVoiceConfig(voiceTargetHost({ host: 'voice.eu.dylaris.com' }, 'play.example.com'), 26003, 24454);
        expect(c.svc).toBe('port=24454\nvoice_host=voice.eu.dylaris.com:26003');
        expect(c.plasmo).toContain('ip = "voice.eu.dylaris.com"');
        expect(c.svc + c.plasmo).not.toContain('play.example.com');
    });
});

describe('linkRouteVoiceConfig with mod_host', () => {
    it.each([
        ['active', 'voice.play.example.com'],
        ['fallen back', 'voice.eu.dylaris.com'],
    ])('names the mod host (%s)', (_s, mod) => {
        const c = linkRouteVoiceConfig(voiceTargetHost({ host: 'voice.eu.dylaris.com', mod_host: mod }, 'play.example.com'), 26003, 24454);
        expect(c.svc).toBe(`port=24454
voice_host=${mod}:26003`);
        expect(c.plasmo).toContain(`ip = "${mod}"`);
    });
});

describe('voiceNameState', () => {
    const at = '2026-11-15T10:30:00Z';
    const base: VoiceNameInput = { ourHost: 'voice.eu.dylaris.com', name: 'voice.play.example.com', port: 26003 };
    it.each<[string, VoiceNameInput, string, string, string[]]>([
        ['default', { ...base, state: '', name: '' }, 'neutral',
            'Voice chat address: voice.eu.dylaris.com:26003. Players keep joining through your own address.', ['useOwn']],
        ['default from an older Core (no state)', { ...base, state: undefined, name: undefined }, 'neutral',
            'Voice chat address: voice.eu.dylaris.com:26003. Players keep joining through your own address.', ['useOwn']],
        ['pending with a target and a last check', { ...base, state: 'pending', target: 'voice.eu.example-hoster.com', lastCheck: 'voice.play.example.com does not point at the gateway yet.' }, 'info',
            'Waiting for your DNS record. Create: voice.play.example.com CNAME voice.eu.example-hoster.com. Last check: voice.play.example.com does not point at the gateway yet.', ['check', 'cancel']],
        ['pending without a target or a check: our name is the target', { ...base, state: 'pending' }, 'info',
            'Waiting for your DNS record. Create: voice.play.example.com CNAME voice.eu.dylaris.com.', ['check', 'cancel']],
        ['pending, a last check without a full stop', { ...base, state: 'pending', lastCheck: 'Not found' }, 'info',
            'Waiting for your DNS record. Create: voice.play.example.com CNAME voice.eu.dylaris.com. Last check: Not found.', ['check', 'cancel']],
        ['active', { ...base, state: 'active' }, 'success',
            'Voice chat address: voice.play.example.com:26003 (your own domain).', ['remove']],
        ['active, failing', { ...base, state: 'active', failingSince: at }, 'warning',
            `Your record voice.play.example.com stopped pointing at the gateway at ${fmtDateTime(at)}. The gateway is checking again. If it stays that way, voice chat switches back to voice.eu.dylaris.com.`, ['remove']],
        ['fallen back with a date', { ...base, state: 'fallen_back', fallenBackAt: at }, 'warning',
            `Voice chat was switched back to voice.eu.dylaris.com on ${fmtDate(at)} because voice.play.example.com no longer points at the gateway. Restart the server to apply it. Fix the record, then press Check again to use your own name.`, ['restart', 'check', 'remove']],
        ['fallen back without a date', { ...base, state: 'fallen_back' }, 'warning',
            'Voice chat was switched back to voice.eu.dylaris.com because voice.play.example.com no longer points at the gateway. Restart the server to apply it. Fix the record, then press Check again to use your own name.', ['restart', 'check', 'remove']],
        ['route-only fallen back: change the line by hand, no restart button', { ...base, state: 'fallen_back', routeOnly: true }, 'warning',
            "Voice chat was switched back to voice.eu.dylaris.com because voice.play.example.com no longer points at the gateway. Change the voice host in your voice mod's config to the line below by hand, then restart the server. Fix the record, then press Check again to use your own name.", ['check', 'remove']],
        // The state is read first: a failing stamp on a row that is not active is no failing state.
        ['pending with a stray failing stamp', { ...base, state: 'pending', failingSince: at }, 'info',
            'Waiting for your DNS record. Create: voice.play.example.com CNAME voice.eu.dylaris.com.', ['check', 'cancel']],
    ])('%s', (_n, input, tone, text, actions) => {
        const got = voiceNameState(input);
        expect(got.tone).toBe(tone);
        expect(got.text).toBe(text);
        expect(got.actions).toEqual(actions);
    });

    it('never says us or our to a customer of a hoster', () => {
        const states: VoiceNameInput[] = ['', 'pending', 'active', 'fallen_back'].map(state => ({ ...base, state, failingSince: at }));
        for (const st of states) expect(voiceNameState(st).text).not.toMatch(/\b(us|our|we)\b/i);
    });
});

describe('showVoiceName', () => {
    it.each<[string, string | undefined, string | undefined, boolean, boolean]>([
        ['no name possible here', '', '', true, false],
        ['own domain: offered', '', 'voice.eu.dylaris.com', false, true],
        ['older Core: the guess', '', undefined, true, true],
        // Hosted passes legacy false: a missing nameTarget (an older provider,
        // a failed read) hides the offer, an existing name still shows.
        ['hosted, no nameTarget, no name: hidden', '', undefined, false, false],
        ['hosted, no nameTarget, active name: shown', 'active', undefined, false, true],
        ['older Core, regional route-only address', undefined, undefined, false, false],
        ['a name exists although none is possible now: still shown, so it can go', 'active', '', false, true],
        ['pending', 'pending', '', false, true],
    ])('%s', (_n, state, target, legacy, want) => {
        expect(showVoiceName(state, target, legacy)).toBe(want);
    });
});

describe('fallenBackAt', () => {
    it('is the state time only while fallen back', () => {
        const at = '2026-11-15T10:30:00Z';
        expect(fallenBackAt('fallen_back', at)).toBe(at);
        expect(fallenBackAt('active', at)).toBeUndefined();
        expect(fallenBackAt('fallen_back', null)).toBeUndefined();
        expect(fallenBackAt('fallen_back', undefined)).toBeUndefined();
    });
});

describe('managedVoiceHelp', () => {
    it('offers voice on an own domain and says the address is the provider\'s name and a restart is needed', () => {
        const t = managedVoiceHelp(true);
        expect(t).toContain("On your own domain, the voice address is on the gateway provider's domain; your DNS needs no change.");
        expect(t).toContain('Restart the server after switching it on.');
        expect(t).not.toContain('not on an own domain');
    });

    it('keeps the hint for a voice chat that went off on its own', () => {
        expect(managedVoiceHelp(false)).toContain('If it went off on its own');
        expect(managedVoiceHelp(true)).not.toContain('If it went off on its own');
    });
});

describe('voiceAccessDescription', () => {
    it('keeps the local sentence and gives the managed gateway its own timing', () => {
        expect(voiceAccessDescription(false)).toBe('Overrides the voice chat switch above for one server, user or role. Server beats user beats role. A deny turns voice off on servers that have it within a minute.');
        const managed = voiceAccessDescription(true);
        expect(managed).toContain('within about five minutes');
        expect(managed).not.toContain('within a minute');
    });
});
