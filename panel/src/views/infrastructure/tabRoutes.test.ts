import { describe, expect, it } from 'vitest';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * The infrastructure tabs became real URLs, and that changed what "hidden"
 * means.
 *
 * Before, a tab that should not exist had no button, and NOT DRAWING IT was the
 * whole check - there was no other way in. Now every tab is one typed address
 * away, so a page whose only protection was a missing button is a page with no
 * protection. This is the same shape as the SFTP surface that read a grant table
 * instead of the resolver: the guard sat where the door was described rather
 * than on the door.
 *
 * Two things are asserted, and neither is visible on screen when it breaks:
 * every tab the bar can draw has a page behind it, and every gated tab's page
 * refuses on its own.
 */

const APP = join(__dirname, '../../app/(authed)/infrastructure');
const SHELL = readFileSync(join(__dirname, 'Shell.tsx'), 'utf8');

/** The slugs the tab bar can render, read from the shell rather than restated. */
function shellSlugs(): string[] {
    const out = [...SHELL.matchAll(/slug:\s*'([a-z-]+)'/g)].map(m => m[1]);
    // A restatement of the list would pass forever after somebody adds a tab.
    expect(out.length, 'no slugs found in Shell.tsx - this test stopped reading it').toBeGreaterThan(0);
    return out;
}

/** Tabs that must not render when their backend is absent, and what gates them. */
const GATED: Record<string, string> = {
    gateway: 'gatewayDeployed',
    bandwidth: 'gatewayEnabled',
    voice: 'gatewayEnabled',
};

describe('infrastructure tab routes', () => {
    it('every tab the bar can draw has a page behind it', () => {
        for (const slug of shellSlugs()) {
            expect(existsSync(join(APP, slug, 'page.tsx')), `no page for tab "${slug}"`).toBe(true);
        }
    });

    it('includes the three node kinds as separate tabs', () => {
        const slugs = shellSlugs();
        for (const s of ['nodes', 'external', 'byon']) expect(slugs).toContain(s);
    });

    it('the three node pages ask for different kinds', () => {
        const kindOf = (slug: string) =>
            readFileSync(join(APP, slug, 'page.tsx'), 'utf8').match(/kind="([a-z]+)"/)?.[1];
        expect(kindOf('nodes')).toBe('platform');
        expect(kindOf('external')).toBe('external');
        expect(kindOf('byon')).toBe('byon');
    });

    it('every gated page guards itself rather than relying on the tab bar', () => {
        for (const [slug, gate] of Object.entries(GATED)) {
            const src = readFileSync(join(APP, slug, 'page.tsx'), 'utf8');
            expect(src, `${slug} does not read ${gate}`).toContain(gate);
            // The guard itself, not the import: a page that imports TabGuard
            // and never returns it passed the old substring check.
            expect(src, `${slug} does not refuse when ${gate} is false`).toMatch(new RegExp(`if \\(!${gate}\\) \\{\\s*return <TabGuard`));
        }
    });

    it('the addresses that moved still redirect', () => {
        // Both were real URLs somebody could have bookmarked or pasted into a
        // ticket. Deleting the page instead of redirecting turns those into a
        // 404 with no clue where the screen went.
        const edges = readFileSync(join(APP, 'edges', 'page.tsx'), 'utf8');
        expect(edges, 'the old Edges address does not redirect').toContain('redirect(');
        expect(edges).toContain('/infrastructure/gateway');

        const routes = readFileSync(join(APP, 'routes', 'page.tsx'), 'utf8');
        expect(routes, 'the old Routes address does not redirect').toContain('redirect(');
        expect(routes).toContain('/admin/routes');
    });

    it('routes live under admin and still refuse without a gateway', () => {
        // The tab bar is a static list there, so the page is the only thing
        // that can tell an operator why the screen is empty.
        const page = readFileSync(join(__dirname, '../../app/(authed)/admin/routes/page.tsx'), 'utf8');
        expect(page).toContain('RoutesPanel');
        expect(page, 'the admin routes page lists nothing and explains nothing without a gateway')
            .toContain('none is deployed');

        const layout = readFileSync(join(__dirname, '../../app/(authed)/admin/layout.tsx'), 'utf8');
        expect(layout, 'the Routes tab is not in the admin tab bar').toContain("slug: 'routes'");
    });

    it('the index redirects rather than becoming an address of its own', () => {
        // Two URLs showing one screen is how a link somebody shared stops
        // matching the tab that is highlighted.
        const idx = readFileSync(join(APP, 'page.tsx'), 'utf8');
        expect(idx).toContain('redirect(');
        expect(idx).toContain('/infrastructure/nodes');
    });

    it('the admin check and the single fetch live in the layout', () => {
        // One fetch for the whole screen: a page fetching for itself turns one
        // overview request into one per tab switch. And the admin check has to
        // cover every tab, not the landing one.
        const layout = readFileSync(join(APP, 'layout.tsx'), 'utf8');
        expect(layout).toContain('user?.isAdmin');
        expect(layout).toContain('InfraProvider');
    });
});

describe('the three kinds of machine', () => {
    it('all three tabs are always drawn, not only when non-empty', () => {
        // They used to appear only when the operator had one, which made "no
        // external nodes" and "this platform has no such thing" the same
        // screen - and the tab somebody needs FIRST is the one for the kind
        // they have not registered yet.
        for (const slug of ['nodes', 'external', 'byon']) {
            const re = new RegExp(`slug: '${slug}'[^}]*visible: true`);
            expect(re.test(SHELL), `the ${slug} tab is conditionally visible`).toBe(true);
        }
    });

    it('the customer tab is called BYON', () => {
        // The word the setting, the plan and the release notes all use. One
        // screen calling it something friendlier costs the reader the
        // connection to all of them.
        expect(SHELL).toContain("label: 'BYON'");
        expect(SHELL).not.toContain("label: 'Customer nodes'");
    });

    it('the customer estate is summarised on the BYON tab and nowhere else', () => {
        // Counted, never judged: these machines belong to tenants and are
        // switched off for ordinary reasons, so a fraction short of whole must
        // not raise a warning anywhere.
        const panel = readFileSync(join(__dirname, 'NodesPanel.tsx'), 'utf8');
        expect(panel).toContain('CustomerEstate');
        expect(panel).toMatch(/kind === 'byon' && <CustomerEstate/);

        const estate = readFileSync(join(__dirname, 'CustomerEstate.tsx'), 'utf8');
        for (const tone of ['--warning', '--error']) {
            expect(estate, `the estate card uses ${tone}, so it can warn about a customer's machine`)
                .not.toContain(tone);
        }
    });
});
