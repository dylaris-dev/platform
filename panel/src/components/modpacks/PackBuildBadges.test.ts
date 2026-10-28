import { createElement } from 'react';
import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import type { PackBuild } from '@/lib/api/packs';
import PackBuildBadges from './PackBuildBadges';

const build=(id:number,channel:string,date:string)=>({id,channel,createdAt:date,frozen:channel==='release'} as PackBuild);
describe('release and recommendation badges',()=>{
    const older=build(1,'release','2026-10-27T10:00:00Z');
    const latest=build(2,'release','2026-10-28T10:00:00Z');
    const draft=build(3,'draft','2026-10-28T11:00:00Z');
    it('distinguishes an older recommendation from the latest release',()=>{
        const recommended=renderToStaticMarkup(createElement(PackBuildBadges,{build:older,builds:[draft,latest,older],recommendedBuildId:1}));
        const release=renderToStaticMarkup(createElement(PackBuildBadges,{build:latest,builds:[draft,latest,older],recommendedBuildId:1}));
        expect(recommended).toContain('Recommended');expect(recommended).toContain('--accent-ghost');expect(recommended).not.toContain('Latest release');
        expect(release).toContain('Latest release');expect(release).toContain('--success-ghost');
    });
    it('shows both roles when they apply to the same build',()=>{
        const html=renderToStaticMarkup(createElement(PackBuildBadges,{build:latest,builds:[latest,older],recommendedBuildId:2}));
        expect(html).toContain('Recommended');expect(html).toContain('Latest release');expect(html).toContain('Read-only');
    });
    it('does not label a new draft as a release',()=>{
        const html=renderToStaticMarkup(createElement(PackBuildBadges,{build:draft,builds:[draft,latest]}));
        expect(html).toContain('Draft');expect(html).not.toContain('Latest release');
    });
});
