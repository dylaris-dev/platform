'use client';

import { useState } from 'react';
import { Gauge, Loader2 } from 'lucide-react';
import { LimitField } from '@/components/settings/LimitField';
import HelpTip from '@/components/ui/HelpTip';
import Select from '@/components/ui/Select';
import { listSpeedTestNodes, runSpeedTest, suggestedCap, type SpeedTestNode, type SpeedTestResult } from '@/lib/api/speedTest';
import { withRateFloor } from '@/components/settings/nodeBackupThrottleHelpers';

const SIZES = [64, 256, 512] as const;

/**
 * A backup storage's upload cap, and the speed test that helps choose it.
 *
 * The cap holds for every backup to this storage, from any node, whether
 * anyone plays or not; with players online the node's own budget may be lower
 * still. The test needs a saved storage (it is run against the stored
 * credentials), so a new one shows only the cap.
 *
 * mine: the caller's own bucket, tested from a node their servers use.
 */
export default function StorageUploadCap({
    value, onChange, storageId, mine,
}: {
    value: number | null;
    onChange: (v: number | null) => void;
    storageId?: number;
    mine: boolean;
}) {
    const [nodes, setNodes] = useState<SpeedTestNode[] | null>(null);
    const [nodesError, setNodesError] = useState('');
    const [nodeId, setNodeId] = useState('');
    const [size, setSize] = useState<number>(64);
    const [running, setRunning] = useState(false);
    const [result, setResult] = useState<SpeedTestResult | null>(null);

    const openTest = async () => {
        if (!storageId) return;
        setNodesError('');
        const res = await listSpeedTestNodes(storageId, mine);
        if (!res.success) {
            setNodesError(res.message || 'Could not list the nodes to test from.');
            return;
        }
        const list = res.nodes ?? [];
        setNodes(list);
        const first = list.find(n => n.online);
        if (first) setNodeId(String(first.id));
    };

    const run = async () => {
        if (!storageId || !nodeId) return;
        setRunning(true);
        setResult(null);
        setResult(await runSpeedTest(storageId, mine, Number(nodeId), size));
        setRunning(false);
    };

    return (
        <div className="space-y-3">
            <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0 max-w-xs">
                    <label className="input-label flex items-center gap-1.5" htmlFor="storage-upload-cap">
                        Upload cap
                        <HelpTip label="About the upload cap">
                            <p className="mb-2">
                                The most a backup uploads to this storage, from any node and whether anyone plays or
                                not. For a storage that is slow, shared or on a metered link.
                            </p>
                            <p>
                                While players are online on a node, its own budget applies too, and the lower of the two
                                wins. Unset is no cap.
                            </p>
                        </HelpTip>
                    </label>
                    <p className="text-xs text-(--base-06) mt-1">Mbit/s, for every backup to this storage.</p>
                </div>
                <LimitField
                    id="storage-upload-cap"
                    unit="Mbit/s"
                    min={1}
                    unlimitedLabel="No cap"
                    value={value}
                    onChange={v => onChange(withRateFloor(v, 100))}
                />
            </div>

            {storageId != null && (
                <div className="rounded-md border border-(--base-04) bg-(--base-02) p-3 space-y-3">
                    {nodes === null ? (
                        <div className="flex flex-wrap items-center justify-between gap-2">
                            <span className="text-xs text-(--base-06)">
                                Measure how fast a node reaches this storage.
                            </span>
                            <button type="button" onClick={openTest} className="btn btn-secondary btn-sm">
                                <Gauge size={13} /> Speed test
                            </button>
                        </div>
                    ) : nodes.length === 0 ? (
                        <p className="text-xs text-(--base-06)">
                            {mine
                                ? 'None of your servers runs on a node yet, so there is nowhere to test from.'
                                : 'There is no node to test from.'}
                        </p>
                    ) : (
                        <>
                            <div className="grid grid-cols-1 sm:grid-cols-[1fr_auto] gap-2 items-end">
                                <div>
                                    <label className="input-label" htmlFor="speedtest-node">From node</label>
                                    <Select
                                        id="speedtest-node"
                                        value={nodeId}
                                        onChange={setNodeId}
                                        options={nodes.map(n => ({ value: String(n.id), label: n.name, badge: n.online ? undefined : 'offline' }))}
                                        placeholder="Pick a node"
                                    />
                                </div>
                                <div role="radiogroup" aria-label="Test size" className="flex gap-1">
                                    {SIZES.map(s => (
                                        <button
                                            key={s}
                                            type="button"
                                            role="radio"
                                            aria-checked={size === s}
                                            onClick={() => setSize(s)}
                                            className={`btn btn-sm ${size === s ? 'btn-primary' : 'btn-secondary'}`}
                                        >
                                            {s} MB
                                        </button>
                                    ))}
                                </div>
                            </div>
                            <div className="flex flex-wrap items-center justify-between gap-2">
                                <span className="text-xs text-(--base-06)">
                                    Uses the node&apos;s upload while it runs. The test file is deleted right after.
                                </span>
                                <button
                                    type="button"
                                    onClick={run}
                                    disabled={running || !nodeId || !nodes.find(n => String(n.id) === nodeId)?.online}
                                    className="btn btn-primary btn-sm disabled:opacity-40"
                                >
                                    {running ? <Loader2 size={13} className="animate-spin" /> : <Gauge size={13} />}
                                    {running ? 'Testing…' : 'Run test'}
                                </button>
                            </div>
                        </>
                    )}
                    {nodesError && <p className="text-xs text-(--error-light)" role="alert">{nodesError}</p>}
                    {result && !result.success && (
                        <p className="text-xs text-(--error-light)" role="alert">{result.message || 'The test failed.'}</p>
                    )}
                    {result?.success && result.mbit != null && (
                        <div className="flex flex-wrap items-center justify-between gap-2" role="status">
                            <span className="text-sm text-(--base-09)">
                                <span className="font-mono">{result.mbit} Mbit/s</span>
                                <span className="text-xs text-(--base-06)">
                                    {' '}from {result.node}{result.lan ? ', over the local network' : ''}
                                </span>
                            </span>
                            <button
                                type="button"
                                onClick={() => onChange(suggestedCap(result.mbit!))}
                                className="btn btn-secondary btn-sm"
                            >
                                Set cap to {suggestedCap(result.mbit)} Mbit/s (70%)
                            </button>
                        </div>
                    )}
                </div>
            )}
        </div>
    );
}
