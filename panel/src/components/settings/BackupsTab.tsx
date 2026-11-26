"use client";

import React, { useState, useEffect, useCallback } from 'react';
import { Plus, Trash2, Pencil, X, HardDrive, Cloud, Save, Cable, Server, Info, AlertTriangle, Link2, Archive, Loader2, Wrench, MoreHorizontal, TriangleAlert } from 'lucide-react';
import {
    BackupStorage,
    BackupConfig,
    BackupProvider,
    listBackupStorages, createBackupStorage, updateBackupStorage, deleteBackupStorage, testBackupStorage,
    getBackupConfig, saveBackupConfig,
    listStorageConnections,
    type StorageConnection, type BackupConnectionConfig,
} from '@/lib/api';
import { LimitField, LimitHelp } from '@/components/settings/LimitField';
import HelpTip from '@/components/ui/HelpTip';
import { useAppData } from '@/lib/AppDataContext';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard, { type SavableForm } from '@/components/settings/SettingsCard';
import { readConnTest, CONN_TEST_TIMEOUT_MS, type ConnTestResult } from '@/lib/connectionTest';
import { ConnectionTestNote } from '@/components/ui/ConnectionTest';
import ModalPanel from '@/components/ui/ModalPanel';
import StorageUploadCap from '@/components/settings/StorageUploadCap';
import StorageMaintenance from '@/components/settings/StorageMaintenance';
import { supportsStorageMaintenance } from '@/lib/storageOrphans';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';
import Checkbox from '@/components/ui/Checkbox';
import Menu from '@/components/ui/Menu';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import Callout from '@/components/server/Callout';
import '@/styles/settings-storage.css';

interface LocalConfig {
    basePath: string;
}
interface S3Config {
    endpoint: string;
    region: string;
    bucket: string;
    accessKeyId: string;
    secretAccessKey: string;
    forcePathStyle: boolean;
}

const S3_LABELS: Record<keyof S3Config, string> = {
    endpoint: 'Endpoint',
    region: 'Region',
    bucket: 'Bucket',
    accessKeyId: 'Access key ID',
    secretAccessKey: 'Secret access key',
    forcePathStyle: 'Force path style',
};

const EMPTY_LOCAL: LocalConfig = { basePath: '/var/lib/dylaris/backups' };
const EMPTY_S3: S3Config = {
    endpoint: '',
    region: 'us-east-1',
    bucket: '',
    accessKeyId: '',
    secretAccessKey: '',
    forcePathStyle: true,
};

// The DB provider column is still "local" for the legacy NFS/shared
// implementation — the UI just relabels it "shared". When the admin picks
// "shared" mode in the panel, the BackupStorage rows continue to be
// provider == "local" on disk; the factory aliases the two so either name
// resolves to the same implementation.
const PROVIDER_FOR_MODE: Record<BackupConfig['mode'], BackupProvider> = {
    shared: 'local',
    s3: 's3',
    'node-local': 'node-local',
};

