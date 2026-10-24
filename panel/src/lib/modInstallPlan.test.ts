import { describe, it, expect } from 'vitest';
import { installQueue, planConfirm, planRefused, updateQueue } from './modInstallPlan';
import type { InstallModPayload, ModInstallPlan, ModPlanEntry } from './api/modrinth';

function entry(id: string, versionNumber = '1.0'): ModPlanEntry {
    return {
        projectId: `p-${id}`, slug: id, title: id, versionId: `v-${id}`,
        versionNumber, fileName: `${id}.jar`, downloadUrl: `https://cdn.modrinth.com/${id}.jar`,
    };
}

const root: InstallModPayload = {
    projectId: 'p-root', projectSlug: 'root', versionId: 'v-root', title: 'Biomes',
    fileName: 'root.jar', downloadUrl: 'https://cdn.modrinth.com/root.jar', sha512: 'abc',
};

function plan(over: Partial<ModInstallPlan> = {}): ModInstallPlan {
    return { install: [entry('root')], unresolved: [], incompatible: [], ...over };
}

describe('installQueue', () => {
    it('keeps the plan order and installs the requested mod last', () => {
        const p = plan({ install: [entry('c'), entry('b'), entry('root')] });
        expect(installQueue(p, root).map(x => x.versionId)).toEqual(['v-c', 'v-b', 'v-root']);
    });

    it('moves the root to the end even if the plan lists it first, without installing it twice', () => {
        const p = plan({ install: [entry('root'), entry('a')] });
        const q = installQueue(p, root);
        expect(q.map(x => x.versionId)).toEqual(['v-a', 'v-root']);
        expect(q[q.length - 1]).toBe(root);
    });

    it('maps a dependency to an install payload', () => {
        const q = installQueue(plan({ install: [entry('a'), entry('root')] }), root);
        expect(q[0]).toEqual({
            projectId: 'p-a', projectSlug: 'a', versionId: 'v-a', title: 'a',
            fileName: 'a.jar', downloadUrl: 'https://cdn.modrinth.com/a.jar',
        });
    });

    it('is just the root when nothing else is needed', () => {
        expect(installQueue(plan(), root)).toEqual([root]);
    });
});

describe('updateQueue', () => {
    it('installs what the new build needs first, then the mod', () => {
        const step = updateQueue(plan({ install: [entry('lib'), entry('root')] }), root);
        expect('queue' in step && step.queue.map(x => x.versionId)).toEqual(['v-lib', 'v-root']);
    });

    it('leaves the mod alone when a dependency cannot be resolved', () => {
        const step = updateQueue(plan({ unresolved: [{ projectId: 'p-x', title: 'Lib X', reason: 'No build fits.' }] }), root);
        expect(step).toEqual({ skip: 'the new build needs or clashes with Lib X' });
    });

    it('leaves the mod alone when it would replace a paused or switched-off mod', () => {
        const step = updateQueue(plan({ install: [entry('lib'), entry('root')] }), root, new Map([['p-lib', 'Lib']]));
        expect(step).toEqual({ skip: 'it would replace Lib, which is paused or switched off' });
        // The root itself being held is the caller's filter, not this one's.
        expect('queue' in updateQueue(plan({ install: [entry('root')] }), root, new Map([['p-root', 'Biomes']]))).toBe(true);
    });

    it('leaves the mod alone when it clashes with an installed mod', () => {
        const step = updateQueue(plan({ incompatible: [{ projectId: 'p-y', title: 'Mod Y', reason: 'marked incompatible' }] }), root);
        expect('skip' in step).toBe(true);
    });
});

describe('planConfirm', () => {
    it('says nothing when the plan adds nothing and warns about nothing', () => {
        expect(planConfirm(plan(), root)).toBeNull();
    });

    it('lists the dependencies with their versions', () => {
        const p = plan({ install: [{ ...entry('TerraBlender', '4.1.0.8') }, { ...entry('GlitchCore', '2.1.0.2') }, entry('root')] });
        expect(planConfirm(p, root)).toEqual({
            title: 'Install "Biomes" with 2 dependencies?',
            message: 'Also installs: TerraBlender 4.1.0.8, GlitchCore 2.1.0.2.',
            confirmLabel: 'Install 3 mods',
            destructive: false,
        });
    });

    it('asks, as a warning, when a dependency cannot be installed', () => {
        const c = planConfirm(plan({
            unresolved: [{ projectId: 'p-x', title: 'Lib', reason: 'No neoforge build for Minecraft 1.21.1 on Modrinth.' }],
        }), root)!;
        expect(c.title).toBe('Install "Biomes"?');
        expect(c.message).toBe('Not installed, so the server may not start without them:\nLib: No neoforge build for Minecraft 1.21.1 on Modrinth.');
        expect(c.destructive).toBe(true);
        expect(c.confirmLabel).toBe('Install anyway');
    });

    it('names an installed mod it clashes with, and an untitled problem by its reason', () => {
        const c = planConfirm(plan({
            install: [entry('a'), entry('root')],
            unresolved: [{ projectId: '', title: '', reason: 'Modrinth has no version zzz.' }],
            incompatible: [{ projectId: 'p-o', title: 'OptiFine', reason: 'Biomes is marked incompatible with it.' }],
        }), root)!;
        expect(c.title).toBe('Install "Biomes" with 1 dependency?');
        expect(c.message).toBe([
            'Also installs: a 1.0.',
            'Not installed, so the server may not start without them:\nModrinth has no version zzz.',
            'Clashes with mods already installed:\nOptiFine: Biomes is marked incompatible with it.',
        ].join('\n\n'));
        expect(c.destructive).toBe(true);
    });
});

// A plan that fails on the session must not open "Install without
// dependencies"; only an answer that says nothing about the build may.
describe('planRefused', () => {
    it.each([
        [401, true], [400, true], [403, true], [404, true], [409, true],
        [502, false], [503, false], [undefined, false],
    ])('status %s -> %s', (status, want) => {
        expect(planRefused(status)).toBe(want);
    });
});
