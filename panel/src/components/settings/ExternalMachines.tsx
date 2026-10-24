'use client';

import { useCallback, useEffect, useState } from 'react';
import { Copy, AlertTriangle, EyeOff, Plus, Network, Trash2, Circle, Check, KeyRound, Terminal, RefreshCw, Ban, RotateCw } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import {
    mintExternalNodeKey, getWarpFirewallSettings, listWarpKeys, revokeWarpKey, deleteWarpKey, rollExternalNodeKey,
    type WarpKeyView, type Node,
} from '@/lib/api/types';
import {
    nodeCompose, deployCli, redeployCli, deployIntro, composeFileName,
    DEPLOY_PORTAINER_NOTE, EXTERNAL_NODE_PORTS, kitInput, watchtowerNote,
} from '@/lib/warpDeploy';
import { getWarpDeployConfig, type WarpDeployConfig } from '@/lib/api/warpDeployConfig';
import { coreOrigin } from '@/lib/api/core';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import HelpTip from '@/components/ui/HelpTip';
import { toast } from '@/components/ui/Toast';
import { platformNote, useDeployPlatform } from '@/components/infra/DeployKit';
import { isLocationName } from '@/lib/validation';
import ModalPanel from '@/components/ui/ModalPanel';
import { fmtDateTime } from '@/lib/time';

const enrollUrl = coreOrigin();

// The same rule Core applies (validate.IsLocationName): the name is the enroll
// token's label, which becomes the node's name when it enrols.
const EXTERNAL_NAME_RULE = '4 to 20 characters: letters, digits and hyphens, not starting or ending with a hyphen.';

/**
 * Adding an External node, its deploy file and a new key for it. This used to
 * live under Settings -> Warp while the machines it adds were listed here and
 * under My infrastructure, three places for one thing.
 */