// ─────────────────────────────────────────────────────────────────────
// Storage mode picker — the three options the admin chooses between.
// Lives at the top of the tab so the *_storages section below can hide
// the "Add Local" / "Add S3" buttons that don't match the current mode.
// ─────────────────────────────────────────────────────────────────────
function StorageModeCard({
    config,
    onChange,
    form,
}: {
    config: BackupConfig;
    onChange: (next: BackupConfig) => void;
    form: SavableForm;
}) {
    const { featureFlags } = useAppData();
    const hosted = featureFlags.hosted;
    const setMode = (mode: BackupConfig['mode']) => onChange({ ...config, mode });

    return (
        <SettingsCard
            title="Storage mode"
            description="Where backup archives live. Per-job credentials (S3 keys, NFS paths) still come from the storages list below."
            form={form}
        >
            <div className="space-y-2" role="group" aria-label="Storage mode">
                <ModeOption
                    selected={config.mode === 's3'}
                    onClick={() => setMode('s3')}
                    icon={<Cloud size={16} className="text-(--accent-light)" />}
                    title="S3 / Object Storage"
                    desc="Backups stream to a configured bucket (Hetzner, AWS, R2). Independent of the Nodes — survives any single host."
                />
                <ModeOption
                    selected={config.mode === 'node-local'}
                    onClick={() => setMode('node-local')}
                    icon={<Server size={16} className="text-(--primary-light)" />}
                    title="Node-local (on MC host)"
                    desc="Backups are stored inside each server's container folder under a hidden .dylaris-backups/ subdirectory. They migrate automatically when the server moves nodes. Separate quota tracking still applies."
                />
                <ModeOption
                    selected={config.mode === 'shared'}
                    onClick={() => setMode('shared')}
                    icon={<HardDrive size={16} className="text-(--base-07)" />}
                    title="Shared filesystem (NFS / network mount)"
                    desc="Backups land in a directory the Core reaches over a network mount. Use when every Core replica sees the same disk."
                />
            </div>

            {config.mode === 'node-local' && (
                <NodeLocalQuotaPanel config={config} onChange={onChange} />
            )}

            {/* Outside the node-local block on purpose: this one is per OWNER
                and applies in every mode, while the panel above bounds one
                server's folder on one node's disk. */}
            <div className="set-sto-divide">
                <div className="flex flex-wrap items-start justify-between gap-4">
                    <div className="min-w-0 max-w-md">
                        <label className="input-label flex items-center gap-1.5" htmlFor="backup-default-user-quota">
                            {hosted ? 'Allowance per user without a subscription' : 'Allowance per user'}
                            <HelpTip label="About the allowance">
                                <p className="mb-2">
                                    {hosted ? (
                                        <>
                                            How much backup storage <strong>this platform</strong> holds for
                                            someone who owns a server but has bought no node or route-only
                                            location. Customers who bought one are judged by what their
                                            purchase includes instead, set under Settings, Billing.
                                        </>
                                    ) : (
                                        <>
                                            How much backup storage <strong>this platform</strong> holds for
                                            each user who owns a server.
                                        </>
                                    )}
                                </p>
                                {LimitHelp}
                                <p className="mt-2">
                                    Administrators are never subject to it, and neither are backups
                                    written to a storage the user connected themselves. New backups
                                    are refused once it is reached; the ones already stored are kept.
                                </p>
                            </HelpTip>
                        </label>
                        <p id="backup-default-user-quota-hint" className="text-xs text-(--base-06) mt-1">
                            {hosted
                                ? 'Leave it unset for no limit. This lived under Settings, Billing until 2026-09-07, where a self-hosted install could not reach it.'
                                : 'Leave it unset for no limit.'}
                        </p>
                    </div>
                    <LimitField
                        id="backup-default-user-quota"
                        describedBy="backup-default-user-quota-hint"
                        unit="GB"
                        value={config.defaultUserQuotaGb}
                        onChange={defaultUserQuotaGb => onChange({ ...config, defaultUserQuotaGb })}
                    />
                </div>
            </div>

            <BackupPacingFields config={config} onChange={onChange} />
        </SettingsCard>
    );
}

