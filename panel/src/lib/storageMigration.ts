// Pure, framework-free logic for the Storage migration tab so it can be unit
// tested without React. Every guard here MIRRORS a server-side rule; the
// server remains authoritative (StorageMigrationRequest.Validate returns 400),
// and these exist so the operator is never allowed to submit something that
// would be rejected.

export type StorageMigrationPhase =
  | 'preparing'
  | 'manifesting'
  | 'copying'
  | 'verifying'
  | 'switching_config'
  | 'deleting_source'
  | 'done'
  | 'failed'
  | 'cancelled';

export type StorageJobKind = 'migrate' | 'manifest' | 'verify';
export type VerifyMode = 'full' | 'sample';

export type StorageVerifyStatus =
  | 'ok'
  | 'missing'
  | 'extra'
  | 'size_mismatch'
  | 'checksum_mismatch'
  | 'unreadable';

export interface StorageVerifyEntry {
  key: string;
  status: StorageVerifyStatus;
  expectedSize: number;
  actualSize: number;
}

export interface StorageVerifyReport {
  ok: boolean;
  mode: VerifyMode;
  manifestId: number;
  capturedAt: string;
  objectsInManifest: number;
  objectsChecked: number;
  bytesInManifest: number;
  /**
   * Manifest size of the objects the run attempted to read, i.e. the population
   * objectsChecked counts. This is the denominator bytesChecked belongs over;
   * bytesInManifest describes the whole manifest and in sample mode is a
   * different, larger population.
   */
  bytesExamined: number;
  bytesChecked: number;
  checkedFraction: number;
  /** bytesChecked over the WHOLE manifest, not over bytesExamined. */
  bytesCheckedFraction: number;
  problems: StorageVerifyEntry[];
  problemsTotal: number;
  log: string[];
}

export interface StorageMigrationJob {
  id: string;
  kind: StorageJobKind;
  phase: StorageMigrationPhase;
  dataSet: string;
  sourceLabel: string;
  targetLabel: string;
  startedBy: string;
  startedByName: string;
  startedAt: string;
  updatedAt: string;
  finishedAt?: string;
  objectsTotal: number;
  objectsDone: number;
  objectsSkipped: number;
  bytesTotal: number;
  bytesDone: number;
  currentKey: string;
  manifestId?: number;
  deleteSource: boolean;
  // configSwitched is true once the active storage config has been repointed at
  // the target. It stays false for a row-to-row migrate, where Core owns no
  // such config, and after a switch that failed.
  configSwitched: boolean;
  // '' is a real value here, not a typing gap: Validate does not require
  // verifyMode for kind: "manifest" (there is nothing to verify yet), so a
  // manifest-only job really does serialize verifyMode as "". Only migrate
  // and verify jobs populate it with 'full' or 'sample'.
  verifyMode: VerifyMode | '';
  verify?: StorageVerifyReport;
  log: string[];
  error?: string;
  stale: boolean;
}

export interface StorageManifest {
  id: number;
  dataSet: string;
  backendLabel: string;
  algo: string;
  capturedAt: string;
  objectCount: number;
  totalBytes: number;
  createdBy: string;
}

export interface StorageDataSet {
  id: string;
  label: string;
  backendLabel: string;
  migratable: boolean;
  // supportsTargetConfig says HOW this data set names a migrate target. True:
  // it owns a settings-configured backend of its own (core-storage as a
  // whole, or modpacks), so the target is an ad-hoc storage config typed into
  // the wizard. False: this data set is either a namespace INSIDE the shared
  // Core file storage (ticket-attachments, ticket-backups,
  // modpacks@core-storage - switching on behalf of one would strand the
  // others) or a server-backups row (already multi-storage by design), and
  // its target is another data set id instead.
  supportsTargetConfig: boolean;
  note?: string;
  latestManifest?: StorageManifest | null;
}

// TargetConfigForm mirrors the server's CoreStorageConfig wire shape, and the
// Core file storage settings form uses the same fields. s3SecretKey is
// write-only: it is typed in, sent once, and never read back from the server.
export interface TargetConfigForm {
  backend: '' | 'path' | 's3';
  path: string;
  pathConfirmed: boolean;
  s3Endpoint: string;
  s3Bucket: string;
  s3Region: string;
  s3AccessKey: string;
  s3SecretKey: string;
  s3PathStyle: boolean;
  s3Prefix: string;
  // When > 0 the s3 target is a saved storage connection; the inline s3 fields
  // are ignored and the credentials come from the connection.
  connectionId: number;
}

