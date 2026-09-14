'use client';

import { useCallback, useEffect, useState } from 'react';
import { Copy, AlertTriangle, EyeOff, Plus, Network, Trash2, Server, Circle, Shield, Check, KeyRound, Terminal, RefreshCw, Ban } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import {
    getWarpRegions, upsertWarpRegion, deleteWarpRegion,
    upsertWarpLeader, deleteWarpLeader, mintExternalNodeKey,
    getWarpFirewallSettings, saveWarpFirewallSettings,
    listWarpKeys, revokeWarpKey, deleteWarpKey,
    type WarpRegionView, type WarpKeyView,
} from '@/lib/api/types';
import {
    nodeCompose, deployCli, deployIntro, composeFileName,
    DEPLOY_PORTAINER_NOTE, nodeIdFromLabel, EXTERNAL_NODE_PORTS, kitGrpcTlsFingerprint,
} from '@/lib/warpDeploy';
import type { DeployPlatform } from '@/lib/warpDeploy';
import { getWarpDeployConfig, type WarpDeployConfig } from '@/lib/api/warpDeployConfig';
import { coreOrigin } from '@/lib/api/core';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import HelpTip from '@/components/ui/HelpTip';
import { platformNote } from '@/components/infra/DeployKit';
import { isLocationName } from '@/lib/validation';

const enrollUrl = coreOrigin();

// The same rule Core applies (validate.IsLocationName): the name is slugged into
// the node's NODE_ID by the deploy file.
const EXTERNAL_NAME_RULE = '4 to 20 characters: letters, digits and hyphens, not starting or ending with a hyphen.';

type WarpSubTab = 'settings' | 'nodes';

