import { describe, expect, it } from 'vitest';
import { plannedSetupRoute } from './setupRoute';

describe('plannedSetupRoute', () => {
    it('creates a route for a picked subdomain', () => {
        const p = plannedSetupRoute({ targetPort: 25565, subdomain: 'Play', hosterDomain: 'example.com' }, []);
        expect(p).toEqual({ hasDomain: true, effectiveDomain: 'play.example.com', alreadyOurs: false, createsRoute: true });
    });
    it('creates a route for a custom domain', () => {
        expect(plannedSetupRoute({ targetPort: 25565, customDomain: 'mc.mine.net' }, []).createsRoute).toBe(true);
    });
    it('creates none without a domain', () => {
        expect(plannedSetupRoute({ targetPort: 25565 }, []).createsRoute).toBe(false);
    });
    it('creates none when the domain is already this server\'s', () => {
        const p = plannedSetupRoute({ targetPort: 25565, subdomain: 'play', hosterDomain: 'example.com' }, [{ domain: 'PLAY.example.com' }]);
        expect(p.alreadyOurs).toBe(true);
        expect(p.createsRoute).toBe(false);
    });
});