// How every node's backups yield to players. The uplink itself is per node
// (Settings, Nodes); these two shape what is done with it.
function BackupPacingFields({ config, onChange }: { config: BackupConfig; onChange: (next: BackupConfig) => void }) {
    return (
        <div className="set-sto-divide space-y-4">
            <div>
                <h3 className="text-sm font-medium text-(--base-09)">While players are online</h3>
                <p className="text-xs text-(--base-06) mt-1 max-w-xl">
                    A backup slows its upload down while anyone plays on its node, so the uplink never fills up
                    and the players&apos; ping stays steady. It only does that on nodes whose uplink is known: set it
                    per node under Settings, Nodes, or with NODE_UPLINK_MBIT on the node.
                </p>
            </div>
            <div className="flex flex-wrap items-start justify-between gap-4">
                <div className="min-w-0 max-w-md">
                    <label className="input-label" htmlFor="backup-throttle-headroom">Uplink kept for players</label>
                    <p id="backup-throttle-headroom-hint" className="text-xs text-(--base-06) mt-1">
                        A share of the uplink the backup never uses, on top of what the servers are measured sending.
                        Never below 10% of the uplink, so a backup always finishes.
                    </p>
                </div>
                <div className="flex items-center gap-1.5 shrink-0">
                    <input
                        id="backup-throttle-headroom"
                        type="number"
                        min={0}
                        max={90}
                        step={1}
                        value={config.throttleHeadroomPct}
                        aria-describedby="backup-throttle-headroom-hint"
                        onChange={e => {
                            if (e.target.value === '') return;
                            const n = Math.trunc(Number(e.target.value));
                            if (Number.isFinite(n)) onChange({ ...config, throttleHeadroomPct: Math.min(90, Math.max(0, n)) });
                        }}
                        className="input-mono w-20 text-center"
                    />
                    <span className="text-xs text-(--base-06) font-mono">%</span>
                </div>
            </div>
            <div className="flex flex-wrap items-start justify-between gap-4">
                <div className="min-w-0 max-w-md">
                    <label className="input-label flex items-center gap-1.5" htmlFor="backup-throttle-saveoff">
                        Stop slowing down after
                        <HelpTip label="About the save-off ceiling">
                            <p className="mb-2">
                                While a running server is backed up, it does not save its world, so the archive is
                                consistent. A slower backup keeps saving off for longer, and a crash in that time
                                loses everything since the backup started.
                            </p>
                            <p className="mb-2">
                                After this many minutes the backup runs at full speed for the rest of the run.
                                0 never slows down while saving is off; no limit slows down for as long as it takes.
                            </p>
                            <p>
                                A node that spools its backups (Settings, Nodes) saves again as soon as the archive is
                                packed, and only the upload is slowed.
                            </p>
                        </HelpTip>
                    </label>
                    <p className="text-xs text-(--base-06) mt-1">Minutes of world saving being off.</p>
                </div>
                <LimitField
                    id="backup-throttle-saveoff"
                    unit="min"
                    value={config.throttleSaveOffMaxMin}
                    onChange={throttleSaveOffMaxMin => onChange({ ...config, throttleSaveOffMaxMin })}
                />
            </div>
        </div>
    );
}

function ModeOption({
    selected, onClick, icon, title, desc,
}: {
    selected: boolean;
    onClick: () => void;
    icon: React.ReactNode;
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
            <span className="mt-0.5 shrink-0">{icon}</span>
            <span className="min-w-0">
                <span className="block text-sm font-medium text-(--base-09)">{title}</span>
                <span className="block text-xs text-(--base-06) mt-0.5">{desc}</span>
            </span>
        </button>
    );
}

// ─────────────────────────────────────────────────────────────────────
// Quota controls shown only when mode == "node-local". The hard quota
// is currently advisory at the filesystem level — Core checks current
// usage before approving a new run. XFS / ZFS project quota
// enforcement is on the roadmap; the banner below makes the gap
// visible to the admin so nothing's pretending to be enforced.
// ─────────────────────────────────────────────────────────────────────
function NodeLocalQuotaPanel({
    config, onChange,
}: {
    config: BackupConfig;
    onChange: (next: BackupConfig) => void;
}) {
    return (
        <div className="set-sto-panel space-y-3">
            <div className="flex items-start gap-2 text-xs text-(--base-06)">
                <Info size={13} className="mt-0.5 shrink-0" />
                <span>
                    Per-server backup-folder caps. Enforcement is application-level — Core checks current usage before approving a new run. Filesystem-level enforcement (XFS / ZFS project quotas) requires extra host setup and is a follow-up.
                </span>
            </div>

            <div className="flex items-center gap-3">
                <label className="input-label whitespace-nowrap flex items-center gap-1.5" htmlFor="backup-quota-per-server">
                    Backup quota per server
                    <HelpTip label="About the backup quota">
                        <p className="mb-2">
                            How much the backup folder of <strong>one</strong> server may hold on the
                            node it runs on.
                        </p>
                        {LimitHelp}
                        <p className="mt-2">
                            Enforced by Core before it approves a run, not by the filesystem. A
                            process writing into that folder by other means is not stopped by this
                            number.
                        </p>
                    </HelpTip>
                </label>
                {/* One control, not a number field plus an "unlimited" checkbox.
                    The old pair could not say "none": ticking unlimited WROTE 0,
                    so 0 and unlimited were the same value and a cap of none was
                    unreachable from this screen. */}
                <LimitField
                    id="backup-quota-per-server"
                    value={config.quotaPerServerGb}
                    onChange={quotaPerServerGb => onChange({ ...config, quotaPerServerGb })}
                    unit="GB"
                />
            </div>

            <Checkbox
                checked={config.shareQuotaWithServer}
                onChange={c => onChange({ ...config, shareQuotaWithServer: c })}
                label="Share quota with server container storage"
                hint="When on, the backup folder is folded into the server's main disk quota — one combined cap covers both, no separate budget."
            />
        </div>
    );
}

