"use client";

import { toast as notifyToast } from '@/components/ui/Toast';

import React, { useState, useEffect, useRef } from 'react';
import { usePathname, useRouter } from 'next/navigation';
import { AlertTriangle, Upload, X } from 'lucide-react';
import {
    deleteServer, serverPower, getServerRoutes, getInstallCooldown, getMigrationStatus, cancelMigration,
    type MigrationStatus, GatewayRoute, TabPermissions,
} from '@/lib/api';
import { useAppData } from '@/lib/AppDataContext';
import RoutesModal from '@/components/RoutesModal';
import { useServerUploadLock } from '@/lib/uploadManager';
import { listServerTabs, type ServerTab } from '@/lib/api/serverTabs';
import { tabRunsOnActiveSubServer } from '@/lib/tabProxy';
import { systemEvents, ROUTES_CHANGED_EVENT } from '@/lib/systemEvents';
import { useNow } from '@/lib/useNow';
import { useRouteId } from '@/lib/routeParams';
import ModalPanel from '@/components/ui/ModalPanel';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { oomDismissKey, showOomBanner } from '@/lib/memoryGuard';
import PowerControls from '@/components/server/PowerControls';
import ServerHeader from '@/components/server/ServerHeader';
import ServerNav, { type NavTab } from '@/components/server/ServerNav';
import ServerSettingsDrawer, { TERMINAL_PHASES } from '@/components/server/ServerSettingsDrawer';
import Callout from '@/components/server/Callout';
import { ShownStatusContext } from '@/components/server/ShownStatus';
import { advancePending, shownStatus, startPending, POWER_COOLDOWN_MS, type PendingPower, type PowerAction } from '@/lib/powerControls';
import { fmtDateTime } from '@/lib/time';
import { nodeConnectivity, connLabel } from '@/lib/connectivity';
import { domainAccess, primaryAddress } from '@/lib/serverDomainStatus';
import { trustedDomains, useServerDomains } from '@/lib/serverDomainsStore';