export default function WarpTab() {
    const { gatewayEnabled, fileAccessMode } = useAppData();
    const [subTab, setSubTab] = useState<WarpSubTab>('settings');
    // The External Nodes sub-tab used to show only with store wiring or the BYON
    // flag, because it minted keys for customer machines as well. It now adds
    // only the platform's own External nodes, and Core deliberately does not
    // gate that on BYON: a self-hoster can run a machine outside their
    // datacenter too. Customer machines and protected addresses stay behind
    // BYON on the customer's own pages, not here.
    //
    // gatewayEnabled is routing gateway OR both, the same test Core's mint and
    // warp enrol apply. Beam file access stays required: without it the
    // servers on such a machine have no file access path.
    const gateOpen = gatewayEnabled && fileAccessMode === 'beam';

    const [regions, setRegions] = useState<WarpRegionView[]>([]);
    const [loading, setLoading] = useState(true);
    const [toast, setToast] = useState<{ msg: string; ok: boolean } | null>(null);

    const [newRegion, setNewRegion] = useState({ region: '', subnet: '' });
    const [addingRegion, setAddingRegion] = useState(false);

    const [extName, setExtName] = useState('');
    const extNameValid = isLocationName(extName.trim());
    const [minting, setMinting] = useState(false);
    // Both secrets of a freshly minted External node, shown once.
    const [revealed, setRevealed] = useState<{
        name: string; nodeId: string; apiKey: string; enrollToken: string; grpcTlsFingerprint: string;
    } | null>(null);
    const [keys, setKeys] = useState<WarpKeyView[]>([]);
    const [keysLoading, setKeysLoading] = useState(true);
    // Deploy instructions for an already-minted key: same panel as the reveal
    // modal, minus the secret, which cannot be shown again.
    const [showDeploy, setShowDeploy] = useState<WarpKeyView | null>(null);

    const [fwPorts, setFwPorts] = useState('');
    const [fwSubnets, setFwSubnets] = useState('');
    // What Core detected, shown when nothing is stored yet so the operator can
    // accept it rather than look the CIDR up. Kept separate from fwSubnets so
    // "detected" and "saved" never look the same.
    const [subnetHint, setSubnetHint] = useState<{ suggested: string; candidates: string[]; source: string } | null>(null);
    const [fwLoaded, setFwLoaded] = useState(false);
    const [savingFw, setSavingFw] = useState(false);
    // Core's overlay-side gRPC and Redis addresses, so the deploy snippet is a
    // copy-paste rather than two values the operator has to go and find.
    const [deployConfig, setDeployConfig] = useState<WarpDeployConfig | null>(null);

    const showToast = (msg: string, ok = true) => { setToast({ msg, ok }); setTimeout(() => setToast(null), 3500); };

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

    const loadKeys = useCallback(async () => {
        try {
            const res = await listWarpKeys();
            if (res.success) setKeys(res.keys || []);
        } catch {
            // Non-fatal: the rest of the tab still works without the inventory.
        } finally {
            setKeysLoading(false);
        }
    }, []);

    useEffect(() => { load(); loadKeys(); }, [load, loadKeys]);

    useEffect(() => {
        let cancelled = false;
        getWarpDeployConfig().then(res => {
            if (!cancelled && res.success && res.config) setDeployConfig(res.config);
        }).catch(() => { /* the snippet falls back to its placeholders */ });
        return () => { cancelled = true; };
    }, []);

    const handleRevokeKey = async (k: WarpKeyView) => {
        const live = k.peers.length;
        const ok = await confirmDialog({
            title: `Revoke "${k.name}"?`,
            message: live > 0
                ? `${live} connected ${live === 1 ? 'node is' : 'nodes are'} using this key. Revoking blocks future enrolments AND disconnects ${live === 1 ? 'it' : 'them'} from the overlay immediately. The key cannot be restored - you would have to mint a new one and redeploy.${unusedTokenNote(k)}`
                : `This key has no connected nodes. Revoking blocks any future enrolment with it. It cannot be restored.${unusedTokenNote(k)}`,
            confirmLabel: 'Revoke',
            destructive: true,
        });
        if (!ok) return;
        const res = await revokeWarpKey(k.id);
        if (res.success) {
            showToast(res.disconnected ? `Revoked. ${res.disconnected} peer(s) disconnected.` : 'Key revoked.');
            loadKeys();
        } else {
            showToast(res.message || 'Revoke failed.', false);
        }
    };

    const handleDeleteKey = async (k: WarpKeyView) => {
        const live = k.peers.length;
        const ok = await confirmDialog({
            title: `Delete "${k.name}"?`,
            message: live > 0
                ? `Removes the key and its history for good, and disconnects ${live} connected ${live === 1 ? 'node' : 'nodes'} from the overlay. Revoke instead if you want the record kept.${unusedTokenNote(k)}`
                : `Removes the key and its history for good. Revoke instead if you want the record kept.${unusedTokenNote(k)}`,
            confirmLabel: 'Delete',
            destructive: true,
        });
        if (!ok) return;
        const res = await deleteWarpKey(k.id);
        if (res.success) {
            showToast(res.disconnected ? `Deleted. ${res.disconnected} peer(s) disconnected.` : 'Key deleted.');
            loadKeys();
        } else {
            showToast(res.message || 'Delete failed.', false);
        }
    };

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

    const handleMint = async () => {
        const name = extName.trim();
        if (!isLocationName(name)) { showToast(`Location name: ${EXTERNAL_NAME_RULE}`, false); return; }
        setMinting(true);
        try {
            const res = await mintExternalNodeKey(name);
            if (res.success && res.warp_key && res.enroll_token && res.node_id) {
                setRevealed({
                    name, nodeId: res.node_id, apiKey: res.warp_key, enrollToken: res.enroll_token,
                    grpcTlsFingerprint: res.grpc_tls_fingerprint ?? '',
                });
                setExtName('');
                loadKeys();
            } else {
                showToast(res.message || res.error || 'Mint failed', false);
            }
        } catch {
            showToast('Mint failed: Core could not be reached.', false);
        } finally {
            setMinting(false);
        }
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

            <div className="flex gap-1 border-b border-(--base-03)">
                    {([
                        { id: 'settings' as const, label: 'Overlay Settings', icon: Shield },
                        { id: 'nodes' as const, label: 'External Nodes', icon: Server },
                    ]).map(({ id, label, icon: Icon }) => (
                        <button
                            key={id}
                            onClick={() => setSubTab(id)}
                            className={`flex items-center gap-2 px-3 py-2 text-sm font-medium -mb-px border-b-2 transition-colors ${
                                subTab === id
                                    ? 'border-(--accent) text-(--accent-light)'
                                    : 'border-transparent text-(--base-07) hover:text-(--base-09)'
                            }`}
                        >
                            <Icon size={14} />
                            {label}
                        </button>
                    ))}
            </div>

            {subTab === 'settings' && (<>
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

            </>)}

            {subTab === 'nodes' && (<>
            {/* Mint an External node */}
            <div className="card p-5 space-y-4">
                <h3 className="text-sm font-display font-semibold text-(--accent-light) flex items-center gap-2"><Network size={15} /> Connect External Node</h3>
                {/* This card mints exactly one kind of key. It used to mint a
                    plain admin key and offer three deploy files for it, and two
                    of them could never work: link-boot answers only a key made
                    for one machine, and an admin key was never that. */}
                <p className="text-xs text-(--base-06)">
                    An External node is a machine the platform runs outside the datacenter. Minting gives it an
                    overlay key and a single-use enroll token together. The node it enrols belongs to nobody - not
                    to you - and appears under{' '}
                    <a href="/nodes?tab=external" className="text-(--accent-light) hover:underline rounded-sm focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">My infrastructure -&gt; External nodes</a>.
                    The file offered next runs warp, the node and its Link.
                </p>
                <p className="text-xs text-(--base-06)">
                    Customer machines and protected addresses are not set up here, because a key an admin mints
                    can never boot their Link. The customer adds a machine under{' '}
                    <a href="/nodes?tab=machines" className="text-(--accent-light) hover:underline rounded-sm focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">My infrastructure -&gt; Bring your own node</a>{' '}
                    and a protected address under{' '}
                    <a href="/nodes?tab=routes" className="text-(--accent-light) hover:underline rounded-sm focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">My infrastructure -&gt; Protected addresses</a>,
                    and gets the working deploy file there.
                </p>
                <fieldset disabled={!gateOpen} className="space-y-2 disabled:opacity-50">
                    <div className="flex flex-col md:flex-row gap-3 md:items-end">
                        <div className="flex-1 flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="external-node-name">Location name</label>
                            <input
                                id="external-node-name"
                                className="input-field"
                                value={extName}
                                onChange={e => setExtName(e.target.value)}
                                onKeyDown={e => { if (e.key === 'Enter' && extNameValid && !minting) handleMint(); }}
                                placeholder="ext-frankfurt"
                                maxLength={20}
                                aria-invalid={extName !== '' && !extNameValid}
                                aria-describedby="external-node-name-rule"
                            />
                        </div>
                        <button onClick={handleMint} disabled={minting || !gateOpen || !extNameValid} className="btn btn-primary disabled:opacity-40">
                            <Plus size={14} /> {minting ? 'Minting...' : 'Mint External node key'}
                        </button>
                    </div>
                    <p id="external-node-name-rule" className={`text-[11px] ${extName !== '' && !extNameValid ? 'text-(--error-light)' : 'text-(--base-06)'}`}>
                        {EXTERNAL_NAME_RULE} It becomes the node&apos;s NODE_ID in the file.
                    </p>
                </fieldset>
            </div>

            {/* Enrolled external nodes */}
            <div className="card p-5 space-y-4">
                <div className="flex items-center justify-between">
                    <h3 className="text-sm font-display font-semibold text-(--accent-light) flex items-center gap-2">
                        <KeyRound size={15} /> Enrollment Keys &amp; Connected Nodes
                    </h3>
                    <button onClick={loadKeys} className="btn btn-secondary btn-sm" title="Refresh">
                        <RefreshCw size={12} /> Refresh
                    </button>
                </div>

                {keysLoading ? (
                    <p className="text-xs text-(--base-06)">Loading...</p>
                ) : keys.length === 0 ? (
                    <p className="text-xs text-(--base-06)">
                        No enrollment keys yet. Mint one above to connect an external node.
                    </p>
                ) : (
                    <div className="flex flex-col gap-2">
                        {keys.map(k => (
                            <div key={k.id} className={`rounded-md border p-3 ${k.revoked ? 'border-(--base-04) opacity-60' : 'border-(--base-03)'}`}>
                                <div className="flex items-start justify-between gap-3">
                                    <div className="min-w-0">
                                        <div className="flex items-center gap-2 flex-wrap">
                                            <span className="text-sm text-(--base-09) font-medium truncate">{k.name || `key-${k.id}`}</span>
                                            {k.revoked
                                                ? <span className="badge badge-error">Revoked</span>
                                                : k.peers.length > 0
                                                    ? <span className="badge badge-success">{k.peers.length} connected</span>
                                                    : <span className="badge badge-neutral">Not used yet</span>}
                                            {isExternalNodeKey(k)
                                                ? <span className="badge badge-accent">External node</span>
                                                : (
                                                    <span className="inline-flex items-center gap-1">
                                                        <span className="badge badge-neutral">Legacy key</span>
                                                        <HelpTip label="About legacy keys">
                                                            <p className="mb-2">
                                                                Minted before External node keys existed. It is not made for
                                                                one machine, so it can <strong>never boot a Link</strong>, and a
                                                                node deployed with it cannot be reached by players.
                                                            </p>
                                                            <p>
                                                                Add a new External node for that machine instead, then revoke
                                                                this key.
                                                            </p>
                                                        </HelpTip>
                                                    </span>
                                                )}
                                            <span className="badge badge-neutral">{k.policy === 'fixed' ? 'Fixed (1)' : `General (max ${k.max_conns})`}</span>
                                            {k.region && <span className="badge badge-accent">{k.region}</span>}
                                        </div>
                                        <p className="text-xs text-(--base-06) mt-1">
                                            Created {new Date(k.created_at).toLocaleString()}
                                            {k.region ? '' : ' - region assigned at enrolment'}
                                        </p>
                                    </div>
                                    <div className="flex items-center gap-2 shrink-0">
                                        <button onClick={() => setShowDeploy(k)} className="btn btn-secondary btn-sm" title="Show deploy instructions">
                                            <Terminal size={12} /> Deploy
                                        </button>
                                        {!k.revoked && (
                                            <button onClick={() => handleRevokeKey(k)} className="btn btn-secondary btn-sm" title="Block future enrolments and disconnect its nodes; keeps the record">
                                                <Ban size={12} /> Revoke
                                            </button>
                                        )}
                                        <button onClick={() => handleDeleteKey(k)} className="btn btn-danger btn-sm" title="Remove the key and its history for good">
                                            <Trash2 size={12} />
                                        </button>
                                    </div>
                                </div>

                                {k.peers.length > 0 && (
                                    <div className="mt-3 border-t border-(--base-03) pt-2 flex flex-col gap-1">
                                        {k.peers.map(p => (
                                            <div key={p.pubkey} className="flex items-center gap-2 text-xs flex-wrap">
                                                <Circle size={7} className="fill-(--success-light) text-(--success-light) shrink-0" />
                                                <code className="font-mono text-(--base-09)">{p.wg_ip}</code>
                                                <span className="text-(--base-06)">{p.region}</span>
                                                {p.assigned_leader && <span className="text-(--base-06)">via {p.assigned_leader}</span>}
                                                <code className="font-mono text-(--base-05) truncate max-w-[14rem]" title={p.pubkey}>{p.pubkey}</code>
                                            </div>
                                        ))}
                                    </div>
                                )}
                            </div>
                        ))}
                    </div>
                )}
            </div>
            </>)}

            {(revealed || showDeploy) && (
                <DeployModal
                    name={revealed ? revealed.name : (showDeploy!.name || `key-${showDeploy!.id}`)}
                    keyNodeId={revealed ? revealed.nodeId : showDeploy!.node_id}
                    apiKey={revealed ? revealed.apiKey : null}
                    enrollToken={revealed ? revealed.enrollToken : null}
                    grpcTlsFingerprint={revealed ? revealed.grpcTlsFingerprint : undefined}
                    enrollUrl={enrollUrl}
                    tunnelSubnets={fwSubnets}
                    config={deployConfig}
                    onClose={() => { setRevealed(null); setShowDeploy(null); }}
                    showToast={showToast}
                />
            )}

            {toast && <div className={`fixed bottom-4 right-4 px-4 py-2 rounded-md text-sm font-medium ${toast.ok ? 'bg-(--success)/20 text-(--success-light) border border-(--success)/40' : 'bg-(--error)/20 text-(--error-light) border border-(--error)/40'}`}>{toast.msg}</div>}
        </div>
    );
}

