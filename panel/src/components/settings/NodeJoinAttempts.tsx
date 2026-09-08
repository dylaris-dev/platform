'use client';

import { useCallback, useEffect, useState } from 'react';
import { AlertTriangle, Check, ShieldQuestion, X } from 'lucide-react';
import {
    listNodeJoinAttempts, approveNodeJoinAttempt, dismissNodeJoinAttempt,
    type NodeJoinAttempt,
} from '@/lib/api/nodeAdmission';
import SettingsCard from '@/components/settings/SettingsCard';
import { Skeleton } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { useBusy } from '@/lib/useBusy';
import { timeAgo } from '@/lib/time';

/**
 * The machines Core is turning away.
 *
 * This screen exists because a refusal used to be a single line on Core's
 * stdout, which dies with the container. A node whose secret and Core's had
 * diverged retried every thirty seconds, forever, and looked in the panel
 * exactly like a machine somebody had switched off - so eu-node-00 sat out for
 * six hours before anyone looked at a file modification time to find out why.
 *
 * Everything except the source address is what the machine SAID about itself,
 * sent before any proof was checked. It is labelled that way and never shown as
 * fact: it is here so an operator recognises their own hardware, not so the
 * panel can decide anything from it.
 */
function formatMemory(bytes: number): string {
    if (!bytes) return '';
    const gb = bytes / (1024 * 1024 * 1024);
    return gb >= 1 ? `${gb.toFixed(1)} GB RAM` : `${Math.round(bytes / (1024 * 1024))} MB RAM`;
}