// The server detail chrome: header, power controls, tab row, banners and the
// "Server settings" drawer. It WAS layout.tsx, and moved here so that
// layout.tsx can be a server component - generateStaticParams cannot be
// exported from a client one, and the static export requires it.
export default function ServerShell({ children }: { children: React.ReactNode }) {
    const paramId = useRouteId('servers');
    const pathname = usePathname();
    const router = useRouter();
    const { servers, user, refreshServers, gatewayEnabled, routingMode, featureFlags } = useAppData();
    const now = useNow();

    const serverId = Number(paramId);
    const selectedServer = servers.find(s => s.id === serverId);

    // Shares the Domains card's poller; it runs only while the shell is mounted and the tab is visible.
    const canReadDomains = !!selectedServer && domainAccess(selectedServer).read && routingMode !== 'gateway';
    const domainsState = useServerDomains(String(serverId), canReadDomains);
    const directIpPort = selectedServer ? `${selectedServer.nodeAddress}:${selectedServer.hostPort}` : '';
    const directDomain = primaryAddress(trustedDomains(domainsState, now), directIpPort);

    const [showDeletePopup, setShowDeletePopup] = useState(false);
    const [deleteError, setDeleteError] = useState('');
    const [deleteWarning, setDeleteWarning] = useState('');
    const [deleteCountdown, setDeleteCountdown] = useState(5);

    // The "Server settings" drawer (admins) or its transfer-only cut (BYON owners).
    const [settingsOpen, setSettingsOpen] = useState(false);
    const [settingsAdvanced, setSettingsAdvanced] = useState(false);
    // The lastCrashAt of the OOM banner this viewer dismissed (per browser).
    const [oomDismissed, setOomDismissed] = useState<string | null>(null);

    // Polled while a move is in flight (server status === 'migrating', or right
    // after a manual move seeded it from the drawer).
    const [migrationStatus, setMigrationStatus] = useState<MigrationStatus | null>(null);
    const [cancellingMigration, setCancellingMigration] = useState(false);

    // Power action this browser sent and is waiting on, per server id. It ends
    // only on an outcome or a refusal; see lib/powerControls.
    const [pendingPower, setPendingPower] = useState<Record<number, PendingPower>>({});
    const pendingPowerRef = useRef(pendingPower);
    pendingPowerRef.current = pendingPower;
    // When start/stop/restart unlock again after a wait ended, per server id.
    const [powerCooldownUntil, setPowerCooldownUntil] = useState<Record<number, number>>({});
    const [killCooldown, setKillCooldown] = useState(false);

    useEffect(() => {
        if (!serverId) return;
        try { setOomDismissed(localStorage.getItem(oomDismissKey(serverId))); } catch { setOomDismissed(null); }
    }, [serverId]);

    const [serverRoutes, setServerRoutes] = useState<GatewayRoute[]>([]);
    const [showRoutesModal, setShowRoutesModal] = useState(false);

    // User-defined extra tabs. Loaded on server change + refreshed
    // when the SSE channel signals a CRUD change on this server.
    const [customTabs, setCustomTabs] = useState<ServerTab[]>([]);
    useEffect(() => {
        if (!selectedServer?.id) { setCustomTabs([]); return; }
        let cancelled = false;
        // On failure the previous tabs are KEPT rather than cleared. Removing a
        // navigation entry says the tab is gone; a request that did not answer
        // says nothing, and the next event or server switch reloads it anyway.
        const apply = (p: Promise<ServerTab[]>) =>
            p.then(list => { if (!cancelled) setCustomTabs(list); }).catch(() => {});
        apply(listServerTabs(selectedServer.id));
        const sid = selectedServer.id;
        const unsub = systemEvents.on('server_tabs.changed', (evt) => {
            const evtSid = (evt.payload as any)?.serverId;
            if (evtSid === undefined || evtSid === sid) {
                apply(listServerTabs(sid));
            }
        });
        return () => { cancelled = true; unsub(); };
    }, [selectedServer?.id]);

    // Load gateway routes when relevant, and again when the Setup tab creates
    // one: loaded only on a server change, the header said "No gateway route
    // configured" about the route just made until a reload.
    useEffect(() => {
        setServerRoutes([]);
        if (!selectedServer || !gatewayEnabled) return;
        const sid = selectedServer.id;
        const load = () => getServerRoutes(sid).then(res => {
            if (Array.isArray(res)) setServerRoutes(res);
            else if (res && Array.isArray(res.routes)) setServerRoutes(res.routes);
        });
        load();
        const onChanged = (e: Event) => {
            if ((e as CustomEvent).detail?.serverId === sid) load();
        };
        window.addEventListener(ROUTES_CHANGED_EVENT, onChanged);
        return () => window.removeEventListener(ROUTES_CHANGED_EVENT, onChanged);
    }, [selectedServer?.id, gatewayEnabled]);

    // Every pending action follows its own server's status, so one sent before
    // switching to another server still ends when that server gets there.
    useEffect(() => {
        // A wait that ends here starts the short pause before the next power
        // action, so a click landing as the buttons come back cannot queue a
        // second command behind the first.
        const ended: number[] = [];
        for (const [id, p] of Object.entries(pendingPowerRef.current)) {
            const srv = servers.find(s => s.id === Number(id));
            if (srv && advancePending(p, srv.status) === null) ended.push(Number(id));
        }
        if (ended.length > 0) {
            const until = Date.now() + POWER_COOLDOWN_MS;
            setPowerCooldownUntil(prev => {
                const next = { ...prev };
                for (const id of ended) next[id] = until;
                return next;
            });
        }
        setPendingPower(prev => {
            let next = prev;
            for (const [id, p] of Object.entries(prev)) {
                const srv = servers.find(s => s.id === Number(id));
                const after = srv ? advancePending(p, srv.status) : null;
                if (after === p) continue;
                if (next === prev) next = { ...prev };
                if (after) next[Number(id)] = after;
                else delete next[Number(id)];
            }
            return next;
        });
    }, [servers]);

    // Migration status poll. Runs while the server DB status is `migrating`
    // (set by the orchestrator during a move) so the status line keeps current
    // even on page-reload mid-move. A manual Move kicks it off optimistically
    // by seeding migrationStatus with phase 'starting'. Polling stops on any
    // terminal phase so we don't spin forever on a finished/failed move.
    useEffect(() => {
        const active = selectedServer?.status === 'migrating' ||
            (migrationStatus !== null && !TERMINAL_PHASES.includes(migrationStatus.phase));
        if (!selectedServer || !active) return;
        let cancelled = false;
        const poll = async () => {
            const res = await getMigrationStatus(selectedServer.id);
            if (cancelled) return;
            if (res.success && res.status) {
                setMigrationStatus(res.status);
                if (TERMINAL_PHASES.includes(res.status.phase)) {
                    clearInterval(timer);
                    // A finished/failed move flips node assignment + status — pull
                    // fresh server state so the UI reflects the new home node.
                    refreshServers();
                }
            }
        };
        poll();
        const timer = setInterval(poll, 4000);
        return () => { cancelled = true; clearInterval(timer); };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [selectedServer?.id, selectedServer?.status, migrationStatus?.phase]);

    // Post-install cooldown. The node sets a Redis key with 30s TTL on
    // setup/reinstall; the core PowerAction handler gates start/stop/
    // restart/kill on it for non-admins. We fetch the TTL here so the UI
    // can show a countdown and pre-disable buttons instead of letting the
    // user click and eat a 429. Reload survives because the TTL lives in
    // Redis and we re-fetch on mount.
    const [cooldownEndsAt, setCooldownEndsAt] = useState<number | null>(null);
    const [nowTick, setNowTick] = useState<number>(() => Date.now());

    useEffect(() => {
        if (!selectedServer) return;
        let cancelled = false;
        getInstallCooldown(selectedServer.id).then(res => {
            if (cancelled) return;
            if (res?.seconds && res.seconds > 0) {
                setCooldownEndsAt(Date.now() + res.seconds * 1000);
            } else {
                setCooldownEndsAt(null);
            }
        }).catch(() => { /* non-fatal: backend still gates via 429 */ });
        return () => { cancelled = true; };
        // selectedServer.status is included so we refetch when the server
        // flips out of pending_setup (i.e. install just completed) — that's
        // the exact moment the 30s window starts.
    }, [selectedServer?.id, selectedServer?.status]);

    useEffect(() => {
        if (!cooldownEndsAt) return;
        const tick = setInterval(() => {
            const now = Date.now();
            setNowTick(now);
            if (now >= cooldownEndsAt) setCooldownEndsAt(null);
        }, 1000);
        return () => clearInterval(tick);
    }, [cooldownEndsAt]);

    const cooldownSecondsLeft = cooldownEndsAt
        ? Math.max(0, Math.ceil((cooldownEndsAt - nowTick) / 1000))
        : 0;
    // Admins bypass the gate server-side; mirror that here so the buttons
    // stay live for them and only the badge is shown.
    const powerCooldownActive = cooldownSecondsLeft > 0 && !user?.isAdmin;

    if (!selectedServer) {
        return (
            <main className="flex-1 flex flex-col overflow-hidden relative z-10">
                <div className="flex-1 flex items-center justify-center text-(--base-06)">
                    Server not found.
                </div>
            </main>
        );
    }

    const isPendingSetup = selectedServer.status === 'pending_setup';
    // While the server is migrating to another node, ALL power actions are locked
    // (including for admins) so a user can't fight the move. Admins additionally
    // get a cancel/rollback button (see the header) that aborts a pre-cutover move.
    const isMigrating = selectedServer.status === 'migrating';

    // Safety lock: while one or more uploads are streaming bytes into this
    // server's directory, the only mutating actions allowed are the ones
    // the upload itself drives. Power buttons (Start/Stop/Restart/Kill)
    // and the Configuration tab are gated until uploads finish, so a user
    // can't e.g. boot the server with a half-uploaded file in place. The
    // FileBrowser stays navigable on purpose so the user can watch progress.
    const uploadLocked = useServerUploadLock(selectedServer.uuid);

    const handleOpenDeletePopup = () => {
        setDeleteCountdown(5);
        setShowDeletePopup(true);
        const interval = setInterval(() => {
            setDeleteCountdown(prev => {
                if (prev <= 1) { clearInterval(interval); return 0; }
                return prev - 1;
            });
        }, 1000);
    };
    // Core refuses a delete for a real reason (deleting servers needs elevated
    // permissions, 403), so a refusal must not close the popup and navigate as
    // if it had worked.
    //
    // Core also answers with a `warning` when the row went but the node was never
    // told to remove the container. That is not a failure, but it says data is
    // left behind on a machine, so the modal stays open until the operator has
    // acknowledged it rather than navigating away with a toast the route change
    // would take with it.
    const handleDelete = async () => {
        setDeleteError('');
        const res: any = await deleteServer(selectedServer.id);
        if (res && (res.success === false || res.error)) {
            setDeleteError(res.error || res.message || 'Delete failed.');
            return;
        }
        await refreshServers();
        if (res?.warning) {
            setDeleteWarning(res.warning);
            return;
        }
        setShowDeletePopup(false);
        router.push('/servers');
    };

    const dismissDeleteWarning = () => {
        setDeleteWarning('');
        setShowDeletePopup(false);
        router.push('/servers');
    };

    const isOwner = selectedServer.role !== 'invited' && selectedServer.role !== 'inherited' && selectedServer.role !== 'demo';
    // 'demo' is a read-only viewer role (showcase server the user does not own),
    // so it is NOT an owner: keep it out of every owner-gated control.
    const isDemoView = selectedServer.role === 'demo';
    const perms = selectedServer.permissions;
    const shown = shownStatus(selectedServer.status, pendingPower[selectedServer.id] ?? null, now);

    // Who may move this server to another node. Admins in either routing mode;
    // a BYON server owner when tenancy is on, which runs on the gateway.
    const canTransfer = !!user?.isAdmin || (featureFlags.byon && isOwner && routingMode !== 'ip_port');

    const oomTab = pathname.endsWith('/overview') || pathname.endsWith('/console');
    const oomBanner = oomTab && showOomBanner(selectedServer, now, oomDismissed);
    const dismissOomBanner = () => {
        const at = selectedServer.lastCrashAt ?? null;
        setOomDismissed(at);
        try { if (at) localStorage.setItem(oomDismissKey(selectedServer.id), at); } catch { /* per-viewer convenience only */ }
    };

    const handlePower = async (action: PowerAction, opts?: { skipCooldownPrompt?: boolean }) => {
        // Admins get an explicit "still settling" prompt before we send the
        // command. Non-admins are already disabled by powerCooldownActive
        // upstream, so this branch only ever fires for admins.
        if (cooldownSecondsLeft > 0 && user?.isAdmin && !opts?.skipCooldownPrompt) {
            if (!(await confirmDialog({
                title: 'Server is still installing',
                message: `The post-install settling window has ${cooldownSecondsLeft}s left. Power actions during this window can leave the freshly-installed world in a half-written state or kill the JVM mid chunk-generation.\n\nAs an admin you can override the cooldown. Only do this if you understand the consequences.`,
                confirmLabel: 'Override & continue',
                cancelLabel: 'Wait for settle',
            }))) return;
        }
        const sid = selectedServer.id;
        const clearPending = () => setPendingPower(prev => {
            const next = { ...prev };
            delete next[sid];
            return next;
        });
        // Set before the request: the servers.changed Core publishes can land
        // before the response does.
        setPendingPower(prev => ({ ...prev, [sid]: startPending(action, selectedServer.status, Date.now()) }));
        if (action === 'kill' && !user?.isAdmin) {
            setKillCooldown(true);
            setTimeout(() => setKillCooldown(false), 60000);
        }
        try {
            const res: any = await serverPower(sid, action);
            // The backend may reject (e.g. 429 install cooldown). fetchAPI
            // wraps that as {success:false, error, message}. Drop the wait and
            // surface the message so the user knows why nothing happened.
            if (res && (res.success === false || res.error)) {
                clearPending();
                notifyToast(res.error || res.message || 'Action rejected', false);
                // Re-fetch cooldown: a 429 means we missed the install
                // window's start (different tab finished setup, etc.).
                try {
                    const c = await getInstallCooldown(sid);
                    if (c?.seconds && c.seconds > 0) setCooldownEndsAt(Date.now() + c.seconds * 1000);
                } catch { /* non-fatal */ }
            }
        } catch { /* network error: the command may have gone out, so keep waiting; the timeout says so */ }
    };

    // Force kill: the settling question for admins and its own confirm are
    // independent warnings, so both are asked, in that order.
    const handleKill = async () => {
        if (cooldownSecondsLeft > 0 && user?.isAdmin && !(await confirmDialog({
            title: 'Server is still installing',
            message: `The post-install settling window has ${cooldownSecondsLeft}s left. Power actions during this window can leave the freshly-installed world in a half-written state or kill the JVM mid chunk-generation.\n\nAs an admin you can override the cooldown. Only do this if you understand the consequences.`,
            confirmLabel: 'Override & continue',
            cancelLabel: 'Wait for settle',
        }))) return;
        if (!(await confirmDialog({
            title: 'Force kill server?',
            message: 'Stops the server immediately without saving. Anything not yet saved to the world is lost.',
            confirmLabel: 'Force kill',
        }))) return;
        // The settling prompt was answered above; asking again would chain a
        // second prompt for the same decision.
        handlePower('kill', { skipCooldownPrompt: true });
    };

    // Admin-only: abort an in-flight migration. The backend rolls the server back
    // to its current node (safe pre-cutover; a no-op 409 once cut over). The
    // migration poll picks up the 'cancelled' terminal phase and refreshes state.
    const handleCancelMigration = async () => {
        setCancellingMigration(true);
        try {
            const res: any = await cancelMigration(selectedServer.id);
            if (res && (res.success === false || res.error)) {
                notifyToast(res.error || res.message || 'Could not cancel the migration', false);
            }
        } catch { /* network error — the move continues; admin can retry */ }
        setCancellingMigration(false);
    };

    const tabDisabled = (perm: keyof TabPermissions) => isPendingSetup || (!isOwner && !!perms && !perms[perm]);
    // `perms.power` is power.start alone; each other action has its own bit.
    const powerAllow = isOwner ? undefined : {
        start: !!perms?.power, stop: !!perms?.powerStop, restart: !!perms?.powerRestart, kill: !!perms?.powerKill,
    };
    const canPower = !powerAllow || Object.values(powerAllow).some(Boolean);

    // Tabs (path segments). When an upload is active for this server the
    // Configuration tab is locked: editing properties / JVM flags while
    // bytes are still being written into the same directory is exactly
    // the kind of foot-gun the safety lock prevents.
    const tabs: NavTab[] = [
        { slug: 'setup',   icon: 'wrench',          label: 'Setup',         disabled: !isOwner && !!perms && !perms.setup },
        { slug: 'overview',icon: 'house',           label: 'Overview',      disabled: isPendingSetup },
        { slug: 'console', icon: 'square-terminal', label: 'Console',       disabled: tabDisabled('console') },
        { slug: 'files',   icon: 'folder-open',     label: 'Files',         disabled: tabDisabled('files') },
        { slug: 'config',  icon: 'settings',        label: 'Configuration', disabled: tabDisabled('config') || uploadLocked },
        // Modrinth-driven mod/plugin browser. Its routes require mods.read, not config.read.
        { slug: 'content', icon: 'package',         label: 'Content',       disabled: tabDisabled('mods') },
        { slug: 'network', icon: 'network',         label: 'Network',       disabled: tabDisabled('network') },
        // Players tab. RCON-driven (online list + ban/kick/op/whitelist).
        // Power-class permission since every action mutates server state.
        { slug: 'players', icon: 'users-round',     label: 'Players',       disabled: isPendingSetup || (!isOwner && (!perms || !perms.players)) },
        { slug: 'backups', icon: 'hard-drive',      label: 'Backups',       disabled: tabDisabled('backups') },
        // Audit tab. Owner + admin only. The API gates the audit log on
        // overview.read, the cap every invite carries, so graying the tab out
        // was long the only thing between a member and the log of what the
        // other members did; Core now enforces it with its own
        // server.audit.read cap. The one case this is still stricter than the
        // backend is a member delegated that cap in Admin-roles mode: they may
        // call the API but see a gray tab, because the server list carries the
        // legacy TabPermissions blob, not caps.
        { slug: 'audit',   icon: 'file-text',       label: 'Audit',         disabled: !isOwner && !user?.isAdmin },
        // User-defined custom tabs follow the built-ins. Disabled rows are
        // filtered out so the nav matches the user's intent without greying logic.
        ...customTabs
            // A tab pinned to another sub-server addresses a port that only
            // exists while that one runs; Core refuses it, so it does not belong here.
            .filter(t => t.enabled && tabRunsOnActiveSubServer(t.subServerName, selectedServer.activeSubServer))
            .map(t => ({ slug: `t/${t.id}`, icon: t.icon || 'layout-grid', label: t.name, disabled: false })),
    ];

    const { tier } = nodeConnectivity(selectedServer.nodeStatus, selectedServer.nodeLastSeenAt, now);
    const settingsKind = user?.isAdmin ? 'resources' : canTransfer ? 'transfer' : null;

    return (
        <main className="flex-1 flex flex-col overflow-hidden relative z-10">
            <div className="shrink-0 border-b border-(--base-03)">
                <div className="srv-gutter pt-5 md:pt-7">
                    {/* Full-width line above the name so the long unreachable/down
                        label never crowds the name row. */}
                    {tier !== 'ok' && (
                        <div className="mb-3">
                            <Callout tone="warning" icon={<AlertTriangle size={16} />} role="status">
                                {connLabel(tier, selectedServer.nodeLastSeenAt)}
                            </Callout>
                        </div>
                    )}
                    <ServerHeader
                        server={shown === selectedServer.status ? selectedServer : { ...selectedServer, status: shown }}
                        servers={servers}
                        now={now}
                        isDemoView={isDemoView}
                        isPendingSetup={isPendingSetup}
                        uploadLocked={uploadLocked}
                        cooldownSecondsLeft={cooldownSecondsLeft}
                        settings={settingsKind ? { kind: settingsKind, onOpen: () => { setSettingsAdvanced(false); setSettingsOpen(true); } } : null}
                        migration={isMigrating && user?.isAdmin ? {
                            active: true,
                            cancelling: cancellingMigration,
                            cancellable: migrationStatus?.cancellable !== false,
                            onCancel: handleCancelMigration,
                        } : null}
                        directDomain={directDomain}
                        directIpPort={directIpPort}
                        serverRoutes={serverRoutes}
                        onOpenRoutes={() => setShowRoutesModal(true)}
                        power={
                            <PowerControls
                                status={selectedServer.status}
                                pending={pendingPower[selectedServer.id] ?? null}
                                gates={{
                                    canPower,
                                    allow: powerAllow,
                                    uploadLocked,
                                    cooldownSeconds: powerCooldownActive ? cooldownSecondsLeft : 0,
                                    killCooldown,
                                    cooldownUntil: powerCooldownUntil[selectedServer.id],
                                }}
                                onAction={action => { if (action !== 'kill') handlePower(action); else handleKill(); }}
                                onDelete={user?.isAdmin ? handleOpenDeletePopup : undefined}
                            />
                        }
                    />
                    <div className="mt-4 -mx-1">
                        <ServerNav serverId={selectedServer.id} tabs={tabs} pathname={pathname} />
                    </div>
                </div>
            </div>

            {(uploadLocked || oomBanner) && (
                <div className="srv-gutter flex shrink-0 flex-col gap-2 pt-4">
                    {uploadLocked && (
                        <Callout tone="info" icon={<Upload size={16} />} title="Upload in progress">
                            Destructive actions (power, resources, delete, config) are locked until the transfer finishes or is cancelled. The file browser stays available.
                        </Callout>
                    )}
                    {oomBanner && (
                        <Callout
                            tone="error"
                            icon={<AlertTriangle size={16} />}
                            role="alert"
                            title={`Killed: out of memory${selectedServer.lastCrashAt ? ` at ${fmtDateTime(selectedServer.lastCrashAt)}` : ''}`}
                            actions={<>
                                {user?.isAdmin && (
                                    <button type="button" onClick={() => { setSettingsAdvanced(true); setSettingsOpen(true); }} disabled={uploadLocked} className="btn btn-secondary btn-sm">
                                        Open resources
                                    </button>
                                )}
                                <button type="button" onClick={dismissOomBanner} aria-label="Dismiss" className="btn btn-secondary btn-sm btn-icon">
                                    <X size={14} />
                                </button>
                            </>}
                        >
                            The server reached its memory limit and was killed; anything not yet saved was lost. It needs more RAM or RAM headroom.
                        </Callout>
                    )}
                </div>
            )}

            <div className="srv-page flex-1 min-h-0 overflow-y-auto"><ShownStatusContext.Provider value={shown}>{children}</ShownStatusContext.Provider></div>

            {settingsKind && (
                <ServerSettingsDrawer
                    open={settingsOpen}
                    focusAdvanced={settingsAdvanced}
                    onOpenChange={setSettingsOpen}
                    mode={settingsKind === 'resources' ? 'full' : 'transfer'}
                    server={selectedServer}
                    canTransfer={canTransfer}
                    migrationStatus={migrationStatus}
                    setMigrationStatus={setMigrationStatus}
                />
            )}

            {/* Delete Confirmation Popup */}
            {showDeletePopup && (
                <div className="modal-overlay animate-fade-in">
                    <ModalPanel className="modal-panel w-full max-w-md">
                        <div className="modal-header">
                            <h3 className="modal-title text-(--error-light) flex items-center gap-2">
                                <AlertTriangle size={20} />
                                Delete Server
                            </h3>
                        </div>
                        <div className="modal-body space-y-3">
                            {deleteWarning ? (
                                <p className="text-sm text-(--warning-light)">{deleteWarning}</p>
                            ) : (
                                <p className="text-sm text-(--base-07)">
                                    Are you sure you want to delete <span className="font-semibold text-(--base-09)">{selectedServer.name}</span>?
                                    This cannot be undone and all server data will be lost.
                                </p>
                            )}
                            {deleteError && (
                                <p className="text-sm text-(--error-light)">{deleteError}</p>
                            )}
                        </div>
                        <div className="modal-footer">
                            {deleteWarning ? (
                                <button onClick={dismissDeleteWarning} className="btn btn-primary">Understood</button>
                            ) : (
                                <>
                                    <button onClick={() => { setDeleteError(''); setShowDeletePopup(false); }} className="btn btn-secondary">Cancel</button>
                                    <button
                                        onClick={handleDelete}
                                        disabled={deleteCountdown > 0}
                                        className="btn btn-danger disabled:opacity-40 disabled:cursor-not-allowed"
                                    >
                                        {deleteCountdown > 0 ? `Delete (${deleteCountdown}s)` : 'Delete'}
                                    </button>
                                </>
                            )}
                        </div>
                    </ModalPanel>
                </div>
            )}

            {showRoutesModal && (
                <RoutesModal
                    serverId={selectedServer.id}
                    serverName={selectedServer.name}
                    server={selectedServer}
                    onClose={() => setShowRoutesModal(false)}
                    onRoutesChanged={setServerRoutes}
                />
            )}
        </main>
    );
}
