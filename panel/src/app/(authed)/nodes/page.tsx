"use client";

import { Suspense, useState, useEffect, useCallback } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import {
    HardDrive, Globe, Server, Plus, Trash2, AlertTriangle, Clock,
    ExternalLink, ShoppingCart, RefreshCw,
} from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import {
    getNodes,
    listNodeWarpKeys, mintNodeWarpKey, revokeNodeWarpKey, rollNodeWarpKey, bindNodeWarpKey, type NodeWarpKey,
} from '@/lib/api';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import Select from '@/components/ui/Select';
import { coreOrigin } from '@/lib/api/core';
import { getStoreStatus } from '@/lib/api/store';
import { getMyUsage } from '@/lib/api/usage';
import { mintEnrollToken, listEnrollTokens, revokeEnrollToken } from '@/lib/api/nodeAdmission';
import type { NodeEnrollToken } from '@/lib/api/types';
import { nodeLabel } from '@/lib/nodeLabel';
import { nodeConnectivity, dotFor } from '@/lib/connectivity';
import { nodeIdFromLabel } from '@/lib/warpDeploy';
import { isLocationName } from '@/lib/validation';
import { getWarpDeployConfig, type WarpDeployConfig } from '@/lib/api/warpDeployConfig';
import { SkeletonCard } from '@/components/Skeleton';
import { resolveInfraTab, showInfraTabBar, infraAvailability, type InfraTab } from '@/lib/infraTab';
import { DeployKit, DEPLOY_ASIDE_STICKY, DEPLOY_GRID, NotIncluded, SecretField, usageLabel } from '@/components/infra/DeployKit';
import RouteOnlyPanel from '@/components/infra/RouteOnlyPanel';
import { CustomDomainsPanel } from '@/components/infra/CustomDomainsPanel';
import ExternalNodesPanel from '@/components/infra/ExternalNodesPanel';
import TrafficPools from '@/components/infra/TrafficPools';
import RemoveMachineDialog from '@/components/infra/RemoveMachineDialog';

// ---------------------------------------------------------------------------
// "My infrastructure" - hardware that is not in the cluster, in three tabs.
//
//   External nodes      - the OPERATOR's own machines outside the swarm.
//                         Admin only, and their default.
//   Bring your own node - Dylaris runs Minecraft servers ON a customer's
//                         machine.
//   Protected addresses - the customer runs the server; Dylaris gives it an
//                         address and absorbs the attack traffic.
//
// The first two are different sets of machines with different owners, not two
// views of one list, which is why they are separate tabs rather than a filter.
// Before they were split, an admin opening "my machines" got the entire fleet -
// every swarm host included - because the node list was only ever scoped for
// non-admins. Core now scopes it with ?scope=, so the split is enforced where it
// matters and not only where it is drawn.
//
// The last two were two PAGES until the split stopped following a seam in the
// product: route-only was minted here AND on /routes, with the deploy snippet on
// one and the address form on the other.
//
// Both customer-facing halves are per-LOCATION and can be held several times
// over: a box at home and one in a datacenter needs one of each. So neither is a
// yes/no - each shows how many are in use against the plan's cap, and the create
// control disables on the cap rather than on the entitlement alone.
//
// The store link is ALWAYS present, entitled or not: someone who already has
// BYON is exactly the person who buys a second one.
// ---------------------------------------------------------------------------

interface OwnNode {
    id: number;
    name: string;
    displayName?: string;
    status: string;
    lastSeenAt?: string;
    serverCount?: number;
    region?: string;
}

const NAME_RULE = '4 to 20 characters: letters, digits and hyphens, not starting or ending with a hyphen.';

