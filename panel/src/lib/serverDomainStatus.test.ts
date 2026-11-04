import { describe, it, expect } from 'vitest';
import { statusStyle, primaryAddress, domainAccess, checkedAgo, previewFqdn } from './serverDomainStatus';

const d = (fqdn: string, state: 'grey' | 'yellow' | 'green' | 'paused') => ({ fqdn, state });

describe('statusStyle', () => {
    it('maps all four states', () => {
        expect(statusStyle('grey')).toEqual({ label: 'Not resolving yet', tone: 'neutral' });
        expect(statusStyle('yellow')).toEqual({ label: 'Needs attention', tone: 'warning' });
        expect(statusStyle('green')).toEqual({ label: 'Ready', tone: 'accent' });
        expect(statusStyle('paused')).toEqual({ label: 'Paused: gateway routing', tone: 'neutral' });
    });
});

describe('primaryAddress', () => {
    it('prefers the first green domain', () => {
        expect(primaryAddress([d('a.x.io', 'yellow'), d('b.x.io', 'green'), d('c.x.io', 'green')], '1.2.3.4:25565')).toBe('b.x.io');
    });
    it('falls back to ip:port, never a yellow/grey/paused domain', () => {
        expect(primaryAddress([d('a.x.io', 'yellow'), d('b.x.io', 'grey'), d('c.x.io', 'paused')], '1.2.3.4:25565')).toBe('1.2.3.4:25565');
        expect(primaryAddress([], '1.2.3.4:25565')).toBe('1.2.3.4:25565');
    });
});

describe('domainAccess', () => {
    it('owner and admin rows may do everything', () => {
        expect(domainAccess({ role: 'owner' })).toEqual({ read: true, write: true });
        expect(domainAccess({ role: 'admin' })).toEqual({ read: true, write: true });
    });
    it('members need the resolved bits; demo gets nothing', () => {
        expect(domainAccess({ role: 'invited', permissions: { domains: true } as never })).toEqual({ read: true, write: false });
        expect(domainAccess({ role: 'invited', permissions: { domains: true, domainsWrite: true } as never })).toEqual({ read: true, write: true });
        expect(domainAccess({ role: 'invited' })).toEqual({ read: false, write: false });
        expect(domainAccess({ role: 'demo' })).toEqual({ read: false, write: false });
    });
});

describe('checkedAgo', () => {
    const now = Date.parse('2026-11-04T12:00:30Z');
    it('formats seconds, minutes, hours and never', () => {
        expect(checkedAgo('2026-11-04T12:00:00Z', now)).toBe('checked 30 s ago');
        expect(checkedAgo('2026-11-04T11:57:30Z', now)).toBe('checked 3 min ago');
        expect(checkedAgo('2026-11-04T09:00:30Z', now)).toBe('checked 3 h ago');
        expect(checkedAgo('', now)).toBe('not checked yet');
        expect(checkedAgo('0001-01-01T00:00:00Z', now)).toBe('not checked yet');
    });
});

describe('previewFqdn', () => {
    it('joins label and zone, empty until both exist', () => {
        expect(previewFqdn('play', 'example.com')).toBe('play.example.com');
        expect(previewFqdn(' Play ', 'example.com')).toBe('play.example.com');
        expect(previewFqdn('', 'example.com')).toBe('');
        expect(previewFqdn('play', '')).toBe('');
    });
});
