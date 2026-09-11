import { describe, it, expect } from 'vitest';
import { nodeActionClass, canRollKey } from './NodesTab';

// The armed re-admission a roll arms lapses after 15 minutes (same window an
// approval arms). A node offline longer than that comes back refused and
// needs a manual admit, so the copy shown while rolling ("restored ... within
// a minute") is only ever true while the node is online - the action must not
// be offered otherwise.
describe('canRollKey', () => {
    it('is offered only for an online node', () => {
        expect(canRollKey('online')).toBe(true);
        expect(canRollKey('offline')).toBe(false);
        expect(canRollKey('')).toBe(false);
    });
});

// The rule this decides: what a node action looks like once it has been used.
//
// Clicking "Configure" removed the Configure button from the row. The form did
// open underneath, but the control you just pressed disappearing reads as a
// broken click - and there was then no way to close the form from the place you
// opened it.
describe('nodeActionClass', () => {
    it('lights the action that is open instead of hiding it', () => {
        const open = nodeActionClass(true, false);
        expect(open).toContain('bg-(--accent)');
        expect(open).toContain('text-(--accent-light)');
    });

    it('leaves a closed action in the muted resting style', () => {
        const closed = nodeActionClass(false, false);
        expect(closed).toContain('text-(--base-06)');
        expect(closed).not.toContain('bg-(--accent)');
    });

    // A node that was never configured is highlighted to pull the eye. Once its
    // form is open the highlight has done its job, and the two states must not
    // fight over the same button.
    it('being open outranks the needs-attention highlight', () => {
        expect(nodeActionClass(true, true)).toEqual(nodeActionClass(true, false));
        expect(nodeActionClass(false, true)).toContain('text-(--accent-light)');
    });

    it('every state keeps the shared layout so the row does not shift', () => {
        for (const cls of [nodeActionClass(true, true), nodeActionClass(false, true), nodeActionClass(false, false)]) {
            expect(cls).toContain('inline-flex');
            expect(cls).toContain('px-1.5');
        }
    });
});
