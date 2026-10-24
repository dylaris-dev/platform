import { describe, it, expect } from 'vitest';
import { mapRenderCommand } from './mapRender';

describe('mapRenderCommand', () => {
    it('renders the named world with squaremap', () => {
        expect(mapRenderCommand('squaremap', 'survival_2')).toBe('map fullrender survival_2');
    });
    it('falls back to "world" for a name that is not plain', () => {
        expect(mapRenderCommand('squaremap', '')).toBe('map fullrender world');
        expect(mapRenderCommand('squaremap', 'a; op me')).toBe('map fullrender world');
    });
    it('uses bluemap update for BlueMap', () => {
        expect(mapRenderCommand('bluemap', 'world')).toBe('bluemap update');
    });
});
