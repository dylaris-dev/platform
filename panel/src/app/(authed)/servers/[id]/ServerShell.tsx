"use client";

import React, { useState, useEffect } from 'react';
import { usePathname, useRouter } from 'next/navigation';
import Link from 'next/link';
import {
    Pencil, SlidersHorizontal, Trash2, AlertTriangle, Play, Square, RotateCcw, Skull,
    HardDrive, MoveHorizontal, RefreshCw, Copy, Globe, Link2, ChevronDown, Move, Clock, Undo2,
} from 'lucide-react';
import { DynamicIcon } from '@/lib/icons';
import {
    deleteServer, updateServerName, updateServerResources, serverPower,
    getServerStoragePath, migrateServerStorage, getServerRoutes, setServerAutoMove,
    getInstallCooldown, moveServer, transferServer, setServerDemo, getMigrationStatus, cancelMigration, type MigrationStatus, type Node,
    GatewayRoute, StoragePathInfo, TabPermissions,
} from '@/lib/api';
import { getNodes } from '@/lib/api/resources';
import { useAppData } from '@/lib/AppDataContext';
import RoutesModal from '@/components/RoutesModal';
import RegionBadge from '@/components/RegionBadge';
import CpuPinningControl from '@/components/CpuPinningControl';
import { useServerUploadLock } from '@/lib/uploadManager';
import { Upload } from 'lucide-react';
import { listServerTabs, type ServerTab } from '@/lib/api/serverTabs';
import { tabRunsOnActiveSubServer } from '@/lib/tabProxy';
import { systemEvents } from '@/lib/systemEvents';
import { useBusy } from '@/lib/useBusy';
import { nodeConnectivity, dotFor, connLabel } from '@/lib/connectivity';
import { useNow } from '@/lib/useNow';
import { useRouteId } from '@/lib/routeParams';

