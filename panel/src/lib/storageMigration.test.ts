import { describe, it, expect } from 'vitest';
import {
  storageMigrationInProgress,
  isCancellablePhase,
  deleteSourceAllowed,
  targetConfigValid,
  canStartMigration,
  startBlockReason,
  startMigrateFromForm,
  formatPercent,
  formatBytes,
  verifyVerdictLabel,
  examinedBytesFraction,
  progressPercent,
  EMPTY_MIGRATION_FORM,
  EMPTY_TARGET_CONFIG,
  type MigrationForm,
  type StorageDataSet,
  type StorageMigrationJob,
  type StorageVerifyReport,
  type TargetConfigForm,
} from './storageMigration';

// Mirrors core/handlers/storageapi/storage_migration.go List(): core-storage is the one
// entry that can switch the shared Core file storage config; library and
// modpacks@core-storage are namespaces INSIDE that shared config and so
// cannot switch it on their own (they would strand the others); modpacks owns
// its own settings namespace and can switch; server-backups rows target
// another row, never a config.
const dataSets: StorageDataSet[] = [
  { id: 'core-storage', label: 'Core file storage (all namespaces)', backendLabel: 'path:/mnt/shared', migratable: true, supportsTargetConfig: true, note: '' },
  { id: 'modpacks', label: 'Modpacks', backendLabel: 's3:https://s3.example.com/packs', migratable: true, supportsTargetConfig: true, note: '' },
  { id: 'modpacks@core-storage', label: 'Modpacks on Core file storage', backendLabel: 'path:/mnt/shared/modpacks', migratable: true, supportsTargetConfig: false, note: '' },
  { id: 'library', label: 'Library', backendLabel: 'path:/mnt/shared/library', migratable: true, supportsTargetConfig: false, note: '' },
  { id: 'server-backups:1', label: 'Server backups: Hetzner S3', backendLabel: 'Hetzner S3 (s3)', migratable: true, supportsTargetConfig: false, note: '' },
  { id: 'server-backups:3', label: 'Server backups: Wasabi', backendLabel: 'Wasabi (s3)', migratable: true, supportsTargetConfig: false, note: '' },
  { id: 'server-backups:2', label: 'Server backups: node disk', backendLabel: 'node disk (node-local)', migratable: false, supportsTargetConfig: false, note: 'not migratable here' },
];

const validTargetConfig = (over: Partial<TargetConfigForm> = {}): TargetConfigForm => ({
  ...EMPTY_TARGET_CONFIG,
  backend: 's3',
  s3Endpoint: 'https://new.example.com',
  s3Bucket: 'dylaris-new',
  s3AccessKey: 'AKIA',
  s3SecretKey: 'secret',
  ...over,
});

// form is the ad-hoc-target shape: how the settings-configured data sets
// migrate. Their saved config says where they live now, so the destination is
// named inline instead. core-storage is the only data set that both takes an
// ad-hoc target config AND is not itself a namespace inside another one.
const form = (over: Partial<MigrationForm> = {}): MigrationForm => ({
  ...EMPTY_MIGRATION_FORM,
  dataSet: 'core-storage',
  targetKind: 'config',
  targetDataSet: '',
  targetConfig: validTargetConfig(),
  verifyMode: 'full',
  deleteSource: false,
  ...over,
});

// rowForm is the other shape: one server-backups row to another.
const rowForm = (over: Partial<MigrationForm> = {}): MigrationForm => ({
  ...EMPTY_MIGRATION_FORM,
  dataSet: 'server-backups:1',
  targetKind: 'dataset',
  targetDataSet: 'server-backups:3',
  verifyMode: 'full',
  deleteSource: false,
  ...over,
});

describe('storageMigrationInProgress', () => {
  it('is true for every live phase', () => {
    for (const p of ['preparing', 'manifesting', 'copying', 'verifying', 'switching_config', 'deleting_source'] as const) {
      expect(storageMigrationInProgress(p)).toBe(true);
    }
  });
  it('is false for terminal phases and for undefined', () => {
    for (const p of ['done', 'failed', 'cancelled'] as const) {
      expect(storageMigrationInProgress(p)).toBe(false);
    }
    expect(storageMigrationInProgress(undefined)).toBe(false);
  });
});

