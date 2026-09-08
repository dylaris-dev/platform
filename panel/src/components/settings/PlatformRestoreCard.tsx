'use client';

import React, { useState } from 'react';
import { AlertTriangle, Info, RotateCcw, Upload, X } from 'lucide-react';
import {
    EMPTY_TARGET, inspectBundle, restoreBundle,
    type BundleInspection, type PlatformRestoreResult, type PlatformRestoreSelection,
    type RestoreTarget,
} from '@/lib/api/platformBackups';
import SettingsCard from '@/components/settings/SettingsCard';
import { toast } from '@/components/ui/Toast';
import { useBusy } from '@/lib/useBusy';

/**
 * Reading a bundle back.
 *
 * The database goes into a NEW, empty database the operator names, never over
 * the one this Core is running on. The live Core keeps working throughout, a
 * failed restore costs nothing, and the switch-over is a restart with the new
 * DB_* values once the result can be seen.
 */
export default function PlatformRestoreCard() {
    const [file, setFile] = useState<File | null>(null);
    const [passphrase, setPassphrase] = useState('');
    const [info, setInfo] = useState<BundleInspection | null>(null);
    const [inspecting, runInspect] = useBusy();
    const [restoring, runRestore] = useBusy();
    const [result, setResult] = useState<PlatformRestoreResult | null>(null);

    const [components, setComponents] = useState<PlatformRestoreSelection>({
        database: false, library: false, modpacks: false,
    });
    const [target, setTarget] = useState<RestoreTarget>({ ...EMPTY_TARGET });
    const [overwrite, setOverwrite] = useState(false);

    const clearChosenBundle = () => {
        setFile(null); setInfo(null); setResult(null);
        setComponents({ database: false, library: false, modpacks: false });
        setOverwrite(false);
    };

    const inspect = async (f: File, pass: string) => {
        await runInspect(async () => {
            const res = await inspectBundle(f, pass);
            setInfo(res);
            if (!res.success) toast(res.message || 'This is not a Dylaris backup bundle.', false);
        });
    };

    const pickFile = async (f: File | null) => {
        setFile(f); setInfo(null); setResult(null);
        if (f) await inspect(f, '');
    };

    const anySelected = components.database || components.library || components.modpacks;
    const needsTarget = components.database;
    const targetIncomplete = needsTarget && (!target.host || !target.user || !target.dbName);
    const canRestore = !!file && !!passphrase && anySelected && !targetIncomplete && info?.passphraseOk;

    const restore = async () => {
        if (!file) return;
        await runRestore(async () => {
            const res = await restoreBundle(file, { passphrase, components, target, overwrite });
            setResult(res.result || null);
            if (res.success) {
                toast('Restore finished.');
            } else {
                toast(res.message || 'The restore failed.', false);
            }
        });
    };

    return (
        <SettingsCard
            title="Restore a bundle"
            description="Read a platform bundle back, from this instance or from another one."
            icon={RotateCcw}
        >
            <div className="alert alert-info text-xs mb-3">
                <Info size={13} className="text-(--accent-light) shrink-0 mt-0.5" />
                <p>
                    The database is restored into a <strong>new, empty database you name</strong>, never
                    over the one this Core is running on. Nothing here interrupts the running
                    platform: once you have checked the result, you switch over by restarting Core
                    with the new <code>DB_*</code> values.
                </p>
            </div>

            {!file ? (
                <label className="min-h-[110px] border-2 border-dashed border-(--base-04) rounded-lg flex flex-col items-center justify-center gap-2 cursor-pointer transition-all hover:border-(--accent) hover:bg-(--accent-ghost)/50">
                    <Upload size={20} className="text-(--base-06)" />
                    <span className="text-sm text-(--base-07)">Choose a bundle</span>
                    <span className="text-xs text-(--base-06)">.dylaris-bundle</span>
                    <input type="file" className="hidden" accept=".dylaris-bundle,application/octet-stream"
                        onChange={e => pickFile(e.target.files?.[0] || null)} />
                </label>
            ) : (
                <div className="space-y-3">
                    <div className="flex items-center gap-3 p-3 rounded-md border border-(--base-04) bg-(--base-02)">
                        <div className="flex-1 min-w-0">
                            <p className="text-sm font-mono text-(--base-09) truncate">{file.name}</p>
                            <p className="text-xs text-(--base-06)">
                                {inspecting ? 'Reading...' : info?.success
                                    ? `written by ${info.source || 'an unknown release'}${info.createdAt ? ` on ${new Date(info.createdAt).toLocaleString()}` : ''}`
                                    : 'not readable as a bundle'}
                            </p>
                        </div>
                        <button type="button" onClick={clearChosenBundle} aria-label="Choose a different bundle"
                            className="text-(--base-06) hover:text-(--error-light) transition-colors">
                            <X size={16} />
                        </button>
                    </div>

                    <div>
                        <label className="input-label mb-1 block" htmlFor="pr-pass">Backup passphrase</label>
                        <div className="flex gap-2">
                            <input id="pr-pass" type="password" className="input flex-1" value={passphrase}
                                autoComplete="off"
                                onChange={e => {
                                    setPassphrase(e.target.value);
                                    // The confirmation belongs to the passphrase that was checked,
                                    // not to the field: editing it makes the green line a lie.
                                    setInfo((prev: BundleInspection | null) => prev ? { ...prev, passphraseOk: false } : prev);
                                }} />
                            <button type="button" className="btn btn-sm" disabled={!passphrase || inspecting}
                                onClick={() => file && inspect(file, passphrase)}>
                                Check
                            </button>
                        </div>
                        {info?.passphraseOk && (
                            <p className="text-xs text-(--success-light) mt-1">
                                This passphrase opens the bundle.
                                {info.selection && (
                                    <> It holds: {describeBundle(info)}.</>
                                )}
                            </p>
                        )}
                        {info && info.success && !info.passphraseOk && passphrase && !inspecting && (
                            <p className="text-xs text-(--error-light) mt-1">
                                {info.message || 'This passphrase does not open the bundle.'}
                            </p>
                        )}
                    </div>

                    {info?.passphraseOk && info.carriesClusterSecret === false && (
                        <div className="alert alert-warning text-xs">
                            <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <p>
                                This bundle carries no cluster secret, so the credentials inside it
                                cannot be re-encrypted for this installation. Nodes will have to
                                re-pair and storage credentials will have to be entered again.
                            </p>
                        </div>
                    )}

                    <div>
                        <p className="input-label mb-2">What to restore</p>
                        <div className="space-y-1.5">
                            {([
                                ['database', 'Platform database'],
                                ['library', 'Library'],
                                ['modpacks', 'Modpacks'],
                            ] as [keyof PlatformRestoreSelection, string][]).map(([key, label]) => (
                                <label key={key} className="flex items-center gap-2.5 cursor-pointer">
                                    <input type="checkbox" checked={components[key]}
                                        onChange={e => setComponents((prev: PlatformRestoreSelection) => ({ ...prev, [key]: e.target.checked }))} />
                                    <span className="text-sm text-(--base-09)">{label}</span>
                                </label>
                            ))}
                        </div>
                        <p className="text-xs text-(--base-06) mt-1">
                            The Library and Modpacks are written straight into this installation&apos;s
                            Core storage, replacing files of the same name.
                        </p>
                    </div>

                    {needsTarget && (
                        <div className="space-y-2">
                            <p className="input-label">Target database</p>
                            <div className="grid grid-cols-1 md:grid-cols-2 gap-2">
                                <input className="input" placeholder="Host" value={target.host}
                                    onChange={e => setTarget(t => ({ ...t, host: e.target.value }))} />
                                <input className="input" placeholder="Port" value={target.port}
                                    onChange={e => setTarget(t => ({ ...t, port: e.target.value }))} />
                                <input className="input" placeholder="User" value={target.user}
                                    onChange={e => setTarget(t => ({ ...t, user: e.target.value }))} />
                                <input className="input" type="password" placeholder="Password" value={target.password}
                                    autoComplete="off"
                                    onChange={e => setTarget(t => ({ ...t, password: e.target.value }))} />
                                <input className="input" placeholder="Database name" value={target.dbName}
                                    onChange={e => setTarget(t => ({ ...t, dbName: e.target.value }))} />
                                <select className="input" value={target.sslMode}
                                    onChange={e => setTarget(t => ({ ...t, sslMode: e.target.value }))}>
                                    <option value="disable">sslmode: disable</option>
                                    <option value="require">sslmode: require</option>
                                    <option value="verify-full">sslmode: verify-full</option>
                                </select>
                            </div>
                            <label className="flex items-start gap-2.5 cursor-pointer">
                                <input type="checkbox" className="mt-0.5" checked={overwrite}
                                    onChange={e => setOverwrite(e.target.checked)} />
                                <span className="min-w-0">
                                    <span className="text-sm text-(--base-09)">This database already has tables, overwrite them</span>
                                    <span className="block text-xs text-(--base-06)">
                                        Without this a target that is not empty is refused. Restoring over an
                                        existing schema leaves rows from two installations in one database,
                                        with nothing afterwards saying which came from where.
                                    </span>
                                </span>
                            </label>
                        </div>
                    )}

                    <button type="button" className="btn btn-primary btn-sm" disabled={!canRestore || restoring}
                        onClick={restore}>
                        {restoring ? 'Restoring...' : 'Restore'}
                    </button>

                    {result && <RestoreResult result={result} />}
                </div>
            )}
        </SettingsCard>
    );
}

