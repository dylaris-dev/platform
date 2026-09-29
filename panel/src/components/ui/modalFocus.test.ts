import { describe, expect, it } from 'vitest';
import { nextTrapIndex, ownsKey } from './modalFocus';

describe('nextTrapIndex', () => {
    it('moves forward and wraps from the last element to the first', () => {
        expect(nextTrapIndex(3, 0, false)).toBe(1);
        expect(nextTrapIndex(3, 2, false)).toBe(0);
    });

    it('moves backward and wraps from the first element to the last', () => {
        expect(nextTrapIndex(3, 1, true)).toBe(0);
        expect(nextTrapIndex(3, 0, true)).toBe(2);
    });

    // Focus on the panel itself, or anywhere outside the list: Tab starts at
    // the first element, Shift+Tab at the last - never out of the dialog.
    it('enters the list from outside at the natural end', () => {
        expect(nextTrapIndex(3, -1, false)).toBe(0);
        expect(nextTrapIndex(3, -1, true)).toBe(2);
        expect(nextTrapIndex(3, 7, false)).toBe(0);
    });

    it('has nowhere to go in a panel with nothing focusable', () => {
        expect(nextTrapIndex(0, -1, false)).toBe(-1);
    });

    it('stays on the only element', () => {
        expect(nextTrapIndex(1, 0, false)).toBe(0);
        expect(nextTrapIndex(1, 0, true)).toBe(0);
    });
});

describe('ownsKey', () => {
    // A confirm dialog opened over a modal holds focus; Escape there must not
    // also close the modal underneath.
    it('leaves a key to the dialog that holds focus', () => {
        expect(ownsKey(false, false)).toBe(false);
    });

    it('acts when focus is inside this panel, or on nothing at all', () => {
        expect(ownsKey(true, false)).toBe(true);
        expect(ownsKey(false, true)).toBe(true);
    });
});
