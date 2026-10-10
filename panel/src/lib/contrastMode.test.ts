import { describe, it, expect } from 'vitest';
import { CONTRAST_BOOTSTRAP, CONTRAST_KEY, wantsHighContrast } from './contrastMode';

// Runs the inline bootstrap string the way the browser would, against stubbed
// globals, and returns the attribute it left on <html>.
function boot(stored: string | null | 'throw', prefersMore: boolean): string | null {
    const attrs: Record<string, string> = {};
    const localStorage = {
        getItem: (k: string) => {
            if (stored === 'throw') throw new Error('SecurityError');
            return k === CONTRAST_KEY ? stored : null;
        },
    };
    const window = { matchMedia: (q: string) => ({ matches: q === '(prefers-contrast: more)' && prefersMore }) };
    const document = { documentElement: { setAttribute: (k: string, v: string) => { attrs[k] = v; } } };
    new Function('localStorage', 'window', 'document', CONTRAST_BOOTSTRAP)(localStorage, window, document);
    return attrs['data-contrast'] ?? null;
}

describe('high-contrast preference', () => {
    const cases: [string | null, boolean, boolean][] = [
        ['high', false, true],
        ['high', true, true],
        ['normal', true, false],
        ['normal', false, false],
        [null, true, true],
        [null, false, false],
        ['garbage', true, true],
    ];

    it.each(cases)('stored %s, OS prefers more %s -> high %s', (stored, more, want) => {
        expect(wantsHighContrast(stored, more)).toBe(want);
        expect(boot(stored, more)).toBe(want ? 'high' : null);
    });

    it('falls back to the OS preference when storage throws', () => {
        expect(boot('throw', true)).toBe('high');
        expect(boot('throw', false)).toBe(null);
    });
});
