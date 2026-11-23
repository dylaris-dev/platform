"use client";

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
    HardDrive, Loader2, CircleCheck, CircleAlert, Play, ShieldAlert,
    Download, FileSearch, X, ArrowRight, Info, MoreHorizontal,
} from 'lucide-react';
import {
    getStorageOverview, getStorageMigration, startStorageMigration,
    cancelStorageMigration, downloadManifestCSV, startMigrateFromForm,
} from '@/lib/api/storageMigration';
import {
    storageMigrationInProgress, isCancellablePhase, deleteSourceAllowed,
    canStartMigration, startBlockReason, formatPercent, formatBytes, verifyVerdictLabel, examinedBytesFraction,
    progressPercent, EMPTY_MIGRATION_FORM, EMPTY_TARGET_CONFIG,
    type MigrationForm, type StorageDataSet, type StorageMigrationJob,
    type StorageMigrationPhase, type StorageVerifyReport, type TargetConfigForm,
    type VerifyMode,
} from '@/lib/storageMigration';
import { systemEvents } from '@/lib/systemEvents';
import { listStorageConnections, type StorageConnection } from '@/lib/api';
import { toast } from '@/components/ui/Toast';
import SettingsPage from '@/components/settings/SettingsPage';
import ModalPanel from '@/components/ui/ModalPanel';
import { fmtDateTime } from '@/lib/time';
import Select from '@/components/ui/Select';
import Checkbox from '@/components/ui/Checkbox';
import Callout from '@/components/server/Callout';
import Menu from '@/components/ui/Menu';
import EmptyState from '@/components/ui/EmptyState';
import Section from '@/components/ui/Section';
import '@/styles/settings-storage.css';

const PHASE_LABEL: Record<StorageMigrationPhase, string> = {
    preparing: 'Preparing',
    manifesting: 'Inventorying',
    copying: 'Copying',
    verifying: 'Verifying',
    switching_config: 'Switching config',
    deleting_source: 'Deleting source',
    done: 'Done',
    failed: 'Failed',
    cancelled: 'Cancelled',
};

// MODPACKS_PREFIX matches both modpack-flavored data set ids ("modpacks" and
// "modpacks@core-storage"); their verification covers database-referenced
// objects only, so the report renders an extra note for both.
const MODPACKS_PREFIX = 'modpacks';

