"use client";

import React, { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import {
    Database, Loader2, CircleCheck, CircleAlert, Play, ServerCog,
    ShieldAlert, ListChecks, Gauge, Zap, TrendingUp,
} from 'lucide-react';
import {
    getDBMigration, startDBMigration, testDBMigrationConnection, verifyDBMigration,
    getHypertableStatus, convertHypertable,
    dbMigrationInProgress,
    DBMigrationJob, DBTargetForm, VerifyReport, DBMigrationPhase, HypertableStatus,
} from '@/lib/api/dbmigration';
import { systemEvents } from '@/lib/systemEvents';
import HelpTip from '@/components/ui/HelpTip';
import { useConnectionTest, readConnTest } from '@/lib/connectionTest';
import { TestConnectionButton, ConnectionTestNote } from '@/components/ui/ConnectionTest';

const EMPTY_FORM: DBTargetForm = {
    host: '', port: '5432', user: 'dylaris', password: '', dbName: 'dylaris',
    sslMode: 'disable', dbType: 'postgres',
};

const PHASE_LABEL: Record<DBMigrationPhase, string> = {
    preparing: 'Preparing',
    schema: 'Creating schema',
    copying: 'Copying data',
    verifying: 'Verifying',
    done: 'Done',
    failed: 'Failed',
};

export default function DatabaseTab() {
    const [form, setForm] = useState<DBTargetForm>(EMPTY_FORM);
    const [job, setJob] = useState<DBMigrationJob | null>(null);
    const [hasJob, setHasJob] = useState(false);
    const [loading, setLoading] = useState(true);


    const [testResult, setTestResult] = useState<{ version: string; timescaleInstalled: boolean } | null>(null);

    const [verifying, setVerifying] = useState(false);
    const [verifyReport, setVerifyReport] = useState<VerifyReport | null>(null);

    const [confirmStart, setConfirmStart] = useState(false);
    const [starting, setStarting] = useState(false);

    const [hyper, setHyper] = useState<HypertableStatus | null>(null);
    const [converting, setConverting] = useState(false);
    const [confirmConvert, setConfirmConvert] = useState(false);

    const [toast, setToast] = useState<{ msg: string; ok: boolean } | null>(null);
    const flash = (msg: string, ok = true) => {
        setToast({ msg, ok });
        setTimeout(() => setToast(null), 3500);
    };

    const load = useCallback(async () => {
        const res = await getDBMigration();
        if (res.success) {
            setHasJob(!!res.hasJob);
            setJob(res.job ?? null);
        }
        setLoading(false);
    }, []);

    const loadHyper = useCallback(async () => {
        const res = await getHypertableStatus();
        if (res.success) {
            setHyper({
                dbType: res.dbType,
                timescaleInstalled: res.timescaleInstalled,
                isHypertable: res.isHypertable,
                eligibleForConversion: res.eligibleForConversion,
                serverStatsRowsEstimate: res.serverStatsRowsEstimate,
                recommendTimescale: res.recommendTimescale,
                recommendThreshold: res.recommendThreshold,
            });
        }
    }, []);

    useEffect(() => { load(); loadHyper(); }, [load, loadHyper]);

    // Live refresh: subscribe to the shared event + poll while a job runs.
    useEffect(() => {
        const unsub = systemEvents.on('dbmigration.changed', () => load());
        return () => unsub();
    }, [load]);

    useEffect(() => {
        if (!job || !dbMigrationInProgress(job.phase)) return;
        const t = setInterval(load, 2500);
        return () => clearInterval(t);
    }, [job, load]);

    const set = (k: keyof DBTargetForm, v: string) => {
        // A retyped host makes the last verdict a claim about a connection
        // nobody made, so it goes with the edit.
        test.clear();
        setTestResult(null);
        setForm(f => ({ ...f, [k]: v }));
    };
    const canSubmit = !!(form.host && form.port && form.user && form.dbName);
    const busy = !!job && dbMigrationInProgress(job.phase);

    // The shared lifecycle owns the button state and the deadline; the version
    // and the extension check stay here, because they are not a verdict on the
    // connection but a description of what was found at the other end.
    const test = useConnectionTest(useCallback(async (signal: AbortSignal) => {
        setTestResult(null);
        const res = await testDBMigrationConnection(form, signal);
        if (res.success) {
            setTestResult({ version: res.version, timescaleInstalled: res.timescaleInstalled });
        }
        return readConnTest(res, 'Connected.');
    }, [form]));

    const doVerify = async () => {
        setVerifying(true);
        setVerifyReport(null);
        const res = await verifyDBMigration(form);
        setVerifying(false);
        if (res.success) {
            setVerifyReport(res.report);
            flash(res.report.ok ? 'Verify passed' : 'Verify found differences', res.report.ok);
        } else {
            flash(res.message || 'Verify failed', false);
        }
    };

    const doStart = async () => {
        setStarting(true);
        const res = await startDBMigration(form);
        setStarting(false);
        setConfirmStart(false);
        if (res.success) {
            setJob(res.job);
            setHasJob(true);
            flash('Migration started. Maintenance mode is now ON.');
        } else {
            flash(res.message || 'Failed to start', false);
        }
    };

    const doConvert = async () => {
        setConverting(true);
        const res = await convertHypertable();
        setConverting(false);
        setConfirmConvert(false);
        if (res.success) {
            flash(res.alreadyHypertable ? 'server_stats is already a hypertable' : 'Converted server_stats to a hypertable');
            loadHyper();
        } else {
            flash(res.message || 'Conversion failed', false);
        }
    };

    const tsMismatch = !!testResult && form.dbType === 'timescaledb' && !testResult.timescaleInstalled;

    return (
        <div className="space-y-6">
            <header className="flex items-center gap-3">
                <Database size={20} className="text-(--accent-light)" />
                <div>
                    <h1 className="text-lg font-semibold">Database migration</h1>
                    <p className="text-sm text-(--base-06)">
                        Copy the entire platform database onto a new target, then restart Core pointing at it.
                    </p>
                </div>
            </header>

            {/* What this does / honest notes */}
            <div className="card p-5 space-y-2 text-sm text-(--base-07)">
                <p>
                    This connects a <strong>new, empty target database</strong> and copies every table into it
                    while the platform is in maintenance mode, so nothing changes underneath the copy. Works in any
                    direction: PostgreSQL to TimescaleDB, TimescaleDB to PostgreSQL, or relocating between hosts.
                </p>
                <ul className="list-disc pl-5 space-y-1 text-(--base-06)">
                    <li>The current (source) database is only ever <strong>read</strong> and is never modified.</li>
                    <li>The target is <strong>truncated and overwritten</strong> — point it at a fresh database.</li>
                    <li>Starting a migration turns <strong>maintenance mode ON</strong> (everyone but admins is blocked).</li>
                    <li>After it finishes and verifies, restart Core with the new <code className="font-mono text-xs">DB_*</code> (and <code className="font-mono text-xs">DB_TYPE</code>) env vars, then turn maintenance off.</li>
                </ul>
            </div>

            {/* Current backend status + in-place hypertable upgrade + recommendation */}
            {hyper && (
                <div className="card p-5 space-y-3">
                    <div className="mono-label flex items-center gap-2"><Gauge size={13} /> Current backend</div>
                    <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
                        <Stat label="DB_TYPE" value={hyper.dbType} />
                        <Stat label="TimescaleDB ext" value={hyper.timescaleInstalled ? 'installed' : 'absent'} />
                        <Stat label="server_stats" value={hyper.isHypertable ? 'hypertable' : 'plain table'} />
                        <Stat label="rows (est.)" value={fmtNum(hyper.serverStatsRowsEstimate)} />
                    </div>

                    {hyper.recommendTimescale && (
                        <div className="flex items-start gap-2 bg-(--warning-ghost) border border-(--warning-border) rounded-md p-3 text-xs">
                            <TrendingUp size={14} className="text-(--warning-light) mt-0.5 shrink-0" />
                            <div className="text-(--base-07) space-y-0.5">
                                <div className="text-(--warning-light) font-medium">Consider switching to TimescaleDB</div>
                                Your metrics history has grown large ({fmtNum(hyper.serverStatsRowsEstimate)} rows). On plain
                                PostgreSQL, time-series queries and the hourly retention sweep can slow down as this grows.
                                TimescaleDB stores server_stats as a hypertable with native retention. Migrate to a TimescaleDB
                                target below, or switch the image and convert in place.
                            </div>
                        </div>
                    )}

                    {hyper.eligibleForConversion && (
                        <div className="space-y-2">
                            <p className="text-xs text-(--base-06)">
                                The TimescaleDB extension is present but server_stats is still a plain table (e.g. you swapped a
                                plain-Postgres image for a TimescaleDB one on the same database). Convert it in place to a hypertable.
                            </p>
                            {!confirmConvert ? (
                                <button className="btn btn-secondary" onClick={() => setConfirmConvert(true)} disabled={busy}>
                                    <Zap size={14} /> Convert metrics table to hypertable
                                </button>
                            ) : (
                                <div className="flex flex-wrap items-center gap-2 bg-(--warning-ghost) border border-(--warning-border) rounded-md px-3 py-2">
                                    <ShieldAlert size={14} className="text-(--warning-light)" />
                                    <span className="text-xs text-(--base-07)">Rewrites server_stats in place and briefly locks it. On a large table, enable maintenance first. Continue?</span>
                                    <button className="btn btn-primary btn-sm" onClick={doConvert} disabled={converting}>
                                        {converting ? <Loader2 size={14} className="animate-spin" /> : null} Convert
                                    </button>
                                    <button className="btn btn-secondary btn-sm" onClick={() => setConfirmConvert(false)} disabled={converting}>Cancel</button>
                                </div>
                            )}
                        </div>
                    )}
                </div>
            )}

            {/* Target connection form */}
            <div className="card p-5 space-y-4">
                <div className="mono-label flex items-center gap-2">
                    <ServerCog size={13} /> Target connection
                    <HelpTip label="About the migration">
                        <p className="mb-2">
                            This copies everything into the database you describe here. It does
                            <strong> not switch over</strong>: Core keeps running on the current
                            database the whole time and after the copy finishes.
                        </p>
                        <p className="mb-2">
                            Making the new one live is a separate, manual step - set the{' '}
                            <code>DB_*</code> environment variables to point at it and restart Core.
                            Until you do, anything written after the copy started stays on the old
                            database only.
                        </p>
                        <p>
                            So plan the cutover: run the copy, verify, put the platform into
                            maintenance, run it once more, then restart with the new variables.
                        </p>
                    </HelpTip>
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                    <Field label="Host" value={form.host} onChange={v => set('host', v)} placeholder="db.example.internal" />
                    <Field label="Port" value={form.port} onChange={v => set('port', v)} placeholder="5432" />
                    <Field label="User" value={form.user} onChange={v => set('user', v)} placeholder="dylaris" />
                    <Field label="Password" value={form.password} onChange={v => set('password', v)} type="password" placeholder="••••••••" />
                    <Field label="Database name" value={form.dbName} onChange={v => set('dbName', v)} placeholder="dylaris" />
                    <div>
                        <label className="input-label flex items-center gap-1.5">
                            SSL mode
                            <HelpTip label="About the SSL mode">
                                <p className="mb-2">
                                    <code>disable</code> - no TLS. Only defensible when the database
                                    is reachable over a private network you trust end to end.
                                </p>
                                <p className="mb-2">
                                    <code>require</code> encrypts but verifies nothing, so it stops
                                    passive listening and not an attacker in the middle.
                                    <code> verify-ca</code> checks the certificate is signed by a CA
                                    you trust, and <code>verify-full</code> also checks the hostname
                                    matches.
                                </p>
                                <p>
                                    Over the public internet use <code>verify-full</code>. Managed
                                    providers all support it; a self-signed certificate does not, and
                                    that is the usual reason someone reaches for <code>require</code>.
                                </p>
                            </HelpTip>
                        </label>
                        <select className="input-field w-full mt-1" value={form.sslMode} onChange={e => set('sslMode', e.target.value)}>
                            <option value="disable">disable</option>
                            <option value="require">require</option>
                            <option value="verify-ca">verify-ca</option>
                            <option value="verify-full">verify-full</option>
                        </select>
                    </div>
                </div>

                {/* DB_TYPE multiple choice */}
                <div>
                    <label className="input-label">Target backend</label>
                    <div className="mt-1 grid grid-cols-2 gap-2 max-w-md">
                        <TypeChoice
                            active={form.dbType === 'postgres'}
                            onClick={() => set('dbType', 'postgres')}
                            title="PostgreSQL"
                            sub="Plain table + retention sweep. Small/medium fleets."
                        />
                        <TypeChoice
                            active={form.dbType === 'timescaledb'}
                            onClick={() => set('dbType', 'timescaledb')}
                            title="TimescaleDB"
                            sub="Hypertable + native retention. Larger fleets."
                        />
                    </div>
                </div>

                {/* Test result: the verdict first, then what was found there. */}
                <ConnectionTestNote result={test.result} />

                {testResult && (
                    <div className="text-xs font-mono text-(--base-06) bg-(--base-01) border border-(--base-03) rounded-md p-3 space-y-1">
                        <div className="text-(--success-light)">{testResult.version}</div>
                        <div>timescaledb extension on target: {testResult.timescaleInstalled ? 'installed' : 'not installed'}</div>
                        {tsMismatch && (
                            <div className="text-(--warning-light) flex items-center gap-1">
                                <ShieldAlert size={12} /> You chose TimescaleDB but the target has no timescaledb extension. The hypertable step will fail.
                            </div>
                        )}
                    </div>
                )}

                {/* Actions */}
                <div className="flex flex-wrap gap-2 pt-1">
                    <TestConnectionButton
                        test={test}
                        blockedReason={canSubmit ? null : 'Fill in host, port, user and database first.'}
                    />
                    <button className="btn btn-secondary" onClick={doVerify} disabled={!canSubmit || verifying}>
                        {verifying ? <Loader2 size={14} className="animate-spin" /> : <ListChecks size={14} />} Verify against target
                    </button>
                    {!confirmStart ? (
                        <button className="btn btn-primary" onClick={() => setConfirmStart(true)} disabled={!canSubmit || busy}>
                            <Play size={14} /> Start migration
                        </button>
                    ) : (
                        <div className="flex items-center gap-2 bg-(--warning-ghost) border border-(--warning-border) rounded-md px-3 py-2">
                            <ShieldAlert size={14} className="text-(--warning-light)" />
                            <span className="text-xs text-(--base-07)">This enables maintenance mode and overwrites the target. Continue?</span>
                            <button className="btn btn-primary btn-sm" onClick={doStart} disabled={starting}>
                                {starting ? <Loader2 size={14} className="animate-spin" /> : null} Yes, start
                            </button>
                            <button className="btn btn-secondary btn-sm" onClick={() => setConfirmStart(false)} disabled={starting}>Cancel</button>
                        </div>
                    )}
                </div>
            </div>

            {/* Live job */}
            {loading ? (
                <div className="card p-5 text-sm text-(--base-06) flex items-center gap-2"><Loader2 size={14} className="animate-spin" /> Loading…</div>
            ) : hasJob && job ? (
                <JobPanel job={job} report={verifyReport} />
            ) : (
                <div className="card p-5 text-sm text-(--base-06)">No migration has run yet.</div>
            )}

            {verifyReport && (
                <div className="card p-5 space-y-3">
                    <div className="mono-label flex items-center gap-2"><ListChecks size={13} /> Manual verify result</div>
                    <VerifyView report={verifyReport} />
                </div>
            )}

            {toast && (
                <div className={`fixed bottom-4 right-4 px-4 py-3 rounded-md shadow-lg text-sm font-medium ${toast.ok ? 'bg-(--success-ghost) text-(--success-light)' : 'bg-(--error-ghost) text-(--error-light)'}`}>
                    <span className="inline-flex items-center gap-2">
                        {toast.ok ? <CircleCheck size={14} /> : <CircleAlert size={14} />}{toast.msg}
                    </span>
                </div>
            )}
        </div>
    );
}

function Field({ label, value, onChange, type = 'text', placeholder }: {
    label: string; value: string; onChange: (v: string) => void; type?: string; placeholder?: string;
}) {
    return (
        <div>
            <label className="input-label">{label}</label>
            <input
                className="input-field input-mono w-full mt-1"
                type={type}
                value={value}
                placeholder={placeholder}
                onChange={e => onChange(e.target.value)}
                autoComplete="off"
            />
        </div>
    );
}

function TypeChoice({ active, onClick, title, sub }: { active: boolean; onClick: () => void; title: string; sub: string }) {
    return (
        <button
            type="button"
            onClick={onClick}
            className={`text-left rounded-md border p-3 transition-colors ${active
                ? 'border-(--accent) bg-(--accent-ghost)'
                : 'border-(--base-04) bg-(--base-03) hover:border-(--base-05)'}`}
        >
            <div className={`text-sm font-medium ${active ? 'text-(--accent-light)' : 'text-(--base-08)'}`}>{title}</div>
            <div className="text-[11px] text-(--base-06) mt-0.5">{sub}</div>
        </button>
    );
}

function JobPanel({ job, report }: { job: DBMigrationJob; report: VerifyReport | null }) {
    const pct = job.tablesTotal > 0 ? Math.round((job.tablesDone / job.tablesTotal) * 100) : 0;
    const inProgress = dbMigrationInProgress(job.phase);
    const effectiveVerify = job.verify ?? report;

    return (
        <div className="card p-5 space-y-4">
            <div className="flex items-center justify-between">
                <div className="mono-label">Migration job</div>
                <PhaseBadge phase={job.phase} stale={job.stale} />
            </div>

            <div className="text-xs text-(--base-06) space-y-0.5">
                <div>Started by <span className="text-(--base-08)">{job.startedByName || job.startedBy || 'unknown'}</span> · {fmtTime(job.startedAt)}</div>
                <div>Target: <span className="font-mono">{job.target.host}:{job.target.port}/{job.target.dbName}</span> ({job.target.dbType})</div>
                {job.maintenanceOn && (
                    <div className="text-(--warning-light) flex items-center gap-1">
                        <ShieldAlert size={12} /> Maintenance mode is ON.{' '}
                        <Link href="/settings/maintenance" className="underline">Manage</Link>
                    </div>
                )}
                {job.stale && (
                    <div className="text-(--error-light) flex items-center gap-1">
                        <CircleAlert size={12} /> No heartbeat — the Core running this job may have stopped.
                    </div>
                )}
            </div>

            {(inProgress || job.phase === 'done') && (
                <div>
                    <div className="flex justify-between text-[11px] text-(--base-06) mb-1">
                        <span>{job.currentTable ? `current: ${job.currentTable}` : PHASE_LABEL[job.phase]}</span>
                        <span>{job.tablesDone}/{job.tablesTotal} tables</span>
                    </div>
                    <div className="h-2 rounded-full bg-(--base-03) overflow-hidden">
                        <div
                            className={`h-full transition-all ${job.phase === 'done' ? 'bg-(--success-light)' : 'bg-(--accent)'}`}
                            style={{ width: `${job.phase === 'done' ? 100 : pct}%` }}
                        />
                    </div>
                </div>
            )}

            {job.error && (
                <div className="text-xs text-(--error-light) bg-(--error-ghost) border border-(--error-border) rounded-md p-3">
                    {job.error}
                </div>
            )}

            {job.phase === 'done' && effectiveVerify && !effectiveVerify.ok && (
                <div className="text-xs text-(--error-light) bg-(--error-ghost) border border-(--error-border) rounded-md p-3">
                    Copy finished but verification found differences. Do NOT switch yet — review below.
                </div>
            )}
            {job.phase === 'done' && effectiveVerify && effectiveVerify.ok && (
                <div className="text-xs text-(--success-light) bg-(--success-ghost) border border-(--success-border) rounded-md p-3 space-y-1">
                    <div className="flex items-center gap-1"><CircleCheck size={12} /> Migration complete and verified.</div>
                    <div className="text-(--base-07)">Next: restart Core with the new DB_* (+ DB_TYPE) env vars, then turn off maintenance.</div>
                </div>
            )}

            {effectiveVerify && (
                <details className="text-xs">
                    <summary className="cursor-pointer text-(--base-06) mono-label">Verification report ({effectiveVerify.tables.length} tables)</summary>
                    <div className="mt-2"><VerifyView report={effectiveVerify} /></div>
                </details>
            )}

            {job.log?.length > 0 && (
                <details className="text-xs" open={inProgress}>
                    <summary className="cursor-pointer text-(--base-06) mono-label">Log ({job.log.length})</summary>
                    <pre className="mt-2 max-h-64 overflow-auto bg-(--base-01) border border-(--base-03) rounded-md p-3 font-mono text-[11px] text-(--base-06) whitespace-pre-wrap">
{job.log.join('\n')}
                    </pre>
                </details>
            )}
        </div>
    );
}

function VerifyView({ report }: { report: VerifyReport }) {
    return (
        <div className="overflow-auto">
            <table className="w-full text-xs font-mono">
                <thead>
                    <tr className="text-(--base-05) text-left">
                        <th className="py-1 pr-4 font-normal">Table</th>
                        <th className="py-1 pr-4 font-normal text-right">Source</th>
                        <th className="py-1 pr-4 font-normal text-right">Target</th>
                        <th className="py-1 font-normal">Status</th>
                    </tr>
                </thead>
                <tbody>
                    {report.tables.map(t => (
                        <tr key={t.table} className="border-t border-(--base-03)">
                            <td className="py-1 pr-4 text-(--base-08)">{t.table}</td>
                            <td className="py-1 pr-4 text-right text-(--base-06)">{t.sourceExists ? t.sourceRows : '—'}</td>
                            <td className="py-1 pr-4 text-right text-(--base-06)">{t.targetExists ? t.targetRows : '—'}</td>
                            <td className="py-1">
                                {t.ok
                                    ? <span className="text-(--success-light)">match</span>
                                    : !t.targetExists
                                        ? <span className="text-(--error-light)">missing on target</span>
                                        : !t.sourceExists
                                            ? <span className="text-(--warning-light)">extra on target</span>
                                            : <span className="text-(--error-light)">count mismatch</span>}
                            </td>
                        </tr>
                    ))}
                </tbody>
            </table>
        </div>
    );
}

function PhaseBadge({ phase, stale }: { phase: DBMigrationPhase; stale: boolean }) {
    const cls = phase === 'done'
        ? 'bg-(--success-ghost) text-(--success-light) border-(--success-border)'
        : phase === 'failed'
            ? 'bg-(--error-ghost) text-(--error-light) border-(--error-border)'
            : stale
                ? 'bg-(--error-ghost) text-(--error-light) border-(--error-border)'
                : 'bg-(--accent-ghost) text-(--accent-light) border-(--accent)';
    return (
        <span className={`inline-flex items-center gap-1 px-2 py-0.5 rounded text-[10px] font-mono uppercase tracking-wide border ${cls}`}>
            {dbMigrationInProgress(phase) && !stale && <Loader2 size={10} className="animate-spin" />}
            {PHASE_LABEL[phase]}
        </span>
    );
}

function Stat({ label, value }: { label: string; value: string }) {
    return (
        <div className="bg-(--base-01) border border-(--base-03) rounded-md p-2.5">
            <div className="input-label">{label}</div>
            <div className="text-sm font-mono text-(--base-08) mt-0.5 break-all">{value}</div>
        </div>
    );
}

function fmtNum(n: number): string {
    if (!isFinite(n)) return '0';
    return new Intl.NumberFormat().format(Math.round(n));
}

function fmtTime(iso: string): string {
    if (!iso) return '';
    const d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toLocaleString();
}