export interface MigrationForm {
  dataSet: string;
  // targetKind selects which of the two target fields is meaningful. The
  // server rejects a request that sets both or neither.
  targetKind: 'dataset' | 'config';
  targetDataSet: string;
  targetConfig: TargetConfigForm;
  verifyMode: VerifyMode;
  deleteSource: boolean;
}

export const EMPTY_TARGET_CONFIG: TargetConfigForm = {
  backend: '',
  path: '',
  pathConfirmed: false,
  s3Endpoint: '',
  s3Bucket: '',
  s3Region: '',
  s3AccessKey: '',
  s3SecretKey: '',
  s3PathStyle: false,
  s3Prefix: '',
  connectionId: 0,
};

export const EMPTY_MIGRATION_FORM: MigrationForm = {
  dataSet: '',
  targetKind: 'config',
  targetDataSet: '',
  targetConfig: EMPTY_TARGET_CONFIG,
  verifyMode: 'full',
  deleteSource: false,
};

const LIVE_PHASES: StorageMigrationPhase[] = [
  'preparing', 'manifesting', 'copying', 'verifying', 'switching_config', 'deleting_source',
];

export function storageMigrationInProgress(phase?: StorageMigrationPhase): boolean {
  return !!phase && LIVE_PHASES.includes(phase);
}

// isCancellablePhase excludes switching_config and deleting_source
// deliberately: by then verification has passed, and a half-applied config
// switch or a half-cancelled delete is strictly worse than a completed one.
// The server refuses both too.
const UNCANCELLABLE_PHASES: StorageMigrationPhase[] = ['switching_config', 'deleting_source'];

export function isCancellablePhase(phase?: StorageMigrationPhase): boolean {
  return storageMigrationInProgress(phase) && !UNCANCELLABLE_PHASES.includes(phase!);
}

// deleteSourceAllowed mirrors safety invariant 2: a sampled verification did
// not check every object, so it can never authorize deleting the source.
export function deleteSourceAllowed(verifyMode: VerifyMode): boolean {
  return verifyMode === 'full';
}

// targetConfigValid mirrors the server's validateCoreStorageConfig. The server
// stays authoritative; this exists so the wizard's submit button is not offered
// for a config the API would reject with 400.
export function targetConfigValid(cfg: TargetConfigForm): boolean {
  if (cfg.backend === 's3') {
    // A saved connection supplies the credentials; the inline s3 fields are
    // then irrelevant. The server validates the connection itself.
    if (cfg.connectionId > 0) return true;
    return !!cfg.s3Bucket && !!cfg.s3AccessKey && !!cfg.s3SecretKey;
  }
  if (cfg.backend === 'path') {
    // Absolute means a Linux absolute path: the configured path is always a
    // path inside the Core container, which ships as a Linux image only. Same
    // rule, and same reasoning, as the server-side check.
    return cfg.path.startsWith('/') && cfg.pathConfirmed;
  }
  return false;
}

export function canStartMigration(form: MigrationForm, dataSets: StorageDataSet[]): boolean {
  if (!form.dataSet) return false;
  if (form.deleteSource && !deleteSourceAllowed(form.verifyMode)) return false;
  // Mirrors services/storage_migration_job.go Validate: "deleteSource
  // requires targetConfig". With a targetDataSet target nothing repoints the
  // consuming subsystem, so deleting the source here would orphan live
  // references. The operator migrates, verifies, repoints by hand, and only
  // then removes the old storage themselves.
  if (form.deleteSource && form.targetKind !== 'config') return false;

  const source = dataSets.find(d => d.id === form.dataSet);
  if (!source || !source.migratable) return false;

  // A data set advertises which target shape it takes; using the other one
  // would be a 400 from the server, so it is not offered here either.
  if (form.targetKind === 'config') {
    if (!source.supportsTargetConfig) return false;
    return targetConfigValid(form.targetConfig);
  }

  // supportsTargetConfig gates ONLY the ad-hoc-config target shape above; it
  // says nothing about whether this data set may pair with another one in the
  // row-to-row shape, in EITHER role. Start() resolves TargetDataSet through
  // plain Resolve() and checks only that it resolves and differs from the
  // source - it never consults supportsTargetConfig on the target, so this
  // must not either. Blocking it would forbid legitimate row-to-row pairs in
  // both directions: modpacks -> modpacks@core-storage (stage a copy on Core
  // storage, verify, repoint by hand) and modpacks@core-storage -> modpacks
  // (catch a leftover Core-storage copy up to an already-repointed dedicated
  // modpack backend).
  //
  // Neither is the ONLY way to move modpacks between backends, and neither is
  // the preferred one. dataSet "modpacks" + targetConfig is a separate,
  // server-supported flow (sourceCoreStorageConfigFor accepts modpacks,
  // because modpack_storage_* is its own settings namespace), and it is the
  // better expression: it repoints modpack_storage_provider automatically
  // after a passing verify, and it is the only form Validate permits
  // deleteSource on.
  if (!form.targetDataSet || form.dataSet === form.targetDataSet) return false;
  const target = dataSets.find(d => d.id === form.targetDataSet);
  return !!target && target.migratable;
}