export default function StorageMigrationTab() {
    const [dataSets, setDataSets] = useState<StorageDataSet[]>([]);
    const [job, setJob] = useState<StorageMigrationJob | null>(null);
    const [loading, setLoading] = useState(true);
    // Kept as two separate error slots (rather than one shared loadError) so a
    // job-status failure never displaces the data-set list, and a data-set
    // overview failure never gets reported under the job panel: each renders
    // only next to the section it actually describes.
    const [overviewError, setOverviewError] = useState<string | null>(null);
    const [jobError, setJobError] = useState<string | null>(null);

    const [wizardOpen, setWizardOpen] = useState(false);
    const [form, setForm] = useState<MigrationForm>(EMPTY_MIGRATION_FORM);
    const [connections, setConnections] = useState<StorageConnection[]>([]);
    useEffect(() => {
        listStorageConnections().then(res => {
            if (res.success && res.connections) setConnections(res.connections);
        });
    }, []);
    const [submitting, setSubmitting] = useState(false);

    const [cancelling, setCancelling] = useState(false);
    const [busyDataSet, setBusyDataSet] = useState('');
    const [downloading, setDownloading] = useState(0);

    const flash = (msg: string, ok = true) => toast(msg, ok);

    // load() must not silently swallow a failed request: Overview deliberately
    // returns 500 (rather than an empty list) when it cannot read manifests,
    // specifically so this is never mistaken for "storage not configured yet".
    // Rendering the empty state on a failed request would tell the operator
    // their storage is unconfigured when Core actually could not read it.
    const load = useCallback(async () => {
        const [ov, jb] = await Promise.all([getStorageOverview(), getStorageMigration()]);
        if (ov.success && ov.dataSets) {
            setDataSets(ov.dataSets);
            setOverviewError(null);
        } else {
            setOverviewError(ov.message || 'Could not load the storage overview.');
        }
        if (jb.success) {
            setJob(jb.hasJob && jb.job ? jb.job : null);
            setJobError(null);
        } else {
            setJobError(jb.message || 'Could not load the migration job status.');
        }
        setLoading(false);
    }, []);

    useEffect(() => { load(); }, [load]);

    // Live refresh, mirroring DatabaseTab exactly: the SSE event covers the
    // start/finish transitions, and the poll covers in-phase progress, which
    // advances continuously (bytes/objects) without any handler firing.
    useEffect(() => {
        const unsub = systemEvents.on('storagemigration.changed', () => load());
        return () => unsub();
    }, [load]);

    useEffect(() => {
        if (!job || !storageMigrationInProgress(job.phase)) return;
        const t = setInterval(load, 2500);
        return () => clearInterval(t);
    }, [job, load]);

    const busy = !!job && storageMigrationInProgress(job.phase);
    const migratable = useMemo(() => dataSets.filter(d => d.migratable), [dataSets]);
    const sourceSet = useMemo(() => dataSets.find(d => d.id === form.dataSet), [dataSets, form.dataSet]);
    const targetSet = useMemo(() => dataSets.find(d => d.id === form.targetDataSet), [dataSets, form.targetDataSet]);
    // sameLocationSuspected flags a row-to-row pair whose backendLabel strings
    // are identical. That label is location-COMPLETE for the Core-storage-
    // derived rows (core-storage/ticket-attachments/ticket-backups/
    // modpacks-on-core-storage all encode endpoint+bucket+prefix or path, plus
    // sub-prefix) and for modpacks when modpack_storage_provider is
    // "core-storage" - so an equal label there reliably means the same
    // physical location (e.g. modpacks <-> modpacks@core-storage under that
    // setting). It is NOT location-complete for a server-backups row, whose
    // label is only "Name (provider)": two distinct backups configs can share
    // that label. So an equal label is real signal; an UNequal label proves
    // nothing either way. This is a warning, not a block, precisely because
    // the check cannot see location equality it isn't told about.
    const sameLocationSuspected = form.targetKind === 'dataset'
        && !!sourceSet?.backendLabel
        && sourceSet.backendLabel === targetSet?.backendLabel;
    // Every migratable data set except the source itself is offerable as a
    // row-to-row target. Do NOT additionally filter on supportsTargetConfig:
    // that field says how a data set names ITS OWN target when acting as a
    // source, and Start() never consults it on the target side (it resolves
    // TargetDataSet through plain Resolve and checks only that it resolves and
    // differs from the source). Filtering it out here would make the panel
    // stricter than the API and would hide the legitimate
    // modpacks@core-storage -> modpacks pairing, which is how an operator
    // catches a leftover Core-storage copy up to an already-repointed
    // dedicated modpack backend.
    const targets = useMemo(
        () => migratable.filter(d => d.id !== form.dataSet),
        [migratable, form.dataSet],
    );
    const formValid = canStartMigration(form, dataSets);
    // The API refuses deleteSource unless the target is an ad-hoc config: with
    // a row-to-row target nothing repoints the consuming subsystem, so the
    // delete would orphan live references. The checkbox is not rendered at all
    // for that shape - a disabled-but-visible control with a 400 behind it is
    // worse than an explanation.
    const canOfferDelete = form.targetKind === 'config';

    // pickSource keeps targetKind consistent with what the chosen data set
    // actually accepts, and drops the other target field so a stale value can
    // never travel with the request. deleteSource resets too: leaving it set
    // while switching to a row-to-row source would silently disable the submit
    // button via canStartMigration with nothing on screen explaining why.
    const pickSource = (d: StorageDataSet) => setForm(f => ({
        ...f,
        dataSet: d.id,
        targetKind: d.supportsTargetConfig ? 'config' : 'dataset',
        targetDataSet: '',
        targetConfig: EMPTY_TARGET_CONFIG,
        deleteSource: false,
    }));

    // pickTargetKind switches which target shape is active for a source that
    // supports both (supportsTargetConfig: true - core-storage, modpacks). It
    // clears the OTHER target field for the same reason pickSource does: a
    // stale value must never travel with the request. deleteSource resets too
    // - it is only legal for the config shape (canOfferDelete).
    const pickTargetKind = (kind: 'dataset' | 'config') => setForm(f => ({
        ...f,
        targetKind: kind,
        targetDataSet: '',
        targetConfig: EMPTY_TARGET_CONFIG,
        deleteSource: false,
    }));

    const openWizard = () => {
        const first = migratable[0];
        setForm(first
            ? {
                ...EMPTY_MIGRATION_FORM,
                dataSet: first.id,
                targetKind: first.supportsTargetConfig ? 'config' : 'dataset',
            }
            : EMPTY_MIGRATION_FORM);
        setWizardOpen(true);
    };

    const submitMigration = async () => {
        if (!formValid) return;
        setSubmitting(true);
        const res = await startStorageMigration(startMigrateFromForm(form));
        setSubmitting(false);
        if (!res.success) {
            flash(res.message || 'Could not start the migration.', false);
            return;
        }
        setWizardOpen(false);
        flash('Migration started.');
        load();
    };

    const captureManifest = async (dataSet: string) => {
        setBusyDataSet(dataSet);
        const res = await startStorageMigration({ kind: 'manifest', dataSet });
        setBusyDataSet('');
        if (!res.success) {
            flash(res.message || 'Could not start the inventory.', false);
            return;
        }
        flash('Inventory started. Every object the data set lists is read once and checksummed.');
        load();
    };

    const verifyAgainstLatest = async (ds: StorageDataSet, mode: VerifyMode) => {
        if (!ds.latestManifest) {
            flash('Capture a manifest first: there is nothing to verify against.', false);
            return;
        }
        setBusyDataSet(ds.id);
        const res = await startStorageMigration({
            kind: 'verify', dataSet: ds.id, verifyMode: mode, manifestId: ds.latestManifest.id,
        });
        setBusyDataSet('');
        if (!res.success) {
            flash(res.message || 'Could not start the verification.', false);
            return;
        }
        flash(`Verification started (${mode}).`);
        load();
    };

    const doCancel = async () => {
        setCancelling(true);
        const res = await cancelStorageMigration();
        setCancelling(false);
        if (!res.success) {
            flash(res.message || 'Could not cancel.', false);
            return;
        }
        flash('Cancellation requested; it takes effect at the next object boundary.');
        load();
    };

    const doDownload = async (manifestId: number, dataSet: string) => {
        setDownloading(manifestId);
        const res = await downloadManifestCSV(manifestId, dataSet);
        setDownloading(0);
        if (!res.success) flash(res.message || 'Download failed.', false);
    };

    if (loading) return <SettingsPage title="Storage" icon={HardDrive} width="full" loading skeletonCards={2}>{null}</SettingsPage>;

    return (
        <SettingsPage
            title="Storage"
            icon={HardDrive}
            width="full"
            description={<>
                        Inventory, move and verify the blob data sets. Every migration is manifest-based:
                        the objects a data set lists are read once and checksummed, the copy skips only objects
                that are already byte-identical, and verification compares the target against that
                manifest. The source is never modified unless you explicitly opt in to deleting it.
            </>}
            actions={
                <button className="btn btn-primary shrink-0" onClick={openWizard} disabled={busy || migratable.length === 0}>
                    <Play size={14} /> New migration
                </button>
            }
        >

            {jobError && (
                <Callout tone="error" icon={<CircleAlert size={14} />} role="alert">{jobError}</Callout>
            )}
            {job && (
                <JobPanel job={job} onCancel={doCancel} cancelling={cancelling} />
            )}

            <Section title="Data sets" description="Each blob data set Core knows about, with its last inventory.">
                {overviewError && (
                    <Callout tone="error" icon={<CircleAlert size={14} />} role="alert">{overviewError}</Callout>
                )}
                {dataSets.length > 0 ? (
                    <div className="srv-surface">
                        <div role="list" aria-label="Data sets" className="flex flex-col gap-0.5 p-1.5">
                            {dataSets.map(ds => (
                                <DataSetRow
                                    key={ds.id}
                                    ds={ds}
                                    busy={busy || busyDataSet === ds.id}
                                    downloading={downloading}
                                    onManifest={() => captureManifest(ds.id)}
                                    onVerifyFull={() => verifyAgainstLatest(ds, 'full')}
                                    onVerifySample={() => verifyAgainstLatest(ds, 'sample')}
                                    onDownload={() => ds.latestManifest && doDownload(ds.latestManifest.id, ds.id)}
                                />
                            ))}
                        </div>
                    </div>
                ) : !overviewError ? (
                    <EmptyState
                        icon={<HardDrive size={20} />}
                        title="No storage data sets yet"
                        description="Configure Core file storage first."
                    />
                ) : null}
            </Section>

            <Section title="Manual migration" description="Three steps, in this order.">
                <ol className="srv-surface space-y-3 p-5 text-[13px] text-(--base-07)">
                    <li className="flex gap-3">
                        <span className="srv-num w-4 shrink-0 text-(--base-06)">1</span>
                        <span>
                            <strong className="text-(--base-09)">Capture a manifest.</strong>{' '}
                            Use <em>Inventory</em> on the data set below. This reads every object the data set
                            lists once and records its SHA-256, so it costs a full pass over what it lists.
                            Then download the CSV.
                        </span>
                    </li>
                    <li className="flex gap-3">
                        <span className="srv-num w-4 shrink-0 text-(--base-06)">2</span>
                        <span>
                            <strong className="text-(--base-09)">Move the data and reconfigure.</strong>{' '}
                            Copy the objects to the new backend with whatever tool you like (rclone, aws s3 sync, rsync),
                            then point Core file storage at it under Settings &gt; Core Storage. The panel does nothing in this step.
                        </span>
                    </li>
                    <li className="flex gap-3">
                        <span className="srv-num w-4 shrink-0 text-(--base-06)">3</span>
                        <span>
                            <strong className="text-(--base-09)">Verify.</strong>{' '}
                            Run <em>Verify (full)</em> against the manifest you captured. A full verification hashes
                            every object the manifest lists; a sample hashes a bounded subset and can never
                            authorize deleting a source.
                        </span>
                    </li>
                </ol>
            </Section>

            {wizardOpen && (
                <div className="modal-overlay animate-fade-in" onClick={() => !submitting && setWizardOpen(false)}>
                    <ModalPanel onClose={() => !submitting && setWizardOpen(false)} className="modal-panel w-full max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header flex items-center justify-between">
                            <h3 className="modal-title">New migration</h3>
                            <button onClick={() => setWizardOpen(false)} className="p-1 text-(--base-06) hover:text-(--base-09)" disabled={submitting}>
                                <X size={16} />
                            </button>
                        </div>
                        <div className="modal-body max-h-[70vh] overflow-y-auto space-y-5">
                            <div className="space-y-2">
                                <div className="srv-label">1. Source</div>
                                {migratable.map(d => (
                                    <ModeOption
                                        key={d.id}
                                        selected={form.dataSet === d.id}
                                        onClick={() => pickSource(d)}
                                        title={d.label}
                                        desc={d.backendLabel}
                                    />
                                ))}
                            </div>

                            <div className="space-y-2">
                                <div className="srv-label">2. Target</div>
                                {/* The source's own note is the only place the operator learns
                                    this data set's caveat before deciding a target - whether
                                    that is WHY a namespace inside the shared Core file storage
                                    cannot name a new backend, or that its verification only
                                    covers database-referenced objects. Rendered unconditionally:
                                    gating this on supportsTargetConfig used to hide it from
                                    exactly core-storage and modpacks, the only two sources for
                                    which the delete checkbox below is ever offered - so the
                                    operators most able to opt into a delete never saw the
                                    caveat that bears on it. */}
                                {sourceSet?.note && (
                                    <Callout tone="info" icon={<Info size={14} />}>{sourceSet.note}</Callout>
                                )}
                                {/* supportsTargetConfig gates the ad-hoc-config shape only, not
                                    whether this source may ALSO pair row-to-row with another data
                                    set (see the `targets` comment above). A source that supports
                                    both gets an explicit shape choice; a source that supports only
                                    the row shape gets no toggle, because there is no choice to make
                                    (an ad-hoc config would be a 400 for it). */}
                                {sourceSet?.supportsTargetConfig && (
                                    <div className="space-y-2">
                                        <ModeOption
                                            selected={form.targetKind === 'config'}
                                            onClick={() => pickTargetKind('config')}
                                            title="A new storage backend"
                                            desc="Type in an ad-hoc destination. The active config is repointed here after a passing verification."
                                        />
                                        <ModeOption
                                            selected={form.targetKind === 'dataset'}
                                            onClick={() => pickTargetKind('dataset')}
                                            title="Another data set"
                                            desc="Copy onto an already-configured data set instead. Nothing is repointed automatically; repoint the consumer yourself afterwards."
                                        />
                                    </div>
                                )}
                                {form.targetKind === 'config' ? (
                                    <TargetConfigFields
                                        cfg={form.targetConfig}
                                        connections={connections}
                                        onChange={patch => setForm(f => ({ ...f, targetConfig: { ...f.targetConfig, ...patch } }))}
                                    />
                                ) : targets.length === 0 ? (
                                    <Callout tone="info" icon={<Info size={14} />}>No other migratable data set is available as a target.</Callout>
                                ) : targets.map(d => (
                                    <ModeOption
                                        key={d.id}
                                        selected={form.targetDataSet === d.id}
                                        onClick={() => setForm(f => ({ ...f, targetDataSet: d.id }))}
                                        title={d.label}
                                        desc={d.backendLabel}
                                    />
                                ))}
                                {/* Rendered here, at the target picker, rather than next to the
                                    submit button: the modal body scrolls while the footer does
                                    not, so a warning placed at the bottom sits below the fold
                                    while Start stays clickable. This is the decision point. */}
                                {sameLocationSuspected && (
                                    <Callout tone="warning" icon={<ShieldAlert size={14} />} role="alert">
                                        Source and target report the same backend location ({sourceSet?.backendLabel}). If
                                        they really are the same place, the copy would do nothing, a verification would
                                        pass trivially by comparing the target against its own manifest, and removing
                                        &quot;the old copy&quot; afterwards would delete the only copy there is. This only
                                        compares the displayed labels and is not exhaustive - it can miss cases where two
                                        data sets alias the same storage under different labels, so the absence of this
                                        warning does not mean the locations are actually different.
                                    </Callout>
                                )}
                            </div>

                            {/* Scoped to the shape it is true for, matching services/storage_migration_job.go's
                                Start(): only the config-target shape has a config to switch and a
                                delete gate that can ever open. Rendering the config-shape wording
                                unconditionally used to promise a switch and a delete on the
                                row-to-row shape too, where neither step ever runs - and section 4
                                below already says so, so the wizard was contradicting itself on
                                the screen where the operator decides. */}
                            {form.targetKind === 'config' ? (
                                <Callout tone="info" icon={<Info size={14} />}>
                                    Order: copy, verify, switch the active config to the target, then delete the
                                    source objects named in the manifest if you opt in. The switch happens only
                                    after a passing verification, and if it fails nothing is deleted - the data
                                    stays in both places and the job panel tells you which config is live.
                                </Callout>
                            ) : (
                                <Callout tone="info" icon={<Info size={14} />}>
                                    Order: copy, then verify. This target is another data set, so the job repoints
                                    no config and deletes nothing; repoint the consuming subsystem yourself once
                                    the verification has passed.
                                </Callout>
                            )}

                            <div className="space-y-2">
                                <div className="srv-label">3. Verification scope</div>
                                <ModeOption
                                    selected={form.verifyMode === 'full'}
                                    onClick={() => setForm(f => ({ ...f, verifyMode: 'full' }))}
                                    title="Full"
                                    desc="Hash every object the manifest lists and compare it to the target. Slower, and the only scope that can authorize deleting the source."
                                />
                                <ModeOption
                                    selected={form.verifyMode === 'sample'}
                                    onClick={() => setForm(f => ({ ...f, verifyMode: 'sample', deleteSource: false }))}
                                    title="Sample"
                                    desc="Hash every small object plus a bounded draw of the large ones. Missing and extra objects are still detected in full."
                                />
                                {form.verifyMode === 'sample' && (
                                    <Callout tone="warning" icon={<ShieldAlert size={14} />}>
                                        A sampled verification does not check every object, so it can never authorize deleting the source.
                                    </Callout>
                                )}
                            </div>

                            <div className="space-y-2">
                                <div className="srv-label">4. After a passing verification</div>
                                {!canOfferDelete ? (
                                    <Callout tone="info" icon={<Info size={14} />}>
                                        Moving into another data set leaves the active config pointing at the source, so this
                                        job cannot delete it: nothing would repoint the subsystem that reads it. Migrate,
                                        verify, repoint the storage yourself, and remove the old backend afterwards.
                                    </Callout>
                                ) : (
                                    <>
                                        <Checkbox
                                            checked={form.deleteSource}
                                            disabled={!deleteSourceAllowed(form.verifyMode)}
                                            onChange={c => setForm(f => ({ ...f, deleteSource: c }))}
                                            label="Delete the source after a passing verification"
                                        />
                                        <Callout tone="warning" icon={<ShieldAlert size={14} />} role="alert">
                                            This is irreversible and cannot be cancelled once it starts. Objects written to the source
                                            after the manifest was captured are not in it, are not copied to the target, and are not
                                            deleted - and this verification cannot see them at all, because it compares the target
                                            against the manifest. Quiesce the source before a delete run, or capture a fresh inventory first.
                                        </Callout>
                                    </>
                                )}
                            </div>

                        </div>
                        <div className="modal-footer">
                            {!formValid && (
                                <span className="text-xs text-(--base-06) mr-auto self-center">{startBlockReason(form, dataSets)}</span>
                            )}
                            <button onClick={() => setWizardOpen(false)} className="btn btn-secondary" disabled={submitting}>Cancel</button>
                            <button
                                onClick={submitMigration}
                                className="btn btn-primary disabled:opacity-40 disabled:cursor-not-allowed"
                                disabled={!formValid || submitting}
                            >
                                {submitting ? <Loader2 size={14} className="animate-spin" /> : <ArrowRight size={14} />} Start migration
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </SettingsPage>
    );
}

function DataSetRow({
    ds, busy, downloading, onManifest, onVerifyFull, onVerifySample, onDownload,
}: {
    ds: StorageDataSet;
    busy: boolean;
    downloading: number;
    onManifest: () => void;
    onVerifyFull: () => void;
    onVerifySample: () => void;
    onDownload: () => void;
}) {
    const m = ds.latestManifest;
    return (
        <div role="listitem" className="app-row">
            <HardDrive size={16} className="shrink-0 text-(--base-06)" />
            <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                    <span className="text-sm font-medium text-(--base-09)">{ds.label}</span>
                    {ds.migratable
                        ? <span className="badge badge-accent">migratable</span>
                        : <span className="badge badge-neutral">manual only</span>}
                </div>
                <div className="srv-num mt-0.5 truncate text-xs text-(--base-06)">{ds.backendLabel}</div>
                <div className="mt-0.5 text-xs text-(--base-06)">
                    {m
                        ? <>Last inventory: {m.objectCount.toLocaleString()} objects, {formatBytes(m.totalBytes)} ({fmtDateTime(m.capturedAt)})</>
                        : <>Not yet inventoried</>}
                </div>
                {ds.note && (
                    <div className="mt-0.5 flex items-start gap-1 text-xs text-(--base-05)">
                        <Info size={12} className="mt-0.5 shrink-0" /> {ds.note}
                    </div>
                )}
            </div>
            <div className="flex shrink-0 items-center gap-1.5">
                <button onClick={onManifest} className="btn btn-secondary btn-sm" disabled={busy || !ds.migratable} title="Read every object this data set lists once and record its checksum">
                    <FileSearch size={12} /> Inventory
                </button>
                <Menu
                    trigger={
                        <button type="button" className="app-icon-btn" aria-label={`More actions for ${ds.label}`}>
                            <MoreHorizontal size={16} />
                        </button>
                    }
                    items={[
                        { label: 'Verify (full)', icon: <ShieldAlert size={14} />, disabled: busy || !m, onSelect: onVerifyFull },
                        { label: 'Verify (sample)', icon: <ShieldAlert size={14} />, disabled: busy || !m, onSelect: onVerifySample },
                        { type: 'separator' },
                        {
                            label: m && downloading === m.id ? 'Downloading...' : 'Download manifest (CSV)',
                            icon: m && downloading === m.id ? <Loader2 size={14} className="animate-spin" /> : <Download size={14} />,
                            disabled: !m || downloading === m?.id,
                            onSelect: onDownload,
                        },
                    ]}
                />
            </div>
        </div>
    );
}

function PhaseBadge({ phase, stale }: { phase: StorageMigrationPhase; stale: boolean }) {
    const cls = phase === 'done'
        ? 'bg-(--success-ghost) text-(--success-light) border-(--success-border)'
        : phase === 'failed'
            ? 'bg-(--error-ghost) text-(--error-light) border-(--error-border)'
            : phase === 'cancelled'
                ? 'bg-(--base-03) text-(--base-06) border-(--base-04)'
                : phase === 'deleting_source' || phase === 'switching_config'
                    ? 'bg-(--warning-ghost) text-(--warning-light) border-(--warning-border)'
                    : stale
                        ? 'bg-(--error-ghost) text-(--error-light) border-(--error-border)'
                        : 'bg-(--accent-ghost) text-(--accent-light) border-(--accent)';
    return (
        <span className={`inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs font-medium ${cls}`}>
            {storageMigrationInProgress(phase) && !stale && <Loader2 size={10} className="animate-spin" />}
            {PHASE_LABEL[phase]}
        </span>
    );
}

function JobPanel({ job, onCancel, cancelling }: { job: StorageMigrationJob; onCancel: () => void; cancelling: boolean }) {
    const [confirmCancel, setConfirmCancel] = useState(false);
    const pct = progressPercent(job);
    const inProgress = storageMigrationInProgress(job.phase);

    return (
        <div className="srv-surface space-y-4 p-5">
            <div className="flex items-center justify-between">
                <div className="text-sm font-semibold capitalize text-(--base-09)">{job.kind} job</div>
                <PhaseBadge phase={job.phase} stale={job.stale} />
            </div>

            <div className="text-xs text-(--base-06) space-y-0.5">
                <div>
                    Started by <span className="text-(--base-08)">{job.startedByName || job.startedBy || 'unknown'}</span>
                    {' '}&middot; {fmtDateTime(job.startedAt)}
                </div>
                <div>Source: <span className="font-mono">{job.sourceLabel || job.dataSet}</span></div>
                {job.targetLabel && <div>Target: <span className="font-mono">{job.targetLabel}</span></div>}
                {job.deleteSource && (
                    <div className="text-(--warning-light) flex items-center gap-1">
                        <ShieldAlert size={12} /> The source will be deleted, but only after a passing full verification AND a successful config switch.
                    </div>
                )}
                {job.phase === 'switching_config' && (
                    <div className="text-(--warning-light) flex items-center gap-1">
                        <ArrowRight size={12} /> Pointing the active storage config at the target. This step is not cancellable.
                    </div>
                )}
                {job.configSwitched && job.phase !== 'switching_config' && (
                    <div className="text-(--success-light) flex items-center gap-1">
                        <CircleCheck size={12} /> The active config now points at the target; the system is reading from it.
                    </div>
                )}
                {job.kind === 'migrate' && !job.configSwitched && job.phase === 'done' && (
                    <div className="text-(--warning-light) flex items-center gap-1">
                        {/* Mirrors the finish() comment in storage_migration_job.go:
                            say what the engine did, not what configs exist. Whether
                            anything still points at the source is unknowable from
                            here - the same over-claim that had to come out of the
                            delete log applies to this notice too. */}
                        <Info size={12} /> The engine did not repoint any config for this migrate, and it cannot
                        tell what still points at the source. Repoint the consuming subsystem at the target
                        yourself, and remove the old copy only once you have.
                    </div>
                )}
                {job.stale && (
                    <div className="text-(--error-light) flex items-center gap-1">
                        <CircleAlert size={12} /> No heartbeat - the Core running this job may have stopped.
                    </div>
                )}
            </div>

            {(inProgress || job.phase === 'done') && (
                <div>
                    <div className="flex justify-between text-[11px] text-(--base-06) mb-1">
                        <span className="truncate pr-2">{job.currentKey ? `current: ${job.currentKey}` : PHASE_LABEL[job.phase]}</span>
                        <span className="shrink-0">
                            {formatBytes(job.bytesDone)} / {formatBytes(job.bytesTotal)}
                            {' '}&middot; {job.objectsDone.toLocaleString()}/{job.objectsTotal.toLocaleString()} objects
                            {job.objectsSkipped > 0 && <> &middot; {job.objectsSkipped.toLocaleString()} skipped</>}
                        </span>
                    </div>
                    <div className="srv-meter-track" role="progressbar" aria-label="Migration progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)}>
                        <div
                            className="srv-meter-fill"
                            style={{ width: `${pct}%`, ...(job.phase === 'done' ? { background: 'var(--success-light)' } : {}) }}
                        />
                    </div>
                </div>
            )}

            {job.error && (
                <Callout tone="error" icon={<CircleAlert size={14} />} role="alert">{job.error}</Callout>
            )}

            {/* A manifest job carries no verification report, so it must not be
                labelled NOT VERIFIED and must not point at a report that is not
                there. Only a run that actually produced one gets a verdict. */}
            {job.phase === 'done' && (
                <div className="space-y-1 rounded-lg bg-(--success-ghost) p-3 text-xs text-(--success-light)">
                    <div className="flex items-center gap-1">
                        <CircleCheck size={12} />
                        {job.verify
                            ? `${verifyVerdictLabel(job.verify)} - see the report below.`
                            : 'Finished.'}
                    </div>
                    {/* finish() (storage_migration_job.go) always appends this
                        outcome sentence - what happened to the old copy - as the
                        LAST log line before the job goes done, so it is safe to
                        read directly rather than making the operator expand the
                        log to learn it. */}
                    {job.log?.length > 0 && (
                        <div className="text-(--base-08)">{job.log[job.log.length - 1]}</div>
                    )}
                </div>
            )}

            {job.verify && <VerifyView report={job.verify} dataSet={job.dataSet} />}

            {job.log?.length > 0 && (
                <details
                    className="text-xs"
                    open={inProgress || job.phase === 'done' || job.phase === 'failed' || job.phase === 'cancelled'}
                >
                    <summary className="srv-label cursor-pointer">Log ({job.log.length})</summary>
                    <pre className="mt-2 max-h-64 overflow-auto rounded-lg bg-(--base-01) p-3 font-mono text-[11px] text-(--base-06) whitespace-pre-wrap">
{job.log.join('\n')}
                    </pre>
                </details>
            )}

            {isCancellablePhase(job.phase) && (
                !confirmCancel ? (
                    <button className="btn btn-secondary btn-sm" onClick={() => setConfirmCancel(true)} disabled={cancelling}>
                        Cancel job
                    </button>
                ) : (
                    <div className="flex flex-wrap items-center gap-2 rounded-lg bg-(--warning-ghost) px-3 py-2">
                        <ShieldAlert size={14} className="text-(--warning-light)" />
                        <span className="text-xs text-(--base-07)">
                            Cancelling stops at the next object boundary. Nothing half-written is left behind, but any
                            objects already copied stay on the target; re-running with the same manifest resumes from
                            there. The source is untouched. Continue?
                        </span>
                        <button className="btn btn-primary btn-sm" onClick={onCancel} disabled={cancelling}>
                            {cancelling ? <Loader2 size={14} className="animate-spin" /> : null} Yes, cancel
                        </button>
                        <button className="btn btn-secondary btn-sm" onClick={() => setConfirmCancel(false)} disabled={cancelling}>Keep running</button>
                    </div>
                )
            )}
        </div>
    );
}

function VerifyView({ report, dataSet }: { report: StorageVerifyReport; dataSet: string }) {
    return (
        <div className="space-y-2">
            <div className={`text-xs ${report.mode === 'sample' ? 'rounded-lg bg-(--accent-ghost) px-3.5 py-2.5 text-(--base-08)' : 'text-(--base-06)'}`}>
                {/* "Examined", not "Checked": objectsChecked counts every object the
                    run attempted, including one reported missing or unreadable, so a
                    run where every object was unreadable still reaches 100% here while
                    the verdict is FAIL. Splitting out "successfully read" ties the byte
                    figure to the objects that actually passed, so the two numbers
                    explain each other instead of silently contradicting.

                    The "of those" denominator is bytesExamined, NOT bytesInManifest:
                    "those" is the examined population, and in sample mode the manifest
                    is a strictly larger one. Pairing them made a flawless sample run
                    read as though nearly every byte had failed. The whole-manifest
                    share is still worth stating, so sample mode gets it as its own
                    sentence where its denominator is named. */}
                <span className="mr-2 font-semibold text-(--base-09)">{verifyVerdictLabel(report)}</span>
                Examined {report.objectsChecked.toLocaleString()} of {report.objectsInManifest.toLocaleString()} objects
                ({formatPercent(report.checkedFraction)}); of those, {formatBytes(report.bytesChecked)} of {formatBytes(report.bytesExamined)}
                {' '}({formatPercent(examinedBytesFraction(report))}) were successfully read. A missing or unreadable
                object counts as examined here but contributes no bytes.
                {report.mode === 'sample' && ` That is ${formatPercent(report.bytesCheckedFraction)} of the manifest's ${formatBytes(report.bytesInManifest)} in total. Missing and extra objects were still detected in full.`}
            </div>

            {dataSet.startsWith(MODPACKS_PREFIX) && (
                <Callout tone="info" icon={<Info size={14} />}>
                    Modpack keys come from the database, so this verification covers database-referenced objects only.
                    An object in storage that no row points at is invisible to it.
                </Callout>
            )}

            {report.problemsTotal > 0 && (
                <details className="text-xs" open>
                    <summary className="srv-label cursor-pointer">
                        Problems ({report.problemsTotal.toLocaleString()}
                        {report.problems.length < report.problemsTotal && `, showing the first ${report.problems.length.toLocaleString()}`})
                    </summary>
                    <div className="mt-2 overflow-auto">
                        <table className="w-full text-xs font-mono">
                            <thead>
                                <tr className="text-(--base-05) text-left">
                                    <th className="py-1 pr-4 font-normal">Key</th>
                                    <th className="py-1 pr-4 font-normal text-right">Expected</th>
                                    <th className="py-1 pr-4 font-normal text-right">Actual</th>
                                    <th className="py-1 font-normal">Status</th>
                                </tr>
                            </thead>
                            <tbody>
                                {report.problems.map(p => (
                                    <tr key={`${p.key}-${p.status}`} className="border-t border-(--base-03)">
                                        <td className="py-1 pr-4 text-(--base-08) break-all">{p.key}</td>
                                        <td className="py-1 pr-4 text-right text-(--base-06)">{p.expectedSize > 0 ? formatBytes(p.expectedSize) : '-'}</td>
                                        <td className="py-1 pr-4 text-right text-(--base-06)">{p.actualSize > 0 ? formatBytes(p.actualSize) : '-'}</td>
                                        <td className="py-1">
                                            <span className={
                                                p.status === 'extra'
                                                    ? 'text-(--warning-light)'
                                                    : 'text-(--error-light)'
                                            }>{p.status}</span>
                                        </td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                        {report.problems.length < report.problemsTotal && (
                            <p className="text-[11px] text-(--base-05) mt-2">
                                The full list is in the manifest CSV export and in the job log.
                            </p>
                        )}
                    </div>
                </details>
            )}
        </div>
    );
}

// TargetConfigFields is the ad-hoc target storage form. It deliberately mirrors
// the Core file storage settings form field-for-field, because it IS the same
// config shape - the operator is describing a second instance of the thing they
// already configured once, so the two screens must not look like different
// concepts.
//
// The secret follows that form's UX exactly: type="password", never prefilled,
// never read back. No endpoint returns a stored S3 secret, and this one is for a
// backend that has not been saved yet, so there is nothing to prefill from and
// the operator always enters it fresh.
function TargetConfigFields({
    cfg, connections, onChange,
}: {
    cfg: TargetConfigForm;
    connections: StorageConnection[];
    onChange: (patch: Partial<TargetConfigForm>) => void;
}) {
    return (
        <div className="space-y-3">
            <ModeOption
                selected={cfg.backend === 's3'}
                onClick={() => onChange({ backend: 's3' })}
                title="S3-compatible"
                desc="Another bucket, another provider, or the same bucket under a different prefix."
            />
            <ModeOption
                selected={cfg.backend === 'path'}
                onClick={() => onChange({ backend: 'path' })}
                title="Filesystem path"
                desc="A directory inside the Core container: an NFS, WebDAV or SMB mount, or a local volume."
            />

            {cfg.backend === 's3' && (
                <div className="space-y-2">
                    <div className="flex flex-col gap-[5px]">
                        <label className="srv-label">Storage connection</label>
                        <Select
                            ariaLabel="Storage connection"
                            value={String(cfg.connectionId)}
                            onChange={v => onChange({ connectionId: Number(v) })}
                            options={[{ value: '0', label: 'Enter credentials manually' }, ...connections.map(c => ({ value: String(c.id), label: c.name }))]}
                        />
                    </div>
                    {cfg.connectionId > 0 ? (
                        <Callout tone="info" icon={<Info size={14} />}>Migrating to the saved connection. Its credentials are managed on the Storage Connections page.</Callout>
                    ) : (
                    <>
                    <Field label="Endpoint" value={cfg.s3Endpoint} onChange={v => onChange({ s3Endpoint: v })} placeholder="https://s3.example.com" />
                    <Field label="Bucket" value={cfg.s3Bucket} onChange={v => onChange({ s3Bucket: v })} />
                    <Field label="Region" value={cfg.s3Region} onChange={v => onChange({ s3Region: v })} placeholder="auto" />
                    <Field label="Access key" value={cfg.s3AccessKey} onChange={v => onChange({ s3AccessKey: v })} />
                    <Field label="Secret key" value={cfg.s3SecretKey} onChange={v => onChange({ s3SecretKey: v })} type="password" autoComplete="new-password" />
                    <Field label="Prefix (optional)" value={cfg.s3Prefix} onChange={v => onChange({ s3Prefix: v })} placeholder="prod" />
                    <Checkbox checked={cfg.s3PathStyle} onChange={c => onChange({ s3PathStyle: c })}
                        label="Use path-style addressing (required by MinIO and some S3-compatible providers)" />
                    </>
                    )}
                </div>
            )}

            {cfg.backend === 'path' && (
                <div className="space-y-2">
                    <Field label="Absolute path" value={cfg.path} onChange={v => onChange({ path: v })} placeholder="/mnt/new-storage" />
                    <Checkbox checked={cfg.pathConfirmed} onChange={c => onChange({ pathConfirmed: c })}
                        label="I confirm this path is reachable from every Core instance and is not the same directory as the current one. The server checks that too, by device and inode rather than by name, so a symlink or a bind mount to the current location is refused." />
                </div>
            )}
        </div>
    );
}

// Field is the shared labelled input for the target config form, using the
// established token classes so it matches the Core file storage settings form.
function Field({
    label, value, onChange, placeholder, type = 'text', autoComplete,
}: {
    label: string;
    value: string;
    onChange: (v: string) => void;
    placeholder?: string;
    type?: string;
    autoComplete?: string;
}) {
    return (
        <label className="block">
            <span className="srv-label">{label}</span>
            <input
                type={type}
                value={value}
                placeholder={placeholder}
                autoComplete={autoComplete}
                onChange={e => onChange(e.target.value)}
                className="input-field mt-1 w-full"
            />
        </label>
    );
}

// ModeOption is local to this tab: BackupsTab's copy is not exported.
function ModeOption({
    selected, onClick, title, desc,
}: {
    selected: boolean;
    onClick: () => void;
    title: string;
    desc: string;
}) {
    return (
        <button
            type="button"
            onClick={onClick}
            className="set-sto-option"
            aria-pressed={selected}
            data-selected={selected ? '' : undefined}
        >
            <span aria-hidden className="set-sto-option-dot" />
            <span className="min-w-0">
                <span className="block text-sm font-medium text-(--base-09)">{title}</span>
                <span className="block text-xs text-(--base-06) mt-0.5 break-all">{desc}</span>
            </span>
        </button>
    );
}
