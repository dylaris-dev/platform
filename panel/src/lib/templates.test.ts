import { describe, expect, it } from 'vitest';
import {
    formatTemplateSize, groupTemplates, templateCap, limitFromSetting, limitToSetting, softwareVersionLabel, templateInstallPayload, templatesVisible,
} from './templates';
import type { Template } from './api/templates';

const me = '11111111-1111-1111-1111-111111111111';
const other = '22222222-2222-2222-2222-222222222222';

function tpl(id: number, ownerId: string, visibility: Template['visibility']): Template {
    return {
        id, ownerId, ownerName: ownerId === me ? 'me' : 'other', name: `t${id}`, kind: 'server', visibility,
        software: '', mcVersion: '', softwareVersion: '', storageConnectionId: 0, sizeBytes: 0,
        createdAt: '', updatedAt: '', canEdit: false, canShare: false, shareLevel: '',
    };
}

describe('groupTemplates', () => {
    it('sorts a mixed list into the four groups', () => {
        const list = [
            tpl(1, me, 'personal'),
            tpl(2, other, 'personal'),
            tpl(3, me, 'admin'),
            tpl(4, other, 'platform'),
            tpl(5, other, 'admin'),
            tpl(6, me, 'platform'),
        ];
        const g = groupTemplates(list, me);
        expect(g.mine.map((t) => t.id)).toEqual([1]);
        expect(g.shared.map((t) => t.id)).toEqual([2]);
        // Admin and platform templates belong to the platform, whoever made them.
        expect(g.admin.map((t) => t.id)).toEqual([3, 5]);
        expect(g.platform.map((t) => t.id)).toEqual([4, 6]);
    });

    it('returns empty groups for an empty list', () => {
        expect(groupTemplates([], me)).toEqual({ mine: [], shared: [], admin: [], platform: [] });
    });
});

describe('templateCap', () => {
    it('prefers the cap Core sent for the template, including none', () => {
        expect(templateCap({ maxSizeBytes: 2048 }, 512)).toBe(2048);
        expect(templateCap({ maxSizeBytes: null }, 512)).toBeNull();
    });
    it('falls back to the list cap when an older Core sent none', () => {
        expect(templateCap({}, 512)).toBe(512);
    });
});

describe('formatTemplateSize', () => {
    it('shows the size against the limit', () => {
        expect(formatTemplateSize(12 * 1024 * 1024, 512 * 1024 * 1024)).toBe('12.0 MB of 512 MB');
    });
    it('shows only the size when there is no limit', () => {
        expect(formatTemplateSize(12 * 1024 * 1024, null)).toBe('12.0 MB, no limit');
    });
    it('keeps a limit of zero, which means none', () => {
        expect(formatTemplateSize(0, 0)).toBe('0.0 MB of 0 MB');
    });
});

describe('templatesVisible', () => {
    it.each([
        ['all', false, true],
        ['admin', false, false],
        ['off', false, false],
        [undefined, false, false],
        ['off', true, true],
        ['admin', true, true],
    ] as const)('audience %s, admin %s -> %s', (audience, isAdmin, want) => {
        expect(templatesVisible(audience, isAdmin)).toBe(want);
    });
});

describe('templateInstallPayload', () => {
    // Core refuses any other installer field next to type template.
    it('is exactly type and id', () => {
        expect(templateInstallPayload(7)).toEqual({ type: 'template', templateId: 7 });
    });
});

describe('limitFromSetting / limitToSetting', () => {
    it.each([
        ['unlimited', null],
        ['0', 0],
        ['25', 25],
        ['', 10],
        [undefined, 10],
        ['garbage', 10],
    ] as const)('%s reads as %s', (raw, want) => {
        expect(limitFromSetting(raw, 10)).toBe(want);
    });
    it('writes null as unlimited and keeps zero', () => {
        expect(limitToSetting(null)).toBe('unlimited');
        expect(limitToSetting(0)).toBe('0');
        expect(limitToSetting(512)).toBe('512');
    });
});

describe('softwareVersionLabel', () => {
    it.each([
        ['fabric', 'Loader version'],
        ['forge', 'Loader version'],
        ['neoforge', 'NeoForge version'],
        ['paper', 'Software build'],
        ['', 'Software build'],
    ])('%s -> %s', (software, want) => {
        expect(softwareVersionLabel(software)).toBe(want);
    });
});
