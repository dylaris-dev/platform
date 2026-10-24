import { describe, it, expect } from 'vitest';
import { packAuditText } from './packAudit';

describe('packAuditText', () => {
    it('words known actions', () => {
        expect(packAuditText('build.published')).toBe('published a build');
    });
    it('names the collaborator', () => {
        expect(packAuditText('collaborator.invited', { user: 'dave' })).toBe('invited a collaborator: dave');
        expect(packAuditText('collaborator.removed', { user: 'dave', left: true })).toBe('left the packs (dave)');
    });
    it('falls back to the raw action', () => {
        expect(packAuditText('something.new')).toBe('something.new');
    });
});
