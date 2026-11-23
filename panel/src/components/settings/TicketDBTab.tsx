"use client";

import { toast as notifyToast } from '@/components/ui/Toast';

import React, { useCallback, useEffect, useState } from 'react';
import {
    Database, ArrowRightCircle, ShieldAlert, Download, Trash2, Loader2,
    CircleCheck, CircleAlert, AlertTriangle, X, MoreHorizontal,
} from 'lucide-react';
import {
    getTicketMigrationStatus, testTicketDBConnection,
    dryRunTicketMigration, executeTicketMigration,
    createTicketBackup, listTicketBackups, deleteTicketBackup, downloadTicketBackupURL,
    initTicketRestore, executeTicketRestore,
    BackupSummary,
} from '@/lib/api/ticketMigration';
import { SkeletonCard } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import SettingsPage from '@/components/settings/SettingsPage';
import { useConnectionTest, readConnTest } from '@/lib/connectionTest';
import { TestConnectionButton, ConnectionTestNote } from '@/components/ui/ConnectionTest';
import ModalPanel from '@/components/ui/ModalPanel';
import { fmtDateTime } from '@/lib/time';
import Menu from '@/components/ui/Menu';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import SettingsCard from '@/components/settings/SettingsCard';

export default function TicketDBTab() {
    const [status, setStatus] = useState<{ mainCounts: Record<string, number>; externalConfigured: boolean } | null>(null);
    const [loading, setLoading] = useState(true);
    const [loadError, setLoadError] = useState<string | null>(null);

    const flash = (msg: string, ok = true) => {
        notifyToast(msg, ok);

    };

    const reload = async () => {
        const res = await getTicketMigrationStatus();
        if (res.success) {
            setStatus({ mainCounts: res.mainCounts || {}, externalConfigured: !!res.externalConfigured });
            setLoadError(null);
        } else {
            setLoadError(res.message || 'Failed to load the ticket database status.');
        }
        setLoading(false);
    };

    useEffect(() => { reload(); }, []);

    if (loading) return (
        <SettingsPage title="Ticket DB management" loading skeletonCards={3} width="4xl">{null}</SettingsPage>
    );

    return (
        <SettingsPage
            title="Ticket DB management"
            icon={Database}
            width="4xl"
            description="Migrate the ticket store to an external Postgres instance, take JSON backups, and restore from one under a danger-zone gate."
        >
            {loadError && (
                <EmptyState tone="error" icon={<CircleAlert size={20} />} title="Ticket database status could not be loaded" description={loadError} />
            )}

            {status && (
                <SettingsCard title="Current state" icon={Database}>
                    <div className="text-sm">
                        {status.externalConfigured ? (
                            <p className="text-(--success-light) flex items-center gap-1">
                                <CircleCheck size={14} /> <span className="font-mono">EXTERNAL_TICKET_DB_URL</span> is configured.
                            </p>
                        ) : (
                            <p className="text-(--base-06) flex items-center gap-1">
                                <CircleAlert size={14} /> No <span className="font-mono">EXTERNAL_TICKET_DB_URL</span> env var set. Migration UI below still works against any DSN you provide; live hot-swap is a future polish.
                            </p>
                        )}
                    </div>
                    <CountsTable label="Main DB" counts={status.mainCounts} />
                </SettingsCard>
            )}

            <MigrationCard onChanged={reload} flash={flash} />

            <BackupCard onChanged={reload} flash={flash} />
        </SettingsPage>
    );
}

function CountsTable({ label, counts }: { label: string; counts: Record<string, number> }) {
    const entries = Object.entries(counts);
    if (entries.length === 0) return null;
    return (
        <div>
            <p className="srv-label mb-1.5">{label}</p>
            <div className="grid grid-cols-2 sm:grid-cols-3 gap-1.5">
                {entries.map(([table, n]) => (
                    <div key={table} className="flex items-center justify-between rounded-lg bg-(--base-03) p-2 text-xs">
                        <span className="font-mono text-(--base-07) truncate">{table}</span>
                        <span className="font-mono text-(--base-09)">{n}</span>
                    </div>
                ))}
            </div>
        </div>
    );
}

