import { describe, it, expect } from 'vitest';
import { compareVersionsDesc } from './VersionPicker';

describe('compareVersionsDesc', () => {
    // Each pair: a must sort ABOVE b (newest first).
    const above: [string, string][] = [
        ['1.21.11', '1.21.2'],
        ['26.3', '1.21.11'],
        ['26.1.2', '26.1'],
        // A release and its candidates used to tie: parseInt read "11-rc3" as 11.
        ['1.21.11', '1.21.11-rc3'],
        ['1.21.11-rc3', '1.21.11-pre5'],
        ['1.21.11-pre5', '1.21.10'],
        ['1.21-pre1', '1.20.6'],
        ['26.1.0.0-alpha.10+snapshot-6', '26.1.0.0-alpha.2+snapshot-1'],
        ['1.21.1-52.0.16', '1.21.1-52.0.9'],
        ['3.5.1', '3.4.0'],
    ];
    it.each(above)('%s sorts above %s', (a, b) => {
        expect(compareVersionsDesc(a, b)).toBeLessThan(0);
        expect(compareVersionsDesc(b, a)).toBeGreaterThan(0);
    });

    it('treats a missing component as zero', () => {
        expect(compareVersionsDesc('1.21', '1.21.0')).toBe(0);
    });

    it('sorts a mixed list newest first', () => {
        const list = ['1.21.10', '1.21.11-rc3', '1.21.2', '1.21.11', '26.3', '1.21.11-pre5'];
        expect([...list].sort(compareVersionsDesc)).toEqual(['26.3', '1.21.11', '1.21.11-rc3', '1.21.11-pre5', '1.21.10', '1.21.2']);
    });
});
