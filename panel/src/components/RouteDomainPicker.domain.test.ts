import { describe, expect, it } from 'vitest';
import { customDomainOk, normHost } from './RouteDomainPicker';

// Core drops the trailing dot of a fully qualified name and checks every label;
// the picker agrees, so it neither refuses what Core takes nor checks
// availability for a name Core will refuse.
describe('custom domain format', () => {
    it('drops the trailing dot', () => {
        expect(normHost(' Play.Example.com. ')).toBe('play.example.com');
        expect(customDomainOk(normHost('play.example.com.'))).toBe(true);
    });

    it('refuses what the old pattern let through', () => {
        for (const d of ['a..b.com', 'a.-b.com', 'a-.com', 'localhost', 'a.b.c.d.com', `x.${'a'.repeat(64)}.com`]) {
            expect(customDomainOk(d), d).toBe(false);
        }
    });
});