export default function ExternalMachines({ nodes }: { nodes: Node[] }) {
    const { gatewayEnabled, fileAccessMode } = useAppData();
    // gatewayEnabled is routing gateway OR both, the same test Core's mint and
    // warp enrol apply. Beam file access stays required: without it the
    // servers on such a machine have no file access path.
    const gateOpen = gatewayEnabled && fileAccessMode === 'beam';

    const [extName, setExtName] = useState('');
    const extNameValid = isLocationName(extName.trim());
    const [minting, setMinting] = useState(false);
    const [keys, setKeys] = useState<WarpKeyView[]>([]);
    const [keysLoading, setKeysLoading] = useState(true);
    const [rollingId, setRollingId] = useState<number | null>(null);
    // What the deploy dialog shows: a fresh mint (key + token, once), a rolled
    // key (once), or an existing key's file with placeholders.
    const [deploy, setDeploy] = useState<{
        key: Pick<WarpKeyView, 'name' | 'node_id' | 'bound_node_id'>;
        apiKey: string | null; enrollToken: string | null; grpcTlsFingerprint?: string; rolled?: boolean;
    } | null>(null);
    const [tunnelSubnets, setTunnelSubnets] = useState('');
    const [deployConfig, setDeployConfig] = useState<WarpDeployConfig | null>(null);

    const loadKeys = useCallback(async () => {
        try {
            const res = await listWarpKeys();
            if (res.success) setKeys(res.keys || []);
        } catch {
            // Non-fatal: the node list below still works without the keys.
        } finally {
            setKeysLoading(false);
        }
    }, []);

    useEffect(() => {
        loadKeys();
        let cancelled = false;
        getWarpDeployConfig().then(res => {
            if (!cancelled && res.success && res.config) setDeployConfig(res.config);
        }).catch(() => { /* the file falls back to its placeholders */ });
        // The saved overlay segmentation; Core's detected value is the fallback.
        getWarpFirewallSettings().then(res => {
            if (!cancelled && res.success) setTunnelSubnets(res.settings.tunnelSubnets || res.suggestedTunnelSubnets?.suggested || '');
        }).catch(() => { /* deployConfig.tunnelSubnets covers it */ });
        return () => { cancelled = true; };
    }, [loadKeys]);

    const machineOf = (k: WarpKeyView) => nodes.find(n => n.id === k.bound_node_id);

    const handleMint = async () => {
        const name = extName.trim();
        if (!isLocationName(name)) { toast(`Location name: ${EXTERNAL_NAME_RULE}`, false); return; }
        setMinting(true);
        try {
            const res = await mintExternalNodeKey(name);
            if (res.success && res.warp_key && res.enroll_token && res.node_id) {
                setDeploy({
                    key: { name, node_id: res.node_id },
                    apiKey: res.warp_key, enrollToken: res.enroll_token,
                    grpcTlsFingerprint: res.grpc_tls_fingerprint ?? '',
                });
                setExtName('');
                loadKeys();
            } else {
                toast(res.message || res.error || 'Adding the machine failed.', false);
            }
        } catch {
            toast('Adding the machine failed: Core could not be reached.', false);
        } finally {
            setMinting(false);
        }
    };

    const handleRoll = async (k: WarpKeyView) => {
        const ok = await confirmDialog({
            title: `New key for "${k.name}"?`,
            message: 'You get a new overlay key, shown once. The current key keeps working for one more hour (a key replaced before that stops at once), so redeploy within it. '
                + 'Redeploy warp and the node only, with the command shown: the node then moves the link onto the new key itself, starting the new link before the old one stops. '
                + 'Players stay connected and new players can join throughout. A plain "docker compose up -d" would stop the link first, and nobody new could join until it had drained.',
            confirmLabel: 'Get a new key',
            destructive: false,
        });
        if (!ok) return;
        setRollingId(k.id);
        try {
            const res = await rollExternalNodeKey(k.id);
            if (res.success && res.warp_key) {
                setDeploy({ key: k, apiKey: res.warp_key, enrollToken: null, rolled: true });
            } else {
                toast(res.message || res.error || 'Rolling the key failed.', false);
            }
        } catch {
            toast('Rolling the key failed: Core could not be reached.', false);
        } finally {
            setRollingId(null);
        }
    };

    const handleRevokeKey = async (k: WarpKeyView) => {
        const live = k.peers.length;
        const ok = await confirmDialog({
            title: `Revoke "${k.name}"?`,
            message: live > 0
                ? `${live} connected ${live === 1 ? 'node is' : 'nodes are'} using this key. Revoking blocks future enrolments AND disconnects ${live === 1 ? 'it' : 'them'} from the overlay immediately. The key cannot be restored - you would have to add the machine again and redeploy.${unusedTokenNote(k)}`
                : `This key has no connected nodes. Revoking blocks any future enrolment with it. It cannot be restored.${unusedTokenNote(k)}`,
            confirmLabel: 'Revoke',
            destructive: true,
        });
        if (!ok) return;
        const res = await revokeWarpKey(k.id);
        if (res.success) {
            toast(res.disconnected ? `Revoked. ${res.disconnected} peer(s) disconnected.` : 'Key revoked.');
            loadKeys();
        } else {
            toast(res.message || 'Revoke failed.', false);
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
            toast(res.disconnected ? `Deleted. ${res.disconnected} peer(s) disconnected.` : 'Key deleted.');
            loadKeys();
        } else {
            toast(res.message || 'Delete failed.', false);
        }
    };

    return (
        <div className="space-y-4">
            <div className="card p-5 space-y-4">
                <h3 className="text-sm font-display font-semibold text-(--accent-light) flex items-center gap-2"><Network size={15} /> Add a machine</h3>
                <p className="text-xs text-(--base-06)">
                    An External node is a machine the platform runs outside the datacenter. Adding one gives it an
                    overlay key and a single-use enroll token together, with a deploy file that runs warp, the node and
                    its Link. The node it enrols belongs to nobody - not to you - and appears in the list below.
                </p>
                <p className="text-xs text-(--base-06)">
                    Customer machines and protected addresses are not set up here, because a key an admin mints
                    can never boot their Link. The customer adds a machine under{' '}
                    <a href="/nodes?tab=machines" className="text-(--accent-light) hover:underline rounded-sm focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">My infrastructure -&gt; Bring your own node</a>{' '}
                    and gets the working deploy file there.
                </p>
                {!gateOpen && (
                    <p className="text-xs text-(--warning-light)">
                        External nodes need routing gateway or both AND Beam file access (Settings -&gt; Gateway). Until
                        then their servers could not receive players or file access, so adding one is disabled.
                    </p>
                )}
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
                            <Plus size={14} /> {minting ? 'Adding...' : 'Add machine'}
                        </button>
                    </div>
                    <p id="external-node-name-rule" className={`text-[11px] ${extName !== '' && !extNameValid ? 'text-(--error-light)' : 'text-(--base-06)'}`}>
                        {EXTERNAL_NAME_RULE} The machine shows up under this name.
                    </p>
                </fieldset>
            </div>

            <div className="card p-5 space-y-4">
                <div className="flex items-center justify-between">
                    <h3 className="text-sm font-display font-semibold text-(--accent-light) flex items-center gap-2">
                        <KeyRound size={15} /> Machine keys
                    </h3>
                    <button onClick={loadKeys} className="btn btn-secondary btn-sm" title="Refresh">
                        <RefreshCw size={12} /> Refresh
                    </button>
                </div>

                {keysLoading ? (
                    <p className="text-xs text-(--base-06)">Loading...</p>
                ) : keys.length === 0 ? (
                    <p className="text-xs text-(--base-06)">No machines added yet.</p>
                ) : (
                    <div className="flex flex-col gap-2">
                        {keys.map(k => {
                            const machine = machineOf(k);
                            return (
                                <div key={k.id} className={`rounded-md border p-3 ${k.revoked ? 'border-(--base-04) opacity-60' : 'border-(--base-03)'}`}>
                                    <div className="flex flex-wrap items-start justify-between gap-3">
                                        <div className="min-w-0">
                                            <div className="flex items-center gap-2 flex-wrap">
                                                <span className="text-sm text-(--base-09) font-medium truncate">{k.name || `key-${k.id}`}</span>
                                                {k.revoked
                                                    ? <span className="badge badge-error">Revoked</span>
                                                    : k.peers.length > 0
                                                        ? <span className="badge badge-success">{k.peers.length} connected</span>
                                                        : <span className="badge badge-neutral">Not used yet</span>}
                                                {!isExternalNodeKey(k) && (
                                                    <span className="inline-flex items-center gap-1">
                                                        <span className="badge badge-neutral">Legacy key</span>
                                                        <HelpTip label="About legacy keys">
                                                            <p className="mb-2">
                                                                Minted before External node keys existed. It is not made for
                                                                one machine, so it can <strong>never boot a Link</strong>, and a
                                                                node deployed with it cannot be reached by players.
                                                            </p>
                                                            <p>Add the machine again instead, then revoke this key.</p>
                                                        </HelpTip>
                                                    </span>
                                                )}
                                                {k.region && <span className="badge badge-accent">{k.region}</span>}
                                            </div>
                                            <p className="text-xs text-(--base-06) mt-1">
                                                {machine ? <>Node <span className="text-(--base-08)">{machine.displayName || machine.name}</span> - </> : k.bound_node_id ? 'Node removed - ' : 'Not enrolled yet - '}
                                                added {fmtDateTime(k.created_at)}
                                            </p>
                                        </div>
                                        <div className="flex flex-wrap items-center gap-2 shrink-0">
                                            <button onClick={() => setDeploy({ key: k, apiKey: null, enrollToken: null })} className="btn btn-secondary btn-sm" title="The deploy file, with placeholders where the secrets go">
                                                <Terminal size={12} /> Deploy file
                                            </button>
                                            {!k.revoked && isExternalNodeKey(k) && (
                                                <button onClick={() => handleRoll(k)} disabled={rollingId === k.id} className="btn btn-secondary btn-sm disabled:opacity-40" title="A new overlay key, filled into the deploy file; the old one keeps working for an hour">
                                                    <RotateCw size={12} /> {rollingId === k.id ? 'Rolling...' : 'New key'}
                                                </button>
                                            )}
                                            {!k.revoked && (
                                                <button onClick={() => handleRevokeKey(k)} className="btn btn-secondary btn-sm" title="Block future enrolments and disconnect its nodes; keeps the record">
                                                    <Ban size={12} /> Revoke
                                                </button>
                                            )}
                                            <button onClick={() => handleDeleteKey(k)} className="btn btn-danger btn-sm" title="Remove the key and its history for good" aria-label={`Delete ${k.name}`}>
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
                            );
                        })}
                    </div>
                )}
            </div>

            {deploy && (
                <DeployModal
                    name={deploy.key.name || 'machine'}
                    keyNodeId={deploy.key.node_id}
                    enrolled={!!deploy.key.bound_node_id}
                    rolled={!!deploy.rolled}
                    apiKey={deploy.apiKey}
                    enrollToken={deploy.enrollToken}
                    grpcTlsFingerprint={deploy.grpcTlsFingerprint}
                    tunnelSubnets={tunnelSubnets}
                    config={deployConfig}
                    onClose={() => setDeploy(null)}
                />
            )}
        </div>
    );
}

