import { describe, it, expect } from 'vitest';
import { nagText, blockingNags, canSubmitFrom } from './modrinthChecklist';

describe('nagText', () => {
    it('explains a known item and its numbers', () => {
        expect(nagText({ kind: 'description_too_short', severity: 'required', details: { length: 102, min_chars: 125 } }))
            .toBe('Write a longer description. It has 102 of at least 125 characters.');
    });
    it('still shows an item Modrinth adds later', () => {
        expect(nagText({ kind: 'brand_new_rule', severity: 'required' })).toBe('Brand new rule.');
    });
});

describe('blockingNags', () => {
    it('only required items block submission', () => {
        const nags = [{ kind: 'add_icon', severity: 'suggestion' }, { kind: 'select_environment', severity: 'required' }, { kind: 'too_many_tags', severity: 'warning' }];
        expect(blockingNags(nags).map(n => n.kind)).toEqual(['select_environment']);
    });
});

describe('canSubmitFrom', () => {
    it('allows a draft and a resubmission, not a project already in review or public', () => {
        expect(canSubmitFrom('draft')).toBe(true);
        expect(canSubmitFrom('rejected')).toBe(true);
        expect(canSubmitFrom('processing')).toBe(false);
        expect(canSubmitFrom('approved')).toBe(false);
    });
});