// ── Migration card: connection-test → dry-run → execute ─────────────

function MigrationCard({ onChanged, flash }: { onChanged: () => void; flash: (msg: string, ok?: boolean) => void }) {
    const [url, setUrl] = useState('');
    const [dryRun, setDryRun] = useState<{ source: Record<string, number>; target: Record<string, number> } | null>(null);
    const [dryRunning, setDryRunning] = useState(false);
    const [executing, setExecuting] = useState(false);
    const [migrationResult, setMigrationResult] = useState<{ migrated: Record<string, number>; skipped: Record<string, number> } | null>(null);

    // Shared lifecycle: disabled while it runs, a deadline, and a stage in the
    // answer - a DSN typed by hand gets the host wrong far more often than the
    // password, and the two used to arrive as the same red line.
    const test = useConnectionTest(useCallback(async (signal: AbortSignal) => {
        const res = await testTicketDBConnection(url, signal);
        return readConnTest(res, res.version || 'Connected.');
    }, [url]));

    const handleDryRun = async () => {
        if (!url.trim()) return;
        setDryRunning(true);
        const res = await dryRunTicketMigration(url);
        setDryRunning(false);
        if (res.success) {
            setDryRun({ source: res.sourceCounts || {}, target: res.targetCounts || {} });
        } else {
            flash(res.message || 'Dry-run failed.', false);
        }
    };

    const handleExecute = async () => {
        if (!url.trim()) return;
        if (!(await confirmDialog({ title: 'Run ticket DB migration', message: 'Run the migration now? Schema will be applied to the target if missing, and rows will be copied. Safe to re-run.', confirmLabel: 'Run migration' }))) return;
        setExecuting(true);
        const res = await executeTicketMigration(url);
        setExecuting(false);
        if (res.success) {
            setMigrationResult({ migrated: res.migrated || {}, skipped: res.skipped || {} });
            flash('Migration finished.');
            onChanged();
        } else {
            flash(res.message || 'Migration failed.', false);
        }
    };

    return (
        <SettingsCard
            title="Migrate to external DB"
            icon={ArrowRightCircle}
            description={<>
                Enter the target Postgres DSN, test the connection, then dry-run to compare row counts before executing.
                Migrations use <span className="font-mono">ON CONFLICT DO NOTHING</span> so re-runs are safe.
            </>}
        >
            <div className="flex flex-col gap-[5px]">
                <label className="input-label" htmlFor="ticket-db-dsn">Target DSN</label>
                <input
                    id="ticket-db-dsn"
                    type="text"
                    value={url}
                    onChange={e => { test.clear(); setUrl(e.target.value); }}
                    placeholder="postgres://user:pass@host:5432/dbname?sslmode=disable"
                    className="input-field w-full input-mono text-xs"
                />
            </div>

            <div className="flex flex-wrap gap-2">
                <TestConnectionButton
                    test={test}
                    blockedReason={url.trim() ? null : 'Enter the target DSN first.'}
                />
                <button type="button" onClick={handleDryRun} disabled={dryRunning || !url.trim()}
                    className="btn btn-secondary inline-flex items-center gap-2 disabled:opacity-40">
                    {dryRunning && <Loader2 size={14} className="animate-spin" />}
                    Dry-run
                </button>
                <button type="button" onClick={handleExecute} disabled={executing || !url.trim()}
                    className="btn btn-primary inline-flex items-center gap-2 disabled:opacity-40">
                    {executing && <Loader2 size={14} className="animate-spin" />}
                    Execute migration
                </button>
            </div>

            <ConnectionTestNote result={test.result} />

            {dryRun && (
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 pt-3 border-t border-(--base-03)">
                    <CountsTable label="Source (main DB)" counts={dryRun.source} />
                    <CountsTable label="Target (external)" counts={dryRun.target} />
                </div>
            )}

            {migrationResult && (
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 pt-3 border-t border-(--base-03)">
                    <CountsTable label="Migrated" counts={migrationResult.migrated} />
                    <CountsTable label="Skipped (existing)" counts={migrationResult.skipped} />
                </div>
            )}
        </SettingsCard>
    );
}

