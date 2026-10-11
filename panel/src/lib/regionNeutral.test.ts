import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

// The panel is region-neutral: region is shown only where infrastructure
// announces it (node NODE_REGION, edges, warp, beam relays, traffic pools).
// Core removed the user-facing region endpoints in the same release, so any
// call left behind would 404 and any create field would be ignored. Logic-only
// suite, so this reads the source.

const SRC = join(__dirname, '..');
const read = (rel: string) => readFileSync(join(SRC, rel), 'utf8');

function sourceFiles(dir: string): string[] {
    return readdirSync(dir).flatMap(name => {
        const p = join(dir, name);
        if (statSync(p).isDirectory()) return sourceFiles(p);
        return /\.tsx?$/.test(name) && !/\.test\.tsx?$/.test(name) ? [p] : [];
    });
}

describe('the panel no longer treats region as a user concept', () => {
    it('calls none of the removed region endpoints', () => {
        const removed = [/['"`]\/regions['"`]/, /\/admin\/regions/, /\/me\/regions/, /\/users\/[^'"`]*\/regions/, /\/placement\/regions/];
        const hits = sourceFiles(SRC).flatMap(f => {
            const src = readFileSync(f, 'utf8');
            return removed.filter(re => re.test(src)).map(re => `${f}: ${re}`);
        });
        expect(hits).toEqual([]);
    });

    it('creates a user without region access fields', () => {
        const users = read('components/settings/UsersTab.tsx');
        expect(users).toMatch(/createUser\(userForm, reauth\)/);
        expect(users).not.toMatch(/allRegions|regionsExplicit/);
        expect(read('lib/api/authSettings.ts')).not.toMatch(/defaultNewUserAllRegions/);
    });

    it('the create wizard sends no region, and no node means Core auto-places', () => {
        const wizard = read('components/CreateServerWizard.tsx');
        expect(wizard).not.toMatch(/payload\.region/);
        // Tag mode with no tags must reach Core with neither nodeId nor tags
        // rather than being refused in the panel: the old guard demanded "a
        // region or at least one tag", and without regions that blocked the
        // automatic placement an admin used to get by picking a region.
        expect(wizard).not.toMatch(/at least one tag/);
        expect(wizard).toMatch(/if \(targetMode === 'node'\) \{\s*payload\.nodeId = nodeId;\s*\} else if \(selectedTags\.length > 0\) \{\s*payload\.tags = selectedTags;\s*\}/);
    });

    it('has no Regions settings page', () => {
        expect(read('app/(authed)/settings/layout.tsx')).not.toMatch(/slug: 'regions'/);
    });
});