describe('isCancellablePhase', () => {
  it('allows cancelling the read-only and copy phases', () => {
    for (const p of ['preparing', 'manifesting', 'copying', 'verifying'] as const) {
      expect(isCancellablePhase(p)).toBe(true);
    }
  });
  it('refuses deleting_source: it is already the point of no return', () => {
    expect(isCancellablePhase('deleting_source')).toBe(false);
  });
  it('refuses switching_config: a half-applied switch is worse than a completed one', () => {
    expect(isCancellablePhase('switching_config')).toBe(false);
  });
  it('refuses terminal phases', () => {
    for (const p of ['done', 'failed', 'cancelled'] as const) {
      expect(isCancellablePhase(p)).toBe(false);
    }
    expect(isCancellablePhase(undefined)).toBe(false);
  });
});

describe('deleteSourceAllowed', () => {
  it('only a full verification can authorize a delete', () => {
    expect(deleteSourceAllowed('full')).toBe(true);
    expect(deleteSourceAllowed('sample')).toBe(false);
  });
});

describe('targetConfigValid', () => {
  // Mirrors validateCoreStorageConfig. The server is authoritative; this exists
  // so the operator is never allowed to submit something that would 400.
  it('accepts a complete s3 config', () => {
    expect(targetConfigValid(validTargetConfig())).toBe(true);
  });
  it('rejects s3 without a bucket or without credentials', () => {
    expect(targetConfigValid(validTargetConfig({ s3Bucket: '' }))).toBe(false);
    expect(targetConfigValid(validTargetConfig({ s3AccessKey: '' }))).toBe(false);
    expect(targetConfigValid(validTargetConfig({ s3SecretKey: '' }))).toBe(false);
  });
  it('accepts an absolute, confirmed path', () => {
    expect(targetConfigValid({ ...EMPTY_TARGET_CONFIG, backend: 'path', path: '/mnt/new', pathConfirmed: true })).toBe(true);
  });
  it('rejects a relative or unconfirmed path', () => {
    expect(targetConfigValid({ ...EMPTY_TARGET_CONFIG, backend: 'path', path: 'mnt/new', pathConfirmed: true })).toBe(false);
    expect(targetConfigValid({ ...EMPTY_TARGET_CONFIG, backend: 'path', path: '/mnt/new', pathConfirmed: false })).toBe(false);
  });
  it('rejects an unset backend', () => {
    expect(targetConfigValid(EMPTY_TARGET_CONFIG)).toBe(false);
  });
  it('accepts a selected connection even with blank inline s3 fields', () => {
    expect(targetConfigValid(validTargetConfig({ s3Bucket: '', s3AccessKey: '', s3SecretKey: '', connectionId: 5 }))).toBe(true);
  });
});

