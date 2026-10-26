import { describe, it, expect } from 'vitest';
import { proxyConfigFilename, backendAddress, proxyPrereqHint, serverHostname } from './proxyConfig';

describe('proxyConfigFilename', () => {
    it('velocity -> velocity.toml (case-insensitive)', () => {
        expect(proxyConfigFilename('velocity')).toBe('velocity.toml');
        expect(proxyConfigFilename('Velocity')).toBe('velocity.toml');
    });
    it('bungeecord / waterfall / unknown / empty -> config.yml', () => {
        for (const s of ['bungeecord', 'waterfall', 'paper', '', undefined, null]) {
            expect(proxyConfigFilename(s as string | undefined | null)).toBe('config.yml');
        }
    });
});

describe('backendAddress', () => {
    it('builds mc_<uuid>:25565 by default', () => {
        expect(backendAddress('abc')).toBe('mc_abc:25565');
    });
    it('uses a custom container port when set', () => {
        expect(backendAddress('abc', 30000)).toBe('mc_abc:30000');
    });
    it('falls back to 25565 for 0 / undefined / null', () => {
        expect(backendAddress('abc', 0)).toBe('mc_abc:25565');
        expect(backendAddress('abc', undefined)).toBe('mc_abc:25565');
        expect(backendAddress('abc', null)).toBe('mc_abc:25565');
    });
});

describe('serverHostname', () => {
    // The same cases as TestServerHostname in platform/pkg/validate.
    it('matches the alias the node registers', () => {
        expect(serverHostname('befa5934-e5f8-4bb0-b35d-df8da14fc003_d59de1665a')).toBe('mc-befa5934-e5f8-4bb0-b35d-df8da14fc003-d59de1665a');
        expect(serverHostname('Owner_ABC12345')).toBe('mc-owner-abc12345');
        expect(serverHostname('abcdefgh_')).toBe('');
        expect(serverHostname('a'.repeat(61))).toBe('');
        expect(serverHostname('a'.repeat(60))).toBe('mc-' + 'a'.repeat(60));
        expect(serverHostname('../etc/passwd')).toBe('');
    });
    it('is what backendAddress hands out', () => {
        expect(backendAddress('Owner_ABC12345', 30000)).toBe('mc-owner-abc12345:30000');
    });
});

describe('proxyPrereqHint', () => {
    it('velocity mentions forwarding.secret', () => {
        expect(proxyPrereqHint('velocity').toLowerCase()).toContain('forwarding.secret');
    });
    it('bungeecord mentions bungeecord', () => {
        expect(proxyPrereqHint('bungeecord').toLowerCase()).toContain('bungeecord');
    });
    it('both mention online-mode', () => {
        expect(proxyPrereqHint('velocity')).toContain('online-mode');
        expect(proxyPrereqHint('bungeecord')).toContain('online-mode');
    });
});
