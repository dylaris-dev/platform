import { describe, it, expect } from 'vitest';
import { planResourceChange, diskGBToMB, joinFields, type ResourceValues } from './resourceChanges';

const base: ResourceValues = {
    ram: 4096, cpuLimit: 2, diskMB: 10240, hostPort: 25600, containerPort: 25565,
    cpuMode: 'shared', cpuset: '', autoMove: false,
};
const edit = (over: Partial<ResourceValues>): ResourceValues => ({ ...base, ...over });

describe('planResourceChange', () => {
    it('asks before restarting a running server for a RAM change', () => {
        const p = planResourceChange(base, edit({ ram: 6144 }), true, false);
        expect(p.restartFields).toEqual(['RAM']);
        expect(p.needsRestart).toBe(true);
        expect(p.buttonLabel).toBe('Save and restart');
        expect(p.note).toBe('RAM restarts the server.');
    });

    it('does not restart for live fields', () => {
        const p = planResourceChange(base, edit({ cpuLimit: 3, diskMB: 20480, cpuMode: 'auto', autoMove: true }), true, true);
        // base is 'shared' with no cpuset, so moving to 'auto' only adds pinning.
        expect(p.restartFields).toEqual([]);
        expect(p.liveFields).toEqual(['CPU limit', 'CPU pinning', 'disk', 'auto-move']);
        expect(p.needsRestart).toBe(false);
        expect(p.buttonLabel).toBe('Save');
        expect(p.note).toBe('Applied live, no restart.');
    });

    it('names both kinds when a restart and live changes are mixed', () => {
        const p = planResourceChange(base, edit({ ram: 6144, hostPort: 25601, diskMB: 20480 }), true, true);
        expect(p.note).toBe('RAM and host port restart the server. Disk is applied live.');
    });

    it('treats a container-port-only change as a restart', () => {
        const p = planResourceChange(base, edit({ containerPort: 25570 }), true, true);
        expect(p.restartFields).toEqual(['container port']);
        expect(p.needsRestart).toBe(true);
    });

    it('ignores ports for a caller who cannot change them, and 0 as unchanged', () => {
        expect(planResourceChange(base, edit({ hostPort: 25601 }), true, false).needsRestart).toBe(false);
        expect(planResourceChange(base, edit({ hostPort: 0 }), true, true).needsRestart).toBe(false);
    });

    it('never restarts a stopped server', () => {
        const p = planResourceChange(base, edit({ ram: 6144 }), false, true);
        expect(p.needsRestart).toBe(false);
        expect(p.buttonLabel).toBe('Save');
        expect(p.note).toBe('The server is stopped: the change takes effect on its next start.');
    });

    it('a manual cpuset edit is a change, an auto one only by mode', () => {
        const manual = { ...base, cpuMode: 'manual', cpuset: '0-3' };
        expect(planResourceChange(manual, { ...manual, cpuset: '4-7' }, true, false).liveFields).toEqual(['CPU pinning']);
        const auto = { ...base, cpuMode: 'auto', cpuset: '0-3' };
        expect(planResourceChange(auto, { ...auto, cpuset: '' }, true, false).liveFields).toEqual([]);
    });

    it('counts removing a CPU limit as a restart, changing one as live', () => {
        const removed = planResourceChange(base, edit({ cpuLimit: 0 }), true, false);
        expect(removed.restartFields).toEqual(['CPU limit']);
        expect(removed.buttonLabel).toBe('Save and restart');
        expect(planResourceChange(base, edit({ cpuLimit: 1 }), true, false).restartFields).toEqual([]);
        const unlimited = { ...base, cpuLimit: 0 };
        expect(planResourceChange(unlimited, { ...unlimited, cpuLimit: 2 }, true, false).liveFields).toEqual(['CPU limit']);
    });

    it('counts dropping a pinned cpuset as a restart', () => {
        const manual = { ...base, cpuMode: 'manual', cpuset: '0-3' };
        for (const cpuMode of ['shared', 'auto']) {
            const p = planResourceChange(manual, { ...manual, cpuMode }, true, false);
            expect(p.restartFields).toEqual(['CPU pinning']);
            expect(p.needsRestart).toBe(true);
        }
        const auto = { ...base, cpuMode: 'auto', cpuset: '0-3' };
        expect(planResourceChange(auto, { ...auto, cpuMode: 'shared' }, true, false).restartFields).toEqual(['CPU pinning']);
        // Nothing pinned before: nothing to remove.
        expect(planResourceChange(base, edit({ cpuMode: 'auto' }), true, false).liveFields).toEqual(['CPU pinning']);
    });

    it('treats a RAM headroom change as a restart, the same value as none', () => {
        const withPad = { ...base, ramPaddingMb: 512 };
        const p = planResourceChange(withPad, { ...withPad, ramPaddingMb: 1024 }, true, false);
        expect(p.restartFields).toEqual(['RAM headroom']);
        expect(p.needsRestart).toBe(true);
        expect(planResourceChange(withPad, withPad, true, false).needsRestart).toBe(false);
        // A reset whose inherited value could not be loaded still asks.
        expect(planResourceChange(withPad, { ...withPad, ramPaddingMb: null }, true, false).needsRestart).toBe(true);
    });

    it('reports no changes', () => {
        expect(planResourceChange(base, base, true, true).note).toBe('No changes.');
    });
});

describe('diskGBToMB', () => {
    // Core decodes diskLimit as an integer: 2.5 GB * 1024 is whole, 1.3 GB is not.
    it('sends whole MB', () => {
        expect(diskGBToMB(1.3)).toBe(1331);
        expect(Number.isInteger(diskGBToMB(0.7))).toBe(true);
        expect(diskGBToMB(0)).toBe(0);
    });
});

describe('joinFields', () => {
    it('joins with commas and a final "and"', () => {
        expect(joinFields(['RAM'])).toBe('RAM');
        expect(joinFields(['RAM', 'host port', 'container port'])).toBe('RAM, host port and container port');
    });
});