describe('canStartMigration', () => {
  it('accepts a complete ad-hoc-target form', () => {
    expect(canStartMigration(form(), dataSets)).toBe(true);
  });
  it('accepts a complete row-to-row form', () => {
    expect(canStartMigration(rowForm(), dataSets)).toBe(true);
  });
  it('rejects an empty source', () => {
    expect(canStartMigration(form({ dataSet: '' }), dataSets)).toBe(false);
  });
  it('rejects an incomplete target config', () => {
    expect(canStartMigration(form({ targetConfig: validTargetConfig({ s3Bucket: '' }) }), dataSets)).toBe(false);
  });
  it('rejects an empty target data set in the row-to-row shape', () => {
    expect(canStartMigration(rowForm({ targetDataSet: '' }), dataSets)).toBe(false);
  });
  it('rejects source === target in the row-to-row shape', () => {
    expect(canStartMigration(rowForm({ targetDataSet: 'server-backups:1' }), dataSets)).toBe(false);
  });
  it('rejects a non-migratable source or target', () => {
    expect(canStartMigration(rowForm({ dataSet: 'server-backups:2' }), dataSets)).toBe(false);
    expect(canStartMigration(rowForm({ targetDataSet: 'server-backups:2' }), dataSets)).toBe(false);
  });
  it('rejects an unknown data set id', () => {
    expect(canStartMigration(form({ dataSet: 'nope' }), dataSets)).toBe(false);
  });
  // A data set advertises which target shape it takes; using the other one is
  // rejected client-side rather than left to the server's 400.
  it('rejects a target config on a data set that takes another row', () => {
    expect(canStartMigration(form({ dataSet: 'server-backups:1' }), dataSets)).toBe(false);
  });
  // The reverse is NOT symmetric: supportsTargetConfig gates only the
  // ad-hoc-config shape, not whether a data set may appear as a row-to-row
  // source. modpacks -> modpacks@core-storage is a legitimate manual flow
  // (copy modpacks onto Core storage, verify, then switch
  // modpack_storage_provider by hand), even though modpacks itself supports an
  // ad-hoc target config too.
  it('accepts modpacks -> modpacks@core-storage: a legitimate manual flow', () => {
    expect(canStartMigration(rowForm({ dataSet: 'modpacks', targetDataSet: 'modpacks@core-storage' }), dataSets)).toBe(true);
  });
  // supportsTargetConfig gates the ad-hoc-config target shape in EITHER role,
  // so it must not block a row-to-row pair just because the target happens to
  // also accept a config. This is the only row here whose TARGET has
  // supportsTargetConfig true (modpacks), which is why the pair above did not
  // catch the clause this pins.
  it('accepts a row-to-row target whose data set also supports an ad-hoc config', () => {
    expect(canStartMigration(rowForm({ dataSet: 'modpacks@core-storage', targetDataSet: 'modpacks' }), dataSets)).toBe(true);
  });
  // Safety invariant 2, mirrored client-side. The API returns 400 for this
  // too; the UI must not let the operator get that far.
  it('rejects deleteSource under a sample verification', () => {
    expect(canStartMigration(form({ verifyMode: 'sample', deleteSource: true }), dataSets)).toBe(false);
  });
  it('accepts deleteSource under a full verification', () => {
    expect(canStartMigration(form({ verifyMode: 'full', deleteSource: true }), dataSets)).toBe(true);
  });
  it('accepts a sample verification when nothing is being deleted', () => {
    expect(canStartMigration(form({ verifyMode: 'sample', deleteSource: false }), dataSets)).toBe(true);
  });
  // Server-side rule added by Task 13 (services/storage_migration_job.go
  // Validate: "deleteSource requires targetConfig"). With a targetDataSet
  // target nothing repoints the consuming subsystem, so deleting the source
  // would orphan live references.
  it('rejects deleteSource in the row-to-row shape: nothing repoints the consumer, so the delete would orphan live references', () => {
    expect(canStartMigration(rowForm({ verifyMode: 'full', deleteSource: true }), dataSets)).toBe(false);
  });
});

describe('startBlockReason', () => {
  // Pins the equivalence the panel relies on: the footer message is non-empty
  // exactly when the button is disabled. Re-uses every case already exercised
  // above rather than a fresh set, so this stands or falls with the SAME forms
  // canStartMigration is tested against, instead of a hand-picked subset.
  it('is non-empty if and only if canStartMigration rejects the form', () => {
    const cases: MigrationForm[] = [
      form(),
      rowForm(),
      form({ dataSet: '' }),
      form({ targetConfig: validTargetConfig({ s3Bucket: '' }) }),
      rowForm({ targetDataSet: '' }),
      rowForm({ targetDataSet: 'server-backups:1' }),
      rowForm({ dataSet: 'server-backups:2' }),
      rowForm({ targetDataSet: 'server-backups:2' }),
      form({ dataSet: 'nope' }),
      form({ dataSet: 'server-backups:1' }),
      rowForm({ dataSet: 'modpacks', targetDataSet: 'modpacks@core-storage' }),
      rowForm({ dataSet: 'modpacks@core-storage', targetDataSet: 'modpacks' }),
      form({ verifyMode: 'sample', deleteSource: true }),
      form({ verifyMode: 'full', deleteSource: true }),
      form({ verifyMode: 'sample', deleteSource: false }),
      rowForm({ verifyMode: 'full', deleteSource: true }),
    ];
    for (const f of cases) {
      expect(startBlockReason(f, dataSets) !== '').toBe(!canStartMigration(f, dataSets));
    }
  });
});