function MyNodesInner() {
    const { featureFlags, entitlement, user, byonEnabled } = useAppData();
    const router = useRouter();
    const searchParams = useSearchParams();

    const isAdmin = user?.isAdmin ?? false;

    // What this reader HAS - separate from whether this ACCOUNT is entitled to
    // it, which the panels below answer for themselves. The pairing lives in
    // lib/infraTab so it can be tested; see the note on InfraAvailability.routes.
    const have = infraAvailability(isAdmin, byonEnabled);

    // The tab lives in the URL so Create can deep-link straight into the half it
    // means, and so a reload or a shared link lands where it left off.
    const requestedTab = searchParams.get('tab');
    const tab = resolveInfraTab(requestedTab, have);
    const selectTab = useCallback((next: InfraTab) => {
        router.replace(`/nodes?tab=${next}`, { scroll: false });
    }, [router]);

    // A tab that was asked for and not granted - a tenant typing ?tab=external -
    // gets the URL corrected too, not just different content underneath it. An
    // address bar that still says "external" while showing something else is the
    // kind of thing someone reports as a bug in the wrong place.
    useEffect(() => {
        if (!tab || !requestedTab || requestedTab === tab) return;
        router.replace(`/nodes?tab=${tab}`, { scroll: false });
    }, [tab, requestedTab, router]);

    const [nodes, setNodes] = useState<OwnNode[]>([]);
    // Its own fetch, so its own loading flag and its own read time. Sharing the
    // BYON list's would have this panel claim "no external machine" the moment
    // the OTHER request came back.
    const [external, setExternal] = useState<{ nodes: OwnNode[]; loading: boolean; readAt: number }>(
        () => ({ nodes: [], loading: true, readAt: Date.now() })
    );
    const [tokens, setTokens] = useState<NodeEnrollToken[]>([]);
    // The node cap is a LIMIT, not an entitlement: entitlement answers "may
    // they", limits answer "how many". They live in different endpoints because
    // they are set in different places (plan kind vs plan caps).
    const [nodeLimit, setNodeLimit] = useState<number | undefined>(undefined);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    // When the node list was last read. Connectivity is "how long since the last
    // heartbeat", so it needs a clock - but reading one during render makes the
    // rendered output depend on when React happened to re-render. Stamped at
    // load time instead, which is also the only moment the answer can change.
    const [nodesReadAt, setNodesReadAt] = useState(() => Date.now());

    const [nodeLabelDraft, setNodeLabelDraft] = useState('');
    const [minting, setMinting] = useState(false);
    // A BYON machine needs BOTH secrets: a warp key to reach the overlay and an
    // enroll token to become a node. They are minted together here so the deploy
    // snippet is complete - handing over one and a placeholder for the other was
    // the gap this closes.
    // token is absent for a ROLL: the machine enrolled long ago and an enroll
    // token is first-pairing only, so minting one would hand out a credential
    // with nothing to pair.
    // linkBeside says whether the file shown with the keys may run the Link
    // beside the node: always for a new machine (its key is bound when it
    // enrols), and for a roll only when the key is bound already - a rolled key
    // of a machine that enrolled before keys were bound would otherwise get a
    // file whose Link Core cannot answer, on a node told not to start its own.
    const [revealedNode, setRevealedNode] = useState<{ token?: string; warpKey: string; label: string; grpcTlsFingerprint?: string; rolled?: boolean; linkBeside?: boolean } | null>(null);
    const [rollingKey, setRollingKey] = useState('');
    const [nodeKeys, setNodeKeys] = useState<NodeWarpKey[]>([]);
    // Until the keys have been read every machine would look unbound, and its
    // button would offer an update it does not need for a moment.
    const [nodeKeysLoaded, setNodeKeysLoaded] = useState(false);
    // The machine whose Link is being moved out of the node, and the key picked
    // for it. A machine that enrolled before keys were bound at enrol has to be
    // told which of the owner's keys is its own before its kit can run the Link.
    const [linkFor, setLinkFor] = useState<OwnNode | null>(null);
    const [bindKey, setBindKey] = useState('');
    const [binding, setBinding] = useState(false);
    const [nodeUsage, setNodeUsage] = useState<{ used: number; limit?: number } | null>(null);
    // Which machine the removal dialog is open for, by id and by the label the
    // owner reads - the dialog names it back to them before it does anything.
    const [removing, setRemoving] = useState<{ id: number; label: string } | null>(null);

    // Overlay addresses for the deploy snippets. Resolved by Core, which is on
    // that network; there is nowhere else a customer could look them up.
    const [deployConfig, setDeployConfig] = useState<WarpDeployConfig | null>(null);

    // The real storefront origin, not a hardcoded dylaris.com: a self-host
    // install with the store wired points somewhere else, and a link that goes to
    // the wrong shop is worse than no link.
    const [storeUrl, setStoreUrl] = useState<string | null>(null);
    // null = not asked yet. The three-way matters: "we do not know" must not
    // render as "you are not connected", which would tell a linked customer to
    // fix a join that is fine.
    const [storeLinked, setStoreLinked] = useState<boolean | null>(null);
    // Core's own public origin, used in the deploy snippets as ENROLL_URL.
    // Resolved from the API base rather than from the page's own origin: those
    // two are the same host only on a same-origin install, and on any other the
    // panel origin has no /api/warp/enroll for warp to reach.
    const [enrollUrl, setEnrollUrl] = useState('<core-url>');

    const suspended = entitlement?.source === 'suspended';
    // Admins bypass the entitlement gate: their own account may hold no plan at
    // all, and locking the operator out of their own infrastructure page is not a
    // billing decision anyone made.
    const byonAllowed = isAdmin || (entitlement?.byon ?? false);
    const routeOnlyAllowed = isAdmin || (entitlement?.routeOnly ?? false);
    // null means "not fetched yet". Rendering a refusal during that window tells
    // an entitled tenant they have nothing and then takes it back.
    const entitlementKnown = entitlement !== null || isAdmin;

    // Both reads are BYON-gated in Core, so with the subsystem off they answer 403
    // for everyone and the page renders its "turned off on this platform" state
    // from context anyway. Same reasoning as the admin-only external scope below.
    const load = useCallback(async () => {
        if (!byonEnabled) { setLoading(false); return; }
        const [n, t] = await Promise.all([getNodes('byon'), listEnrollTokens()]);
        if (n.success && Array.isArray(n.nodes)) setNodes(n.nodes as OwnNode[]);
        if (t.success && Array.isArray(t.tokens)) setTokens(t.tokens);
        setNodesReadAt(Date.now());
        setLoading(false);
    }, [byonEnabled]);

    useEffect(() => { load(); }, [load]);

    // Only an admin may ask for this scope; Core answers 403 to anyone else, so
    // there is no point spending the request.
    useEffect(() => {
        if (!isAdmin) {
            setExternal({ nodes: [], loading: false, readAt: Date.now() });
            return;
        }
        let cancelled = false;
        getNodes('external').then(res => {
            if (cancelled) return;
            const list = res.success && Array.isArray(res.nodes) ? (res.nodes as OwnNode[]) : [];
            setExternal({ nodes: list, loading: false, readAt: Date.now() });
        }).catch(() => {
            if (!cancelled) setExternal({ nodes: [], loading: false, readAt: Date.now() });
        });
        return () => { cancelled = true; };
    }, [isAdmin]);

    // In an effect, not at render: the API base is resolved in the browser (a
    // runtime /config.js may override the built-in one), so reading it during
    // SSR would bake in a different value than the client computes.
    useEffect(() => {
        setEnrollUrl(coreOrigin() || '<core-url>');
    }, []);

    useEffect(() => {
        let cancelled = false;
        getWarpDeployConfig().then(res => {
            if (!cancelled && res.success && res.config) setDeployConfig(res.config);
        }).catch(() => { /* the snippet falls back to its placeholders */ });
        return () => { cancelled = true; };
    }, []);

    useEffect(() => {
        let cancelled = false;
        getMyUsage().then(res => {
            if (!cancelled && res.success && res.usage?.limits) setNodeLimit(res.usage.limits.maxNodes);
        }).catch(() => { /* the cap is a hint here; the backend enforces it anyway */ });
        return () => { cancelled = true; };
    }, []);

    useEffect(() => {
        if (!featureFlags.store) return;
        let cancelled = false;
        getStoreStatus().then(res => {
            if (cancelled || !res.success) return;
            if (res.storeUrl) setStoreUrl(res.storeUrl);
            // Only meaningful when the store is actually wired up. A self-host
            // install has no store account to connect to.
            setStoreLinked(res.enabled ? !!res.linked : true);
        });
        return () => { cancelled = true; };
    }, [featureFlags.store]);

    const loadNodeKeys = useCallback(async () => {
        // byonEnabled, not gatewayEnabled: Core's ListNodeWarpKeys refuses on
        // byonActive first, so the gateway-only guard still spent a request that
        // could only ever come back 403.
        if (!byonEnabled) return;
        const res = await listNodeWarpKeys();
        if (res.success) {
            setNodeKeys(res.keys || []);
            setNodeUsage({ used: res.used ?? 0, limit: res.limit });
            setNodeKeysLoaded(true);
        }
    }, [byonEnabled]);

    // Required and to a fixed shape. It used to default to "my machine" for
    // everyone who left it blank, which made a list of machines unreadable the
    // moment there was more than one - and the name is what the snippet slugs
    // into NODE_ID, where a leading hyphen reads as a flag. Core rejects the same
    // shapes; this is the version that says so before the round trip.
    const draft = nodeLabelDraft.trim();
    const nameValid = isLocationName(draft);
    const nameError = draft !== '' && !nameValid;

    const handleMintNodeKey = async () => {
        if (!nameValid) {
            setError(`Name this location first — ${NAME_RULE}`);
            return;
        }
        setMinting(true);
        setError('');
        // Warp key first: it is the one with a cap, so a refusal happens before an
        // enroll token is created that nobody could use.
        const warp = await mintNodeWarpKey(draft);
        if (!warp.success || !warp.warp_key) {
            setMinting(false);
            setError(warp.message || 'Could not create the overlay key.');
            return;
        }
        // The key's node_id rides along so the machine's enrol binds this key to
        // it, which is what lets the kit below run the Link beside the node.
        const res = await mintEnrollToken({ label: draft, expiresDays: 7, warpKeyNodeId: warp.node_id });
        setMinting(false);
        if (!res.success || !res.token) {
            setError(res.message || 'The overlay key was created but the enrollment key failed. Revoke the key below and try again.');
            loadNodeKeys();
            return;
        }
        // Core returns the fingerprint only while its gRPC channel is TLS, so it
        // travels with the enroll token and lands in the snippet unchanged.
        setRevealedNode({ token: res.token, warpKey: warp.warp_key, label: draft, grpcTlsFingerprint: res.grpcTlsFingerprint, linkBeside: true });
        setNodeLabelDraft('');
        load();
        loadNodeKeys();
    };

    useEffect(() => { loadNodeKeys(); }, [loadNodeKeys]);

    const handleRevokeNodeKey = async (nodeId: string) => {
        const res = await revokeNodeWarpKey(nodeId);
        if (!res.success) {
            setError('Could not revoke that key.');
            return;
        }
        loadNodeKeys();
    };

    // Roll = new secret, SAME machine. node_id and the location name do not move,
    // so the node keeps identifying itself with the same NODE_ID and every server
    // on it keeps resolving to the same node row - nothing is recreated and no
    // world data is touched.
    //
    // It deliberately does not cut the tunnel. The running machine stays up on
    // its current connection until the customer redeploys with the new key, and
    // the kit's kill_old policy replaces the connection at that moment. The trash
    // button beside this one is still the immediate cutoff, which is what a
    // leaked key wants.
    const handleRollNodeKey = async (nodeId: string, label: string) => {
        const ok = await confirmDialog({
            title: 'Roll this overlay key?',
            message: `The current key for ${label} stops working for new connections straight away, and you get a replacement shown once. `
                + 'Your servers and their data are not affected, and the machine keeps running on its current connection until you redeploy it with the new key.',
            confirmLabel: 'Roll the key',
            destructive: false,
        });
        if (!ok) return;
        setRollingKey(nodeId);
        setError('');
        const res = await rollNodeWarpKey(nodeId);
        setRollingKey('');
        if (!res.success || !res.warp_key) {
            setError(res.message || 'Could not roll that key.');
            return;
        }
        setRevealedNode({
            warpKey: res.warp_key, label, rolled: true,
            linkBeside: !!nodeKeys.find(k => k.node_id === nodeId)?.bound_node_id,
        });
        loadNodeKeys();
    };

    const keyBoundTo = (nodeId: number) => nodeKeys.find(k => k.bound_node_id === nodeId);
    const freeKeys = nodeKeys.filter(k => !k.bound_node_id);

    // Pre-selects the key created under this machine's location name - the node
    // reports that name slugged, which is what nodeIdFromLabel produces from the
    // key's name. Nothing is pre-selected when no name matches: the owner picks.
    const openLinkSetup = (n: OwnNode) => {
        const slug = nodeIdFromLabel(nodeLabel(n));
        const match = freeKeys.find(k => nodeIdFromLabel(k.name) === slug);
        setBindKey(match?.node_id ?? '');
        setError('');
        setLinkFor(n);
    };

    const handleBindKey = async () => {
        if (!linkFor || !bindKey) return;
        setBinding(true);
        setError('');
        const res = await bindNodeWarpKey(bindKey, linkFor.id);
        setBinding(false);
        if (!res.success) {
            setError(res.message || 'Could not connect that key to this machine.');
            return;
        }
        // The re-read is what flips the panel from the key picker to the new file.
        loadNodeKeys();
    };

    const handleRevokeToken = async (id: string) => {
        const res = await revokeEnrollToken(id);
        if (!res.success) {
            setError(res.message || 'Could not revoke that key.');
            return;
        }
        load();
    };

    // Only when NOTHING is available. Returning on BYON alone stranded route-only
    // customers on a platform with BYON off: the top bar offers this page
    // whenever any part exists, and they would land on "your own hardware is
    // turned off" with the routes tab unreachable behind it.
    if (tab === null) {
        return (
            <div className="p-6 max-w-2xl">
                <h1 className="text-lg font-display font-bold text-(--base-09) mb-2">My infrastructure</h1>
                <p className="text-sm text-(--base-07)">
                    Running Minecraft on your own hardware, and pointing protected addresses at your own
                    server, are both turned off on this platform.
                </p>
            </div>
        );
    }

    // Both from /warp/node-keys when it answered: it counts connected nodes AND
    // unredeemed keys, which is exactly what the mint endpoint enforces. Showing
    // a different number would make the refusal look arbitrary the moment it hits.
    const nodesUsed = nodeUsage?.used ?? nodes.length;
    const effectiveNodeLimit = nodeUsage?.limit ?? nodeLimit;
    const nodesAtCap = typeof effectiveNodeLimit === 'number' && effectiveNodeLimit > 0 && nodesUsed >= effectiveNodeLimit;

    const TABS: { id: InfraTab; label: string; icon: typeof HardDrive }[] = [
        { id: 'external', label: 'External nodes', icon: Server },
        { id: 'machines', label: 'Bring your own node', icon: HardDrive },
        { id: 'routes', label: 'Protected addresses', icon: Globe },
    ].filter(t => have[t.id as keyof typeof have]) as { id: InfraTab; label: string; icon: typeof HardDrive }[];

    return (
        /* The SCROLL container is full width and unpadded, so the scrollbar sits
           at the outer edge of the content area. With max-w on the scroller it
           rode the inside of the centred box instead, floating mid-screen. */
        <div className="flex-1 min-w-0 overflow-y-auto">
        <div className="@container p-6 max-w-[92rem] space-y-6">
            <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                    <h1 className="text-lg font-display font-bold text-(--base-09) mb-1">My infrastructure</h1>
                    <p className="text-sm text-(--base-07) max-w-2xl">
                        Hardware that is not in the cluster. Everything here connects outwards through an
                        encrypted tunnel, so nothing needs a public IP or port forwarding — and you can
                        hold several of each, one per location.
                    </p>
                </div>
                {/* Always offered, entitled or not: the person most likely to buy a
                    second location is the one who already has the first. */}
                {featureFlags.store && storeUrl && (
                    <a href={storeUrl} target="_blank" rel="noopener noreferrer" className="btn btn-secondary btn-sm shrink-0">
                        <ShoppingCart size={13} /> Store <ExternalLink size={11} />
                    </a>
                )}
            </div>

            {/* Above the tabs, because it is about everything below them: the
                allowances are the tenant's, not any one location's. */}
            <TrafficPools />

            {/* A bar with one tab is decoration. It appears only where the reader
                actually has somewhere else to go. */}
            {showInfraTabBar(have) && (
                <div className="flex gap-1 border-b border-(--base-03)">
                    {TABS.map(t => {
                        const Icon = t.icon;
                        return (
                            <button
                                key={t.id}
                                type="button"
                                onClick={() => selectTab(t.id)}
                                aria-current={tab === t.id ? 'page' : undefined}
                                className={`flex items-center gap-2 px-4 py-2 text-sm font-medium border-b-2 -mb-px transition-colors ${
                                    tab === t.id
                                        ? 'border-(--accent) text-(--accent-light)'
                                        : 'border-transparent text-(--base-06) hover:text-(--base-08)'
                                }`}
                            >
                                <Icon size={14} />
                                {t.label}
                            </button>
                        );
                    })}
                </div>
            )}

            {error && (
                <div className="alert alert-error">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <span>{error}</span>
                </div>
            )}

            {suspended && (
                <div className="alert alert-warning">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <span>Your account is suspended. Everything below is paused until it is reactivated.</span>
                </div>
            )}

            {tab === 'external' ? (
                <ExternalNodesPanel nodes={external.nodes} loading={external.loading} readAt={external.readAt} />
            ) : tab === 'routes' ? (
                <div className="space-y-4">
                    <RouteOnlyPanel
                        enrollUrl={enrollUrl}
                        config={deployConfig}
                        storeUrl={storeUrl}
                        storeLinked={storeLinked}
                        allowed={routeOnlyAllowed}
                        entitlementKnown={entitlementKnown}
                        suspended={suspended}
                    />
                    {/* Renders nothing until the account actually has a claim, so a
                        tenant who only uses our subdomains never sees it. */}
                    <CustomDomainsPanel />
                </div>
            ) : (
            /* The compose file lives in a column of its own, permanently. It is
               not a detail of adding a machine: it is the thing the reader came
               to run, and they need it again on every rebuild. The keys join it
               there for the one moment they exist. */
            <div className={byonAllowed && entitlementKnown ? DEPLOY_GRID : ''}>
            <section className="card p-5 space-y-4">
                <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                        <h2 className="text-sm font-display font-semibold text-(--accent-light) flex items-center gap-2">
                            <HardDrive size={15} /> Bring your own node
                        </h2>
                        <p className="text-sm text-(--base-07) mt-1 max-w-2xl">
                            Dylaris runs Minecraft servers <strong>on your machine</strong>. You keep the
                            hardware and the disk; the panel, backups and player routing stay ours.
                        </p>
                    </div>
                    <span className="mono-label shrink-0 pt-0.5">{usageLabel(nodesUsed, effectiveNodeLimit)}</span>
                </div>

                {!entitlementKnown ? (
                    <SkeletonCard height="h-16" />
                ) : !byonAllowed ? (
                    <NotIncluded what="bring your own node" storeUrl={storeUrl} suspended={suspended} storeLinked={storeLinked} />
                ) : (
                    <>
                        <div className="flex flex-col sm:flex-row gap-2 sm:items-start">
                            <div className="flex-1 flex flex-col gap-[5px]">
                                <label htmlFor="location-name" className="input-label">Name this location</label>
                                <input
                                    id="location-name"
                                    className="input-field w-full"
                                    value={nodeLabelDraft}
                                    onChange={e => setNodeLabelDraft(e.target.value)}
                                    placeholder="home-desktop"
                                    maxLength={20}
                                    aria-invalid={nameError || undefined}
                                    aria-describedby="location-name-rule"
                                    disabled={nodesAtCap || suspended}
                                />
                                <p
                                    id="location-name-rule"
                                    className={`text-xs ${nameError ? 'text-(--error-light)' : 'text-(--base-06)'}`}
                                >
                                    {NAME_RULE}
                                </p>
                            </div>
                            <button
                                onClick={handleMintNodeKey}
                                disabled={minting || !nameValid || nodesAtCap || suspended}
                                className="btn btn-primary disabled:opacity-40 disabled:cursor-not-allowed sm:mt-[22px]"
                                title={nodesAtCap ? 'You have used every node your plan includes' : undefined}
                            >
                                <Plus size={14} /> {minting ? 'Creating…' : 'Add a machine'}
                            </button>
                        </div>

                        {/* What to DO about it, and the answer is usually not "buy
                            something". Moving a machine is the common case - the old
                            one holds the slot, and the message used to name only the
                            store, so the way out looked like a purchase. */}
                        {nodesAtCap && (
                            <p className="flex items-start gap-1.5 text-xs text-(--warning-light)">
                                <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                                <span>
                                    {effectiveNodeLimit === 1
                                        ? <>Your machine below is using the one slot you have. Remove it to set this
                                          machine up somewhere else, or add another location in the store.</>
                                        : <>All {effectiveNodeLimit} of your slots are in use. Remove a machine below to
                                          replace it, or add another location in the store.</>}
                                </span>
                            </p>
                        )}

                        {nodeKeys.length > 0 && (
                            <div className="border-t border-(--base-03) pt-3 space-y-2">
                                <div className="mono-label">Overlay keys in use</div>
                                <p className="text-xs text-(--base-06)">
                                    Each counts towards your plan whether or not the machine has connected
                                    yet. Revoke one you never used to free the slot.
                                </p>
                                {nodeKeys.map(k => (
                                    <div key={k.id} className="flex items-center justify-between gap-3 text-sm">
                                        <div className="min-w-0">
                                            <div className="text-(--base-08) truncate">{k.name}</div>
                                            <div className="mono-label truncate">{k.node_id}</div>
                                        </div>
                                        <div className="flex items-center gap-0.5 shrink-0">
                                            <button
                                                onClick={() => handleRollNodeKey(k.node_id, k.name)}
                                                disabled={rollingKey === k.node_id}
                                                className="text-(--base-06) hover:text-(--base-09) p-1.5 rounded-md transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
                                                title="Replace this key with a new one, keeping the machine and its servers"
                                            >
                                                <RefreshCw size={14} className={rollingKey === k.node_id ? 'animate-spin' : undefined} />
                                            </button>
                                            <button
                                                onClick={() => handleRevokeNodeKey(k.node_id)}
                                                className="text-(--base-06) hover:text-(--error-light) p-1.5 rounded-md transition-colors"
                                                title="Revoke this overlay key"
                                            >
                                                <Trash2 size={14} />
                                            </button>
                                        </div>
                                    </div>
                                ))}
                            </div>
                        )}

                        {tokens.length > 0 && (
                            <div className="border-t border-(--base-03) pt-3 space-y-2">
                                <div className="mono-label">Keys waiting to be used</div>
                                {tokens.map(t => (
                                    <div key={t.id} className="flex items-center justify-between gap-3 text-sm">
                                        <div className="min-w-0">
                                            <div className="text-(--base-08) truncate">{t.label || '(unnamed)'}</div>
                                            {t.expiresAt && (
                                                <div className="text-xs text-(--base-06) flex items-center gap-1">
                                                    <Clock size={10} /> expires {new Date(t.expiresAt).toLocaleDateString()}
                                                </div>
                                            )}
                                        </div>
                                        <button
                                            onClick={() => handleRevokeToken(t.id)}
                                            className="text-(--base-06) hover:text-(--error-light) p-1.5 rounded-md transition-colors"
                                            title="Revoke this key"
                                        >
                                            <Trash2 size={14} />
                                        </button>
                                    </div>
                                ))}
                            </div>
                        )}

                        <div className="border-t border-(--base-03) pt-3 space-y-2">
                            <div className="mono-label">Your machines</div>
                            {loading ? (
                                <SkeletonCard height="h-16" />
                            ) : nodes.length === 0 ? (
                                <p className="text-sm text-(--base-06)">
                                    No machine connected yet. Create a key above and follow the deploy steps.
                                </p>
                            ) : (
                                nodes.map(n => {
                                    const { tier } = nodeConnectivity(n.status, n.lastSeenAt, nodesReadAt);
                                    return (
                                        <div key={n.id} className="flex items-center justify-between gap-3 rounded-md bg-(--base-02) border border-(--base-03) px-3 py-2.5">
                                            <div className="flex items-center gap-2.5 min-w-0">
                                                <div className={`w-2 h-2 rounded-full shrink-0 ${dotFor(tier, 'bg-(--success-light)')}`} />
                                                <div className="min-w-0">
                                                    <div className="text-sm text-(--base-09) truncate">{nodeLabel(n)}</div>
                                                    <div className="mono-label">
                                                        {n.status}
                                                        {typeof n.serverCount === 'number' && ` · ${n.serverCount} server${n.serverCount === 1 ? '' : 's'}`}
                                                    </div>
                                                </div>
                                            </div>
                                            <div className="flex items-center gap-1 shrink-0">
                                                {/* A machine whose key is not bound still runs its Link inside
                                                    the node; that is the one it has to act on. A bound one only
                                                    ever needs its file again. */}
                                                {nodeKeysLoaded && (
                                                    <button
                                                        type="button"
                                                        onClick={() => openLinkSetup(n)}
                                                        disabled={!!revealedNode}
                                                        title={revealedNode ? 'Save the keys shown first' : undefined}
                                                        className="btn btn-secondary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                                                    >
                                                        {keyBoundTo(n.id) ? 'Deploy file' : 'Update this machine'}
                                                    </button>
                                                )}
                                                {/* The way back out. Without it a machine could be added and
                                                    never removed, so the slot it held was gone for good and
                                                    the cap read as "buy more". */}
                                                <button
                                                    type="button"
                                                    onClick={() => setRemoving({ id: n.id, label: nodeLabel(n) })}
                                                    title={`Remove ${nodeLabel(n)}`}
                                                    aria-label={`Remove ${nodeLabel(n)}`}
                                                    className="shrink-0 text-(--base-06) hover:text-(--error-light) p-1.5 rounded-md transition-colors
                                                               focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                                                >
                                                    <Trash2 size={14} />
                                                </button>
                                            </div>
                                        </div>
                                    );
                                })
                            )}
                        </div>
                    </>
                )}
            </section>

            {/* One machine's Link, moved out of its node: pick its key if it has
                none bound yet, then its file. */}
            {byonAllowed && entitlementKnown && !revealedNode && linkFor && (
                <aside className={`card p-5 space-y-3 min-w-0 ${DEPLOY_ASIDE_STICKY}`}>
                    <div className="text-sm font-medium text-(--base-09)">
                        {nodeLabel(linkFor)}: the Link beside the node
                    </div>
                    <p className="text-xs text-(--base-07)">
                        The Link carries your players to your servers. So far it has run inside the node. To run it
                        beside the node instead, as a service of its own, we need to know which overlay key
                        belongs to this machine.
                    </p>
                    {keyBoundTo(linkFor.id) ? (
                        <>
                            <p className="text-xs text-(--base-07)">
                                This machine&apos;s key is connected. If it still runs an earlier file, redeploy it with
                                the one below, keeping the values of <code className="font-mono">API_KEY</code>,{' '}
                                <code className="font-mono">NODE_ID</code> and <code className="font-mono">NODE_ENROLL_TOKEN</code>{' '}
                                from the file it runs now; <code className="font-mono">LINK_BOOT_KEY</code> takes the same
                                key as <code className="font-mono">API_KEY</code>. Players connected at that moment
                                reconnect once while the Link moves over.
                            </p>
                            <DeployKit
                                kind="node"
                                warpKey={null}
                                enrollUrl={enrollUrl}
                                nodeId={nodeIdFromLabel(nodeLabel(linkFor))}
                                config={deployConfig}
                                linkBesideNode
                            />
                        </>
                    ) : freeKeys.length === 0 ? (
                        <p className="flex items-start gap-1.5 text-xs text-(--warning-light)">
                            <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                            <span>
                                None of your overlay keys is free. This machine needs the key its warp service runs
                                with; if that key is not under Overlay keys in use, it was revoked. The machine keeps
                                running as it is.
                            </span>
                        </p>
                    ) : (
                        <div className="space-y-2">
                            <span id="bind-key-label" className="input-label">Overlay key of this machine</span>
                            <Select
                                value={bindKey}
                                onChange={setBindKey}
                                options={freeKeys.map(k => ({ value: k.node_id, label: k.name }))}
                                placeholder="Choose a key"
                                ariaLabel="Overlay key of this machine"
                                disabled={binding}
                            />
                            <p className="text-xs text-(--base-06)">
                                It is the <code className="font-mono">API_KEY</code> in this machine&apos;s warp service,
                                listed by the location name it was created with. Nothing changes on the machine until
                                you redeploy it.
                            </p>
                            <button
                                type="button"
                                onClick={handleBindKey}
                                disabled={!bindKey || binding}
                                className="btn btn-primary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                            >
                                {binding ? 'Saving…' : 'Use this key'}
                            </button>
                        </div>
                    )}
                    <button type="button" onClick={() => setLinkFor(null)} className="btn btn-secondary btn-sm">
                        Close
                    </button>
                </aside>
            )}
            {/* The keys are shown once. They are stored as hashes, so this is
                the only moment they exist anywhere the reader can see them - the
                copy has to say so before they close it.

                This generic file keeps the node-managed Link: it is shown to every
                owner, including ones whose machine has no bound key yet, and for
                those a file without that Link would leave the machine with none.
                The Link-beside-the-node file appears only where the key is known
                to be bound, or is about to be at enrol. */}
            {byonAllowed && entitlementKnown && !revealedNode && !linkFor && (
                <aside className={`space-y-3 min-w-0 ${DEPLOY_ASIDE_STICKY}`}>
                    {(nodeKeys.length > 0 || tokens.length > 0) && (
                        <p className="text-xs text-(--base-06)">
                            The keys cannot be shown again — only their hashes are stored. Paste the ones you saved where the file says <code className="font-mono">&lt;...&gt;</code>, or revoke the key and create a new one.
                        </p>
                    )}
                    <DeployKit kind="node" warpKey={null} enrollUrl={enrollUrl} config={deployConfig} />
                </aside>
            )}
            {revealedNode && (
                <aside className={`card p-5 space-y-3 border-(--accent-border) bg-(--accent-ghost) ${DEPLOY_ASIDE_STICKY}`}>
                    <div className="text-sm font-medium text-(--base-09)">
                        {revealedNode.rolled
                            ? <>{revealedNode.label} — new overlay key, shown once.</>
                            : <>{revealedNode.label} — two keys, shown once.</>}
                    </div>
                    {revealedNode.rolled ? (
                        <p className="text-xs text-(--base-07)">
                            Only the key changed. Replace <code className="font-mono">API_KEY</code> in
                            this machine&apos;s warp service and deploy again — the name, the address and
                            your servers all stay as they are. Until you do, the machine keeps running
                            on the connection it already has.
                        </p>
                    ) : (
                        <p className="text-xs text-(--base-07)">
                            Both are already filled into the compose file below, so the normal path never
                            needs them by hand. They are blurred because this page is one people have open
                            while sharing a screen; click either to read it.
                        </p>
                    )}
                    <SecretField label="Overlay key (warp API_KEY)" value={revealedNode.warpKey} />
                    {/* A roll has no enrollment key: the machine paired long ago and
                        that token is single-use, first-pairing only. Showing an
                        empty field would read as one that failed to generate. */}
                    {revealedNode.token && (
                        <SecretField
                            label="Enrollment key (NODE_ENROLL_TOKEN)"
                            value={revealedNode.token}
                            note="It expires in 7 days and can be used once."
                        />
                    )}
                    <DeployKit
                        kind="node"
                        warpKey={revealedNode.warpKey}
                        enrollUrl={enrollUrl}
                        nodeEnrollToken={revealedNode.token}
                        grpcTlsFingerprint={revealedNode.grpcTlsFingerprint}
                        nodeId={nodeIdFromLabel(revealedNode.label)}
                        config={deployConfig}
                        linkBesideNode={revealedNode.linkBeside}
                    />
                    <button type="button" onClick={() => setRevealedNode(null)} className="btn btn-secondary btn-sm">
                        {revealedNode.rolled ? 'I saved it' : 'I saved them'}
                    </button>
                </aside>
            )}
            </div>
            )}

            {removing && (
                <RemoveMachineDialog
                    nodeId={removing.id}
                    nodeLabel={removing.label}
                    onClose={() => setRemoving(null)}
                    onRemoved={() => {
                        if (linkFor?.id === removing.id) setLinkFor(null);
                        setRemoving(null);
                        // Re-read rather than splicing the row out: the machine
                        // going away also frees a slot, and the cap message and
                        // the usage counter both read from that.
                        load();
                        // loadNodeKeys is what carries the used/limit pair the cap
                        // message reads, so it has to run too - the count is what
                        // was blocking the reader in the first place.
                        loadNodeKeys();
                    }}
                />
            )}
        </div>
        </div>
    );
}

// useSearchParams needs a Suspense boundary, the same as every other page here
// that reads one. /nodes builds dynamic today so it slipped through, but the
// boundary is what keeps that true: without it, a later change that makes the
// route statically analyzable fails the build instead of the page.
export default function MyNodesPage() {
    return (
        <Suspense fallback={
            <div className="p-6 max-w-[92rem] space-y-6">
                <SkeletonCard height="h-10" />
                <SkeletonCard height="h-64" />
            </div>
        }>
            <MyNodesInner />
        </Suspense>
    );
}