// An External node key carries a node- identity; admin keys could not have one
// any other way (Core refuses the prefix on the plain mint).
function isExternalNodeKey(k: Pick<WarpKeyView, 'node_id'>): boolean {
    return (k.node_id || '').startsWith('node-');
}

// Core accepts an External node's enroll token only while its key is live, so
// revoking or deleting the key retires a token the machine has not used yet.
function unusedTokenNote(k: WarpKeyView): string {
    return isExternalNodeKey(k) ? ' Its enroll token stops working too, if the machine has not used it yet.' : '';
}

/**
 * The "how do I connect or redeploy this machine" dialog.
 *
 * Three entry points, one component: right after adding a machine (key and
 * enroll token, each shown once), right after a new key (the key once), and
 * later from the list (both null, because only hashes are stored). Everything
 * but the secrets is reproducible, so the last case still gives a complete
 * file with placeholders where they go.
 *
 * A machine that has enrolled gets the redeploy commands rather than the
 * first-start ones: recreating its link with compose would stop the link before
 * its replacement runs.
 */
function DeployModal({ name, keyNodeId, enrolled, rolled, apiKey, enrollToken, grpcTlsFingerprint, tunnelSubnets, config, onClose }: {
    name: string;
    /** The key's node_id. "node-..." is an External node key; anything else a legacy admin key. */
    keyNodeId: string;
    /** The machine has enrolled, so this is a redeploy. */
    enrolled: boolean;
    /** apiKey is a new secret for a running machine. */
    rolled: boolean;
    apiKey: string | null;
    enrollToken: string | null;
    /** From the mint answer. undefined falls back to Core's deploy config. */
    grpcTlsFingerprint?: string;
    tunnelSubnets: string;
    config: WarpDeployConfig | null;
    onClose: () => void;
}) {
    const external = keyNodeId.startsWith('node-');
    const [copied, setCopied] = useState<string | null>(null);
    const [platform, setPlatform] = useDeployPlatform();
    const redeploy = rolled || enrolled;

    // Through kitInput like every other kit: an object assembled by hand here
    // is the shape that shipped a file without its link service once already.
    const compose = nodeCompose(kitInput({
        warpKey: apiKey,
        enrollUrl,
        tunnelSubnets,
        config,
        grpcTlsFingerprint,
        nodeEnrollToken: enrollToken ?? undefined,
        platform,
        linkBesideNode: external,
        legacyAdminKey: !external,
        // Every key in this dialog is the platform's, a legacy one included:
        // the machine is not the reader's own.
        externalNode: true,
    }));
    const cli = redeploy && external ? redeployCli() : deployCli('node', true);

    const copy = (what: string, text: string) => {
        navigator.clipboard.writeText(text).then(() => {
            setCopied(what);
            setTimeout(() => setCopied(null), 1600);
            toast('Copied.');
        });
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel max-w-3xl" onClick={e => e.stopPropagation()}>
                <div className="modal-header">
                    <h3 className="modal-title text-(--accent-light)">{name} - {rolled ? 'new key' : 'deploy'}</h3>
                </div>
                <div className="modal-body space-y-4 max-h-[70vh] overflow-y-auto">
                    {apiKey ? (
                        <div className="flex items-start gap-2 p-2.5 rounded-md bg-(--warning)/5 border border-(--warning)/20">
                            <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <div className="text-xs text-(--base-07) space-y-1.5 min-w-0 flex-1">
                                <p>
                                    {rolled
                                        ? <>The new key is shown <strong>once</strong>; only its hash is stored. In the file the machine already runs, replace <code className="font-mono">API_KEY</code> (warp) and <code className="font-mono">LINK_BOOT_KEY</code> (link) with it and leave the rest as it is: the file below is a fresh one, without your own edits such as a data path. The old key keeps working for one hour, until the next new key at the latest - redeploy within it.</>
                                        : <>The key and the enroll token are shown <strong>once</strong>. Both are stored only as hashes, so they cannot be displayed again. The file below already contains them.</>}
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
                            The key cannot be shown again - only its hash is stored. Paste the one you saved where the file
                            says <code className="font-mono">&lt;your-warp-key&gt;</code>, or use <strong>New key</strong> on
                            this machine to get the file with a new key filled in.
                            {!enrolled && <> The machine has not enrolled yet, so it also needs its enroll token
                            at <code className="font-mono">&lt;enroll-token-from-panel&gt;</code>. If that is lost,
                            revoke this key and add the machine again: revoking retires the unused token with it.</>}
                        </p>
                    ) : (
                        <div className="flex items-start gap-2 p-2.5 rounded-md bg-(--warning)/5 border border-(--warning)/40">
                            <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <p className="text-xs text-(--base-07)">
                                This key was minted before External node keys existed. It is not made for one machine,
                                so it can never boot a Link, and the file below runs none - nobody could reach the
                                servers on that machine. Add the machine again instead. The key itself cannot be shown
                                again either; the file has a placeholder where it goes.
                            </p>
                        </div>
                    )}

                    {redeploy && external && (
                        <div className="flex items-start gap-2 p-2.5 rounded-md bg-(--warning)/5 border border-(--warning)/40">
                            <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <p className="text-xs text-(--base-07)">
                                <strong>Redeploy warp and the node only</strong>, with the commands below. The node then
                                moves the link onto the new key or image itself, starting the new link before the old one
                                stops, so players stay connected and new players can join throughout. A plain{' '}
                                <code className="font-mono">docker compose up -d</code> would stop the link first: the
                                players on it are kept until they leave, but nobody new can join until it has drained
                                (up to 6 hours).
                            </p>
                        </div>
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
                        <p className="text-xs text-(--base-06)">{watchtowerNote('node')}</p>
                    </div>

                    <div className="space-y-1">
                        <div className="flex items-center justify-between">
                            <label className="font-mono text-sm font-medium text-(--base-09)">{redeploy && external ? 'Redeploy' : 'Commands'}</label>
                            <button onClick={() => copy('cli', cli)} className="btn btn-secondary btn-sm">
                                {copied === 'cli' ? <Check size={12} /> : <Copy size={12} />} Copy
                            </button>
                        </div>
                        {!(redeploy && external) && <p className="text-xs text-(--base-07)">{deployIntro('node', platform)}</p>}
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
            </ModalPanel>
        </div>
    );
}