describe('startMigrateFromForm', () => {
  it('the dataset shape omits targetConfig entirely', () => {
    const body = startMigrateFromForm(rowForm());
    expect(body.targetDataSet).toBe('server-backups:3');
    expect(body.targetConfig).toBeUndefined();
    expect('targetConfig' in body).toBe(false);
  });

  it('the s3 shape omits path and pathConfirmed', () => {
    const body = startMigrateFromForm(form());
    expect(body.targetDataSet).toBeUndefined();
    expect(body.targetConfig).toBeDefined();
    expect(Object.keys(body.targetConfig!).sort()).toEqual(
      ['backend', 's3AccessKey', 's3Bucket', 's3Endpoint', 's3PathStyle', 's3Prefix', 's3Region', 's3SecretKey'].sort(),
    );
  });

  it('the path shape omits every s3 field', () => {
    const body = startMigrateFromForm(
      form({ targetConfig: { ...EMPTY_TARGET_CONFIG, backend: 'path', path: '/mnt/new', pathConfirmed: true } }),
    );
    expect(body.targetConfig).toEqual({ backend: 'path', path: '/mnt/new', pathConfirmed: true });
  });

  it('a connection target sends only the reference, never the inline secret', () => {
    const body = startMigrateFromForm(
      form({ targetConfig: validTargetConfig({ connectionId: 5, s3SecretKey: 'must-not-be-sent' }) }),
    );
    expect(body.targetConfig).toEqual({ backend: 's3', connectionId: 5 });
  });

  // Minor fix: an unset backend must not be sent as a malformed path request
  // ({backend: 'path', path: '', pathConfirmed: false}). It gets its own
  // explicit branch and produces nothing beyond the bare backend field, so
  // the server's own "targetConfig.backend is required" 400 is what surfaces.
  it('an unset backend produces {backend: ""} with no path or s3 fields', () => {
    const body = startMigrateFromForm(form({ targetConfig: EMPTY_TARGET_CONFIG }));
    expect(body.targetConfig).toEqual({ backend: '' });
  });

  it('drops deleteSource under a sample verification', () => {
    const body = startMigrateFromForm(form({ verifyMode: 'sample', deleteSource: true }));
    expect(body.deleteSource).toBe(false);
  });

  // Pins the fix to Important 2: the submit path must mirror
  // canStartMigration's rule that deleteSource requires the ad-hoc-config
  // target shape, not just a full verify mode. Before the fix this emitted
  // {deleteSource: true} for a row-to-row target under a full verify,
  // guaranteeing a 400 from Validate's "deleteSource requires targetConfig".
  it('drops deleteSource in the dataset shape even under a full verification', () => {
    const body = startMigrateFromForm(rowForm({ verifyMode: 'full', deleteSource: true }));
    expect(body.deleteSource).toBe(false);
  });
});

describe('formatPercent', () => {
  it('renders one decimal place', () => {
    expect(formatPercent(0.101)).toBe('10.1%');
    expect(formatPercent(1)).toBe('100.0%');
    expect(formatPercent(0)).toBe('0.0%');
    expect(formatPercent(0.4288)).toBe('42.9%');
  });
  it('is safe for non-finite input', () => {
    expect(formatPercent(Number.NaN)).toBe('0.0%');
    expect(formatPercent(Number.POSITIVE_INFINITY)).toBe('0.0%');
  });
});

describe('formatBytes', () => {
  it('scales to a readable unit', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(1024)).toBe('1.0 KB');
    expect(formatBytes(1536)).toBe('1.5 KB');
    expect(formatBytes(1024 * 1024)).toBe('1.0 MB');
    expect(formatBytes(4 * 1024 * 1024 * 1024)).toBe('4.0 GB');
  });
  it('is safe for negative and non-finite input', () => {
    expect(formatBytes(-1)).toBe('0 B');
    expect(formatBytes(Number.NaN)).toBe('0 B');
  });
});

