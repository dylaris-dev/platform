import { describe, it, expect } from 'vitest';
import { storagePrefPayload, storageLabel, jobStorageOptions } from './ownBackupStorage';

const own = [{ id: 5, name: 'Backblaze' }, { id: 6, name: 'MinIO' }];

describe('storagePrefPayload', () => {
    it('sends only the field that changed, so the other choice is left alone', () => {
        expect(storagePrefPayload('templates', '5')).toEqual({ templatesStorageId: 5 });
        expect(Object.keys(storagePrefPayload('backups', '6'))).toEqual(['backupsStorageId']);
    });
    it('turns the Dylaris storage option into null', () => {
        expect(storagePrefPayload('backups', '')).toEqual({ backupsStorageId: null });
    });
});

describe('storageLabel', () => {
    it.each([
        [null, true, 'Dylaris storage'],
        [undefined, true, 'Dylaris storage'],
        [null, false, 'Platform storage'],
        [undefined, false, 'Platform storage'],
        [6, false, 'MinIO'],
        // Somebody else's target (a shared template): its name is not ours to show.
        [99, true, "Owner's own storage"],
    ])('%s hosted=%s -> %s', (id, hosted, want) => {
        expect(storageLabel(id as number | null | undefined, own, hosted)).toBe(want);
    });
});

describe('jobStorageOptions', () => {
    it("drops own targets while own storage is off, except the job's current one", () => {
        const platform = [{ id: 1, name: 'R2', provider: 's3' }];
        expect(jobStorageOptions(platform, own, false)).toEqual([{ id: 1, label: 'R2 (s3)' }]);
        expect(jobStorageOptions(platform, own, false, 6)).toEqual([
            { id: 1, label: 'R2 (s3)' },
            { id: 6, label: 'Own: MinIO' },
        ]);
    });
    it('lists platform storages first, then own targets marked as own', () => {
        expect(jobStorageOptions([{ id: 1, name: 'R2', provider: 's3' }], own)).toEqual([
            { id: 1, label: 'R2 (s3)' },
            { id: 5, label: 'Own: Backblaze' },
            { id: 6, label: 'Own: MinIO' },
        ]);
    });
    it('works with only own targets (a tenant cannot list platform storages)', () => {
        expect(jobStorageOptions([], own.slice(0, 1))).toEqual([{ id: 5, label: 'Own: Backblaze' }]);
    });
});
