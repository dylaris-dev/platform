'use client';

import { useState } from 'react';
import { ChevronDown, ChevronRight, Save } from 'lucide-react';
import {
    getNodeBackupThrottle, setNodeBackupThrottle,
    type NodeBackupThrottle as Throttle, type NodeBackupEnv,
} from '@/lib/api';
import { LimitField } from '@/components/settings/LimitField';
import HelpTip from '@/components/ui/HelpTip';
import { SkeletonCard } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';
import { formatBytes } from '@/lib/format';
import { throttleChanged, withRateFloor } from '@/components/settings/nodeBackupThrottleHelpers';

/**
 * How one node's backups yield to players: the node's uplink, an optional
 * disk-read cap, and the spool. Collapsed until opened, so the node list does
 * not fetch it for every node on every refresh.
 *
 * mine: the caller's own BYON machine (/api/me/nodes), whose uplink it is.
 */
export default function NodeBackupThrottle({ nodeId, mine = false }: { nodeId: number; mine?: boolean }) {
    const [open, setOpen] = useState(false);
    const [saved, setSaved] = useState<Throttle | null>(null);
    const [draft, setDraft] = useState<Throttle | null>(null);
    const [env, setEnv] = useState<NodeBackupEnv | null>(null);
    const [loadError, setLoadError] = useState('');
    const [saving, setSaving] = useState(false);

    const toggle = async () => {
        const next = !open;
        setOpen(next);
        if (!next || saved) return;
        setLoadError('');
        const res = await getNodeBackupThrottle(nodeId, mine);
        if (!res.success || !res.throttle) {
            setLoadError(res.message || 'Could not load the backup settings of this node.');
            return;
        }
        setSaved(res.throttle);
        setDraft(res.throttle);
        setEnv(res.node ?? null);
    };

    const save = async () => {
        if (!draft) return;
        setSaving(true);
        const res = await setNodeBackupThrottle(nodeId, draft, mine);
        setSaving(false);
        if (!res.success || !res.throttle) {
            toast(res.message || 'Saving failed', false);
            return;
        }
        setSaved(res.throttle);
        setDraft(res.throttle);
        setEnv(res.node ?? null);
        toast('Backup settings saved', true);
    };

    const envUplink = env?.uplinkMbit ?? null;
    const dirty = !!draft && !!saved && throttleChanged(saved, draft);

    return (
        <div className="mt-3 border-t border-(--base-04) pt-3">
            <button
                type="button"
                onClick={toggle}
                aria-expanded={open}
                className="flex items-center gap-1.5 text-xs font-medium text-(--base-08) hover:text-(--accent-light) transition-colors"
            >
                {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
                Backups while players are online
            </button>

            {open && (
                <div className="mt-3 space-y-4">
                    {loadError && <p className="text-xs text-(--error-light)" role="alert">{loadError}</p>}
                    {!loadError && !draft && <SkeletonCard height="h-24" />}
                    {draft && (
                        <>
                            <Row
                                id={`bt-uplink-${nodeId}`}
                                label="Upload speed of this node"
                                help={
                                    <>
                                        <p className="mb-2">
                                            The node&apos;s real upload capacity. While anyone plays on this node, a backup
                                            uploads with what is left after the players&apos; traffic and the headroom set
                                            under Settings, Backups - never less than 10% of it, so a backup always finishes.
                                        </p>
                                        <p>
                                            Unset uses NODE_UPLINK_MBIT from the node&apos;s environment. With neither, backups
                                            are never slowed down. Uploads to an address in the local network are not slowed
                                            either: they do not share the uplink with the players.
                                        </p>
                                    </>
                                }
                                hint={envUplink != null
                                    ? `Unset uses the node's NODE_UPLINK_MBIT (${envUplink} Mbit/s).`
                                    : 'Unset and no NODE_UPLINK_MBIT on the node: backups are not slowed down.'}
                            >
                                <LimitField
                                    id={`bt-uplink-${nodeId}`}
                                    unit="Mbit/s"
                                    min={1}
                                    unlimitedLabel={envUplink != null ? 'From env' : 'Not set'}
                                    value={draft.uplinkMbit}
                                    onChange={v => setDraft({ ...draft, uplinkMbit: withRateFloor(v, 100) })}
                                />
                            </Row>

                            <Row
                                id={`bt-read-${nodeId}`}
                                label="Disk read while players are online"
                                help={
                                    <p>
                                        Caps how fast a backup reads the server&apos;s files, which also caps the CPU spent
                                        compressing them. Useful on a busy disk or a small CPU. Unset is no cap.
                                    </p>
                                }
                                hint="Uncompressed MB/s."
                            >
                                <LimitField
                                    id={`bt-read-${nodeId}`}
                                    unit="MB/s"
                                    min={1}
                                    unlimitedLabel="No cap"
                                    value={draft.readMBps}
                                    onChange={v => setDraft({ ...draft, readMBps: withRateFloor(v, 50) })}
                                />
                            </Row>

                            <div className="space-y-2">
                                <label className="flex items-start gap-2 text-xs text-(--base-08) cursor-pointer">
                                    <input
                                        type="checkbox"
                                        checked={draft.spoolEnabled}
                                        onChange={e => setDraft({
                                            ...draft,
                                            spoolEnabled: e.target.checked,
                                            spoolMaxGB: e.target.checked && draft.spoolMaxGB < 1 ? 50 : draft.spoolMaxGB,
                                        })}
                                        className="checkbox mt-0.5"
                                    />
                                    <span>
                                        <span className="block">Pack backups on this node first, then upload</span>
                                        <span className="block text-(--base-06)">
                                            The world saves again as soon as the archive is packed, and only the upload is
                                            slowed down. This needs free disk space as large as the backup: a server larger
                                            than the limit below, or a disk without that space plus 5 GB, is uploaded directly.
                                            Only for backups to object storage.
                                        </span>
                                    </span>
                                </label>
                                {draft.spoolEnabled && (
                                    <div className="flex flex-wrap items-center justify-between gap-3 pl-6">
                                        <label htmlFor={`bt-spool-${nodeId}`} className="input-label">Largest backup to pack first</label>
                                        <div className="flex items-center gap-1.5">
                                            <input
                                                id={`bt-spool-${nodeId}`}
                                                type="number"
                                                min={1}
                                                step={1}
                                                value={draft.spoolMaxGB}
                                                onChange={e => {
                                                    if (e.target.value === '') return;
                                                    const n = Math.trunc(Number(e.target.value));
                                                    if (Number.isFinite(n)) setDraft({ ...draft, spoolMaxGB: Math.max(1, n) });
                                                }}
                                                className="input-mono w-20 text-center"
                                            />
                                            <span className="text-[10px] font-mono uppercase text-(--base-06)">GB</span>
                                        </div>
                                    </div>
                                )}
                                {draft.spoolEnabled && (
                                    <p className="pl-6 text-xs text-(--base-06)">
                                        {env?.spoolDir
                                            ? <>Packed into <span className="font-mono">{env.spoolDir}</span>, {formatBytes(env.spoolFreeBytes ?? 0)} free. Change it with NODE_BACKUP_SPOOL_DIR on the node.</>
                                            : 'The node has not reported its spool directory yet (offline, or an older node).'}
                                    </p>
                                )}
                            </div>

                            <div className="flex justify-end">
                                <button type="button" onClick={save} disabled={!dirty || saving} className="btn btn-primary btn-sm disabled:opacity-40">
                                    <Save size={12} /> {saving ? 'Saving…' : 'Save'}
                                </button>
                            </div>
                        </>
                    )}
                </div>
            )}
        </div>
    );
}

function Row({ id, label, help, hint, children }: { id: string; label: string; help: React.ReactNode; hint: string; children: React.ReactNode }) {
    return (
        <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0 max-w-md">
                <label htmlFor={id} className="input-label flex items-center gap-1.5">
                    {label}
                    <HelpTip label={`About ${label.toLowerCase()}`}>{help}</HelpTip>
                </label>
                <p className="text-xs text-(--base-06) mt-1">{hint}</p>
            </div>
            {children}
        </div>
    );
}