// The server detail chrome: the header, the power controls, the tab strip and
// every dialog hanging off them. It WAS layout.tsx, and moved here so that
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

    const [isEditingName, setIsEditingName] = useState(false);
    const [savingResources, runSaveResources] = useBusy();
    const [editedName, setEditedName] = useState('');

    const [showDeletePopup, setShowDeletePopup] = useState(false);
    const [deleteError, setDeleteError] = useState('');
    const [deleteWarning, setDeleteWarning] = useState('');
    const [deleteCountdown, setDeleteCountdown] = useState(5);

    const [showEditResourcesPopup, setShowEditResourcesPopup] = useState(false);
    const [editRam, setEditRam] = useState(0);
    const [editCpuLimit, setEditCpuLimit] = useState(0);
    const [editDiskLimit, setEditDiskLimit] = useState(0);
    const [editCpusetCpus, setEditCpusetCpus] = useState('');
    const [editCpuMode, setEditCpuMode] = useState<'shared' | 'auto' | 'manual'>('shared');
    const [editResourcesAdvancedOpen, setEditResourcesAdvancedOpen] = useState(false);
    const [editHostPort, setEditHostPort] = useState(0);
    const [editContainerPort, setEditContainerPort] = useState(25565);
    const [editAutoMove, setEditAutoMove] = useState(false);

    // Manual move-to-node (admin only). Node list is loaded lazily when the
    // Edit Resources modal opens. migrationStatus is polled while a move is
    // in flight (server status === 'migrating' or right after a manual move).
    const [moveNodes, setMoveNodes] = useState<Node[]>([]);
    const [moveTargetNodeId, setMoveTargetNodeId] = useState<number>(0);
    const [moveBusy, setMoveBusy] = useState(false);
    const [moveMsg, setMoveMsg] = useState('');
    const [demoMsg, setDemoMsg] = useState('');
    const [resourcesMsg, setResourcesMsg] = useState('');
    const [nameMsg, setNameMsg] = useState('');
    const [migrationStatus, setMigrationStatus] = useState<MigrationStatus | null>(null);
    const [cancellingMigration, setCancellingMigration] = useState(false);
    // Dedicated transfer dialog for BYON owners (admins use the Resources popup).
    const [showTransferPopup, setShowTransferPopup] = useState(false);

    const [storageCurrentPath, setStorageCurrentPath] = useState('');
    const [storagePaths, setStoragePaths] = useState<StoragePathInfo[]>([]);
    const [storageMigrateTarget, setStorageMigrateTarget] = useState('');
    const [storageMigrating, setStorageMigrating] = useState(false);
    const [storageMigrateMsg, setStorageMigrateMsg] = useState('');

    const [powerLoading, setPowerLoading] = useState<string | null>(null);
    const [waitingForStatus, setWaitingForStatus] = useState<string | null>(null);
    const [killCooldown, setKillCooldown] = useState(false);
    const [showKillConfirm, setShowKillConfirm] = useState(false);
    const [powerError, setPowerError] = useState<string>('');

    // Brief "copied" feedback for the owner-id click-to-copy in the info bar.
    const [ownerCopied, setOwnerCopied] = useState(false);

    // Admins bypass the install cooldown on the backend, but we still warn
    // them client-side so a Kill mid world-generation isn't a silent footgun.
    // Holds the action the admin is trying to perform; null = no prompt.
    const [pendingCooldownAction, setPendingCooldownAction] = useState<'start' | 'stop' | 'restart' | 'kill' | null>(null);

    // Auto-clear power error after a few seconds so it doesn't stick on screen.
    useEffect(() => {
        if (!powerError) return;
        const t = setTimeout(() => setPowerError(''), 4500);
        return () => clearTimeout(t);
    }, [powerError]);

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

    // Load gateway routes when relevant
    useEffect(() => {
        setServerRoutes([]);
        if (!selectedServer || !gatewayEnabled) return;
        getServerRoutes(selectedServer.id).then(res => {
            if (Array.isArray(res)) setServerRoutes(res);
            else if (res && Array.isArray(res.routes)) setServerRoutes(res.routes);
        });
    }, [selectedServer?.id, gatewayEnabled]);

    // Clear waitingForStatus when server reaches expected status
    useEffect(() => {
        if (waitingForStatus && selectedServer?.status === waitingForStatus) setWaitingForStatus(null);
    }, [selectedServer?.status, waitingForStatus]);

    // Fallback: reset waitingForStatus after 60s
    useEffect(() => {
        if (!waitingForStatus) return;
        const timeout = setTimeout(() => setWaitingForStatus(null), 60000);
        return () => clearTimeout(timeout);
    }, [waitingForStatus]);

    // Migration status poll. Runs while the server DB status is `migrating`
    // (set by the orchestrator during a move) so the status line keeps current
    // even on page-reload mid-move. A manual Move kicks it off optimistically
    // by seeding migrationStatus with phase 'starting'. Polling stops on any
    // terminal phase so we don't spin forever on a finished/failed move.
    const TERMINAL_PHASES = ['done', 'failed', 'failed_post_cutover', 'aborted_players', 'cancelled', 'none'];
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
    const isDiskFull = selectedServer.status === 'disk_full';
    const isServerOffline = ['stopped', 'offline', 'pending_setup', 'disk_full'].includes(selectedServer.status);
    const powerWaiting = waitingForStatus !== null || powerLoading !== null;
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

    const handleStartEditName = () => {
        setEditedName(selectedServer.name || '');
        setIsEditingName(true);
    };
    const handleSaveName = async () => {
        const trimmed = editedName.replace(/\s{2,}/g, ' ').trim();
        if (!trimmed || !/^[a-zA-Z0-9\-+ ]{1,50}$/.test(trimmed)) { setIsEditingName(false); return; }
        setNameMsg('');
        // A refused rename (name already taken, no permission) used to close the
        // editor and let refreshServers put the old name back, which reads as the
        // panel losing the edit rather than the server declining it.
        const res = await updateServerName(selectedServer.id, trimmed);
        if (res?.success === false) {
            setNameMsg(res.message || res.error || 'The name could not be changed.');
            setIsEditingName(false);
            return;
        }
        setIsEditingName(false);
        refreshServers();
    };

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
    // The result was discarded, and this is the button where that costs the most:
    // on a refusal the popup closed and the router navigated to /servers anyway,
    // so a delete that never happened looked exactly like one that did - the
    // server is simply still in the list. Core refuses here for a real reason
    // (deleting servers needs elevated permissions, 403), not only on faults.
    //
    // Core also answers with a `warning` when the row went but the node was never
    // told to remove the container. That is not a failure, so it must not read as
    // one - but it says data is left behind on a machine, so the modal stays open
    // until the operator has acknowledged it rather than navigating away with a
    // toast the route change would take with it.
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

    const handleOpenEditResources = async () => {
        setEditRam(selectedServer.memory || 1024);
        setEditCpuLimit(selectedServer.cpuLimit || 0);
        setEditDiskLimit((selectedServer.diskLimit || 0) / 1024);
        setEditHostPort(selectedServer.hostPort || 0);
        setEditContainerPort(selectedServer.containerPort || 25565);
        setEditCpuMode(selectedServer.cpuPinningMode || 'shared');
        // Manual cpuset comes from the dedicated cpuset field; fall back to the
        // legacy cpusetCpus so older servers still show their pinning.
        setEditCpusetCpus(selectedServer.cpuset || (selectedServer as any).cpusetCpus || '');
        setEditAutoMove(!!(selectedServer as any).autoMove);
        setEditResourcesAdvancedOpen(false);
        setStorageCurrentPath('');
        setStoragePaths([]);
        setStorageMigrateTarget('');
        setStorageMigrateMsg('');
        setMoveMsg('');
        setMoveTargetNodeId(0);
        setShowEditResourcesPopup(true);

        if (user?.isAdmin) {
            try {
                const res = await getServerStoragePath(selectedServer.id);
                if (res.success) {
                    setStorageCurrentPath(res.currentPath || '');
                    const paths: StoragePathInfo[] = Array.isArray(res.storagePaths) ? res.storagePaths : [];
                    setStoragePaths(paths);
                    const others = paths.filter(p => p.path !== res.currentPath);
                    if (others.length > 0) setStorageMigrateTarget(others[0].path);
                }
            } catch { /* ignore */ }
        }
        // Node list for the move/transfer picker — only ONLINE nodes other than
        // the one this server currently lives on, and only ones this caller may
        // actually place on: another tenant's machine is not a transfer target,
        // for an operator either.
        if (canTransfer) {
            try {
                const res = await getNodes('placement');
                if (res.success && Array.isArray(res.nodes)) {
                    const targets: Node[] = res.nodes.filter(
                        (n: Node) => n.status === 'online' && n.id !== selectedServer.nodeId,
                    );
                    setMoveNodes(targets);
                    if (targets.length > 0) setMoveTargetNodeId(targets[0].id);
                }
            } catch { /* ignore */ }
        }
    };

    // Node-to-node move. Admins use the admin endpoint; BYON owners use the
    // tenant transfer endpoint (owner + placement authz). Async on the backend;
    // we seed the migration status optimistically so the poll picks up and the
    // status line shows progress without waiting a full tick.
    const handleMoveServer = async () => {
        if (!moveTargetNodeId || moveBusy) return;
        setMoveBusy(true);
        setMoveMsg('');
        try {
            const res: any = user?.isAdmin
                ? await moveServer(selectedServer.id, moveTargetNodeId)
                : await transferServer(selectedServer.id, moveTargetNodeId);
            if (res && (res.success === false || res.error)) {
                setMoveMsg(res.error || res.message || 'Move failed.');
            } else {
                setMoveMsg(res?.message || 'Migration queued.');
                setMigrationStatus({ phase: 'starting' });
            }
        } catch {
            setMoveMsg('Move request failed.');
        }
        setMoveBusy(false);
    };
    // Admin: flag this server as a public read-only demo (or unflag it).
    //
    // The result is checked for the same reason handleMoveServer directly above
    // checks its own: fetchAPI does not throw on an HTTP error, it returns
    // {success:false}. Discarding it here mattered more than elsewhere - the
    // toggle renders from selectedServer.isDemo, so a refused UNFLAG snapped the
    // switch back to "on" with no explanation, and an admin who read that as a UI
    // glitch would believe the server was no longer publicly viewable while it
    // still was.
    const handleToggleDemo = async () => {
        setDemoMsg('');
        try {
            const res: any = await setServerDemo(selectedServer.id, !selectedServer.isDemo);
            if (res && (res.success === false || res.error)) {
                setDemoMsg(res.error || res.message || 'Could not change the demo flag.');
            } else {
                refreshServers();
            }
        } catch {
            setDemoMsg('Could not change the demo flag.');
        }
    };

    const handleSaveResources = async () => {
        const ports = user?.isAdmin ? { hostPort: editHostPort, containerPort: editContainerPort } : undefined;
        setResourcesMsg('');
        // Both answers used to be discarded and the dialog closed regardless, so
        // a refusal (host port already taken, a cpuset the node cannot honour,
        // over the plan's limits) looked exactly like a save: the popup shut and
        // the old numbers quietly came back with refreshServers.
        const res = await updateServerResources(
            selectedServer.id, editRam, editCpuLimit, editDiskLimit > 0 ? editDiskLimit * 1024 : 0,
            ports, undefined, { mode: editCpuMode, cpuset: editCpusetCpus },
        );
        if (res?.success === false) {
            setResourcesMsg(res.message || res.error || 'The resource change was refused.');
            return;
        }
        // Auto-move is a separate field — flip only when it actually changed.
        if (editAutoMove !== !!(selectedServer as any).autoMove) {
            const moveRes = await setServerAutoMove(selectedServer.id, editAutoMove);
            if (moveRes?.success === false) {
                // The resources DID save, so say precisely that rather than
                // implying the whole dialog failed.
                setResourcesMsg(moveRes.message || moveRes.error || 'Resources saved, but auto-move could not be changed.');
                refreshServers();
                return;
            }
        }
        setShowEditResourcesPopup(false);
        refreshServers();
    };

    // Open the dedicated BYON transfer dialog: load the (tenant-scoped) online
    // node list, then show the picker. Mirrors the node-fetch the admin Resources
    // popup does, but stands alone so owners never see the admin resource editor.
    const handleOpenTransfer = async () => {
        setMoveMsg('');
        setMoveTargetNodeId(0);
        setMoveNodes([]);
        setShowTransferPopup(true);
        try {
            const res = await getNodes('placement');
            if (res.success && Array.isArray(res.nodes)) {
                const targets: Node[] = res.nodes.filter(
                    (n: Node) => n.status === 'online' && n.id !== selectedServer.nodeId,
                );
                setMoveNodes(targets);
                if (targets.length > 0) setMoveTargetNodeId(targets[0].id);
            }
        } catch { /* ignore */ }
    };

    const handleMigrateStorage = async () => {
        if (!storageMigrateTarget) return;
        setStorageMigrating(true);
        setStorageMigrateMsg('');
        try {
            const res = await migrateServerStorage(selectedServer.id, storageMigrateTarget);
            setStorageMigrateMsg(res.success ? res.message : (res.error || 'Migration failed'));
        } catch {
            setStorageMigrateMsg('Migration request failed');
        }
        setStorageMigrating(false);
    };

    const handlePower = async (action: 'start' | 'stop' | 'restart' | 'kill', opts?: { skipCooldownPrompt?: boolean }) => {
        // Admins get an explicit "still settling" prompt before we send the
        // command. Non-admins are already disabled by powerCooldownActive
        // upstream, so this branch only ever fires for admins.
        if (cooldownSecondsLeft > 0 && user?.isAdmin && !opts?.skipCooldownPrompt) {
            setPendingCooldownAction(action);
            return;
        }
        setPowerLoading(action);
        if (action === 'kill') {
            setWaitingForStatus(null);
            if (!user?.isAdmin) setKillCooldown(true);
        } else {
            setWaitingForStatus(action === 'stop' ? 'stopped' : 'online');
        }
        try {
            const res: any = await serverPower(selectedServer.id, action);
            // The backend may reject (e.g. 429 install cooldown). fetchAPI
            // now wraps that as {success:false, error, message}. Reset the
            // optimistic waiter and surface the message so the user knows
            // why nothing happened.
            if (res && (res.success === false || res.error)) {
                setWaitingForStatus(null);
                setPowerError(res.error || res.message || 'Action rejected');
                // Re-fetch cooldown — a 429 means we missed the install
                // window's start (different tab finished setup, etc.).
                try {
                    const c = await getInstallCooldown(selectedServer.id);
                    if (c?.seconds && c.seconds > 0) setCooldownEndsAt(Date.now() + c.seconds * 1000);
                } catch { /* non-fatal */ }
            }
        } catch { /* network error — leave waiter, user can retry */ }
        setPowerLoading(null);
        if (action === 'kill' && !user?.isAdmin) {
            setTimeout(() => setKillCooldown(false), 60000);
        }
    };

    const copyOwnerId = () => {
        navigator.clipboard.writeText(selectedServer.ownerId).then(() => {
            setOwnerCopied(true);
            setTimeout(() => setOwnerCopied(false), 1200);
        }).catch(() => { /* clipboard blocked — nothing to surface */ });
    };

    // Admin-only: abort an in-flight migration. The backend rolls the server back
    // to its current node (safe pre-cutover; a no-op 409 once cut over). The
    // migration poll picks up the 'cancelled' terminal phase and refreshes state.
    const handleCancelMigration = async () => {
        setCancellingMigration(true);
        try {
            const res: any = await cancelMigration(selectedServer.id);
            if (res && (res.success === false || res.error)) {
                setPowerError(res.error || res.message || 'Could not cancel the migration');
            }
        } catch { /* network error — the move continues; admin can retry */ }
        setCancellingMigration(false);
    };

    const getStatusColor = (status: string) => {
        switch (status) {
            case 'online': return 'bg-(--success-light)';
            case 'stopped':
            case 'offline':
            case 'disk_full': return 'bg-(--error)';
            default: return 'bg-(--warning) animate-pulse';
        }
    };

    // 'demo' is a read-only viewer role (showcase server the user does not own),
    // so it is NOT an owner — keep it out of every owner-gated control.
    const isOwner = selectedServer.role !== 'invited' && selectedServer.role !== 'inherited' && selectedServer.role !== 'demo';
    const isDemoView = selectedServer.role === 'demo';
    const perms = selectedServer.permissions;

    // Who may move this server to another node. Admins always (any mode); a BYON
    // server owner when tenancy is on. Migration is gateway-only, so hide the
    // control entirely on ip_port instead of dangling a disabled picker.
    const canTransfer = (!!user?.isAdmin || (featureFlags.byon && isOwner)) && routingMode !== 'ip_port';

    // Auto-move opt-in gating. The backend gates the PATCH on BOTH the platform
    // feature flag AND active gateway routing (503 otherwise), so we mirror both
    // here and surface the matching reason.
    const autoMoveDisabled = !featureFlags.autoMove || routingMode === 'ip_port';
    const autoMoveReason = !featureFlags.autoMove
        ? 'Auto-Move is disabled platform-wide. An admin can enable it in Settings → Features.'
        : 'Requires gateway routing. Auto-Move is unavailable while Game Traffic is on IP:Port.';
    const tabDisabled = (perm: keyof TabPermissions) => isPendingSetup || (!isOwner && !!perms && !perms[perm]);
    const canPower = isOwner || (perms?.power ?? false);

    // Tabs (path segments). When an upload is active for this server the
    // Configuration tab is locked — editing properties / JVM flags while
    // bytes are still being written into the same directory is exactly
    // the kind of foot-gun the safety lock prevents.
    const tabs: { slug: string; icon: string; label: string; disabled: boolean }[] = [
        { slug: 'setup',   icon: 'wrench',          label: 'Setup',         disabled: !isOwner && !!perms && !perms.setup },
        { slug: 'overview',icon: 'house',           label: 'Overview',      disabled: isPendingSetup },
        { slug: 'console', icon: 'square-terminal', label: 'Console',       disabled: tabDisabled('console') },
        { slug: 'files',   icon: 'folder-open',     label: 'Files',         disabled: tabDisabled('files') },
        { slug: 'config',  icon: 'settings',        label: 'Configuration', disabled: tabDisabled('config') || uploadLocked },
        // Modrinth-driven mod/plugin browser.
        { slug: 'content', icon: 'package',         label: 'Content',       disabled: tabDisabled('config') },
        { slug: 'network', icon: 'network',         label: 'Network',       disabled: tabDisabled('network') },
        // Players tab. RCON-driven (online list + ban/kick/op/whitelist).
        // Power-class permission since every action mutates server state.
        { slug: 'players', icon: 'users-round',     label: 'Players',       disabled: isPendingSetup || (!isOwner && (!perms || !perms.players)) },
        { slug: 'backups', icon: 'hard-drive',      label: 'Backups',       disabled: tabDisabled('backups') },
        // Audit tab. Owner + admin only; non-owners (even with
        // permission bundles) can't see who changed what on someone else's
        // server. The view itself self-hides empty audit gracefully.
        //
        // This used to be the ONLY place that rule existed: the API gated the
        // audit log on overview.read, the cap every invite carries, so graying
        // the tab out was all that stood between a member and the log of what
        // the other members did. Core now enforces it with its own
        // server.audit.read cap. The one case this line is still stricter than
        // the backend is a member the owner explicitly delegated that cap to in
        // Admin-roles mode: they may call the API but see a gray tab, because
        // the server list carries the legacy TabPermissions blob, not caps.
        { slug: 'audit',   icon: 'file-text',       label: 'Audit',         disabled: !isOwner && !user?.isAdmin },
    ];

    return (
        <main className="flex-1 flex flex-col overflow-hidden relative z-10">
            {/* Server Header */}
            <div className="bg-(--base-02) border-b border-(--base-03) px-6 pt-3 shrink-0 z-10">
                {/* Node-connectivity notice: full-width line above the name so the long
                    unreachable/down label never crowds the server-name row (M2). */}
                {(() => {
                    const { tier } = nodeConnectivity(selectedServer.nodeStatus, selectedServer.nodeLastSeenAt, now);
                    return tier === 'ok' ? null : (
                        <div className="text-xs text-(--warning) mb-1.5">{connLabel(tier, selectedServer.nodeLastSeenAt)}</div>
                    );
                })()}
                {/* Row 1: Server Name + Actions */}
                <div className="flex items-center justify-between mb-2">
                    <div className="flex items-center space-x-2">
                        {(() => {
                            const { tier } = nodeConnectivity(selectedServer.nodeStatus, selectedServer.nodeLastSeenAt, now);
                            const title = tier === 'ok' ? selectedServer.status : connLabel(tier, selectedServer.nodeLastSeenAt);
                            return <span className={`w-2.5 h-2.5 rounded-full shrink-0 ${dotFor(tier, getStatusColor(selectedServer.status))}`} title={title} />;
                        })()}
                        {isEditingName ? (
                            <input
                                className="text-xl font-bold font-display bg-(--base-03) text-(--base-09) rounded-md px-2 py-0.5 outline-none border border-(--accent) focus:shadow-[0_0_0_3px_rgba(112,72,200,0.15)]"
                                value={editedName}
                                onChange={e => setEditedName(e.target.value)}
                                onKeyDown={e => { if (e.key === 'Enter') handleSaveName(); if (e.key === 'Escape') setIsEditingName(false); }}
                                onBlur={handleSaveName}
                                maxLength={50}
                                autoFocus
                            />
                        ) : (
                            <div className="flex items-center gap-2 bg-(--base-03)/40 border border-(--base-04) rounded-md px-3 py-1">
                                <h1 className="text-xl font-bold font-display text-(--base-09) tracking-wide">{selectedServer.name}</h1>
                                {!isDemoView && (
                                    <button onClick={() => { setNameMsg(''); handleStartEditName(); }} className="text-(--base-06) hover:text-(--base-09) transition-colors">
                                        <Pencil size={16} />
                                    </button>
                                )}
                            </div>
                        )}
                        {nameMsg && (
                            <span className="text-xs text-(--error-light)" role="alert">{nameMsg}</span>
                        )}
                        {isDemoView && (
                            <span className="mono-label bg-(--accent-ghost) border border-(--accent-border) px-2 py-0.5 rounded-sm text-(--accent-light) flex items-center gap-1">
                                Demo · read-only
                            </span>
                        )}
                        {selectedServer.activeSubServer && (
                            <span className="mono-label bg-(--base-03) px-2 py-0.5 rounded-sm text-(--base-07) flex items-center gap-1.5">
                                <span className="text-(--base-05)">Active Server</span>
                                <span className="text-(--base-08)">{selectedServer.activeSubServer}</span>
                            </span>
                        )}
                        {selectedServer.serverType === 'proxy' && (
                            <span className="mono-label bg-(--accent-ghost) px-2 py-0.5 rounded-sm text-(--accent-light) flex items-center gap-1">
                                Proxy
                            </span>
                        )}
                        {selectedServer.proxyId && (() => {
                            const proxy = servers.find(s => s.id === selectedServer.proxyId);
                            return proxy ? (
                                <Link
                                    href={`/servers/${proxy.id}`}
                                    className="mono-label bg-(--accent-ghost) px-2 py-0.5 rounded-sm text-(--accent-light) flex items-center gap-1 hover:bg-(--accent)/20 transition-colors"
                                >
                                    {proxy.name}
                                </Link>
                            ) : null;
                        })()}
                        <RegionBadge region={selectedServer.region} size="sm" displayName />
                        <span className="text-xs text-(--base-07)">
                            Owner:{' '}
                            <button
                                type="button"
                                onClick={copyOwnerId}
                                title={`Owner ID: ${selectedServer.ownerId} — click to copy`}
                                className="font-medium text-(--base-09) hover:text-(--accent-light) transition-colors cursor-pointer"
                            >
                                {selectedServer.owner === user?.username ? 'You' : (selectedServer.owner || selectedServer.ownerId)}
                            </button>
                            {ownerCopied && <span className="ml-1 text-(--success-light)">copied</span>}
                        </span>
                    </div>
                    <div className="flex items-center gap-2">
                        <div className="flex items-center gap-1.5">
                            {isPendingSetup && (
                                <span className="mono-label font-medium px-2.5 py-1 rounded-sm border bg-(--warning-ghost) flex items-center gap-2 border-(--warning)/20 text-(--warning)">
                                    <div className="w-[5px] h-[5px] rounded-full bg-(--warning) animate-pulse"></div>
                                    pending_setup
                                </span>
                            )}
                            {isDiskFull && (
                                <span className="flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-(--error-ghost) border border-(--error-border) text-(--error-light) text-xs font-semibold">
                                    <HardDrive size={14} />
                                    Storage full
                                </span>
                            )}
                            {cooldownSecondsLeft > 0 && (
                                <span
                                    className="flex items-center gap-1.5 px-2.5 py-1 rounded-md bg-(--accent-ghost) border border-(--accent)/20 text-(--accent-light) text-xs font-mono"
                                    title="Server is finishing install — power actions will unlock when this counter reaches 0."
                                >
                                    <Clock size={12} />
                                    Settling {cooldownSecondsLeft}s
                                </span>
                            )}
                            {(isMigrating || selectedServer.status === 'starting') && (
                                <span
                                    className="mono-label font-medium px-2.5 py-1 rounded-sm border bg-(--warning-ghost) flex items-center gap-2 border-(--warning)/20 text-(--warning)"
                                    title={isMigrating ? 'Server is migrating to another node — power actions are locked until it finishes.' : 'Server is starting.'}
                                >
                                    <div className="w-[5px] h-[5px] rounded-full bg-(--warning) animate-pulse"></div>
                                    {selectedServer.status}
                                </span>
                            )}
                            <button
                                onClick={() => handlePower('start')}
                                disabled={!canPower || isPendingSetup || isDiskFull || powerWaiting || !isServerOffline || uploadLocked || powerCooldownActive || isMigrating}
                                className={`flex items-center gap-1.5 px-3 py-1.5 rounded-md transition-colors disabled:opacity-30 disabled:cursor-not-allowed border ${
                                    isServerOffline && !isPendingSetup && !isDiskFull && canPower
                                        ? 'bg-(--success) text-white border-(--success) hover:bg-(--success-light)'
                                        : 'bg-(--success-ghost) text-(--success-light) border-(--success)/15 hover:bg-(--success)/15'
                                }`}
                                title={powerCooldownActive ? `Server is settling — ${cooldownSecondsLeft}s remaining` : isDiskFull ? 'Storage full - delete files or raise the limit' : canPower ? 'Start server' : 'No permission'}
                            >
                                <Play size={16} />
                                <span className="text-xs font-semibold">Start</span>
                            </button>
                            <button
                                onClick={() => handlePower('restart')}
                                disabled={!canPower || isPendingSetup || isDiskFull || powerWaiting || isServerOffline || uploadLocked || powerCooldownActive || isMigrating}
                                className="flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-(--warning-ghost) hover:bg-(--warning)/15 transition-colors disabled:opacity-30 disabled:cursor-not-allowed border border-(--warning)/15"
                                title={powerCooldownActive ? `Server is settling — ${cooldownSecondsLeft}s remaining` : isDiskFull ? 'Storage full' : canPower ? 'Restart server' : 'No permission'}
                            >
                                <RotateCcw size={16} className="text-(--warning)" />
                                <span className="text-xs font-semibold text-(--warning)">Restart</span>
                            </button>
                            <button
                                onClick={() => handlePower('stop')}
                                disabled={!canPower || isPendingSetup || powerWaiting || isServerOffline || uploadLocked || powerCooldownActive || isMigrating}
                                className="flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-(--error-ghost) hover:bg-(--error)/15 transition-colors disabled:opacity-30 disabled:cursor-not-allowed border border-(--error)/15"
                                title={powerCooldownActive ? `Server is settling — ${cooldownSecondsLeft}s remaining` : canPower ? 'Stop server' : 'No permission'}
                            >
                                <Square size={16} className="text-(--error)" />
                                <span className="text-xs font-semibold text-(--error)">Stop</span>
                            </button>
                            <button
                                onClick={() => {
                                    if (cooldownSecondsLeft > 0 && user?.isAdmin) {
                                        setPendingCooldownAction('kill');
                                    } else {
                                        setShowKillConfirm(true);
                                    }
                                }}
                                disabled={!canPower || killCooldown || isServerOffline || uploadLocked || powerCooldownActive || isMigrating}
                                className="flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-(--error-ghost) hover:bg-(--error)/15 transition-colors disabled:opacity-30 disabled:cursor-not-allowed border border-(--error)/15"
                                title={powerCooldownActive ? `Server is settling — ${cooldownSecondsLeft}s remaining` : canPower ? 'Force kill container' : 'No permission'}
                            >
                                <Skull size={16} className="text-(--error)" />
                                <span className="text-xs font-semibold text-(--error)">Kill</span>
                            </button>
                            {isMigrating && user?.isAdmin && (
                                <button
                                    onClick={handleCancelMigration}
                                    disabled={cancellingMigration || migrationStatus?.cancellable === false}
                                    className="flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-(--error-ghost) hover:bg-(--error)/15 transition-colors disabled:opacity-30 disabled:cursor-not-allowed border border-(--error)/15"
                                    title="Cancel this migration and roll the server back to its current node. Only possible before cutover; the old node is kept until the move is verified."
                                >
                                    <Undo2 size={16} className="text-(--error)" />
                                    <span className="text-xs font-semibold text-(--error)">{cancellingMigration ? 'Cancelling...' : 'Cancel Migration'}</span>
                                </button>
                            )}
                        </div>
                        {user?.isAdmin && <div className="w-px h-6 bg-(--base-04)" />}
                        {user?.isAdmin && (
                            <div className="flex items-center gap-1.5">
                                <button
                                    onClick={handleOpenEditResources}
                                    disabled={uploadLocked}
                                    className="btn btn-secondary btn-sm disabled:opacity-30 disabled:cursor-not-allowed"
                                    title={uploadLocked ? 'Upload in progress — wait or cancel it first' : undefined}
                                >
                                    <SlidersHorizontal size={14} />
                                    Resources
                                </button>
                                <button
                                    onClick={handleOpenDeletePopup}
                                    disabled={uploadLocked}
                                    className="btn btn-danger btn-sm disabled:opacity-30 disabled:cursor-not-allowed"
                                    title={uploadLocked ? 'Upload in progress — wait or cancel it first' : undefined}
                                >
                                    <Trash2 size={14} />
                                    Delete
                                </button>
                            </div>
                        )}
                        {/* BYON owners get a dedicated transfer entry (admins use the
                            Resources popup's transfer section instead). */}
                        {canTransfer && !user?.isAdmin && (
                            <>
                                <div className="w-px h-6 bg-(--base-04)" />
                                <button
                                    onClick={handleOpenTransfer}
                                    disabled={uploadLocked}
                                    className="btn btn-secondary btn-sm disabled:opacity-30 disabled:cursor-not-allowed"
                                    title={uploadLocked ? 'Upload in progress — wait or cancel it first' : 'Transfer this server to another of your nodes'}
                                >
                                    <Move size={14} />
                                    Transfer
                                </button>
                            </>
                        )}
                    </div>
                </div>

                {/* Connection Info */}
                {!isPendingSetup && (
                    (routingMode !== 'gateway' && selectedServer.nodeAddress && (selectedServer.hostPort ?? 0) > 0) ||
                    gatewayEnabled
                ) && (
                    <div className={`flex items-center gap-4 py-2 border-t flex-wrap ${
                        routingMode === 'gateway' && gatewayEnabled && serverRoutes.length === 0
                            ? 'border-(--warning)/40 animate-pulse'
                            : 'border-(--base-03)/60'
                    }`}>
                        {routingMode !== 'gateway' && selectedServer.nodeAddress && (selectedServer.hostPort ?? 0) > 0 && (
                            <div className="flex items-center gap-2">
                                <span className="mono-label text-(--base-05)">Direct</span>
                                <div className="flex items-center gap-1.5 bg-(--base-03)/50 border border-(--base-04) rounded-md px-2.5 py-1">
                                    <Link2 size={11} className="text-(--base-06) shrink-0" />
                                    <span className="text-xs font-mono text-(--base-07)">{selectedServer.nodeAddress}:{selectedServer.hostPort}</span>
                                    <button
                                        onClick={() => navigator.clipboard.writeText(`${selectedServer.nodeAddress}:${selectedServer.hostPort}`)}
                                        className="text-(--base-06) hover:text-(--base-09) transition-colors ml-0.5"
                                        title="Copy address"
                                    >
                                        <Copy size={11} />
                                    </button>
                                </div>
                            </div>
                        )}
                        {gatewayEnabled && serverRoutes.length > 0 && (
                            <div className="flex items-center gap-2 flex-wrap">
                                <span className="mono-label text-(--base-05)">Gateway</span>
                                {serverRoutes.slice(0, 3).map(route => (
                                    <div key={route.domain} className="flex items-center gap-1.5 bg-(--accent-ghost) border border-(--accent-border) rounded-md px-2.5 py-1">
                                        <Globe size={11} className="text-(--accent-light) shrink-0" />
                                        <span className="text-xs font-mono text-(--accent-light)">{route.domain}</span>
                                        <button
                                            onClick={() => navigator.clipboard.writeText(route.domain)}
                                            className="text-(--accent-light)/60 hover:text-(--accent-light) transition-colors ml-0.5"
                                            title="Copy domain"
                                        >
                                            <Copy size={11} />
                                        </button>
                                    </div>
                                ))}
                            </div>
                        )}
                        {routingMode === 'gateway' && gatewayEnabled && serverRoutes.length === 0 && (
                            <div className="flex items-center gap-2">
                                <AlertTriangle size={12} className="text-(--warning)" />
                                <span className="text-xs text-(--warning) font-medium">No gateway route configured</span>
                            </div>
                        )}
                        {gatewayEnabled && (
                            <button
                                onClick={() => setShowRoutesModal(true)}
                                className={`ml-auto flex items-center gap-1.5 px-2.5 py-1 rounded-md bg-(--accent-ghost) border border-(--accent-border) text-(--accent-light) text-xs font-medium hover:bg-(--accent)/15 transition-colors ${serverRoutes.length === 0 ? 'shine-border' : ''}`}
                                title="Manage gateway routes &amp; domains"
                            >
                                <Globe size={12} />
                                Routes
                                <span className="mono-label text-(--accent-light)">{serverRoutes.length}</span>
                            </button>
                        )}
                    </div>
                )}

                {/* Tab Bar — wraps onto a second row instead of scrolling so
                    no tab ever ends up hidden under the right edge in
                    narrow windows (esp. the Beam Desktop App). */}
                <div className="flex flex-wrap gap-x-1 gap-y-0">
                    {[
                        ...tabs,
                        // Append any user-defined custom tabs after the
                        // built-ins. Disabled rows are filtered out so the nav
                        // matches the user's intent without extra greying logic.
                        ...customTabs
                            // A tab pinned to another sub-server addresses a
                            // port that only exists while that one runs; Core
                            // refuses it, so it does not belong in the bar.
                            .filter(t => t.enabled && tabRunsOnActiveSubServer(t.subServerName, selectedServer.activeSubServer))
                            .map(t => ({
                                slug: `t/${t.id}`,
                                icon: t.icon || 'layout-grid',
                                label: t.name,
                                disabled: false,
                            })),
                    ].map(tab => {
                        const href = `/servers/${selectedServer.id}/${tab.slug}`;
                        // Configuration is a sub-tab parent (config/properties,
                        // config/display, …); match the whole prefix so the top-level tab stays
                        // highlighted while you click between sub-tabs.
                        const isActive = tab.slug === 'config'
                            ? pathname.startsWith(href)
                            : pathname === href;
                        if (tab.disabled) {
                            return (
                                <span
                                    key={tab.slug}
                                    className="flex items-center space-x-2 px-4 py-3 border-b-2 border-transparent text-(--base-06) opacity-30 cursor-not-allowed whitespace-nowrap"
                                >
                                    <DynamicIcon name={tab.icon} size={20} />
                                    <span>{tab.label}</span>
                                </span>
                            );
                        }
                        return (
                            <Link
                                key={tab.slug}
                                href={href}
                                replace
                                className={`flex items-center space-x-2 px-4 py-3 border-b-2 transition-all whitespace-nowrap ${
                                    isActive
                                        ? 'border-(--accent) text-(--accent-light) font-semibold'
                                        : 'border-transparent text-(--base-07) hover:text-(--base-09) hover:border-(--base-04)'
                                }`}
                            >
                                <DynamicIcon name={tab.icon} size={20} />
                                <span>{tab.label}</span>
                            </Link>
                        );
                    })}
                </div>
            </div>

            {uploadLocked && (
                <div className="px-6 pt-3 shrink-0">
                    <div className="flex items-center gap-2.5 px-3 py-2 rounded-md bg-(--accent-ghost) border border-(--accent-border) text-(--accent-light) text-xs">
                        <Upload size={13} className="shrink-0" />
                        <span>
                            <strong className="font-semibold">Upload in progress</strong> — destructive actions
                            (power, resources, delete, config) are locked until the transfer finishes or is cancelled.
                            The file browser stays available.
                        </span>
                    </div>
                </div>
            )}

            <div className="flex-1 overflow-y-auto p-6">{children}</div>

            {/* Delete Confirmation Popup */}
            {showDeletePopup && (
                <div className="modal-overlay animate-fade-in">
                    <div className="modal-panel w-full max-w-md">
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
                    </div>
                </div>
            )}

            {/* Cooldown override (admin only). Lets the admin push through
                the post-install settling window after explicit confirmation
                instead of bypassing it silently. For Kill we hand off to the
                existing destructive-confirm modal so the admin sees both
                warnings -- they're independent risks. */}
            {pendingCooldownAction && (
                <div className="modal-overlay animate-fade-in" onClick={() => setPendingCooldownAction(null)}>
                    <div className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--warning-light)">
                                <AlertTriangle size={20} />
                                Server is still installing
                            </h3>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-07)">
                                The post-install settling window has{' '}
                                <span className="font-mono text-(--accent-light)">{cooldownSecondsLeft}s</span>{' '}
                                left. Power actions during this window can leave the freshly-installed world in a half-written state or kill the JVM mid chunk-generation.
                            </p>
                            <p className="text-sm text-(--base-07)">
                                As an admin you can override the cooldown — only do this if you understand the consequences.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setPendingCooldownAction(null)} className="btn btn-secondary flex-1">
                                Wait for settle
                            </button>
                            <button
                                onClick={() => {
                                    const action = pendingCooldownAction;
                                    setPendingCooldownAction(null);
                                    if (action === 'kill') {
                                        // Kill has its own dedicated confirm; chain to it.
                                        setShowKillConfirm(true);
                                    } else if (action) {
                                        handlePower(action, { skipCooldownPrompt: true });
                                    }
                                }}
                                className="btn btn-danger flex-1"
                            >
                                Override &amp; continue
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Kill Confirmation */}
            {showKillConfirm && (
                <div className="modal-overlay animate-fade-in" onClick={() => setShowKillConfirm(false)}>
                    <div className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <Skull size={20} className="text-(--error)" />
                                Kill Server?
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                The container will be stopped immediately. Unsaved data will be lost.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setShowKillConfirm(false)} className="btn btn-secondary">Cancel</button>
                            <button
                                onClick={() => {
                                    // skipCooldownPrompt because if cooldown was active we
                                    // routed through the override modal first; re-firing
                                    // it here would chain a second prompt for the same
                                    // decision.
                                    handlePower('kill', { skipCooldownPrompt: true });
                                    setShowKillConfirm(false);
                                }}
                                className="btn btn-danger"
                            >
                                Kill
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Transfer Popup (BYON owners). Admins use the Resources popup. */}
            {showTransferPopup && (
                <div className="modal-overlay animate-fade-in">
                    <div className="modal-panel w-full max-w-md">
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <Move size={18} />
                                Transfer Server
                            </h3>
                        </div>
                        <div className="modal-body space-y-4">
                            {moveNodes.length === 0 ? (
                                <p className="text-sm text-(--base-06)">No other online nodes of yours are available to transfer to.</p>
                            ) : (
                                <>
                                    <div className="flex flex-col gap-[5px]">
                                        <label className="mono-label">Target Node</label>
                                        <select value={moveTargetNodeId} onChange={e => setMoveTargetNodeId(Number(e.target.value))} className="input-field text-sm" disabled={moveBusy}>
                                            {moveNodes.map(n => (
                                                <option key={n.id} value={n.id}>{n.name}{n.region ? ` — ${n.region}` : ''}</option>
                                            ))}
                                        </select>
                                    </div>
                                    <p className="text-xs text-(--base-06)">
                                        The server is stopped, moved to the target node, and restarted there. The gateway keeps the player address stable. Same-LAN moves transfer directly; otherwise the data goes via your backup storage.
                                    </p>
                                </>
                            )}
                            {moveMsg && (
                                <p className={`text-xs ${/queued|migration/i.test(moveMsg) ? 'text-(--success-light)' : 'text-(--error-light)'}`}>{moveMsg}</p>
                            )}
                            {migrationStatus && migrationStatus.phase !== 'none' && (
                                <div className="flex items-start gap-2 text-xs">
                                    {!TERMINAL_PHASES.includes(migrationStatus.phase)
                                        ? <RefreshCw size={13} className="animate-spin text-(--accent-light) mt-0.5 shrink-0" />
                                        : migrationStatus.phase === 'done'
                                            ? <Move size={13} className="text-(--success-light) mt-0.5 shrink-0" />
                                            : <AlertTriangle size={13} className="text-(--error-light) mt-0.5 shrink-0" />}
                                    <span className={migrationStatus.phase === 'done' ? 'text-(--success-light)' : (TERMINAL_PHASES.includes(migrationStatus.phase) ? 'text-(--error-light)' : 'text-(--base-07)')}>
                                        Migration: {migrationStatus.phase.replace(/_/g, ' ')}
                                        {migrationStatus.error ? ` — ${migrationStatus.error}` : ''}
                                    </span>
                                </div>
                            )}
                        </div>
                        <div className="modal-footer">
                            <button type="button" onClick={() => setShowTransferPopup(false)} className="btn btn-ghost btn-sm">Close</button>
                            <button type="button" onClick={handleMoveServer} disabled={moveBusy || !moveTargetNodeId} className="btn btn-primary btn-sm">
                                {moveBusy ? <><RefreshCw size={14} className="animate-spin" /> Queuing...</> : <><Move size={14} /> Transfer</>}
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Edit Resources Popup */}
            {showEditResourcesPopup && (
                <div className="modal-overlay animate-fade-in">
                    <div className="modal-panel w-full max-w-lg">
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <SlidersHorizontal size={18} />
                                Edit Resources
                            </h3>
                        </div>
                        <div className="modal-body space-y-4">
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">RAM (MB)</label>
                                <input type="number" min={256} step={256} value={editRam} onChange={e => setEditRam(Number(e.target.value))} className="input-field w-full" />
                            </div>
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">CPU Limit (Cores)</label>
                                <input type="number" min={0} step={0.5} value={editCpuLimit} onChange={e => setEditCpuLimit(Number(e.target.value))} placeholder="0 = unlimited" className="input-field w-full" />
                                <p className="text-xs text-(--base-06)">0 = no limit. Example: 2.0 = 2 cores</p>
                            </div>
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">Storage Limit (GB)</label>
                                <input type="number" min={0} step={1} value={editDiskLimit} onChange={e => setEditDiskLimit(Number(e.target.value))} placeholder="0 = unlimited" className="input-field w-full" />
                                <p className="text-xs text-(--base-06)">0 = unlimited</p>
                            </div>

                            {/* Demo flag (admin). A flagged server shows up read-only
                                in the sidebar for any user who has no server of their
                                own — a public showcase. Writes stay blocked server-side.
                                Only on the hosted (store-enabled) build. */}
                            {featureFlags.store && (
                            <div className="border-t border-(--base-03) pt-4">
                                <div className="flex items-center justify-between gap-4">
                                    <div className="flex items-start gap-2.5 min-w-0">
                                        <Globe size={14} className={`shrink-0 mt-0.5 ${selectedServer.isDemo ? 'text-(--accent-light)' : 'text-(--base-06)'}`} />
                                        <div className="min-w-0">
                                            <div className="text-sm font-medium text-(--base-09)">Public Demo Server</div>
                                            <p className="text-xs text-(--base-06)">
                                                Show this server read-only to users who have none of their own. They can view overview, console, stats and browse files, but cannot edit, power, download or upload.
                                            </p>
                                        </div>
                                    </div>
                                    <button
                                        type="button"
                                        role="switch"
                                        aria-checked={!!selectedServer.isDemo}
                                        onClick={handleToggleDemo}
                                        className={`shrink-0 toggle-track ${selectedServer.isDemo ? 'toggle-track-on' : 'toggle-track-off'}`}
                                    >
                                        <span className={`toggle-knob ${selectedServer.isDemo ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                                    </button>
                                </div>
                                {demoMsg && (
                                    <p className="mt-2 text-xs text-(--error-light)">{demoMsg}</p>
                                )}
                            </div>
                            )}

                            {/* Auto-move opt-in. Disabled when the platform feature
                                is off or routing is on IP:Port (gateway off) — the
                                backend would 503 either way. */}
                            <div className={`border-t border-(--base-03) pt-4 ${autoMoveDisabled ? 'opacity-60' : ''}`}>
                                <div className="flex items-center justify-between gap-4">
                                    <div className="flex items-start gap-2.5 min-w-0">
                                        <Move size={14} className={`shrink-0 mt-0.5 ${editAutoMove && !autoMoveDisabled ? 'text-(--accent-light)' : 'text-(--base-06)'}`} />
                                        <div className="min-w-0">
                                            <div className="text-sm font-medium text-(--base-09)">Auto-Move</div>
                                            <p className="text-xs text-(--base-06)">
                                                Let the rebalance worker migrate this server to a less-loaded node when the current node is overloaded. Migration only runs while the server is stopped/idle.
                                            </p>
                                        </div>
                                    </div>
                                    <button
                                        type="button"
                                        role="switch"
                                        aria-checked={editAutoMove}
                                        disabled={autoMoveDisabled}
                                        onClick={() => setEditAutoMove(v => !v)}
                                        className={`shrink-0 toggle-track ${editAutoMove ? 'toggle-track-on' : 'toggle-track-off'} disabled:cursor-not-allowed`}
                                    >
                                        <span className={`toggle-knob ${editAutoMove ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                                    </button>
                                </div>
                                {autoMoveDisabled && (
                                    <p className="flex items-start gap-1.5 text-xs text-(--base-06) mt-2">
                                        <AlertTriangle size={12} className="mt-0.5 shrink-0 text-(--warning-light)" />
                                        <span>{autoMoveReason}</span>
                                    </p>
                                )}
                            </div>

                            {/* Node-to-node transfer. Admins move between any nodes;
                                a BYON owner transfers between their own nodes. Migration
                                is gateway-only — hide the control entirely on IP:Port
                                instead of dangling a disabled picker. */}
                            {canTransfer && (
                                <div className="border-t border-(--base-03) pt-4 flex flex-col gap-3">
                                    <div className="flex items-center gap-2">
                                        <Move size={14} className="text-(--accent-light)" />
                                        <span className="input-label mb-0">Transfer to Node</span>
                                    </div>
                                    {moveNodes.length === 0 ? (
                                        <p className="text-xs text-(--base-05) italic">No other online nodes available to transfer to.</p>
                                    ) : (
                                        <>
                                            <div className="flex gap-2 items-end">
                                                <div className="flex flex-col gap-[5px] flex-1 min-w-0">
                                                    <label className="mono-label">Target Node</label>
                                                    <select value={moveTargetNodeId} onChange={e => setMoveTargetNodeId(Number(e.target.value))} className="input-field text-sm" disabled={moveBusy}>
                                                        {moveNodes.map(n => (
                                                            <option key={n.id} value={n.id}>{n.name}{n.region ? ` — ${n.region}` : ''}</option>
                                                        ))}
                                                    </select>
                                                </div>
                                                <button type="button" onClick={handleMoveServer} disabled={moveBusy || !moveTargetNodeId} className="btn btn-secondary btn-sm shrink-0">
                                                    {moveBusy ? <><RefreshCw size={14} className="animate-spin" /> Queuing...</> : <><Move size={14} /> Move</>}
                                                </button>
                                            </div>
                                            <p className="text-xs text-(--base-06)">Live migration to another node. The gateway keeps the player address stable; the server is moved while idle.</p>
                                        </>
                                    )}
                                    {moveMsg && (
                                        <p className={`text-xs ${/queued|migration/i.test(moveMsg) ? 'text-(--success-light)' : 'text-(--error-light)'}`}>{moveMsg}</p>
                                    )}
                                    {migrationStatus && migrationStatus.phase !== 'none' && (
                                        <div className="flex items-start gap-2 text-xs">
                                            {!TERMINAL_PHASES.includes(migrationStatus.phase)
                                                ? <RefreshCw size={13} className="animate-spin text-(--accent-light) mt-0.5 shrink-0" />
                                                : migrationStatus.phase === 'done'
                                                    ? <Move size={13} className="text-(--success-light) mt-0.5 shrink-0" />
                                                    : <AlertTriangle size={13} className="text-(--error-light) mt-0.5 shrink-0" />}
                                            <span className={migrationStatus.phase === 'done' ? 'text-(--success-light)' : (TERMINAL_PHASES.includes(migrationStatus.phase) ? 'text-(--error-light)' : 'text-(--base-07)')}>
                                                Migration: {migrationStatus.phase.replace(/_/g, ' ')}
                                                {migrationStatus.error ? ` — ${migrationStatus.error}` : ''}
                                            </span>
                                        </div>
                                    )}
                                </div>
                            )}

                            {user?.isAdmin && (
                                <div className="border-t border-(--base-03) pt-4 grid grid-cols-2 gap-3">
                                    <div className="flex flex-col gap-[5px]">
                                        <label className="input-label">Host Port</label>
                                        <input type="number" min={0} max={65535} value={editHostPort} onChange={e => setEditHostPort(Number(e.target.value))} placeholder="0 = auto" className="input-field w-full" />
                                        <p className="text-xs text-(--base-06)">0 = auto from range</p>
                                    </div>
                                    <div className="flex flex-col gap-[5px]">
                                        <label className="input-label">Container Port</label>
                                        <input type="number" min={1} max={65535} value={editContainerPort} onChange={e => setEditContainerPort(Number(e.target.value))} className="input-field w-full" />
                                    </div>
                                </div>
                            )}

                            {user?.isAdmin && storagePaths.length >= 1 && (
                                <div className="border-t border-(--base-03) pt-4 flex flex-col gap-3">
                                    <div className="flex items-center gap-2">
                                        <MoveHorizontal size={14} className="text-(--accent-light)" />
                                        <span className="input-label mb-0">{storagePaths.length > 1 ? 'Storage Path Migration' : 'Storage Path'}</span>
                                    </div>
                                    <div className="flex flex-col gap-[5px]">
                                        <label className="mono-label">Current Path</label>
                                        <p className="text-xs font-mono text-(--base-07) bg-(--base-02) border border-(--base-03) rounded-md px-3 py-2 truncate">
                                            {storageCurrentPath || <span className="text-(--base-05) italic">unknown</span>}
                                        </p>
                                    </div>
                                    {storagePaths.length > 1 && (
                                        <>
                                            <div className="flex gap-2 items-end">
                                                <div className="flex flex-col gap-[5px] flex-1 min-w-0">
                                                    <label className="mono-label">Migrate To</label>
                                                    <select value={storageMigrateTarget} onChange={e => setStorageMigrateTarget(e.target.value)} className="input-field text-sm" disabled={storageMigrating}>
                                                        {storagePaths.filter(p => p.path !== storageCurrentPath).map(p => (
                                                            <option key={p.path} value={p.path}>{p.path} — {(p.free_bytes / 1073741824).toFixed(1)} GB free</option>
                                                        ))}
                                                    </select>
                                                </div>
                                                <button type="button" onClick={handleMigrateStorage} disabled={storageMigrating || !storageMigrateTarget} className="btn btn-secondary btn-sm shrink-0">
                                                    {storageMigrating ? <><RefreshCw size={14} className="animate-spin" /> Moving...</> : <><MoveHorizontal size={14} /> Migrate</>}
                                                </button>
                                            </div>
                                            {storageMigrateMsg && (
                                                <p className={`text-xs ${storageMigrateMsg.includes('queued') || storageMigrateMsg.includes('Migration') ? 'text-(--success-light)' : 'text-(--error-light)'}`}>
                                                    {storageMigrateMsg}
                                                </p>
                                            )}
                                            <p className="text-xs text-(--base-06)">Server will be stopped during migration. Restart it manually after.</p>
                                        </>
                                    )}
                                </div>
                            )}

                            {user?.isAdmin && (
                                <div className="border-t border-(--base-03) pt-4">
                                    <button type="button" onClick={() => setEditResourcesAdvancedOpen(o => !o)} className="flex items-center gap-2 text-xs text-(--base-06) hover:text-(--base-08) transition-colors w-full">
                                        <ChevronDown size={13} className={`transition-transform ${editResourcesAdvancedOpen ? 'rotate-180' : ''}`} />
                                        Advanced
                                    </button>
                                    {editResourcesAdvancedOpen && (
                                        <div className="mt-3 flex flex-col gap-2">
                                            <CpuPinningControl
                                                nodeId={selectedServer.nodeId}
                                                mode={editCpuMode}
                                                cpuset={editCpusetCpus}
                                                cpuLimit={editCpuLimit}
                                                onChange={({ mode, cpuset }) => { setEditCpuMode(mode); setEditCpusetCpus(cpuset); }}
                                            />
                                            <p className="text-xs text-(--base-06)">Pin this container to specific CPU cores (useful for AMD 3D V-Cache). Resets if the server is migrated to a different node.</p>
                                        </div>
                                    )}
                                </div>
                            )}
                        </div>
                        {resourcesMsg && (
                            <div className="px-5 pb-1">
                                <div className="alert alert-error text-xs" role="alert">{resourcesMsg}</div>
                            </div>
                        )}
                        <div className="modal-footer">
                            <button onClick={() => { setShowEditResourcesPopup(false); setResourcesMsg(''); }} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runSaveResources(handleSaveResources)} disabled={savingResources} className="btn btn-primary disabled:opacity-40">Save & Restart</button>
                        </div>
                    </div>
                </div>
            )}

            {showRoutesModal && (
                <RoutesModal
                    serverId={selectedServer.id}
                    serverName={selectedServer.name}
                    onClose={() => setShowRoutesModal(false)}
                    onRoutesChanged={setServerRoutes}
                />
            )}

            {/* Floating toast for power-action rejections (cooldown 429s,
                permission errors, etc.). Auto-dismisses after a few seconds. */}
            {powerError && (
                <div className="fixed bottom-6 right-6 z-50 flex items-start gap-2.5 px-4 py-3 rounded-md bg-(--error-ghost) border border-(--error)/30 text-(--error-light) text-sm shadow-lg max-w-sm">
                    <AlertTriangle size={16} className="shrink-0 mt-0.5" />
                    <span>{powerError}</span>
                </div>
            )}
        </main>
    );
}
