"use client";

import { useEffect, useState } from 'react';
import {
    Drawer, DrawerBackdrop, DrawerClose, DrawerContent, DrawerDescription, DrawerTitle,
} from '@ui-sdk/react/drawer';
import {
    AlertTriangle, Cpu, Globe, HardDrive, MoveHorizontal, Move, Plug, RefreshCw, SlidersHorizontal, Wrench, X,
} from 'lucide-react';
import {
    updateServerResources, getServerStoragePath, migrateServerStorage, setServerAutoMove,
    moveServer, transferServer, setServerDemo, getPlacementSettings,
    type MigrationStatus, type Node, type Server, type StoragePathInfo,
} from '@/lib/api';
import { getNodes } from '@/lib/api/resources';
import { useAppData } from '@/lib/AppDataContext';
import { useBusy } from '@/lib/useBusy';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';
import Tabs from '@/components/ui/Tabs';
import NumberField from '@/components/ui/NumberField';
import ResourceSlider from '@/components/ResourceSlider';
import CpuPinningControl from '@/components/CpuPinningControl';
import Callout from '@/components/server/Callout';
import { DISK_SCALE_GB, cpuScale, ramScale } from '@/lib/resourceSlider';
import { formatMB } from '@/lib/format';
import { nodeLabelById, nodePickerLabel } from '@/lib/nodeLabel';
import { planResourceChange, diskGBToMB, joinFields, type ResourceValues } from '@/lib/resourceChanges';
import { advancedStartsOpen, DEFAULT_CONTAINER_PORT, portsEditable } from '@/lib/resourceDialog';
import {
    MAX_RAM_PADDING_MB, RAM_PADDING_HELP, inheritedRamPadding, parseRamPaddingInput, ramPaddingPatch, resetLabel,
} from '@/lib/ramPadding';
import { MEMORY_GUARD_HELP, MEMORY_GUARD_OPTIONS, memoryGuardPatch } from '@/lib/memoryGuard';
import type { MemoryGuardAction } from '@/lib/api/types';
import { pinMismatch } from '@/lib/cpuset';

export const TERMINAL_PHASES = ['done', 'failed', 'failed_post_cutover', 'aborted_players', 'cancelled', 'none'];

type SectionId = 'resources' | 'placement' | 'storage' | 'ports' | 'advanced';

// A titled block inside a settings tab; blocks separate by space, not by rules.
function Section({ icon, title, children }: { icon?: React.ReactNode; title: string; children: React.ReactNode }) {
    return (
        <section className="srv-surface flex flex-col gap-4 p-5">
            <h3 className="flex items-center gap-2 text-sm font-semibold text-(--base-09)">
                {icon && <span className="text-(--base-06)" aria-hidden="true">{icon}</span>}
                {title}
            </h3>
            {children}
        </section>
    );
}

function Msg({ text, ok }: { text: string; ok: boolean }) {
    return <p className={`text-xs ${ok ? 'text-(--success-light)' : 'text-(--error-light)'}`}>{text}</p>;
}

/**
 * Every server-level setting the old header kept in modals and inline panels:
 * resources, placement and transfer, storage, ports, advanced. `mode` "transfer"
 * is the BYON owner's cut of it (no resource editor, only the transfer block).
 */