// startBlockReason explains why "Start migration" is disabled, in the SAME
// order as canStartMigration/targetConfigValid. It leads with a call to
// canStartMigration itself, so the ''-vs-reason split can never drift out of
// step with the button no matter what either function grows later; the
// per-field messages below it are just the human-readable "why", derived by
// calling targetConfigValid rather than re-deriving its pass/fail logic, and
// need to be kept in step with it by hand only for their WORDING, not for
// whether the button is enabled.
export function startBlockReason(form: MigrationForm, dataSets: StorageDataSet[]): string {
  if (canStartMigration(form, dataSets)) return '';

  if (!form.dataSet) return 'Choose a source data set.';
  if (form.deleteSource && !deleteSourceAllowed(form.verifyMode)) {
    return 'A sampled verification cannot authorize deleting the source: switch to a full verification or uncheck delete.';
  }
  if (form.deleteSource && form.targetKind !== 'config') {
    return 'Deleting the source requires the new-storage-backend target shape, not another data set.';
  }
  const source = dataSets.find(d => d.id === form.dataSet);
  if (!source || !source.migratable) return 'The selected source is unknown or not migratable.';
  if (form.targetKind === 'config') {
    if (!source.supportsTargetConfig) return 'The selected source cannot target a new storage backend; choose another data set as the target instead.';
    const cfg = form.targetConfig;
    if (!targetConfigValid(cfg)) {
      if (cfg.backend === 's3') {
        if (!cfg.s3Bucket) return 'Enter the target bucket.';
        if (!cfg.s3AccessKey) return 'Enter the target access key.';
        if (!cfg.s3SecretKey) return 'Enter the target secret key.';
      } else if (cfg.backend === 'path') {
        if (!cfg.path.startsWith('/')) return 'Enter an absolute target path (starting with "/").';
        if (!cfg.pathConfirmed) return 'Confirm the target path is reachable from every Core instance and is not the current location.';
      } else {
        return 'Choose a target storage backend (S3-compatible or filesystem path).';
      }
    }
    return 'Complete the target storage config.';
  }
  if (!form.targetDataSet || form.dataSet === form.targetDataSet) return 'Choose a target data set different from the source.';
  const target = dataSets.find(d => d.id === form.targetDataSet);
  if (!target || !target.migratable) return 'The selected target is unknown or not migratable.';
  return 'Complete the migration form.';
}

// StorageTargetConfigBody mirrors services.StorageTargetConfig. s3SecretKey is
// write-only: it goes up with the start request and is never returned by any
// endpoint, so the wizard must always collect it fresh.
export interface StorageTargetConfigBody {
  backend: string;
  path?: string;
  pathConfirmed?: boolean;
  s3Endpoint?: string;
  s3Bucket?: string;
  s3Region?: string;
  s3AccessKey?: string;
  s3SecretKey?: string;
  s3PathStyle?: boolean;
  s3Prefix?: string;
  connectionId?: number;
}

export interface StartStorageMigrationBody {
  kind: StorageJobKind;
  dataSet: string;
  // Exactly one of these on a migrate job. The server rejects both or neither.
  targetDataSet?: string;
  targetConfig?: StorageTargetConfigBody;
  verifyMode?: VerifyMode;
  deleteSource?: boolean;
  manifestId?: number;
}

