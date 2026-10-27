'use client';

import { toast as notifyToast } from '@/components/ui/Toast';

import { useCallback, useEffect, useState } from 'react';
import { AlertTriangle, Plus, Network, Trash2, Server, Circle, Shield } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import {
    getWarpRegions, upsertWarpRegion, deleteWarpRegion,
    upsertWarpLeader, deleteWarpLeader,
    getWarpFirewallSettings, saveWarpFirewallSettings,
    type WarpRegionView,
} from '@/lib/api/types';
import { confirmDialog } from '@/components/ui/ConfirmDialog';

export default function WarpTab() {
    const { gatewayEnabled, fileAccessMode } = useAppData();
    // gatewayEnabled is routing gateway OR both, the same test Core's warp
    // enrol applies. Beam file access stays required: without it the servers
    // on an External node have no file access path.
    const gateOpen = gatewayEnabled && fileAccessMode === 'beam';

    const [regions, setRegions] = useState<WarpRegionView[]>([]);
    const [loading, setLoading] = useState(true);


    const [newRegion, setNewRegion] = useState({ region: '', subnet: '' });
    const [addingRegion, setAddingRegion] = useState(false);

    const [fwPorts, setFwPorts] = useState('');
    const [fwSubnets, setFwSubnets] = useState('');
    // What Core detected, shown when nothing is stored yet so the operator can
    // accept it rather than look the CIDR up. Kept separate from fwSubnets so
    // "detected" and "saved" never look the same.
    const [subnetHint, setSubnetHint] = useState<{ suggested: string; candidates: string[]; source: string } | null>(null);
    const [fwLoaded, setFwLoaded] = useState(false);
    const [savingFw, setSavingFw] = useState(false);

    const showToast = (msg: string, ok = true) => { notifyToast(msg, ok);  };

    const load = useCallback(async () => {
        try {
            const [res, fw] = await Promise.all([getWarpRegions(), getWarpFirewallSettings()]);
            if (res.success) setRegions(res.regions || []);
            if (fw.success) {
                setFwPorts(fw.settings.allowedPorts);
                const stored = fw.settings.tunnelSubnets || '';
                const hint = fw.suggestedTunnelSubnets ?? null;
                setSubnetHint(hint);
                // Pre-fill the detected value when nothing is saved: the field is
                // then ready to save instead of empty. A stored value always wins.
                setFwSubnets(stored || hint?.suggested || '');
                setFwLoaded(true);
            }
        } catch {
            showToast('Failed to load Warp settings', false);
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => { load(); }, [load]);

    const addRegion = async () => {
        const region = newRegion.region.trim();
        const subnet = newRegion.subnet.trim();
        if (!region || !subnet) { showToast('Region id and subnet are required.', false); return; }
        setAddingRegion(true);
        const res = await upsertWarpRegion({ region, subnet, enabled: true });
        setAddingRegion(false);
        if (res.success) { setNewRegion({ region: '', subnet: '' }); showToast('Region saved.'); load(); }
        else showToast(res.message || res.error || 'Save failed.', false);
    };

    const saveRegion = async (region: string, subnet: string, enabled: boolean) => {
        const res = await upsertWarpRegion({ region, subnet, enabled });
        if (res.success) { showToast('Region updated.'); load(); }
        else showToast(res.message || res.error || 'Save failed.', false);
    };

    const delRegion = async (region: string) => {
        // warp_leaders.region is REFERENCES warp_regions(region) ON DELETE
        // CASCADE, so this one click takes every leader endpoint in the region
        // with it - which the button's own title already claimed and nothing
        // stopped. There is no undo and the endpoints are not recoverable from
        // the panel; they have to be typed back in. Every comparable delete in
        // the settings area asks first (~19 confirmDialog call sites); these two
        // were missed.
        if (!(await confirmDialog({
            title: 'Delete region',
            message: `Delete region "${region}"? Every leader endpoint in it is deleted with it and cannot be restored from here.`,
        }))) return;
        const res = await deleteWarpRegion(region);
        if (res.success) { showToast('Region deleted.'); load(); }
        else showToast(res.message || res.error || 'Delete failed.', false);
    };

    const saveLeader = async (leaderId: string, region: string, endpoint: string, enabled: boolean) => {
        const res = await upsertWarpLeader({ leaderId, region, endpoint, enabled });
        if (res.success) { showToast('Leader saved.'); load(); }
        else showToast(res.message || res.error || 'Save failed.', false);
    };

    const delLeader = async (leaderId: string) => {
        if (!(await confirmDialog({
            title: 'Delete leader',
            message: `Delete leader "${leaderId}"? Its endpoint is removed from the region and has to be re-entered by hand.`,
        }))) return;
        const res = await deleteWarpLeader(leaderId);
        if (res.success) { showToast('Leader deleted.'); load(); }
        else showToast(res.message || res.error || 'Delete failed.', false);
    };

    const saveFw = async () => {
        setSavingFw(true);
        const res = await saveWarpFirewallSettings({ allowedPorts: fwPorts.trim(), tunnelSubnets: fwSubnets.trim() });
        setSavingFw(false);
        if (res.success) {
            if (res.settings) { setFwPorts(res.settings.allowedPorts); setFwSubnets(res.settings.tunnelSubnets || ''); }
            showToast('Spoke firewall allowlist saved.');
        } else {
            showToast(res.message || res.error || 'Save failed.', false);
        }
    };

    if (loading) return <div className="space-y-6"><div className="h-8 w-40 bg-(--base-03) rounded animate-pulse" /><div className="h-64 bg-(--base-02) rounded animate-pulse" /></div>;

    return (
        <div className="space-y-6">
            <div className={`flex items-start gap-3 p-3 rounded-md border ${gateOpen ? 'border-(--success)/30 bg-(--success)/5' : 'border-(--warning)/40 bg-(--warning)/5'}`}>
                <AlertTriangle size={15} className={`mt-0.5 shrink-0 ${gateOpen ? 'text-(--success-light)' : 'text-(--warning-light)'}`} />
                <p className="text-xs text-(--base-07)">
                    {gateOpen
                        ? 'Gateway routing + Beam files are active - external/home nodes are supported. Each region is one WG identity (subnet + key); its leaders are interchangeable endpoints clients fail over between without changing IP.'
                        : 'External nodes require routing_mode=gateway or both, AND file_access=beam (set in the Gateway tab). Until then, connecting external nodes is disabled - their servers could not receive player traffic or file access.'}
                </p>
            </div>

            <p className="text-xs text-(--base-06)">
                External nodes are added, deployed and given new keys under{' '}
                <a href="/settings/nodes?tab=external" className="text-(--accent-light) hover:underline rounded-sm focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">Settings -&gt; Nodes -&gt; External nodes</a>.
            </p>

            {/* Regions + leaders */}
            <div className="card p-5 space-y-4">
                <h3 className="text-sm font-display font-semibold text-(--accent-light) flex items-center gap-2"><Network size={15} /> Regions & Hubs</h3>

                {regions.length === 0 && (
                    <p className="text-xs text-(--base-06)">No regions yet. Add one below (e.g. <span className="font-mono">eu-central</span> = <span className="font-mono">10.99.1.0/24</span>).</p>
                )}

                <div className="space-y-3">
                    {regions.map(r => (
                        <RegionCard key={r.region} region={r} onSaveRegion={saveRegion} onDeleteRegion={delRegion} onSaveLeader={saveLeader} onDeleteLeader={delLeader} />
                    ))}
                </div>

                {/* Add region */}
                <div className="flex flex-col md:flex-row gap-3 md:items-end border-t border-(--base-04) pt-4">
                    <div className="flex-1 flex flex-col gap-[5px]">
                        <label className="input-label">Region id</label>
                        <input className="input-field" value={newRegion.region} onChange={e => setNewRegion(s => ({ ...s, region: e.target.value }))} placeholder="eu-central" />
                    </div>
                    <div className="flex-1 flex flex-col gap-[5px]">
                        <label className="input-label">Subnet (CIDR)</label>
                        <input className="input-field" value={newRegion.subnet} onChange={e => setNewRegion(s => ({ ...s, subnet: e.target.value }))} placeholder="10.99.1.0/24" />
                    </div>
                    <button onClick={addRegion} disabled={addingRegion} className="btn btn-primary disabled:opacity-40">
                        <Plus size={14} /> Add region
                    </button>
                </div>
                <p className="text-xs text-(--base-06)">Subnets must be disjoint from your DC + overlay ranges and from each other. The first host (.1) is reserved for the region&apos;s leaders.</p>
            </div>

            {/* Overlay segmentation: spoke firewall allowlist */}
            <div className="card p-5 space-y-4">
                <h3 className="text-sm font-display font-semibold text-(--accent-light) flex items-center gap-2"><Shield size={15} /> Overlay Segmentation</h3>
                <p className="text-xs text-(--base-07)">
                    Each region&apos;s leader firewalls spoke (external/home node) traffic. Spokes are always isolated
                    from one another and may reach ONLY these destination TCP ports. Everything else (Postgres, the Hub,
                    Core REST, other tenants&apos; servers and nodes) is denied.
                </p>
                <div>
                    <label className="input-label">Allowed spoke ports (comma-separated)</label>
                    <input
                        className="input-field input-mono"
                        value={fwPorts}
                        onChange={e => setFwPorts(e.target.value)}
                        placeholder="6379,25560,25551,25501"
                        disabled={!fwLoaded}
                    />
                    <p className="text-xs text-(--base-06) mt-1">
                        Defaults: <span className="font-mono">6379</span> Redis, <span className="font-mono">25560</span> edge tunnel, <span className="font-mono">25551</span> beam relay, <span className="font-mono">25501</span> Core gRPC.
                    </p>
                </div>
                <div>
                    <label className="input-label">Overlay CIDR(s) clients route through the tunnel</label>
                    <input
                        className="input-field input-mono"
                        value={fwSubnets}
                        onChange={e => setFwSubnets(e.target.value)}
                        placeholder="10.20.0.0/16"
                        disabled={!fwLoaded}
                    />
                    <p className="text-xs text-(--base-06) mt-1">
                        The network where Redis and Core gRPC live. Every deploy snippet is handed out with this value
                        filled in, so nobody has to look it up. Comma-separate several ranges. Must be a network address
                        (<span className="font-mono">10.20.0.0/16</span>, not <span className="font-mono">10.20.0.5/16</span>).
                    </p>
                    {subnetHint && subnetHint.suggested !== '' && (
                        <p className="text-xs text-(--base-06) mt-1">
                            Pre-filled with <span className="font-mono text-(--accent-light)">{subnetHint.suggested}</span>,
                            {' '}{subnetHint.source}. Save to keep it.
                            {subnetHint.candidates.length > 1 && (
                                <> Core is attached to several networks; the others are{' '}
                                    {subnetHint.candidates.slice(1).map((c, i) => (
                                        <span key={c}>
                                            {i > 0 && ', '}
                                            <button
                                                type="button"
                                                onClick={() => setFwSubnets(c)}
                                                className="font-mono text-(--accent-light) hover:underline"
                                            >{c}</button>
                                        </span>
                                    ))}.
                                </>
                            )}
                        </p>
                    )}
                    {subnetHint && subnetHint.suggested === '' && (
                        <p className="text-xs text-(--warning-light) mt-1">
                            Could not be detected on this host - enter it manually. It is the subnet of the Docker
                            network Core and Redis share.
                        </p>
                    )}
                </div>
                <div className="flex items-start gap-2 p-3 rounded-md border border-(--warning)/40 bg-(--warning)/5">
                    <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                    <p className="text-xs text-(--base-07)">
                        Adding ports widens what an untrusted external host can reach inside your network. Only add a port
                        you fully understand. Peer isolation (spoke to spoke) is always enforced and cannot be changed here.
                    </p>
                </div>
                <button onClick={saveFw} disabled={savingFw || !fwLoaded} className="btn btn-primary disabled:opacity-40">
                    Save allowlist
                </button>
            </div>


        </div>
    );
}

function RegionCard({ region, onSaveRegion, onDeleteRegion, onSaveLeader, onDeleteLeader }: {
    region: WarpRegionView;
    onSaveRegion: (region: string, subnet: string, enabled: boolean) => void;
    onDeleteRegion: (region: string) => void;
    onSaveLeader: (leaderId: string, region: string, endpoint: string, enabled: boolean) => void;
    onDeleteLeader: (leaderId: string) => void;
}) {
    const [subnet, setSubnet] = useState(region.subnet);
    const [newLeader, setNewLeader] = useState({ leaderId: '', endpoint: '' });
    const leaders = region.leaders || [];
    const subnetDirty = subnet.trim() !== region.subnet;

    return (
        <div className="rounded-md border border-(--base-04) p-4 space-y-3">
            <div className="flex flex-wrap items-center gap-3">
                <span className="font-mono text-sm text-(--base-09)">{region.region}</span>
                <span className="badge badge-neutral">{region.peerCount} peer{region.peerCount === 1 ? '' : 's'}</span>
                <span className={`badge ${region.enabled ? 'badge-success' : 'badge-neutral'}`}>{region.enabled ? 'enabled' : 'disabled'}</span>
                <div className="ml-auto flex items-center gap-2">
                    <button
                        type="button"
                        onClick={() => onSaveRegion(region.region, region.subnet, !region.enabled)}
                        className="btn btn-secondary btn-sm"
                    >
                        {region.enabled ? 'Disable' : 'Enable'}
                    </button>
                    <button type="button" onClick={() => onDeleteRegion(region.region)} className="btn btn-danger btn-sm" title="Delete region (and its leaders)">
                        <Trash2 size={13} />
                    </button>
                </div>
            </div>

            <div className="flex items-end gap-3">
                <div className="flex-1 flex flex-col gap-[5px]">
                    <label className="input-label">Subnet</label>
                    <input className="input-field input-mono" value={subnet} onChange={e => setSubnet(e.target.value)} />
                </div>
                <button type="button" onClick={() => onSaveRegion(region.region, subnet.trim(), region.enabled)} disabled={!subnetDirty} className="btn btn-secondary btn-sm disabled:opacity-40">
                    Save subnet
                </button>
            </div>

            <div className="space-y-2">
                <label className="input-label flex items-center gap-1.5"><Server size={12} /> Leaders</label>
                {leaders.length === 0 && <p className="text-xs text-(--warning-light)">No leader endpoints yet, so enrolling into this region is refused. A running leader registers itself within a minute; add one by hand only if it cannot detect its own public address.</p>}
                {leaders.map(l => (
                    <div key={l.leaderId} className="flex flex-wrap items-center gap-2 rounded-md border border-(--base-03) px-3 py-2">
                        <Circle size={9} className={l.alive ? 'text-(--success-light) fill-current' : 'text-(--base-05) fill-current'} aria-label={l.alive ? 'live' : 'no heartbeat'} />
                        <span className="font-mono text-xs text-(--base-08)">{l.leaderId}</span>
                        <span className="font-mono text-xs text-(--base-06)">{l.endpoint}</span>
                        <div className="ml-auto flex items-center gap-2">
                            <button type="button" onClick={() => onSaveLeader(l.leaderId, region.region, l.endpoint, !l.enabled)} className="btn btn-secondary btn-sm">
                                {l.enabled ? 'Disable' : 'Enable'}
                            </button>
                            <button type="button" onClick={() => onDeleteLeader(l.leaderId)} className="btn btn-danger btn-sm" title="Delete leader">
                                <Trash2 size={12} />
                            </button>
                        </div>
                    </div>
                ))}
                <div className="flex flex-col md:flex-row gap-3 md:items-end">
                    <div className="flex-1 flex flex-col gap-[5px]">
                        <label className="input-label">Leader id</label>
                        <input className="input-field" value={newLeader.leaderId} onChange={e => setNewLeader(s => ({ ...s, leaderId: e.target.value }))} placeholder={`${region.region}-01`} />
                    </div>
                    <div className="flex-1 flex flex-col gap-[5px]">
                        <label className="input-label">Endpoint (host:port)</label>
                        <input className="input-field" value={newLeader.endpoint} onChange={e => setNewLeader(s => ({ ...s, endpoint: e.target.value }))} placeholder="vpn-eu1.example.com:25599" />
                    </div>
                    <button
                        type="button"
                        onClick={() => {
                            if (!newLeader.leaderId.trim() || !newLeader.endpoint.trim()) return;
                            onSaveLeader(newLeader.leaderId.trim(), region.region, newLeader.endpoint.trim(), true);
                            setNewLeader({ leaderId: '', endpoint: '' });
                        }}
                        className="btn btn-secondary btn-sm"
                    >
                        <Plus size={12} /> Add leader
                    </button>
                </div>
            </div>
        </div>
    );
}