export default function ServerSettingsDrawer({
    open, onOpenChange, mode, server, canTransfer, migrationStatus, setMigrationStatus, focusAdvanced,
}: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    mode: 'full' | 'transfer';
    server: Server;
    canTransfer: boolean;
    migrationStatus: MigrationStatus | null;
    setMigrationStatus: (m: MigrationStatus) => void;
    /** Open on the Advanced tab (RAM headroom, memory guard) regardless of its contents. */
    focusAdvanced?: boolean;
}) {
    const { user, refreshServers, routingMode, featureFlags } = useAppData();
    const isAdmin = !!user?.isAdmin;
    const full = mode === 'full';

    const [section, setSection] = useState<SectionId>(full ? 'resources' : 'placement');
    const [savingResources, runSaveResources] = useBusy();
    const [editRam, setEditRam] = useState(0);
    const [editCpuLimit, setEditCpuLimit] = useState(0);
    const [editDiskLimit, setEditDiskLimit] = useState(0);
    const [editCpusetCpus, setEditCpusetCpus] = useState('');
    const [editCpuMode, setEditCpuMode] = useState<'shared' | 'auto' | 'manual'>('shared');
    const [editHostPort, setEditHostPort] = useState(0);
    const [editContainerPort, setEditContainerPort] = useState(25565);
    const [editAutoMove, setEditAutoMove] = useState(false);
    // RAM headroom override as typed (empty = inherit) and what inheriting
    // resolves to; null until the node and global defaults are loaded.
    const [editPaddingText, setEditPaddingText] = useState('');
    const [paddingInherited, setPaddingInherited] = useState<{ value: number; source: 'node' | 'global' } | null>(null);
    const [editMemoryGuard, setEditMemoryGuard] = useState<MemoryGuardAction>('off');
    const [resourcesMsg, setResourcesMsg] = useState('');
    const [demoMsg, setDemoMsg] = useState('');

    // Every node the placement scope returned, the current one included, so the
    // move source and a running migration's endpoints can be named.
    const [placementNodes, setPlacementNodes] = useState<Node[]>([]);
    const [moveNodes, setMoveNodes] = useState<Node[]>([]);
    const [moveTargetNodeId, setMoveTargetNodeId] = useState<number>(0);
    const [moveBusy, setMoveBusy] = useState(false);
    const [moveMsg, setMoveMsg] = useState('');

    const [storageCurrentPath, setStorageCurrentPath] = useState('');
    const [storagePaths, setStoragePaths] = useState<StoragePathInfo[]>([]);
    const [storageMigrateTarget, setStorageMigrateTarget] = useState('');
    const [storageMigrating, setStorageMigrating] = useState(false);
    const [storageMigrateMsg, setStorageMigrateMsg] = useState('');

    const canEditPorts = portsEditable(isAdmin, routingMode);
    const paddingSupported = server.effectiveRamPaddingMb !== undefined;
    const memoryGuardSupported = server.memoryGuardAction !== undefined;
    const isServerOffline = ['stopped', 'offline', 'pending_setup', 'disk_full'].includes(server.status);
    const autoMoveDisabled = !featureFlags.autoMove;
    const autoMoveReason = 'Auto-Move is disabled platform-wide. An admin can enable it in Settings → Features.';

    // Seeds the form from the server as it is at the moment the drawer opens,
    // and loads what only admins and movers need. Not re-run while open: a
    // refresh of the server list must not overwrite what is being typed.
    useEffect(() => {
        if (!open) return;
        let cancelled = false;
        setSection(!full ? 'placement' : focusAdvanced || advancedStartsOpen({
            paddingText: server.ramPaddingMb != null ? String(server.ramPaddingMb) : '',
            canEditPorts,
            hostPort: server.hostPort || 0,
            containerPort: server.containerPort || DEFAULT_CONTAINER_PORT,
            memoryGuardAction: server.memoryGuardAction,
        }) ? 'advanced' : 'resources');
        setResourcesMsg('');
        setDemoMsg('');
        setMoveMsg('');
        setMoveTargetNodeId(0);
        setMoveNodes([]);
        setStorageCurrentPath('');
        setStoragePaths([]);
        setStorageMigrateTarget('');
        setStorageMigrateMsg('');
        setPaddingInherited(null);
        if (full) {
            setEditRam(server.memory || 1024);
            setEditCpuLimit(server.cpuLimit || 0);
            setEditDiskLimit((server.diskLimit || 0) / 1024);
            setEditHostPort(server.hostPort || 0);
            setEditContainerPort(server.containerPort || 25565);
            setEditCpuMode(server.cpuPinningMode || 'shared');
            // Manual cpuset comes from the dedicated cpuset field; fall back to the
            // legacy cpusetCpus so older servers still show their pinning.
            setEditCpusetCpus(server.cpuset || server.cpusetCpus || '');
            setEditAutoMove(!!(server as any).autoMove);
            setEditPaddingText(server.ramPaddingMb != null ? String(server.ramPaddingMb) : '');
            setEditMemoryGuard(server.memoryGuardAction ?? 'off');

            if (isAdmin && server.effectiveRamPaddingMb !== undefined) {
                Promise.all([getPlacementSettings(), getNodes()]).then(([ps, ns]) => {
                    if (cancelled) return;
                    const node: Node | undefined = ns?.success && Array.isArray(ns.nodes)
                        ? ns.nodes.find((n: Node) => n.id === server.nodeId) : undefined;
                    if (!ps?.success || !node) return;
                    setPaddingInherited(inheritedRamPadding(node.ramPaddingMb, ps.settings?.ramPaddingMb));
                }).catch(() => { /* non-fatal: the source just reads "inherited" */ });
            }
            if (isAdmin) {
                getServerStoragePath(server.id).then(res => {
                    if (cancelled || !res.success) return;
                    setStorageCurrentPath(res.currentPath || '');
                    const paths: StoragePathInfo[] = Array.isArray(res.storagePaths) ? res.storagePaths : [];
                    setStoragePaths(paths);
                    const others = paths.filter(p => p.path !== res.currentPath);
                    if (others.length > 0) setStorageMigrateTarget(others[0].path);
                }).catch(() => { /* ignore */ });
            }
        }
        // Node list for the move/transfer picker: only ONLINE nodes other than
        // the one this server lives on, and only ones this caller may place on.
        if (canTransfer) {
            getNodes('placement').then(res => {
                if (cancelled || !res.success || !Array.isArray(res.nodes)) return;
                const targets: Node[] = res.nodes.filter((n: Node) => n.status === 'online' && n.id !== server.nodeId);
                setPlacementNodes(res.nodes);
                setMoveNodes(targets);
                if (targets.length > 0) setMoveTargetNodeId(targets[0].id);
            }).catch(() => { /* ignore */ });
        }
        return () => { cancelled = true; };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [open]);

    const currentResources: ResourceValues = {
        ram: server.memory || 1024,
        cpuLimit: server.cpuLimit || 0,
        diskMB: server.diskLimit || 0,
        hostPort: server.hostPort || 0,
        containerPort: server.containerPort || 25565,
        cpuMode: server.cpuPinningMode || 'shared',
        cpuset: server.cpuset || server.cpusetCpus || '',
        autoMove: !!(server as any).autoMove,
        ramPaddingMb: server.effectiveRamPaddingMb,
    };
    const currentPaddingOverride = server.ramPaddingMb ?? null;
    const editPadding = parseRamPaddingInput(editPaddingText);
    const editPaddingInvalid = Number.isNaN(editPadding);
    // While the server inherits, its effective value IS the inherited one, so
    // the reset label needs no extra fetch in that case.
    const inheritedPaddingValue = paddingInherited?.value
        ?? (currentPaddingOverride === null ? server.effectiveRamPaddingMb : undefined);
    const editPaddingEffective = editPaddingInvalid
        ? server.effectiveRamPaddingMb
        : editPadding ?? inheritedPaddingValue ?? null;
    const resourcePlan = planResourceChange(currentResources, {
        ram: editRam,
        cpuLimit: editCpuLimit,
        diskMB: diskGBToMB(editDiskLimit),
        hostPort: editHostPort,
        containerPort: editContainerPort,
        cpuMode: editCpuMode,
        cpuset: editCpusetCpus,
        autoMove: editAutoMove,
        ramPaddingMb: paddingSupported ? editPaddingEffective : server.effectiveRamPaddingMb,
    }, !isServerOffline, canEditPorts);
    const pinProblem = pinMismatch(editCpuLimit, editCpuMode, editCpusetCpus);

    const handleSaveResources = async () => {
        const ports = canEditPorts ? { hostPort: editHostPort, containerPort: editContainerPort } : undefined;
        setResourcesMsg('');
        if (paddingSupported && editPaddingInvalid) {
            setSection('advanced');
            setResourcesMsg(`RAM headroom must be a whole number of MB from 0 to ${MAX_RAM_PADDING_MB}, or empty for the default.`);
            return;
        }
        if (resourcePlan.needsRestart && !(await confirmDialog({
            title: 'Restart the server?',
            message: `Changing ${joinFields(resourcePlan.restartFields)} restarts ${server.name}. Players on it are disconnected.`,
            confirmLabel: 'Save and restart',
        }))) {
            return;
        }
        // A refusal (host port already taken, a cpuset the node cannot honour,
        // over the plan's limits) must not look like a save.
        const res = await updateServerResources(
            server.id, editRam, editCpuLimit, diskGBToMB(editDiskLimit),
            ports, undefined, { mode: editCpuMode, cpuset: editCpusetCpus },
            paddingSupported ? ramPaddingPatch(currentPaddingOverride, editPadding) : undefined,
            memoryGuardPatch(server.memoryGuardAction, editMemoryGuard),
        );
        if (res?.success === false) {
            setResourcesMsg(res.message || res.error || 'The resource change was refused.');
            return;
        }
        // Auto-move is a separate field: flip only when it actually changed.
        if (editAutoMove !== !!(server as any).autoMove) {
            const moveRes = await setServerAutoMove(server.id, editAutoMove);
            if (moveRes?.success === false) {
                // The resources DID save, so say precisely that.
                setResourcesMsg(moveRes.message || moveRes.error || 'Resources saved, but auto-move could not be changed.');
                refreshServers();
                return;
            }
        }
        onOpenChange(false);
        refreshServers();
    };

    // Node-to-node move. Admins use the admin endpoint; BYON owners the tenant
    // transfer endpoint. Async on the backend; the migration status is seeded
    // optimistically so the shell's poll picks it up without waiting a tick.
    const handleMoveServer = async () => {
        if (!moveTargetNodeId || moveBusy) return;
        if (!(await confirmDialog({
            title: 'Move server',
            message: `Move ${server.name} from ${nodeLabelById(placementNodes, server.nodeId)} to ${nodeLabelById(placementNodes, moveTargetNodeId)}? It is stopped, its files are copied, and it starts on the target. Players on it are disconnected.`,
            confirmLabel: 'Move',
            destructive: false,
        }))) return;
        setMoveBusy(true);
        setMoveMsg('');
        try {
            const res: any = isAdmin
                ? await moveServer(server.id, moveTargetNodeId)
                : await transferServer(server.id, moveTargetNodeId);
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
    const migrationLine = (m: MigrationStatus) => {
        const phase = m.phase.replace(/_/g, ' ');
        if (!m.sourceNodeID || !m.targetNodeID) return `Migration: ${phase}`;
        return `Migration ${nodeLabelById(placementNodes, m.sourceNodeID)} -> ${nodeLabelById(placementNodes, m.targetNodeID)}: ${phase}`;
    };

    // Admin: flag this server as a public read-only demo (or unflag it). The
    // result is checked because the switch renders from server.isDemo: a refused
    // unflag would otherwise snap back to "on" with no explanation.
    const handleToggleDemo = async () => {
        setDemoMsg('');
        try {
            const res: any = await setServerDemo(server.id, !server.isDemo);
            if (res && (res.success === false || res.error)) {
                setDemoMsg(res.error || res.message || 'Could not change the demo flag.');
            } else {
                refreshServers();
            }
        } catch {
            setDemoMsg('Could not change the demo flag.');
        }
    };

    const handleMigrateStorage = async () => {
        if (!storageMigrateTarget) return;
        setStorageMigrating(true);
        setStorageMigrateMsg('');
        try {
            const res = await migrateServerStorage(server.id, storageMigrateTarget);
            setStorageMigrateMsg(res.success ? res.message : (res.error || 'Migration failed'));
        } catch {
            setStorageMigrateMsg('Migration request failed');
        }
        setStorageMigrating(false);
    };

    const migrationBlock = migrationStatus && migrationStatus.phase !== 'none' && (
        <div className="flex items-start gap-2 text-xs">
            {!TERMINAL_PHASES.includes(migrationStatus.phase)
                ? <RefreshCw size={14} className="animate-spin motion-reduce:animate-none text-(--accent-light) mt-0.5 shrink-0" />
                : migrationStatus.phase === 'done'
                    ? <Move size={14} className="text-(--success-light) mt-0.5 shrink-0" />
                    : <AlertTriangle size={14} className="text-(--error-light) mt-0.5 shrink-0" />}
            <span className={migrationStatus.phase === 'done' ? 'text-(--success-light)' : (TERMINAL_PHASES.includes(migrationStatus.phase) ? 'text-(--error-light)' : 'text-(--base-07)')}>
                {migrationLine(migrationStatus)}
                {migrationStatus.error ? ` — ${migrationStatus.error}` : ''}
            </span>
        </div>
    );

    const transferBlock = canTransfer && (
        <Section icon={<Move size={16} />} title="Transfer to node">
            <p className="text-xs text-(--base-07)">Currently on <span className="text-(--base-09)">{nodeLabelById(placementNodes, server.nodeId)}</span></p>
            {moveNodes.length === 0 ? (
                <p className="text-sm text-(--base-06)">No other online nodes {isAdmin ? '' : 'of yours '}are available to transfer to.</p>
            ) : (
                <>
                    <div className="flex items-end gap-2">
                        <div className="flex min-w-0 flex-1 flex-col gap-1.5">
                            <span className="srv-label">Target node</span>
                            <Select ariaLabel="Target node" value={String(moveTargetNodeId)} onChange={v => setMoveTargetNodeId(Number(v))} options={moveNodes.map(n => ({ value: String(n.id), label: nodePickerLabel(n) }))} disabled={moveBusy} />
                        </div>
                        <button type="button" onClick={handleMoveServer} disabled={moveBusy || !moveTargetNodeId} className="btn btn-secondary shrink-0">
                            {moveBusy ? <><RefreshCw size={16} className="animate-spin motion-reduce:animate-none" /> Queuing...</> : <><Move size={16} /> {isAdmin ? 'Move' : 'Transfer'}</>}
                        </button>
                    </div>
                    <p className="text-xs text-(--base-06)">
                        {!isAdmin
                            ? 'The server is stopped, moved to the target node, and restarted there. The gateway keeps the player address stable. Same-LAN moves transfer directly; otherwise the data goes via your backup storage.'
                            : routingMode === 'ip_port'
                                ? "Moves the server and its files to another node; it is stopped for the move. Players then connect to the new node's address, with the same port if that node's port range has it free. A server domain follows on its own; anyone using IP:port needs the new address."
                                : 'Live migration to another node. The gateway keeps the player address stable; the server is moved while idle.'}
                    </p>
                </>
            )}
            {moveMsg && <Msg text={moveMsg} ok={/queued|migration/i.test(moveMsg)} />}
            {migrationBlock}
        </Section>
    );

    const showStorageTab = full && isAdmin && storagePaths.length >= 1;
    const tabItems: { id: SectionId; label: string; icon: React.ElementType }[] = full
        ? [
            { id: 'resources' as const, label: 'Resources', icon: Cpu },
            { id: 'placement' as const, label: 'Placement', icon: Move },
            ...(showStorageTab ? [{ id: 'storage' as const, label: 'Storage', icon: HardDrive }] : []),
            { id: 'ports' as const, label: 'Ports', icon: Plug },
            { id: 'advanced' as const, label: 'Advanced', icon: Wrench },
        ]
        : [{ id: 'placement' as const, label: 'Transfer', icon: Move }];
    const active = tabItems.some(t => t.id === section) ? section : tabItems[0].id;

    return (
        <Drawer open={open} onOpenChange={onOpenChange} side="right" modal>
            <DrawerBackdrop className="srv-drawer-backdrop" />
            <DrawerContent className="srv-drawer flex h-dvh w-[min(100vw,36rem)] flex-col">
                <div className="flex shrink-0 items-start justify-between gap-4 px-6 pt-6 pb-3">
                    <div className="min-w-0">
                        <DrawerTitle className="font-display text-lg font-bold text-(--base-09)">
                            {full ? 'Server settings' : 'Transfer server'}
                        </DrawerTitle>
                        <DrawerDescription className="truncate text-xs text-(--base-06)">{server.name}</DrawerDescription>
                    </div>
                    <DrawerClose aria-label="Close" className="btn btn-secondary btn-icon btn-sm shrink-0">
                        <X size={16} />
                    </DrawerClose>
                </div>
                {tabItems.length > 1 && (
                    <div className="shrink-0 px-6">
                        <Tabs items={tabItems} active={active} onChange={setSection} ariaLabel="Server settings sections" />
                    </div>
                )}

                <div className="min-h-0 flex-1 overflow-y-auto px-6 py-5" data-drawer-no-drag>
                    <div className="flex flex-col gap-4">
                        {full && active === 'resources' && (
                            <>
                                <Section icon={<SlidersHorizontal size={16} />} title="Limits">
                                    <div className="flex flex-col gap-2">
                                        <NumberField label="RAM (MB)" value={editRam} onChange={setEditRam} min={0} step={1} largeStep={256} />
                                        <ResourceSlider label="RAM" value={editRam} onChange={setEditRam} scale={ramScale()} format={formatMB} />
                                    </div>
                                    <div className="flex flex-col gap-2">
                                        <NumberField label="CPU limit (cores)" value={editCpuLimit} onChange={setEditCpuLimit} min={0} step={0.01} largeStep={1} placeholder="0 = unlimited" hint="0 = no limit. Example: 2.0 = 2 cores" />
                                        <ResourceSlider label="CPU limit" value={editCpuLimit} onChange={setEditCpuLimit} scale={cpuScale()} format={v => `${v} cores`} />
                                    </div>
                                    <div className="flex flex-col gap-2">
                                        <NumberField label="Storage limit (GB)" value={editDiskLimit} onChange={setEditDiskLimit} min={0} step={0.01} largeStep={10} placeholder="0 = unlimited" hint="0 = unlimited" />
                                        <ResourceSlider label="Storage limit" value={editDiskLimit} onChange={setEditDiskLimit} scale={DISK_SCALE_GB} format={v => `${v} GB`} />
                                    </div>
                                </Section>
                                {isAdmin && (
                                    <Section icon={<Cpu size={16} />} title="CPU pinning">
                                        <CpuPinningControl
                                            nodeId={server.nodeId}
                                            serverId={server.id}
                                            mode={editCpuMode}
                                            cpuset={editCpusetCpus}
                                            cpuLimit={editCpuLimit}
                                            onChange={({ mode: m, cpuset }) => { setEditCpuMode(m); setEditCpusetCpus(cpuset); }}
                                        />
                                        <p className="text-xs text-(--base-06)">Pin this container to specific CPU cores (useful for AMD 3D V-Cache). Resets if the server is migrated to a different node.</p>
                                    </Section>
                                )}
                            </>
                        )}

                        {full && active === 'placement' && (
                            <>
                                {/* Auto-move opt-in. Disabled when the platform feature
                                    is off: the backend would answer 503. */}
                                <Section icon={<Move size={16} />} title="Auto-Move">
                                    <div className={`flex items-center justify-between gap-4 ${autoMoveDisabled ? 'opacity-60' : ''}`}>
                                        <p className="min-w-0 text-xs text-(--base-06)">
                                            Let the rebalance worker migrate this server to a less-loaded node when the current node is overloaded. Migration only runs while the server is stopped/idle.
                                        </p>
                                        <Switch checked={editAutoMove} onChange={setEditAutoMove} disabled={autoMoveDisabled} ariaLabel="Auto-Move" className="shrink-0" />
                                    </div>
                                    {autoMoveDisabled && (
                                        <p className="flex items-start gap-1.5 text-xs text-(--base-06)">
                                            <AlertTriangle size={14} className="mt-0.5 shrink-0 text-(--warning-light)" />
                                            <span>{autoMoveReason}</span>
                                        </p>
                                    )}
                                </Section>
                                {transferBlock}
                            </>
                        )}
                        {!full && transferBlock}

                        {full && active === 'storage' && showStorageTab && (
                            <Section icon={<MoveHorizontal size={16} />} title={storagePaths.length > 1 ? 'Storage path migration' : 'Storage path'}>
                                <div className="flex flex-col gap-1.5">
                                    <span className="srv-label">Current path</span>
                                    <p className="truncate rounded-md bg-(--base-03) px-3 py-2 font-mono text-xs text-(--base-07)">
                                        {storageCurrentPath || <span className="italic text-(--base-05)">unknown</span>}
                                    </p>
                                </div>
                                {storagePaths.length > 1 && (
                                    <>
                                        <div className="flex items-end gap-2">
                                            <div className="flex min-w-0 flex-1 flex-col gap-1.5">
                                                <span className="srv-label">Migrate to</span>
                                                <Select ariaLabel="Migrate to" value={storageMigrateTarget} onChange={setStorageMigrateTarget} disabled={storageMigrating} options={storagePaths.filter(p => p.path !== storageCurrentPath).map(p => ({ value: p.path, label: `${p.path} — ${(p.free_bytes / 1073741824).toFixed(1)} GB free` }))} />
                                            </div>
                                            <button type="button" onClick={handleMigrateStorage} disabled={storageMigrating || !storageMigrateTarget} className="btn btn-secondary shrink-0">
                                                {storageMigrating ? <><RefreshCw size={16} className="animate-spin motion-reduce:animate-none" /> Moving...</> : <><MoveHorizontal size={16} /> Migrate</>}
                                            </button>
                                        </div>
                                        {storageMigrateMsg && <Msg text={storageMigrateMsg} ok={storageMigrateMsg.includes('queued') || storageMigrateMsg.includes('Migration')} />}
                                        <p className="text-xs text-(--base-06)">Server will be stopped during migration. Restart it manually after.</p>
                                    </>
                                )}
                            </Section>
                        )}

                        {full && active === 'ports' && (
                            <>
                                {isAdmin && (
                                    <Section icon={<Plug size={16} />} title="Main port">
                                        <div className="grid grid-cols-2 gap-3">
                                            <NumberField label="Host port" value={editHostPort} onChange={setEditHostPort} min={0} max={65535} step={1} placeholder="0 = auto" disabled={!canEditPorts} hint="0 = auto from range" />
                                            <NumberField label="Container port" value={editContainerPort} onChange={setEditContainerPort} min={1} max={65535} step={1} disabled={!canEditPorts} />
                                        </div>
                                        {!canEditPorts && <p className="text-xs text-(--base-06)">Not used: this server is reached through the gateway.</p>}
                                    </Section>
                                )}
                                {/* The extra-ports card opens a fixed-position modal, which the
                                    drawer's transform would re-anchor to the drawer; it stays in
                                    the header beside the address, where owners reach it too. */}
                                <Section icon={<Plug size={16} />} title="Extra ports">
                                    <p className="text-xs text-(--base-06)">Add, rename and remove extra ports (voice chat, map views) with the Ports chip beside the server address.</p>
                                </Section>
                            </>
                        )}

                        {full && active === 'advanced' && (
                            <>
                                {paddingSupported && (
                                    <Section icon={<SlidersHorizontal size={16} />} title="RAM headroom">
                                        <div className="flex flex-col gap-1.5">
                                            <label htmlFor="edit-ram-padding" className="srv-label">RAM headroom (MB)</label>
                                            <input
                                                id="edit-ram-padding"
                                                type="number"
                                                min={0}
                                                max={MAX_RAM_PADDING_MB}
                                                step={64}
                                                value={editPaddingText}
                                                placeholder={inheritedPaddingValue !== undefined ? String(inheritedPaddingValue) : 'Default'}
                                                onChange={e => setEditPaddingText(e.target.value)}
                                                aria-invalid={editPaddingInvalid}
                                                className="input-field srv-num w-full"
                                            />
                                            <p className="text-xs text-(--base-06)">
                                                {/* Core sends the headroom to the node only with a RAM or
                                                    headroom change, so an inherited default is not
                                                    necessarily what the container runs with. */}
                                                {currentPaddingOverride !== null
                                                    ? `Set on this server: ${currentPaddingOverride} MB. Empty uses the default.`
                                                    : `Inherited: ${inheritedPaddingValue} MB from the ${paddingInherited?.source ?? 'node or global'} setting. A changed default applies on the next RAM or headroom change.`}
                                                {' '}{RAM_PADDING_HELP}
                                            </p>
                                            {editPaddingText.trim() !== '' && (
                                                <button type="button" onClick={() => setEditPaddingText('')} className="btn btn-ghost btn-sm self-start">
                                                    {paddingInherited ? resetLabel(paddingInherited.source, paddingInherited.value) : 'Reset to default'}
                                                </button>
                                            )}
                                        </div>
                                    </Section>
                                )}
                                {memoryGuardSupported && (
                                    <Section icon={<AlertTriangle size={16} />} title="Memory guard">
                                        <div className="flex flex-col gap-1.5">
                                            <label htmlFor="edit-memory-guard" className="srv-label">When memory runs out</label>
                                            <Select id="edit-memory-guard" value={editMemoryGuard} onChange={v => setEditMemoryGuard(v as MemoryGuardAction)} options={MEMORY_GUARD_OPTIONS} className="w-full" />
                                            <p className="text-xs text-(--base-06)">{MEMORY_GUARD_HELP}</p>
                                        </div>
                                    </Section>
                                )}
                                {/* Demo flag (admin). A flagged server shows up read-only in the
                                    sidebar for any user who has no server of their own. Writes
                                    stay blocked server-side. Only on the hosted (store) build. */}
                                {featureFlags.store && (
                                    <Section icon={<Globe size={16} />} title="Public demo server">
                                        <div className="flex items-center justify-between gap-4">
                                            <p className="min-w-0 text-xs text-(--base-06)">
                                                Show this server read-only to users who have none of their own. They can view overview, console, stats and browse files, but cannot edit, power, download or upload.
                                            </p>
                                            <Switch checked={!!server.isDemo} onChange={() => handleToggleDemo()} ariaLabel="Public Demo Server" className="shrink-0" />
                                        </div>
                                        {demoMsg && <Msg text={demoMsg} ok={false} />}
                                    </Section>
                                )}
                            </>
                        )}
                    </div>
                </div>

                {full && (
                    <div className="shrink-0 border-t border-(--base-03) bg-(--base-01) px-6 py-4">
                        <p className="mb-2 text-xs text-(--base-06)" aria-live="polite">{resourcePlan.note}</p>
                        {resourcesMsg && (
                            <div className="mb-3">
                                <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{resourcesMsg}</Callout>
                            </div>
                        )}
                        <div className="flex justify-end gap-2">
                            <button type="button" onClick={() => { onOpenChange(false); setResourcesMsg(''); }} className="btn btn-secondary">Cancel</button>
                            <button
                                type="button"
                                onClick={() => runSaveResources(handleSaveResources)}
                                disabled={savingResources || !!pinProblem}
                                title={pinProblem ? 'Pin exactly as many cores as the CPU limit' : undefined}
                                className="btn btn-primary disabled:opacity-40"
                            >
                                {resourcePlan.buttonLabel}
                            </button>
                        </div>
                    </div>
                )}
            </DrawerContent>
        </Drawer>
    );
}
