import { describe, it, expect } from 'vitest';
import { advancedStartsOpen, portsEditable } from './resourceDialog';

describe('portsEditable', () => {
    it.each([
        [true, 'ip_port', true],
        [true, 'both', true],
        [true, 'gateway', false],
        [false, 'ip_port', false],
    ] as const)('admin=%s routing=%s -> %s', (isAdmin, mode, want) => {
        expect(portsEditable(isAdmin, mode)).toBe(want);
    });
});

describe('advancedStartsOpen', () => {
    const defaults = { paddingText: '', canEditPorts: true, hostPort: 0, containerPort: 25565 };
    it('stays closed on defaults', () => {
        expect(advancedStartsOpen(defaults)).toBe(false);
    });
    it('opens for a headroom override', () => {
        expect(advancedStartsOpen({ ...defaults, paddingText: '768' })).toBe(true);
    });
    it('opens for a pinned host port or a custom container port', () => {
        expect(advancedStartsOpen({ ...defaults, hostPort: 25601 })).toBe(true);
        expect(advancedStartsOpen({ ...defaults, containerPort: 25566 })).toBe(true);
    });
    it('ignores ports that cannot be edited', () => {
        expect(advancedStartsOpen({ ...defaults, canEditPorts: false, hostPort: 25601 })).toBe(false);
    });
});
