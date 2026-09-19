import { describe, expect, it } from 'vitest';
import { technicInstaller, technicNotice } from './technic';

describe('technicNotice', () => {
    it('installs a server pack without a warning', () => {
        const n = technicNotice('server');
        expect(n.tone).toBe('info');
        expect(n.installable).toBe(true);
    });

    it.each(['client-zip', 'client-solder'] as const)('warns about the client pack for %s', (v) => {
        const n = technicNotice(v);
        expect(n.tone).toBe('warning');
        expect(n.installable).toBe(true);
        expect(n.text).toContain('client pack will be installed');
    });

    it('refuses a pack with nothing to install', () => {
        expect(technicNotice('none').installable).toBe(false);
    });
});

describe('technicInstaller', () => {
    it('sends only the slug, the build and the version, never a URL', () => {
        const inst = technicInstaller({ slug: 'tekkit', displayName: 'Tekkit', variant: 'server', mcVersion: '1.4.7' });
        expect(inst).toEqual({ type: 'technic', technicSlug: 'tekkit', mcVersion: '1.4.7' });
    });

    it('carries a chosen Solder build', () => {
        const inst = technicInstaller({ slug: 'p', displayName: 'P', variant: 'client-solder', build: '1.0.1' });
        expect(inst.technicBuild).toBe('1.0.1');
    });
});
