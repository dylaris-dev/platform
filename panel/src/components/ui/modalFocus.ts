// Keyboard rules for a modal, kept apart from the component so they can be
// tested without a DOM - the same split as selectKeyboard.ts.

// FOCUSABLE is what Tab can land on inside a modal panel.
export const FOCUSABLE = [
    'a[href]',
    'button:not([disabled])',
    'input:not([disabled]):not([type="hidden"])',
    'select:not([disabled])',
    'textarea:not([disabled])',
    '[tabindex]:not([tabindex="-1"])',
].join(',');

// nextTrapIndex decides where Tab goes inside a modal: it wraps at both ends
// instead of leaving for the page behind the overlay, which a sighted mouse user
// cannot reach either. `current` is -1 when focus is outside the list (on the
// panel itself, or somewhere that is not focusable).
export function nextTrapIndex(count: number, current: number, backwards: boolean): number {
    if (count <= 0) return -1;
    if (current < 0 || current >= count) return backwards ? count - 1 : 0;
    if (backwards) return current === 0 ? count - 1 : current - 1;
    return current === count - 1 ? 0 : current + 1;
}

// ownsKey says whether THIS modal should act on a key press. Only the dialog
// that holds focus does: a confirm or re-auth dialog opened on top of a modal
// keeps its own focus, and Escape there must close that one and not both.
// Focus on the page body - nothing focused - counts as this modal's, so Escape
// still works after a click on a non-focusable part of the panel.
export function ownsKey(panelContainsFocus: boolean, focusIsBody: boolean): boolean {
    return panelContainsFocus || focusIsBody;
}