export default function BackupsTab() {
    const [storages, setStorages] = useState<BackupStorage[]>([]);
    const [savingStorage, runSave] = useBusy();
    const [config, setConfig] = useState<BackupConfig | null>(null);
    const [savedConfig, setSavedConfig] = useState<BackupConfig | null>(null);
    const [loading, setLoading] = useState(true);
    const [editing, setEditing] = useState<BackupStorage | null>(null);
    const [saving, setSaving] = useState(false);
    // Persistent per-storage durability warnings from the last test. A warning
    // that scrolls away in a toast is one nobody acts on.
    const [testWarnings, setTestWarnings] = useState<Record<number, string>>({});
    // Which row is being tested, and the verdict of the last one. Single-slot
    // on purpose: a column of stale green ticks says less than one fresh line.
    const [testingId, setTestingId] = useState<number | null>(null);
    const [rowResult, setRowResult] = useState<{ id: number; result: ConnTestResult } | null>(null);
    // Which storage has its maintenance panel (orphans, lifecycle rules) open.
    const [maintenanceId, setMaintenanceId] = useState<number | null>(null);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const reload = useCallback(async () => {
        setLoading(true);
        const [stRes, cfgRes] = await Promise.all([
            listBackupStorages(),
            getBackupConfig(),
        ]);
        if (stRes.success && stRes.storages) setStorages(stRes.storages);
        if (cfgRes.success && cfgRes.settings) {
            setConfig(cfgRes.settings);
            setSavedConfig(cfgRes.settings);
        }
        setLoading(false);
    }, []);

    // Saved storage connections, so a backup target can reference one instead of
    // repeating its credentials. Non-fatal if it fails: the other providers work.
    const [connections, setConnections] = useState<StorageConnection[]>([]);
    useEffect(() => {
        listStorageConnections().then(res => {
            if (res.success && res.connections) setConnections(res.connections);
        }).catch(() => { });
    }, []);

    useEffect(() => { reload(); }, [reload]);

    const handleNew = (mode: BackupConfig['mode']) => {
        const provider = PROVIDER_FOR_MODE[mode];
        setEditing({
            id: 0,
            name: provider === 's3'
                ? 'Hetzner Object Storage'
                : provider === 'node-local'
                    ? 'Node-local backups'
                    : 'Shared backups',
            provider,
            config: (provider === 's3' ? EMPTY_S3 : EMPTY_LOCAL) as unknown as Record<string, unknown>,
            isDefault: storages.length === 0,
        });
    };

    const handleSave = async () => {
        if (!editing) return;
        const payload = { ...editing };
        const res = editing.id === 0
            ? await createBackupStorage(payload)
            : await updateBackupStorage(editing.id, payload);
        if (res.success) {
            showToast('Storage saved.');
            setEditing(null);
            reload();
        } else {
            // Show what the server said: a duplicate name and a storage that no
            // longer exists are both actionable and both look identical behind a
            // bare "Save failed."
            showToast(res.message || 'Save failed.', false);
        }
    };

    const handleDelete = async (id: number) => {
        if (!(await confirmDialog({ title: 'Delete storage', message: 'Delete this storage? This is refused while a backup or a backup schedule still uses it.' }))) return;
        const res = await deleteBackupStorage(id);
        if (res.success) reload();
        else showToast(res.message || 'Delete failed.', false);
    };

    // The button had no busy state at all: it looked idle for as long as the
    // request took, which on an unreachable endpoint was however long the
    // client waited - and with no deadline, that could be forever. Now it
    // greys out, it has a deadline, and the verdict lands under the list
    // instead of in a toast, because "reached, but that bucket does not exist"
    // is a sentence worth reading twice.
    const handleTest = async (id: number) => {
        setTestingId(id);
        setRowResult(null);
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), CONN_TEST_TIMEOUT_MS);
        try {
            const res = await testBackupStorage(id, controller.signal);
            // Cleared on every test, so a warning never outlives the config
            // that produced it: fixing the mount and re-testing makes it
            // disappear.
            setTestWarnings(prev => {
                const next = { ...prev };
                if (res.success && res.warning) next[id] = res.warning;
                else delete next[id];
                return next;
            });
            setRowResult({ id, result: readConnTest(res, 'Connection OK: write, read and delete all succeeded.') });
        } catch {
            setRowResult({
                id,
                result: {
                    severity: 'error', stage: 'timeout', heading: 'No answer',
                    message: `No answer within ${Math.round(CONN_TEST_TIMEOUT_MS / 1000)} seconds, so the test was stopped.`,
                },
            });
        } finally {
            clearTimeout(timer);
            setTestingId(null);
        }
    };

    // This card had its own Discard/Save pair, hand-rolled and unregistered -
    // so it had no navigation guard and no beforeunload, while the tab beside
    // it prompted. It has a pair again, but a registered one.
    const configDirty = !!savedConfig && JSON.stringify(savedConfig) !== JSON.stringify(config);

    const handleConfigDiscard = () => {
        if (savedConfig) setConfig(savedConfig);
    };

    const handleConfigSave = async (): Promise<boolean> => {
        if (!config) return false;
        setSaving(true);
        try {
            const res = await saveBackupConfig(config);
            if (res.success) {
                setSavedConfig(config);
                showToast('Storage mode saved.');
                return true;
            }
            showToast(res.message || 'Save failed.', false);
            return false;
        } finally {
            setSaving(false);
        }
    };

    useUnsavedChanges({ dirty: configDirty, save: handleConfigSave, discard: handleConfigDiscard, saving });

    if (loading) return <SettingsPage title="Server Backups" icon={Archive} loading skeletonCards={2}>{null}</SettingsPage>;

    if (!config) return (
        <SettingsPage title="Server Backups" icon={Archive}>
            <EmptyState
                tone="error"
                icon={<TriangleAlert size={20} />}
                title="Backup settings could not be loaded"
                description="The values are not shown, so nothing can be saved from here. Check the connection to Core and try again."
                action={<button type="button" onClick={() => { reload(); }} className="btn btn-secondary btn-sm">Retry</button>}
            />
        </SettingsPage>
    );

    const activeProvider = PROVIDER_FOR_MODE[config.mode];

    const modeForm = { dirty: configDirty, saving, save: handleConfigSave, discard: handleConfigDiscard };

    return (
        <SettingsPage
            title="Server Backups"
            icon={Archive}
            description="Where the archives of game servers are stored, and how much each user may keep. This page does not cover the platform's own database."
        >
            <StorageModeCard config={config} onChange={setConfig} form={modeForm} />

            <SettingsCard
                title="Configured storages"
                bodySpacing="none"
                actions={
                    <div className="flex gap-2">
                        {activeProvider === 'local' && (
                            <button onClick={() => handleNew('shared')} className="btn btn-secondary btn-sm">
                                <HardDrive size={12} /> Add shared
                            </button>
                        )}
                        {activeProvider === 's3' && (
                            /* Only "from a connection". Typing endpoint / bucket /
                               key / secret here as well meant one bucket lived in
                               several screens, each with its own copy of the
                               credentials and its own chance to be missed on a
                               rotation. Existing inline targets keep working and
                               stay editable below; there is just no way to make a
                               new one. */
                            <>
                                {connections.length > 0 ? (
                                    <button
                                        onClick={() => setEditing({
                                            id: 0,
                                            name: connections[0].name + ' backups',
                                            provider: 'connection',
                                            config: { connectionId: connections[0].id, prefix: 'server-backups' } as unknown as Record<string, unknown>,
                                            isDefault: storages.length === 0,
                                        })}
                                        className="btn btn-primary btn-sm"
                                        title="Buckets and credentials are defined once under Settings -> Storage Connections"
                                    >
                                        <Link2 size={12} /> Add from a connection
                                    </button>
                                ) : (
                                    <span className="text-xs text-(--base-06) self-center">
                                        Create a storage connection first
                                    </span>
                                )}
                            </>
                        )}
                        {activeProvider === 'node-local' && (
                            <button onClick={() => handleNew('node-local')} className="btn btn-primary btn-sm">
                                <Plus size={12} /> Add node-local
                            </button>
                        )}
                    </div>
                }
            >
                {storages.length === 0 ? (
                    <EmptyState
                        bare
                        icon={<Archive size={20} />}
                        title="No backup storages yet"
                        description="Add one to start scheduling backups."
                    />
                ) : (
                    <>
                        <RowList label="Configured storages">
                            {storages.map(s => (
                                <Row
                                    key={s.id}
                                    leading={s.provider === 'connection'
                                        ? <Link2 size={16} className="text-(--accent-light)" />
                                        : s.provider === 's3'
                                            ? <Cloud size={16} className="text-(--accent-light)" />
                                            : s.provider === 'node-local'
                                                ? <Server size={16} />
                                                : <HardDrive size={16} />}
                                    title={
                                        <>
                                            {s.name}
                                            {s.isDefault && <span className="badge badge-accent ml-2 align-middle">default</span>}
                                        </>
                                    }
                                    meta={s.provider === 'local' ? 'shared' : s.provider}
                                    actions={
                                        <>
                                            <button
                                                onClick={() => handleTest(s.id)}
                                                className="btn btn-secondary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                                                title="Test connection"
                                                disabled={testingId !== null}
                                                aria-busy={testingId === s.id}
                                            >
                                                {testingId === s.id
                                                    ? <Loader2 size={12} className="animate-spin" />
                                                    : <Cable size={12} />}
                                                {testingId === s.id ? 'Testing…' : 'Test'}
                                            </button>
                                            <Menu
                                                trigger={
                                                    <button type="button" className="app-icon-btn" aria-label={`Actions for ${s.name}`}>
                                                        <MoreHorizontal size={16} />
                                                    </button>
                                                }
                                                items={[
                                                    { label: 'Edit', icon: <Pencil size={14} />, onSelect: () => setEditing(s) },
                                                    ...(supportsStorageMaintenance(s.provider)
                                                        ? [{
                                                            label: maintenanceId === s.id ? 'Hide maintenance' : 'Maintenance',
                                                            icon: <Wrench size={14} />,
                                                            onSelect: () => setMaintenanceId(maintenanceId === s.id ? null : s.id),
                                                        }]
                                                        : []),
                                                    { type: 'separator' as const },
                                                    { label: 'Delete', icon: <Trash2 size={14} />, danger: true, onSelect: () => handleDelete(s.id) },
                                                ]}
                                            />
                                        </>
                                    }
                                />
                            ))}
                        </RowList>
                        {/* Verdicts and panels sit under the list, not inside a
                            row: a list only holds rows, and each of these is
                            single-slot or keyed by storage. */}
                        <div className="set-sto-notes">
                            {storages.map(s => {
                                const hasNote = rowResult?.id === s.id || maintenanceId === s.id || !!testWarnings[s.id];
                                if (!hasNote) return null;
                                return (
                                    <div key={s.id} className="space-y-2">
                                        <div className="srv-label">{s.name}</div>
                                        {rowResult?.id === s.id && <ConnectionTestNote result={rowResult.result} />}
                                        {maintenanceId === s.id && <StorageMaintenance storageId={s.id} />}
                                        {testWarnings[s.id] && (
                                            <Callout tone="warning" icon={<AlertTriangle size={14} />} role="alert">
                                                {testWarnings[s.id]}
                                            </Callout>
                                        )}
                                    </div>
                                );
                            })}
                        </div>
                    </>
                )}
            </SettingsCard>

            {editing && (
                <div className="modal-overlay animate-fade-in" onClick={() => setEditing(null)}>
                    <ModalPanel onClose={() => setEditing(null)} className="modal-panel w-full max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header flex items-center justify-between">
                            <h3 className="modal-title">{editing.id === 0 ? 'New Storage' : 'Edit Storage'}</h3>
                            <button onClick={() => setEditing(null)} className="p-1 text-(--base-06) hover:text-(--base-09)">
                                <X size={16} />
                            </button>
                        </div>
                        <div className="modal-body space-y-4">
                            <div className="form-group">
                                <label className="input-label">Name</label>
                                <input type="text" value={editing.name} onChange={e => setEditing({ ...editing, name: e.target.value })} className="input-field" />
                            </div>

                            {editing.provider === 'local' && (
                                <div className="form-group">
                                    <label className="input-label flex items-center gap-1.5">
                                        Base Path
                                        <HelpTip label="About the base path">
                                            <p className="mb-2">
                                                A path <strong>inside the Core container</strong>, not on your
                                                desktop and not on a node.
                                            </p>
                                            <p className="mb-2">
                                                It has to be a mounted volume. A path that only exists in the
                                                container&apos;s own filesystem passes every test on this page and
                                                then loses every archive the next time the container is
                                                recreated - which is any update. The test warns you when it can
                                                tell, but it cannot always tell.
                                            </p>
                                            <p>
                                                Running more than one Core replica? Then this must be the same
                                                network mount on all of them, or a restore will look for an
                                                archive the other replica wrote and find nothing.
                                            </p>
                                        </HelpTip>
                                    </label>
                                    <input
                                        type="text"
                                        value={(editing.config as unknown as LocalConfig).basePath || ''}
                                        onChange={e => setEditing({ ...editing, config: { ...(editing.config as object), basePath: e.target.value } })}
                                        className="input-field font-mono"
                                        placeholder="/var/lib/dylaris/backups"
                                    />
                                    <p className="text-xs text-(--base-06)">Backups land in this directory on the Core host. Use a mount that survives container restarts.</p>
                                </div>
                            )}

                            {editing.provider === 'node-local' && (
                                <Callout tone="info" icon={<Info size={14} />}>
                                    Node-local stores have no per-instance config — backups live inside each server&apos;s container folder on whichever Node hosts it. This row exists so the rest of the system can reference a storage id; you can name it whatever helps you tell instances apart.
                                </Callout>
                            )}

                            {editing.provider === 'connection' && (
                                <>
                                    <div className="form-group">
                                        <label className="input-label">Storage Connection</label>
                                        <Select
                                            ariaLabel="Storage Connection"
                                            value={String((editing.config as unknown as BackupConnectionConfig).connectionId || 0)}
                                            onChange={v => setEditing({
                                                ...editing,
                                                config: { ...(editing.config as object), connectionId: Number(v) },
                                            })}
                                            options={connections.map(c => ({ value: String(c.id), label: c.name }))}
                                        />
                                        <p className="text-xs text-(--base-06)">
                                            Credentials come from the connection, so rotating them there updates every
                                            subsystem that uses it. Nothing is copied into this backup target.
                                        </p>
                                    </div>
                                    <div className="form-group">
                                        <label className="input-label flex items-center gap-1.5">
                                            Prefix
                                            <HelpTip label="About the prefix">
                                                <p className="mb-2">
                                                    Where in the bucket the archives go. It nests{' '}
                                                    <strong>under the prefix the connection already has</strong>,
                                                    so a connection with <code>dylaris/</code> and a prefix of{' '}
                                                    <code>server-backups</code> writes to{' '}
                                                    <code>dylaris/server-backups/</code>.
                                                </p>
                                                <p>
                                                    That matters if the bucket credential is restricted to a
                                                    prefix: the test writes a probe object here, and a token that
                                                    may not write at this exact path fails the test while the same
                                                    connection works everywhere else.
                                                </p>
                                            </HelpTip>
                                        </label>
                                        <input
                                            type="text"
                                            value={(editing.config as unknown as BackupConnectionConfig).prefix ?? ''}
                                            onChange={e => setEditing({
                                                ...editing,
                                                config: { ...(editing.config as object), prefix: e.target.value },
                                            })}
                                            className="input-field font-mono"
                                            placeholder="server-backups"
                                        />
                                        <p className="text-xs text-(--base-06)">
                                            Key prefix inside the bucket. Defaults to <span className="font-mono">server-backups</span>.
                                            Keep it distinct from what other subsystems write (Core file storage uses
                                            <span className="font-mono"> modpacks/</span>,
                                            <span className="font-mono"> ticket-attachments/</span>) so one bucket can hold
                                            all of them without their lifecycle rules or quotas overlapping.
                                        </p>
                                    </div>
                                </>
                            )}

                            {editing.provider === 's3' && (
                                <>
                                    {(['endpoint', 'region', 'bucket', 'accessKeyId', 'secretAccessKey'] as (keyof S3Config)[]).map(field => {
                                        // The secret is never returned by the API. Show the field
                                        // empty with a "leave blank to keep" hint when one is
                                        // stored; a save carrying no secret preserves it server-side.
                                        const isSecret = field === 'secretAccessKey';
                                        const secretStored = isSecret && editing.secretSet;
                                        return (
                                            <div key={field} className="form-group">
                                                <label className="input-label">{S3_LABELS[field]}</label>
                                                <input
                                                    type={isSecret ? 'password' : 'text'}
                                                    value={isSecret ? String((editing.config as unknown as S3Config).secretAccessKey ?? '') : String((editing.config as unknown as S3Config)[field] ?? '')}
                                                    onChange={e => setEditing({ ...editing, config: { ...(editing.config as object), [field]: e.target.value } })}
                                                    className="input-field font-mono"
                                                    autoComplete={isSecret ? 'new-password' : undefined}
                                                    placeholder={
                                                        secretStored ? 'Leave blank to keep the stored secret'
                                                            : field === 'endpoint' ? 'https://fsn1.your-objectstorage.com'
                                                                : ''
                                                    }
                                                />
                                            </div>
                                        );
                                    })}
                                    <div className="flex items-center justify-between">
                                        <label className="input-label">Force Path Style</label>
                                        <Switch
                                            ariaLabel="Force Path Style"
                                            checked={!!(editing.config as unknown as S3Config).forcePathStyle}
                                            onChange={c => setEditing({ ...editing, config: { ...(editing.config as object), forcePathStyle: c } })}
                                        />
                                    </div>
                                    <p className="text-xs text-(--base-06)">Hetzner Object Storage needs path-style ON. AWS S3 / Cloudflare R2 typically OFF.</p>
                                </>
                            )}

                            <div className="pt-2 border-t border-(--base-03)">
                                <StorageUploadCap
                                    value={editing.uploadCapMbit ?? null}
                                    onChange={uploadCapMbit => setEditing({ ...editing, uploadCapMbit })}
                                    // The test signs an upload with the STORED credentials, so it
                                    // needs a saved row of a kind that has object storage behind it.
                                    storageId={editing.id && ['s3', 'connection', 'core-storage'].includes(editing.provider) ? editing.id : undefined}
                                    mine={false}
                                />
                            </div>

                            <div className="flex items-center justify-between pt-2 border-t border-(--base-03)">
                                <label className="input-label flex items-center gap-1.5">
                                    Use as Default
                                    <HelpTip label="About the default target">
                                        <p className="mb-2">
                                            Where a job writes when it has not picked a target itself. Only one
                                            row holds it - turning it on here turns it off wherever it was.
                                        </p>
                                        <p className="mb-2">
                                            Those jobs look this up on <strong>every run</strong>, not once when
                                            they were created. Moving the default therefore moves where they
                                            write from their next run on. Archives already written stay where
                                            they are and are still restorable.
                                        </p>
                                        <p>
                                            A customer who set their own default keeps it - theirs is asked
                                            first, and this one is the fallback.
                                        </p>
                                    </HelpTip>
                                </label>
                                <Switch
                                    ariaLabel="Use as Default"
                                    checked={!!editing.isDefault}
                                    onChange={c => setEditing({ ...editing, isDefault: c })}
                                />
                            </div>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setEditing(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runSave(handleSave)} disabled={savingStorage} className="btn btn-primary disabled:opacity-40">
                                <Save size={13} /> Save
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </SettingsPage>
    );
}
