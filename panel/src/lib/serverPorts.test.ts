import { describe, expect, it } from 'vitest';
import { formatMaxPerUser, parseHostPort, parseMaxPerUser, portAddress, portUsage, portsError } from './serverPorts';

describe('serverPorts helpers', () => {
    it('builds the address', () => {
        expect(portAddress('1.2.3.4', 26001)).toBe('1.2.3.4:26001');
        expect(portAddress('', 26001)).toBe('26001');
    });

    it('reads used against the three limit states', () => {
        expect(portUsage(1, null)).toBe('1 / no limit');
        expect(portUsage(0, 0)).toBe('0 / none');
        expect(portUsage(1, 2)).toBe('1 / 2');
    });

    it('round-trips the per-user setting, 0 staying none', () => {
        expect(parseMaxPerUser('unlimited')).toBeNull();
        expect(parseMaxPerUser('0')).toBe(0);
        expect(parseMaxPerUser('5')).toBe(5);
        expect(parseMaxPerUser('')).toBe(2);
        expect(parseMaxPerUser('x')).toBe(2);
        expect(formatMaxPerUser(null)).toBe('unlimited');
        expect(formatMaxPerUser(0)).toBe('0');
    });

    it('parses an admin host port', () => {
        expect(parseHostPort('')).toBeUndefined();
        expect(parseHostPort('26010')).toBe(26010);
        expect(parseHostPort('80')).toBe('invalid');
        expect(parseHostPort('70000')).toBe('invalid');
        expect(parseHostPort('1.5')).toBe('invalid');
    });

    it('prefers the sentence Core sent', () => {
        expect(portsError({ error: 'Port taken.' }, 'F')).toBe('Port taken.');
        expect(portsError(null, 'F')).toBe('F');
    });
});