// startMigrateFromForm is the wizard's submit path. It never sends BOTH
// targetDataSet and targetConfig (the server rejects both, and neither, with
// 400); that they are also non-empty is canStartMigration's precondition, not
// this builder's - it forwards an empty targetDataSet as-is.
// It mirrors canStartMigration's deleteSource rule in full: dropped
// whenever the verify mode cannot authorize it (safety invariant 2) AND
// whenever the target is the row-to-row shape, where nothing repoints the
// consuming subsystem (Validate's "deleteSource requires targetConfig").
//
// For a path target the s3 fields are omitted entirely rather than sent
// empty, and for an s3 target the path fields are omitted, so a half-filled
// form from a backend the operator switched away from cannot travel with the
// request. An unset backend ('') is its own explicit branch rather than
// falling into the path case: it returns {backend: ''} with no path/s3 fields
// at all, so the server's own "targetConfig.backend is required" 400 is what
// the operator sees, not a fabricated empty path request.
export function startMigrateFromForm(form: MigrationForm): StartStorageMigrationBody {
  const body: StartStorageMigrationBody = {
    kind: 'migrate',
    dataSet: form.dataSet,
    verifyMode: form.verifyMode,
    deleteSource: form.targetKind === 'config' && form.verifyMode === 'full' && form.deleteSource,
  };
  if (form.targetKind === 'dataset') {
    body.targetDataSet = form.targetDataSet;
    return body;
  }
  const c = form.targetConfig;
  if (c.backend === 's3' && c.connectionId > 0) {
    // A saved connection: send only the reference. The server pulls the
    // credentials from the connection; no inline secret goes on the wire.
    body.targetConfig = { backend: 's3', connectionId: c.connectionId };
  } else if (c.backend === 's3') {
    body.targetConfig = {
      backend: 's3',
      s3Endpoint: c.s3Endpoint,
      s3Bucket: c.s3Bucket,
      s3Region: c.s3Region,
      s3AccessKey: c.s3AccessKey,
      s3SecretKey: c.s3SecretKey,
      s3PathStyle: c.s3PathStyle,
      s3Prefix: c.s3Prefix,
    };
  } else if (c.backend === 'path') {
    body.targetConfig = { backend: 'path', path: c.path, pathConfirmed: c.pathConfirmed };
  } else {
    body.targetConfig = { backend: c.backend };
  }
  return body;
}

export function formatPercent(fraction: number): string {
  if (!Number.isFinite(fraction)) return '0.0%';
  return `${(fraction * 100).toFixed(1)}%`;
}

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return unit === 0 ? `${Math.round(value)} B` : `${value.toFixed(1)} ${BYTE_UNITS[unit]}`;
}

// examinedBytesFraction is bytesChecked over the population the run actually
// attempted, which is what a sentence beginning "of those" has to use.
// report.bytesCheckedFraction answers a different question - the share of the
// WHOLE manifest - and in sample mode those two populations differ by design,
// so the fields are deliberately not interchangeable.
//
// Returns 0 when nothing was examined, mirroring the engine's own guard against
// dividing by an empty manifest.
export function examinedBytesFraction(report: StorageVerifyReport): number {
  if (report.bytesExamined <= 0) return 0;
  return report.bytesChecked / report.bytesExamined;
}

// verifyVerdictLabel never renders a bare "OK" for a sampled run: a sample
// checked only part of the data and must not be read as a full guarantee.
export function verifyVerdictLabel(report: StorageVerifyReport | null | undefined): string {
  if (!report) return 'NOT VERIFIED';
  if (!report.ok) return 'FAIL';
  return report.mode === 'sample' ? 'SAMPLE PASS' : 'PASS';
}

// progressPercent is driven by BYTES, not object counts: object sizes are
// wildly non-uniform across these data sets (a 4 GB server-backup archive
// next to a 2 KB ticket attachment), so a byte bar is the only one that
// tracks remaining time usefully. The numeric object counter is shown too.
export function progressPercent(job: StorageMigrationJob): number {
  if (job.phase === 'done') return 100;
  if (!job.bytesTotal || job.bytesTotal <= 0) return 0;
  return Math.min(100, Math.round((job.bytesDone / job.bytesTotal) * 100));
}