// An External node key carries a node- identity; admin keys could not have one
// any other way (Core refuses the prefix on the plain mint).
function isExternalNodeKey(k: WarpKeyView): boolean {
    return (k.node_id || '').startsWith('node-');
}

// Core accepts an External node's enroll token only while its key is live, so
// revoking or deleting the key retires a token the machine has not used yet.
function unusedTokenNote(k: WarpKeyView): string {
    return isExternalNodeKey(k) ? ' Its enroll token stops working too, if the machine has not used it yet.' : '';
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

/**
 * DeployModal is the "how do I actually connect this machine" panel.
 *
 * Two entry points, one component: right after minting (the key and the enroll
 * token present, each shown once and never again) and later from the inventory
 * (both null, because only hashes are stored). Everything except the secrets is
 * reproducible, so the second case still gives a complete stack with
 * placeholders where they go.
 *
 * It is locked to one target, the External node. It used to offer BYON and
 * route-only files for the same admin key as well, and neither could work:
 * link-boot answers only a key made for one machine, which an admin key never
 * was. Which file a key gets is decided by the key itself - a node- identity is
 * an External node key and gets its Link, anything else is a key minted before
 * those existed and gets a file that says it cannot have one.
 */
function DeployModal({ name, keyNodeId, apiKey, enrollToken, grpcTlsFingerprint, enrollUrl, tunnelSubnets, config, onClose, showToast }: {
    name: string;
    /** The key's node_id. "node-..." is an External node key; "" or anything else a legacy admin key. */
    keyNodeId: string;
    apiKey: string | null;
    /** The platform enroll token minted with the key; null once it can no longer be shown. */
    enrollToken: string | null;
    /** From the mint answer. undefined falls back to Core's deploy config. */
    grpcTlsFingerprint?: string;
    enrollUrl: string;
    /** From the Overlay Segmentation setting; "" leaves a placeholder in the snippet. */
    tunnelSubnets: string;
    /** Overlay addresses resolved by Core; null while loading or undetectable. */
    config: WarpDeployConfig | null;
    onClose: () => void;
    showToast: (msg: string, ok?: boolean) => void;
}) {
    const external = keyNodeId.startsWith('node-');
    const [copied, setCopied] = useState<string | null>(null);
    const [platform, setPlatform] = useState<DeployPlatform>('linux');

    const compose = nodeCompose({
        apiKey: apiKey ?? '<your-warp-key>',
        enrollUrl,
        // The saved setting wins; Core's detected value is the fallback, so a
        // snippet is complete even before anyone visits Overlay Segmentation.
        tunnelSubnets: tunnelSubnets || config?.tunnelSubnets || '',
        grpcTlsFingerprint: kitGrpcTlsFingerprint(grpcTlsFingerprint, config),
        nodeId: nodeIdFromLabel(name),
        nodeEnrollToken: enrollToken ?? undefined,
        platform,
        linkBesideNode: external,
        legacyAdminKey: !external,
        // Every key in this dialog is the platform's, a legacy one included:
        // the machine is not the reader's own.
        externalNode: true,
    });
    const cli = deployCli('node', true);

    const copy = (what: string, text: string) => {
        navigator.clipboard.writeText(text).then(() => {
            setCopied(what);
            setTimeout(() => setCopied(null), 1600);
            showToast('Copied.');
        });
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <div className="modal-panel max-w-3xl" onClick={e => e.stopPropagation()}>
                <div className="modal-header">
                    <h3 className="modal-title text-(--accent-light)">{name} - deploy</h3>
                </div>
                <div className="modal-body space-y-4 max-h-[70vh] overflow-y-auto">
                    {apiKey ? (
                        <div className="flex items-start gap-2 p-2.5 rounded-md bg-(--warning)/5 border border-(--warning)/20">
                            <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <div className="text-xs text-(--base-07) space-y-1.5 min-w-0 flex-1">
                                <p>
                                    The key and the enroll token are shown <strong>once</strong>. Both are stored only as
                                    hashes, so they cannot be displayed again. The file below already contains them.
                                </p>
                                <div className="flex items-center gap-2">
                                    <code className="font-mono text-xs bg-(--base-02) px-2 py-1 rounded truncate flex-1">{apiKey}</code>
                                    <button onClick={() => copy('key', apiKey)} className="btn btn-secondary btn-sm shrink-0">
                                        {copied === 'key' ? <Check size={12} /> : <Copy size={12} />} Copy key
                                    </button>
                                </div>
                                {enrollToken && (
                                    <div className="flex items-center gap-2">
                                        <code className="font-mono text-xs bg-(--base-02) px-2 py-1 rounded truncate flex-1">{enrollToken}</code>
                                        <button onClick={() => copy('token', enrollToken)} className="btn btn-secondary btn-sm shrink-0">
                                            {copied === 'token' ? <Check size={12} /> : <Copy size={12} />} Copy token
                                        </button>
                                    </div>
                                )}
                            </div>
                        </div>
                    ) : external ? (
                        <p className="text-xs text-(--base-06)">
                            The key and its enroll token cannot be shown again - only their hashes are stored. Paste
                            what you saved at mint time where the file says <code className="font-mono">&lt;your-warp-key&gt;</code>{' '}
                            and <code className="font-mono">&lt;enroll-token-from-panel&gt;</code>. A machine that has
                            already enrolled no longer needs the token. If it has not and the token is lost, revoke this
                            key and mint a new one: revoking retires the unused token with it.
                        </p>
                    ) : (
                        <div className="flex items-start gap-2 p-2.5 rounded-md bg-(--warning)/5 border border-(--warning)/40">
                            <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <p className="text-xs text-(--base-07)">
                                This key was minted before External node keys existed. It is not made for one machine,
                                so it can never boot a Link, and the file below runs none - nobody could reach the
                                servers on that machine. Mint a new External node key for it instead. The key itself
                                cannot be shown again either; the file has a placeholder where it goes.
                            </p>
                        </div>
                    )}

                    {external && (
                        <p className="text-xs text-(--base-06)">
                            A machine the platform runs outside the datacenter. warp joins the overlay first; the node
                            then enrols with the token in this file and belongs to nobody, and the link service carries
                            players to its servers once the node has enrolled - until then it waits and retries. The
                            machine never holds CLUSTER_SECRET.
                        </p>
                    )}

                    <div className="space-y-1">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                            <label className="font-mono text-sm font-medium text-(--base-09)">{composeFileName('node')}</label>
                            <div className="flex items-center gap-1" role="group" aria-label="Target machine">
                                {(['linux', 'windows'] as const).map((p) => (
                                    <button
                                        key={p}
                                        type="button"
                                        onClick={() => setPlatform(p)}
                                        aria-pressed={platform === p}
                                        className={`rounded-md px-2 py-0.5 text-xs transition-colors focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] ${platform === p ? 'bg-(--accent) text-(--base-00)' : 'bg-(--base-02) text-(--base-07) hover:bg-(--base-03) hover:text-(--base-09)'}`}
                                    >
                                        {p === 'linux' ? 'Linux' : 'Docker Desktop'}
                                    </button>
                                ))}
                            </div>
                            <button onClick={() => copy('compose', compose)} className="btn btn-secondary btn-sm">
                                {copied === 'compose' ? <Check size={12} /> : <Copy size={12} />} Copy
                            </button>
                        </div>
                        <p className="text-xs text-(--base-06)">{platformNote('node', platform)}</p>
                        <pre className="p-3 rounded-md bg-(--base-02) border border-(--base-04) font-mono text-xs whitespace-pre overflow-x-auto">{compose}</pre>
                    </div>

                    <div className="space-y-1">
                        <div className="flex items-center justify-between">
                            <label className="font-mono text-sm font-medium text-(--base-09)">Commands</label>
                            <button onClick={() => copy('cli', cli)} className="btn btn-secondary btn-sm">
                                {copied === 'cli' ? <Check size={12} /> : <Copy size={12} />} Copy
                            </button>
                        </div>
                        <p className="text-xs text-(--base-07)">{deployIntro('node', platform)}</p>
                        <pre className="p-3 rounded-md bg-(--base-02) border border-(--base-04) font-mono text-xs whitespace-pre overflow-x-auto">{cli}</pre>
                        <p className="text-xs text-(--base-06)">{DEPLOY_PORTAINER_NOTE}</p>
                    </div>

                    <div className="p-2.5 rounded-md bg-(--base-02) border border-(--base-04) space-y-1.5">
                        <p className="text-xs text-(--base-07)">
                            <strong>This machine will bind these ports.</strong> <code className="font-mono">NODE_EXTERNAL</code> only
                            changes the advertised routing and file-access mode - it does not close listeners.
                            Firewall them if the host is exposed.
                        </p>
                        {EXTERNAL_NODE_PORTS.map(p => (
                            <div key={p.port} className="flex items-center gap-2 text-xs">
                                <code className="font-mono text-(--accent-light)">{p.port}</code>
                                <span className="text-(--base-08)">{p.what}</span>
                                <span className="text-(--base-06)">{p.note}</span>
                            </div>
                        ))}
                    </div>
                </div>
                <div className="modal-footer">
                    <button onClick={onClose} className="btn btn-primary"><EyeOff size={12} /> Done</button>
                </div>
            </div>
        </div>
    );
}