// ── Backup card + restore Danger Zone ───────────────────────────────

function BackupCard({ onChanged, flash }: { onChanged: () => void; flash: (msg: string, ok?: boolean) => void }) {
    const [backups, setBackups] = useState<BackupSummary[]>([]);
    const [loading, setLoading] = useState(true);
    const [creating, setCreating] = useState(false);
    const [deleting, setDeleting] = useState<string | null>(null);
    const [restoreFor, setRestoreFor] = useState<BackupSummary | null>(null);

    const load = async () => {
        const res = await listTicketBackups();
        if (res.success) setBackups(res.backups || []);
        else flash(res.message || 'Failed to load backups.', false);
        setLoading(false);
    };

    useEffect(() => { load(); }, []);

    const handleCreate = async () => {
        setCreating(true);
        const res = await createTicketBackup();
        setCreating(false);
        if (res.success) {
            flash('Backup created.');
            await load();
            onChanged();
        } else {
            flash(res.message || 'Backup failed.', false);
        }
    };

    const handleDelete = async (b: BackupSummary) => {
        if (!(await confirmDialog({ title: 'Delete backup', message: `Delete backup ${b.name}?` }))) return;
        setDeleting(b.name);
        const res = await deleteTicketBackup(b.name);
        setDeleting(null);
        if (res.success) {
            flash('Deleted.');
            await load();
        } else {
            flash(res.message || 'Delete failed.', false);
        }
    };

    return (
        <SettingsCard
            title="Backups"
            icon={Database}
            description={<>JSON dumps of every ticket table stored locally under <span className="font-mono">dylaris_data/ticket-backups/</span>.</>}
            actions={
                <button type="button" onClick={handleCreate} disabled={creating}
                    className="btn btn-primary btn-sm inline-flex items-center gap-2">
                    {creating && <Loader2 size={12} className="animate-spin" />}
                    Create backup
                </button>
            }
        >
            {loading ? (
                <div className="space-y-2" aria-busy="true">
                    <SkeletonCard height="h-12" />
                    <SkeletonCard height="h-12" />
                </div>
            ) : backups.length === 0 ? (
                <EmptyState bare icon={<Database size={20} />} title="No backups yet" />
            ) : (
                <RowList label="Ticket backups" className="-mx-1.5 !p-0">
                    {backups.map(b => (
                        <Row
                            key={b.name}
                            title={<span className="font-mono">{b.name}</span>}
                            meta={<>{formatBytes(b.size)} &middot; {fmtDateTime(b.createdAt)}</>}
                            actions={
                                deleting === b.name
                                    ? <Loader2 size={14} className="mx-2 animate-spin text-(--base-06)" aria-label="Deleting" />
                                    : (
                                        <Menu
                                            trigger={<button type="button" className="app-icon-btn" aria-label={`Actions for ${b.name}`}><MoreHorizontal size={16} /></button>}
                                            items={[
                                                { label: 'Download', icon: <Download size={14} />, render: <a href={downloadTicketBackupURL(b.name)} download={b.name} /> },
                                                { label: 'Restore (Danger Zone)', icon: <ShieldAlert size={14} />, onSelect: () => setRestoreFor(b) },
                                                { type: 'separator' },
                                                { label: 'Delete', icon: <Trash2 size={14} />, danger: true, onSelect: () => handleDelete(b) },
                                            ]}
                                        />
                                    )
                            }
                        />
                    ))}
                </RowList>
            )}

            {restoreFor && (
                <RestoreDangerModal backup={restoreFor} onClose={() => setRestoreFor(null)} onDone={() => { setRestoreFor(null); flash('Restore complete.'); onChanged(); }} />
            )}
        </SettingsCard>
    );
}

