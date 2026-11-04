"use client";

import { useEffect, useState } from 'react';
import { Globe, Plus, Trash2, Copy, Check, Loader2, X } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import Select from '@/components/ui/Select';
import Segmented from '@/components/ui/Segmented';
import { toast } from '@/components/ui/Toast';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import {
    addServerDomain, deleteServerDomain, type ServerDomain,
} from '@/lib/api/serverDomains';
import { useServerDomains } from '@/lib/serverDomainsStore';
import { checkedAgo, previewFqdn, statusStyle, type StatusTone } from '@/lib/serverDomainStatus';

// A spinner under this long is a flash, not feedback.
const SPINNER_AFTER_MS = 400;

const TONE: Record<StatusTone, { badge: string; dot: string }> = {
    neutral: { badge: 'badge-neutral', dot: 'bg-(--base-06)' },
    warning: { badge: 'badge-warning', dot: 'bg-(--warning)' },
    accent: { badge: 'badge-accent', dot: 'bg-(--accent)' },
};

function useSlow(active: boolean): boolean {
    const [slow, setSlow] = useState(false);
    useEffect(() => {
        if (!active) { setSlow(false); return; }
        const t = setTimeout(() => setSlow(true), SPINNER_AFTER_MS);
        return () => clearTimeout(t);
    }, [active]);
    return slow;
}