// The panel sentence reads "Examined N of M objects; of those, X of Y bytes
// were successfully read". Y has to be the examined population. Using the whole
// manifest instead made a flawless sample run render as though nearly every
// byte had failed to read.
describe('examinedBytesFraction', () => {
  const sampled: StorageVerifyReport = {
    ok: true, mode: 'sample', manifestId: 1, capturedAt: '2026-07-19T09:00:00Z',
    objectsInManifest: 1000, objectsChecked: 100,
    bytesInManifest: 1000, bytesExamined: 100, bytesChecked: 100,
    checkedFraction: 0.1, bytesCheckedFraction: 0.1,
    problems: [], problemsTotal: 0, log: [],
  };
  it('a clean sample reads as fully read over the population it examined', () => {
    expect(examinedBytesFraction(sampled)).toBe(1);
  });
  it('is not the whole-manifest share, which stays available separately', () => {
    expect(sampled.bytesCheckedFraction).toBe(0.1);
    expect(examinedBytesFraction(sampled)).not.toBe(sampled.bytesCheckedFraction);
  });
  it('an unreadable object lowers it, because it contributes no bytes', () => {
    expect(examinedBytesFraction({ ...sampled, bytesChecked: 40 })).toBe(0.4);
  });
  it('returns 0 rather than dividing by an empty population', () => {
    expect(examinedBytesFraction({ ...sampled, bytesExamined: 0, bytesChecked: 0 })).toBe(0);
  });
});

describe('verifyVerdictLabel', () => {
  const base: StorageVerifyReport = {
    ok: true, mode: 'full', manifestId: 1, capturedAt: '2026-07-19T09:00:00Z',
    objectsInManifest: 10, objectsChecked: 10, bytesInManifest: 100, bytesExamined: 100, bytesChecked: 100,
    checkedFraction: 1, bytesCheckedFraction: 1, problems: [], problemsTotal: 0, log: [],
  };
  it('a passing full run reads as PASS', () => {
    expect(verifyVerdictLabel(base)).toBe('PASS');
  });
  // A sampled OK must NEVER render as a bare "OK" - it did not check
  // everything and cannot authorize a delete.
  it('a passing sample run reads as SAMPLE PASS', () => {
    expect(verifyVerdictLabel({ ...base, mode: 'sample', checkedFraction: 0.101 })).toBe('SAMPLE PASS');
  });
  it('any failure reads as FAIL regardless of mode', () => {
    expect(verifyVerdictLabel({ ...base, ok: false, problemsTotal: 3 })).toBe('FAIL');
    expect(verifyVerdictLabel({ ...base, mode: 'sample', ok: false, problemsTotal: 1 })).toBe('FAIL');
  });
  it('no report reads as not verified', () => {
    expect(verifyVerdictLabel(null)).toBe('NOT VERIFIED');
    expect(verifyVerdictLabel(undefined)).toBe('NOT VERIFIED');
  });
});

describe('progressPercent', () => {
  const job = (over: Partial<StorageMigrationJob>): StorageMigrationJob => ({
    id: 'x', kind: 'migrate', phase: 'copying', dataSet: 'modpacks',
    sourceLabel: '', targetLabel: '', startedBy: '', startedByName: '',
    startedAt: '', updatedAt: '',
    objectsTotal: 0, objectsDone: 0, objectsSkipped: 0,
    bytesTotal: 0, bytesDone: 0, currentKey: '',
    deleteSource: false, configSwitched: false, verifyMode: 'full', log: [], stale: false,
    ...over,
  });
  // Bytes, not objects: object counts are wildly non-uniform across these
  // data sets (a 4 GB archive next to a 2 KB attachment).
  it('is driven by bytes', () => {
    expect(progressPercent(job({ bytesTotal: 200, bytesDone: 50, objectsTotal: 2, objectsDone: 2 }))).toBe(25);
  });
  it('is 0 rather than NaN when nothing is known yet', () => {
    expect(progressPercent(job({ bytesTotal: 0, bytesDone: 0 }))).toBe(0);
  });
  it('is 100 on done regardless of counters', () => {
    expect(progressPercent(job({ phase: 'done', bytesTotal: 0, bytesDone: 0 }))).toBe(100);
  });
  it('never exceeds 100', () => {
    expect(progressPercent(job({ bytesTotal: 10, bytesDone: 99 }))).toBe(100);
  });
});