function RestoreDangerModal({ backup, onClose, onDone }: { backup: BackupSummary; onClose: () => void; onDone: () => void }) {
    const [token, setToken] = useState('');
    const [confirmationPhrase, setConfirmationPhrase] = useState('');
    const [typedPhrase, setTypedPhrase] = useState('');
    const [totpCode, setTotpCode] = useState('');
    const [cooldown, setCooldown] = useState(0);
    const [error, setError] = useState('');
    const [initializing, setInitializing] = useState(false);
    const [executing, setExecuting] = useState(false);

    const initialize = async () => {
        setInitializing(true);
        setError('');
        const res = await initTicketRestore(backup.name);
        setInitializing(false);
        if (!res.success) {
            setError(res.message || 'Failed to start restore.');
            return;
        }
        setToken(res.token);
        setConfirmationPhrase(res.confirmationPhrase);
        setCooldown(res.cooldownSeconds || 15);
    };

    useEffect(() => { initialize(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

    useEffect(() => {
        if (cooldown <= 0) return;
        const t = setTimeout(() => setCooldown(c => Math.max(0, c - 1)), 1000);
        return () => clearTimeout(t);
    }, [cooldown]);

    const matchesPhrase = typedPhrase === confirmationPhrase;
    const codeValid = /^\d{6}$/.test(totpCode.trim());
    const canExecute = cooldown === 0 && matchesPhrase && codeValid && !executing && !!token;

    const handleExecute = async () => {
        setExecuting(true);
        setError('');
        const res = await executeTicketRestore({ token, totpCode: totpCode.trim(), confirmationPhrase: typedPhrase });
        setExecuting(false);
        if (res.success) {
            onDone();
        } else {
            setError(res.message || 'Restore failed.');
        }
    };

    return (
        <div className="modal-overlay">
            <ModalPanel className="card w-full max-w-lg mx-4 border border-(--error-border)">
                <div className="modal-header flex items-center justify-between bg-(--error-ghost) text-(--error-light) rounded-t-md">
                    <h3 className="modal-title flex items-center gap-2"><ShieldAlert size={18} /> Danger Zone — Restore</h3>
                    <button type="button" onClick={onClose} className="text-(--base-07) hover:text-(--base-09)"><X size={18} /></button>
                </div>

                <div className="p-6 space-y-4">
                    <div className="alert alert-error text-xs flex items-start gap-2">
                        <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                        <span>
                            Restoring will <strong>wipe every ticket, message, attachment, category, watcher, and audit row</strong> in
                            the main DB and replace them with the contents of <span className="font-mono">{backup.name}</span>. This cannot be undone.
                        </span>
                    </div>

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">2FA code (TOTP, 6 digits)</label>
                        <input
                            type="text"
                            value={totpCode}
                            onChange={e => setTotpCode(e.target.value.replace(/\s/g, ''))}
                            inputMode="numeric"
                            autoComplete="one-time-code"
                            placeholder="123 456"
                            maxLength={6}
                            className="input-field input-mono w-full text-center tracking-widest"
                        />
                    </div>

                    {confirmationPhrase && (
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">Type to confirm</label>
                            <p className="text-xs text-(--base-06) font-mono">
                                {confirmationPhrase}
                            </p>
                            <input
                                type="text"
                                value={typedPhrase}
                                onChange={e => setTypedPhrase(e.target.value)}
                                className="input-field input-mono w-full text-xs"
                                placeholder="Type the phrase above exactly"
                            />
                            {typedPhrase && !matchesPhrase && (
                                <p className="text-xs text-(--error-light)">Phrase does not match yet.</p>
                            )}
                        </div>
                    )}

                    {cooldown > 0 ? (
                        <p className="text-xs text-(--warning-light) flex items-center gap-1.5">
                            <Loader2 size={12} className="animate-spin" />
                            Cooldown: execute available in {cooldown} second{cooldown === 1 ? '' : 's'}.
                        </p>
                    ) : (
                        <p className="text-xs text-(--success-light)">Cooldown elapsed — ready to execute.</p>
                    )}

                    {error && <p className="text-xs text-(--error-light)">{error}</p>}
                </div>

                <div className="modal-footer flex justify-end gap-2">
                    <button type="button" onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button
                        type="button"
                        onClick={handleExecute}
                        disabled={!canExecute || initializing}
                        className="btn btn-danger inline-flex items-center gap-2 disabled:opacity-40"
                    >
                        {executing && <Loader2 size={14} className="animate-spin" />}
                        Execute restore
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}

function formatBytes(n: number): string {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(1) + ' GB';
}