export default function ServerDomainsCard({ serverId, canWrite }: { serverId: string; canWrite: boolean }) {
    const { data, hidden, error: pollError, stale, refresh } = useServerDomains(serverId);
    const loadError = data ? '' : pollError;
    const [adding, setAdding] = useState(false);
    const [now, setNow] = useState(() => Date.now());
    const [copied, setCopied] = useState<number | null>(null);
    const [removing, setRemoving] = useState<number | null>(null);
    // checked-ago lines tick with each poll result
    useEffect(() => { setNow(Date.now()); }, [data, stale]);

    const slowLoad = useSlow(!data && !loadError && !hidden);

    if (hidden) return null;

    const writeTitle = 'You need the "Manage domains" permission on this server';
    const limits = data?.limits;
    const full = !!limits && ((limits.max !== null && limits.used >= limits.max) || (limits.serverMax !== null && limits.serverUsed >= limits.serverMax));

    const copy = async (d: ServerDomain) => {
        try {
            await navigator.clipboard.writeText(d.fqdn);
            setCopied(d.id);
            setTimeout(() => setCopied(c => (c === d.id ? null : c)), 1500);
        } catch {
            toast('Could not copy to the clipboard.', false);
        }
    };

    const remove = async (d: ServerDomain) => {
        const ok = await confirmDialog({
            title: 'Remove domain',
            message: `Remove ${d.fqdn} from this server?${d.mode === 'managed'
                ? '\n\nThe DNS record DYLARIS created for it is deleted too.'
                : '\n\nYour own DNS records are not touched. Delete them at your DNS provider if you no longer need them.'}`,
            confirmLabel: 'Remove',
        });
        if (!ok) return;
        setRemoving(d.id);
        const res = await deleteServerDomain(serverId, d.id);
        setRemoving(null);
        if (!res.success) { toast(res.message || 'Could not remove the domain.', false); return; }
        toast(`Removed ${d.fqdn}`);
        void refresh();
    };

    return (
        <div className="card p-6">
            <div className="flex items-start justify-between gap-3 mb-1">
                <h2 className="modal-title flex items-center gap-2">
                    <Globe size={20} className="text-(--accent-light)" />
                    Domains
                </h2>
                <div className="flex items-center gap-3 shrink-0">
                    {limits && (
                        <span className="mono-label text-(--base-06)" title="Domains used / allowed on your account. Own-DNS and managed domains both count.">
                            {limits.used}/{limits.max ?? 'no limit'}
                        </span>
                    )}
                    <button
                        type="button"
                        onClick={() => setAdding(true)}
                        disabled={!canWrite || full || !data}
                        title={!canWrite ? writeTitle : full ? 'Domain limit reached' : 'Add a domain'}
                        className="btn btn-primary btn-sm focus-ring"
                    >
                        <Plus size={14} />
                        Add domain
                    </button>
                </div>
            </div>
            <p className="text-sm text-(--base-06) mb-5">
                Let players join with a name such as play.example.com instead of the node address.
                {limits && limits.serverMax !== null && ` This server may have ${limits.serverMax}.`}
            </p>

            {loadError && (
                <p role="alert" className="text-sm text-(--error-light) mb-3">{loadError}</p>
            )}
            {data && stale && (
                <p role="status" className="text-xs text-(--warning-light) mb-3">
                    Could not refresh just now. The list below may be out of date.
                </p>
            )}
            {!data && !loadError && slowLoad && (
                <div aria-busy="true" aria-label="Loading domains">
                    <div className="h-12 rounded-md bg-(--base-03) animate-pulse" />
                </div>
            )}
            {data && data.domains.length === 0 && (
                <p className="text-sm text-(--base-06) border border-dashed border-(--base-04) rounded-md px-4 py-6 text-center">
                    No domains yet. Add one to give this server a name players can remember.
                </p>
            )}
            {data && data.domains.length > 0 && (
                <ul className="space-y-2">
                    {data.domains.map(d => {
                        const st = statusStyle(d.state);
                        const tone = TONE[st.tone];
                        return (
                            <li key={d.id} className="flex items-start gap-3 rounded-md border border-(--base-04) bg-(--base-02) px-3 py-2.5">
                                <span className={`mt-1.5 inline-block w-2 h-2 rounded-full shrink-0 ${tone.dot}`} aria-hidden="true" />
                                <div className="min-w-0 flex-1">
                                    <div className="flex flex-wrap items-center gap-2">
                                        <span className="font-mono text-sm text-(--base-09) break-all">{d.fqdn}</span>
                                        <span className={`badge ${tone.badge}`}>{st.label}</span>
                                    </div>
                                    <p className="text-xs text-(--base-06) mt-0.5">
                                        {d.mode === 'managed' ? 'Managed by DYLARIS' : 'Your own DNS'} · {checkedAgo(d.observedAt, now)}
                                    </p>
                                    {d.problems.length > 0 && (
                                        <ul className="mt-1.5 space-y-0.5 text-xs text-(--warning-light)">
                                            {d.problems.map((p, i) => <li key={i}>{p}</li>)}
                                        </ul>
                                    )}
                                </div>
                                <div className="flex items-center gap-1 shrink-0">
                                    <button
                                        type="button"
                                        onClick={() => void copy(d)}
                                        aria-label={`Copy ${d.fqdn}`}
                                        title="Copy domain"
                                        className="btn btn-ghost btn-icon btn-sm focus-ring"
                                    >
                                        {copied === d.id ? <Check size={14} /> : <Copy size={14} />}
                                    </button>
                                    <button
                                        type="button"
                                        onClick={() => void remove(d)}
                                        disabled={!canWrite || removing === d.id}
                                        aria-label={`Remove ${d.fqdn}`}
                                        title={canWrite ? 'Remove domain' : writeTitle}
                                        className="btn btn-ghost btn-icon btn-sm focus-ring"
                                    >
                                        {removing === d.id ? <Loader2 size={14} className="animate-spin" /> : <Trash2 size={14} />}
                                    </button>
                                </div>
                            </li>
                        );
                    })}
                </ul>
            )}

            {adding && data && (
                <AddDomainModal
                    serverId={serverId}
                    zones={data.zones}
                    onClose={() => setAdding(false)}
                    onAdded={() => { setAdding(false); void refresh(); }}
                />
            )}
        </div>
    );
}

