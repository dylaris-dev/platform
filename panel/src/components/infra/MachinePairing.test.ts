import { describe, expect, it } from 'vitest';
import { normalizeFingerprint } from './MachinePairing';

// The owner copies the fingerprint out of a log line; whatever case and dashes
// it arrives with, Core has to get the hex it binds the admission to.
describe('normalizeFingerprint', () => {
    it('keeps the hex a person copied', () => {
        expect(normalizeFingerprint(' ABCD-EF01-2345-6789 ')).toBe('abcdef0123456789');
    });
    it('refuses anything that is not hex', () => {
        expect(normalizeFingerprint('abcd-zzzz')).toBe('');
    });
});
