import { describe, it, expect } from 'vitest';
import { usageLabel } from './DeployKit';

// The platform's limits convention, on the screen: nothing means no cap, 0 means
// none, n is the cap. This label used to read `limit <= 0` as "no cap", so the
// one number an operator can type to say "none" was shown to the customer as
// unlimited - and the refusal then arrived as a 403 on the button beside it.
describe('usageLabel', () => {
    it('shows the cap when there is one', () => {
        expect(usageLabel(3, 5)).toBe('3 of 5 in use');
    });

    it('says none at a cap of zero', () => {
        expect(usageLabel(0, 0)).toBe('none allowed');
    });

    it('treats nothing at all as no cap', () => {
        expect(usageLabel(3, undefined)).toBe('3 in use');
        expect(usageLabel(3, null)).toBe('3 in use');
    });
});