function AddDomainModal({ serverId, zones, onClose, onAdded }: {
    serverId: string;
    zones: { id: number; name: string }[];
    onClose: () => void;
    onAdded: () => void;
}) {
    const [mode, setMode] = useState<'manual' | 'managed'>('manual');
    const [fqdn, setFqdn] = useState('');
    const [zoneId, setZoneId] = useState(zones[0] ? String(zones[0].id) : '');
    const [label, setLabel] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');
    const slow = useSlow(busy);

    const zone = zones.find(z => String(z.id) === zoneId);
    const preview = previewFqdn(label, zone?.name ?? '');
    const ready = mode === 'manual' ? fqdn.trim() !== '' : !!zone && label.trim() !== '';

    const submit = async (e: React.FormEvent) => {
        e.preventDefault();
        if (!ready || busy) return;
        setBusy(true); setError('');
        const res = await addServerDomain(serverId, mode === 'manual'
            ? { mode, fqdn: fqdn.trim().toLowerCase() }
            : { mode, zoneId: Number(zoneId), label: label.trim().toLowerCase() });
        setBusy(false);
        if (!res.success) { setError(res.message || 'Could not add the domain.'); return; }
        toast(`Added ${res.domain?.fqdn ?? 'domain'}`);
        onAdded();
    };

    return (
        <div className="modal-overlay animate-fade-in">
            <ModalPanel onClose={onClose} className="modal-panel w-[calc(100%-32px)] max-w-md">
                <form onSubmit={submit}>
                    <div className="modal-header flex items-center justify-between">
                        <h3 className="modal-title">Add domain</h3>
                        <button type="button" onClick={onClose} aria-label="Close" title="Close" className="btn btn-ghost btn-icon btn-sm">
                            <X size={16} />
                        </button>
                    </div>
                    <div className="p-6 space-y-4">
                        <Segmented
                            ariaLabel="How the domain is set up"
                            value={mode}
                            onChange={m => { setMode(m); setError(''); }}
                            options={[
                                { id: 'manual', label: 'My own DNS', hint: 'You create the DNS record at your own provider' },
                                {
                                    id: 'managed', label: 'Managed', disabled: zones.length === 0,
                                    hint: zones.length ? 'DYLARIS creates the DNS record for you' : 'No zone is available to you yet',
                                },
                            ]}
                        />
                        {zones.length === 0 && (
                            <p className="text-xs text-(--base-06)">Managed domains are unavailable: you may not use any zone yet. Ask an administrator for access.</p>
                        )}
                        {mode === 'manual' ? (
                            <div>
                                <label htmlFor="dom-fqdn" className="mono-label text-(--base-05) block mb-1.5">Domain name</label>
                                <input
                                    id="dom-fqdn" className="input-field w-full text-sm font-mono" autoFocus autoComplete="off" spellCheck={false}
                                    placeholder="play.example.com" value={fqdn} onChange={e => setFqdn(e.target.value)}
                                />
                                <p className="text-xs text-(--base-06) mt-1.5">
                                    Create an A record for it at your DNS provider, pointing at this server&apos;s node address. If the server does not use port 25565, also add an SRV record _minecraft._tcp.&lt;name&gt; pointing to &lt;name&gt; and the server port. DYLARIS only checks the records and tells you what is missing.
                                </p>
                            </div>
                        ) : (
                            <div className="space-y-3">
                                <div>
                                    <span className="mono-label text-(--base-05) block mb-1.5">Zone</span>
                                    <Select
                                        ariaLabel="Zone" value={zoneId} onChange={setZoneId}
                                        options={zones.map(z => ({ value: String(z.id), label: z.name }))}
                                    />
                                </div>
                                <div>
                                    <label htmlFor="dom-label" className="mono-label text-(--base-05) block mb-1.5">Name</label>
                                    <input
                                        id="dom-label" className="input-field w-full text-sm font-mono" autoComplete="off" spellCheck={false}
                                        placeholder="play" value={label} onChange={e => setLabel(e.target.value)}
                                    />
                                    <p className="text-xs text-(--base-06) mt-1.5 font-mono min-h-4" aria-live="polite">
                                        {preview ? `Result: ${preview}` : ''}
                                    </p>
                                </div>
                            </div>
                        )}
                        {error && <p role="alert" className="text-sm text-(--error-light)">{error}</p>}
                    </div>
                    <div className="modal-footer flex justify-end gap-2">
                        <button type="button" onClick={onClose} className="btn btn-ghost btn-sm focus-ring">Cancel</button>
                        <button type="submit" disabled={!ready || busy} className="btn btn-primary btn-sm focus-ring">
                            {slow && <Loader2 size={14} className="animate-spin" />}
                            Add domain
                        </button>
                    </div>
                </form>
            </ModalPanel>
        </div>
    );
}
