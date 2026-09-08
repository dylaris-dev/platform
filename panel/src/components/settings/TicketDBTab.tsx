"use client";

import React, { useCallback, useEffect, useState } from 'react';
import {
    Database, ArrowRightCircle, ShieldAlert, Download, Trash2, Loader2,
    CircleCheck, CircleAlert, AlertTriangle, X,
} from 'lucide-react';
import {
    getTicketMigrationStatus, testTicketDBConnection,
    dryRunTicketMigration, executeTicketMigration,
    createTicketBackup, listTicketBackups, deleteTicketBackup, downloadTicketBackupURL,
    initTicketRestore, executeTicketRestore,
    BackupSummary,
} from '@/lib/api/ticketMigration';
import { SkeletonHeader, SkeletonCard } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import SettingsPage from '@/components/settings/SettingsPage';
import { useConnectionTest, readConnTest } from '@/lib/connectionTest';
import { TestConnectionButton, ConnectionTestNote } from '@/components/ui/ConnectionTest';

export default function TicketDBTab() {
    const [status, setStatus] = useState<{ mainCounts: Record<string, number>; externalConfigured: boolean } | null>(null);
    const [loading, setLoading] = useState(true);
    const [toast, setToast] = useState<{ msg: string; ok: boolean } | null>(null);

    const flash = (msg: string, ok = true) => {
        setToast({ msg, ok });
        setTimeout(() => setToast(null), 3000);
    };

    const reload = async () => {
        const res = await getTicketMigrationStatus();
        if (res.success) {
            setStatus({ mainCounts: res.mainCounts || {}, externalConfigured: !!res.externalConfigured });
        }
        setLoading(false);
    };

    useEffect(() => { reload(); }, []);

    if (loading) return (
        <div className="space-y-8 max-w-4xl">
            <SkeletonHeader />
            <SkeletonCard height="h-32" />
            <SkeletonCard height="h-56" />
            <SkeletonCard height="h-40" />
        </div>
    );

    return (
        <SettingsPage
            title="Ticket DB management"
            icon={Database}
            width="4xl"
            description="Migrate the ticket store to an external Postgres instance, take JSON backups, and restore from one under a danger-zone gate."
        >

            {status && (
                <section className="card p-5 border border-(--base-03) space-y-3">
                    <h3 className="mono-label flex items-center gap-2">Current state</h3>
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
                </section>
            )}

            <MigrationCard onChanged={reload} flash={flash} />

            <BackupCard onChanged={reload} flash={flash} />

            {toast && (
                <div className={`fixed bottom-4 right-4 px-4 py-3 rounded-md shadow-lg text-sm font-medium ${
                    toast.ok ? 'bg-(--success-ghost) text-(--success-light)' : 'bg-(--error-ghost) text-(--error-light)'
                }`}>
                    <span className="inline-flex items-center gap-2">
                        {toast.ok ? <CircleCheck size={14} /> : <CircleAlert size={14} />}
                        {toast.msg}
                    </span>
                </div>
            )}
        </SettingsPage>
    );
}

function CountsTable({ label, counts }: { label: string; counts: Record<string, number> }) {
    const entries = Object.entries(counts);
    if (entries.length === 0) return null;
    return (
        <div>
            <p className="text-xs font-mono text-(--base-06) mb-1.5">{label}</p>
            <div className="grid grid-cols-2 sm:grid-cols-3 gap-1.5">
                {entries.map(([table, n]) => (
                    <div key={table} className="flex items-center justify-between p-2 rounded-md bg-(--base-02) border border-(--base-03) text-xs">
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
        <section className="card p-5 border border-(--base-03) space-y-4">
            <header className="flex items-center justify-between gap-3">
                <h3 className="mono-label flex items-center gap-2"><ArrowRightCircle size={14} /> Migrate to external DB</h3>
            </header>
            <p className="text-xs text-(--base-06)">
                Enter the target Postgres DSN, test the connection, then dry-run to compare row counts before executing.
                Migrations use <span className="font-mono">ON CONFLICT DO NOTHING</span> so re-runs are safe.
            </p>
            <div className="flex flex-col gap-[5px]">
                <label className="input-label">Target DSN</label>
                <input
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
        </section>
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
        <section className="card p-5 border border-(--base-03) space-y-4">
            <header className="flex items-center justify-between gap-3">
                <h3 className="mono-label flex items-center gap-2"><Database size={14} /> Backups</h3>
                <button type="button" onClick={handleCreate} disabled={creating}
                    className="btn btn-primary btn-sm inline-flex items-center gap-2 disabled:opacity-40">
                    {creating && <Loader2 size={12} className="animate-spin" />}
                    Create backup
                </button>
            </header>
            <p className="text-xs text-(--base-06)">
                JSON dumps of every ticket table stored locally under <span className="font-mono">dylaris_data/ticket-backups/</span>.
            </p>

            {loading ? (
                <p className="text-sm text-(--base-06)">Loading…</p>
            ) : backups.length === 0 ? (
                <p className="text-sm text-(--base-06) italic">No backups yet.</p>
            ) : (
                <ul className="space-y-1.5">
                    {backups.map(b => (
                        <li key={b.name} className="flex items-center justify-between gap-2 p-2 rounded-md bg-(--base-02) border border-(--base-03)">
                            <div className="min-w-0">
                                <p className="text-sm font-mono truncate">{b.name}</p>
                                <p className="text-[10px] text-(--base-06)">
                                    {formatBytes(b.size)} · {new Date(b.createdAt).toLocaleString()}
                                </p>
                            </div>
                            <div className="flex items-center gap-1 shrink-0">
                                <a href={downloadTicketBackupURL(b.name)} download={b.name}
                                    className="p-1.5 rounded text-(--accent-light) hover:bg-(--accent-ghost)" title="Download">
                                    <Download size={13} />
                                </a>
                                <button type="button" onClick={() => setRestoreFor(b)}
                                    className="p-1.5 rounded text-(--warning-light) hover:bg-(--warning-ghost)" title="Restore (Danger Zone)">
                                    <ShieldAlert size={13} />
                                </button>
                                <button type="button" onClick={() => handleDelete(b)} disabled={deleting === b.name}
                                    className="p-1.5 rounded text-(--error-light) hover:bg-(--error-ghost) disabled:opacity-40" title="Delete">
                                    {deleting === b.name ? <Loader2 size={13} className="animate-spin" /> : <Trash2 size={13} />}
                                </button>
                            </div>
                        </li>
                    ))}
                </ul>
            )}

            {restoreFor && (
                <RestoreDangerModal backup={restoreFor} onClose={() => setRestoreFor(null)} onDone={() => { setRestoreFor(null); flash('Restore complete.'); onChanged(); }} />
            )}
        </section>
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
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
            <div className="card w-full max-w-lg mx-4 border-2 border-(--error)/30">
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
            </div>
        </div>
    );
}

function formatBytes(n: number): string {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(1) + ' GB';
}
