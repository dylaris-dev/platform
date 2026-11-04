import { describe, it, expect } from 'vitest';
import {
    credentialFieldsFor, orphanMessage, intervalError, MIN_INTERVAL_SECONDS,
    parseLimit, formatLimit, principalOf, splitPrincipal,
} from './dnsTabHelpers';
import type { DnsKind } from '@/lib/api/serverDomains';

const kinds: DnsKind[] = [
    { id: 'cloudflare', label: 'Cloudflare', zoneListing: true, fields: [{ key: 'apiToken', label: 'API token', secret: true, optional: false }] },
    { id: 'inwx', label: 'INWX', zoneListing: false, fields: [
        { key: 'user', label: 'Username', secret: false, optional: false },
        { key: 'otp', label: '2FA secret', secret: true, optional: true },
    ] },
];

describe('credentialFieldsFor', () => {
    it('returns the fields of the chosen kind', () => {
        expect(credentialFieldsFor(kinds, 'inwx').map(f => f.key)).toEqual(['user', 'otp']);
    });
    it('returns nothing for an unknown kind', () => {
        expect(credentialFieldsFor(kinds, 'nope')).toEqual([]);
    });
});

describe('orphanMessage', () => {
    it('lists every name', () => {
        const m = orphanMessage(['a.example.com', '_minecraft._tcp.b.example.com']);
        expect(m).toContain('a.example.com');
        expect(m).toContain('_minecraft._tcp.b.example.com');
    });
    it('is empty when nothing was left behind', () => {
        expect(orphanMessage([])).toBe('');
    });
});

describe('intervalError', () => {
    it('refuses below the minimum', () => {
        expect(intervalError('29')).toBe('Minimum is 30 seconds');
        expect(MIN_INTERVAL_SECONDS).toBe(30);
    });
    it('accepts the minimum and above', () => {
        expect(intervalError('30')).toBe('');
        expect(intervalError('300')).toBe('');
    });
    it('refuses non-numbers and fractions', () => {
        expect(intervalError('')).not.toBe('');
        expect(intervalError('30.5')).not.toBe('');
    });
});

describe('limit strings', () => {
    it('maps the API strings to the LimitField convention', () => {
        expect(parseLimit('unlimited')).toBeNull();
        expect(parseLimit('')).toBeNull();
        expect(parseLimit('0')).toBe(0);
        expect(parseLimit('5')).toBe(5);
    });
    it('round-trips', () => {
        expect(formatLimit(null)).toBe('unlimited');
        expect(formatLimit(0)).toBe('0');
        expect(formatLimit(7)).toBe('7');
    });
});

describe('principals', () => {
    it('builds and splits', () => {
        expect(principalOf('user', '12')).toBe('user:12');
        expect(splitPrincipal('role:3')).toEqual({ kind: 'role', id: '3' });
        expect(splitPrincipal('garbage')).toBeNull();
    });
});