function RestoreResult({ result }: { result: PlatformRestoreResult }) {
    return (
        <div className="border border-(--base-04) rounded-md p-3 space-y-2 bg-(--base-02)">
            <p className="text-sm text-(--base-09)">
                From {result.source || 'an unknown release'}
                {result.createdAt ? `, ${new Date(result.createdAt).toLocaleString()}` : ''}
            </p>
            {(result.components || []).map((c, i) => (
                <p key={i} className="text-xs text-(--base-07)">
                    <span className={
                        c.status === 'included' ? 'text-(--success-light)'
                            : c.status === 'failed' ? 'text-(--error-light)' : 'text-(--warning-light)'
                    }>{c.status}</span>{' '}{c.kind}{c.message ? ` - ${c.message}` : ''}
                </p>
            ))}
            {result.reseal && (
                <p className="text-xs text-(--base-06)">
                    Credentials re-encrypted for this installation:{' '}
                    {result.reseal.buckets.map(b => `${b.name} ${b.moved}`).join(', ')}
                </p>
            )}
            {(result.warnings || []).map((wmsg, i) => (
                <div key={i} className="alert alert-warning text-xs">
                    <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                    <p>{wmsg}</p>
                </div>
            ))}
        </div>
    );
}

/** What a bundle says it holds, for the line under the passphrase field. */
export function describeBundle(info: BundleInspection): string {
    const sel = info.selection;
    if (!sel) return 'nothing it could describe';
    const parts: string[] = [];
    if (sel.database) parts.push('the database');
    if (sel.library) parts.push('the Library');
    if (sel.modpacks) parts.push('Modpacks');
    const servers = (info.components || []).filter(c => c.kind === 'server').length;
    if (servers > 0) parts.push(`${servers} server${servers === 1 ? '' : 's'}`);
    return parts.length ? parts.join(', ') : 'nothing';
}
