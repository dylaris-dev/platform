import { describe, expect, it } from 'vitest';

import { tabHost } from './tabHost';

describe('tabHost', () => {
    it('names the host of a normal tab URL', () => {
        expect(tabHost('https://map.example.com/live/')).toBe('map.example.com');
        expect(tabHost('http://10.0.0.5:8100/')).toBe('10.0.0.5:8100');
    });

    // The whole point of showing a host: a URL built to READ like one domain
    // while loading another. new URL resolves it the way the browser will.
    it('names the host the browser will actually load', () => {
        expect(tabHost('https://panel.dylaris.com@evil.example/')).toBe('evil.example');
    });

    // Core refuses these, so reaching here means something upstream changed.
    // Returning "" makes the notice generic rather than absent.
    it('names nothing for a non-web or unparseable URL', () => {
        expect(tabHost('javascript:alert(1)')).toBe('');
        expect(tabHost('data:text/html,<h1>hi')).toBe('');
        expect(tabHost('not a url')).toBe('');
        expect(tabHost('')).toBe('');
    });
});