export default function NodeJoinAttempts({ onAdmitted }: { onAdmitted: () => void }) {
    const [attempts, setAttempts] = useState<NodeJoinAttempt[]>([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);
    const [busyToken, setBusyToken] = useState<string | null>(null);
    const [approving, runApprove] = useBusy();
    const [dismissing, runDismiss] = useBusy();

    const load = useCallback(async () => {
        const res = await listNodeJoinAttempts();
        setLoading(false);
        if (res.success && Array.isArray(res.attempts)) {
            setAttempts(res.attempts);
            setError(null);
            return;
        }
        // A dropped failure would leave the last list on screen and read as "no
        // node is being refused", which is the one answer that must not be
        // guessed here.
        setError(res.message || 'Could not read the connection attempts.');
    }, []);

    useEffect(() => {
        void load();
        // The node retries every 30s, so a slower poll than the node list is
        // enough and keeps this off the 5-second cadence.
        const t = setInterval(() => { void load(); }, 15000);
        return () => clearInterval(t);
    }, [load]);

    const doApprove = async (a: NodeJoinAttempt) => {
        const name = a.displayName || a.nodeName || a.hostname || a.nodeToken.slice(0, 8);
        if (!(await confirmDialog({
            title: 'Admit this node',
            message: `Let "${name}" back in from ${a.peerIp}? Core will issue it a new secret on its next attempt, within about a minute. Approve this only if you recognise the machine and the address.`,
            confirmLabel: 'Admit',
            destructive: false,
        }))) return;
        setBusyToken(a.nodeToken);
        await runApprove(async () => {
            const res = await approveNodeJoinAttempt(a.nodeToken);
            setBusyToken(null);
            if (!res.success) { setError(res.message || 'Could not admit the node.'); return; }
            setError(null);
            await load();
            onAdmitted();
        });
    };

    const doDismiss = async (a: NodeJoinAttempt) => {
        setBusyToken(a.nodeToken);
        await runDismiss(async () => {
            const res = await dismissNodeJoinAttempt(a.nodeToken);
            setBusyToken(null);
            if (!res.success) { setError(res.message || 'Could not dismiss the attempt.'); return; }
            await load();
        });
    };

    if (loading) return <Skeleton className="h-24 w-full" />;
    // Nothing being refused is the normal state, and an empty card every day
    // would train an operator to stop reading this area.
    if (attempts.length === 0 && !error) return null;

    return (
        <SettingsCard
            title="Connection attempts"
            description="Nodes Core is refusing. They keep retrying on their own, so admitting one here is all that is needed - nothing has to be changed on the machine."
            icon={ShieldQuestion}
        >
            {error && (
                <div className="p-3 border border-(--error) rounded-md text-(--error) text-sm mb-3">{error}</div>
            )}

            <div className="space-y-3">
                {attempts.map(a => {
                    const armed = !!a.approvedUntil && new Date(a.approvedUntil) > new Date();
                    const name = a.displayName || a.nodeName || a.nodeToken.slice(0, 8);
                    const specs = [
                        a.hostname && `host ${a.hostname}`,
                        a.cpuCores ? `${a.cpuCores} cores` : '',
                        a.cpuModel,
                        formatMemory(a.memoryBytes),
                        a.releaseVersion && `node ${a.releaseVersion}`,
                    ].filter(Boolean).join(' - ');

                    return (
                        <div key={a.nodeToken} className="card p-3 space-y-2">
                            <div className="flex items-start justify-between gap-3">
                                <div className="min-w-0">
                                    <div className="flex items-center gap-2 flex-wrap">
                                        <span className="font-medium text-sm text-(--base-09)">{name}</span>
                                        <span className="badge badge-warning inline-flex items-center gap-1">
                                            <AlertTriangle size={11} />
                                            refused {a.attempts}x
                                        </span>
                                        {armed && <span className="badge badge-accent">admitted, waiting for it to reconnect</span>}
                                    </div>
                                    <p className="text-xs text-(--base-06) mt-0.5">{a.reason}</p>
                                </div>
                                <div className="flex items-center gap-1 shrink-0">
                                    <button
                                        type="button"
                                        className="btn btn-sm btn-primary"
                                        disabled={approving || dismissing || armed}
                                        onClick={() => doApprove(a)}
                                    >
                                        <Check size={13} />
                                        {busyToken === a.nodeToken && approving ? 'Admitting...' : 'Admit'}
                                    </button>
                                    <button
                                        type="button"
                                        className="btn btn-sm"
                                        title="Remove this from the list. It reappears if the machine keeps trying."
                                        disabled={approving || dismissing}
                                        onClick={() => doDismiss(a)}
                                    >
                                        <X size={13} />
                                    </button>
                                </div>
                            </div>

                            <dl className="grid grid-cols-1 sm:grid-cols-2 gap-x-4 gap-y-1 text-xs">
                                <div className="flex gap-2">
                                    <dt className="mono-label shrink-0">From</dt>
                                    <dd className="text-(--base-08) font-mono">{a.peerIp || 'unknown'}</dd>
                                </div>
                                <div className="flex gap-2">
                                    <dt className="mono-label shrink-0">Last try</dt>
                                    <dd className="text-(--base-08)">{timeAgo(a.lastSeenAt)}</dd>
                                </div>
                                {specs && (
                                    <div className="flex gap-2 sm:col-span-2">
                                        <dt className="mono-label shrink-0">Reported</dt>
                                        <dd className="text-(--base-07)">{specs}</dd>
                                    </div>
                                )}
                                {(a.reportedPublicIp || a.reportedPrivateIps) && (
                                    <div className="flex gap-2 sm:col-span-2">
                                        <dt className="mono-label shrink-0">Reported IPs</dt>
                                        <dd className="text-(--base-07) font-mono break-all">
                                            {[a.reportedPublicIp, a.reportedPrivateIps].filter(Boolean).join(', ')}
                                        </dd>
                                    </div>
                                )}
                            </dl>
                        </div>
                    );
                })}
            </div>

            <p className="text-xs text-(--base-06) mt-3">
                Only <span className="font-mono">From</span> is observed by Core. Everything under
                Reported is what the machine says about itself, sent before it proves anything, so
                treat it as a way to recognise your own hardware and not as evidence. Admitting is
                tied to that address and lapses after 15 minutes.
            </p>
        </SettingsCard>
    );
}
