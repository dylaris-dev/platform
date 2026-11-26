"use client";

import { useState, type ReactNode } from 'react';
import Link from 'next/link';
import { Clock, Globe, Link2, Move, Pencil, Plus, ShieldCheck, SlidersHorizontal, Undo2, AlertTriangle } from 'lucide-react';
import { updateServerName, type GatewayRoute, type Server } from '@/lib/api';
import { useAppData } from '@/lib/AppDataContext';
import { nodeConnectivity, dotFor } from '@/lib/connectivity';
import { loaderName } from '@/lib/modFit';
import CopyText from '@/components/ui/CopyText';
import Tooltip from '@/components/ui/Tooltip';
import ServerPortsCard from '@/components/ServerPortsCard';

// Core's status word, as a person would say it.
const STATE_WORD: Record<string, string> = {
    online: 'Running',
    stopped: 'Stopped',
    offline: 'Stopped',
    starting: 'Starting',
    stopping: 'Stopping',
    restarting: 'Restarting',
    installing: 'Installing',
    migrating: 'Migrating',
    pending_setup: 'Setup required',
    disk_full: 'Storage full',
    pending_deletion: 'Pending deletion',
    suspended: 'Suspended',
};

function stateDot(status: string): string {
    switch (status) {
        case 'online': return 'bg-(--success-light)';
        case 'stopped':
        case 'offline': return 'bg-(--base-05)';
        case 'disk_full': return 'bg-(--error)';
        default: return 'bg-(--warning) animate-pulse motion-reduce:animate-none';
    }
}

export const stateWord = (status: string) => {
    const w = STATE_WORD[status] ?? status.replace(/_/g, ' ');
    return w.charAt(0).toUpperCase() + w.slice(1);
};
export { stateDot };

/**
 * The server's identity bar: state, name (editable), quiet chips for what it
 * runs and who owns it, the address as copy chips, and the actions on the right.
 * `power` is the already-wired PowerControls; the shell owns that state.
 */
export default function ServerHeader({
    server, servers, now, power, isDemoView, isPendingSetup, uploadLocked, cooldownSecondsLeft,
    settings, migration, directDomain, directIpPort, serverRoutes, onOpenRoutes,
}: {
    server: Server;
    servers: Server[];
    now: number;
    power: ReactNode;
    isDemoView: boolean;
    isPendingSetup: boolean;
    uploadLocked: boolean;
    cooldownSecondsLeft: number;
    // The drawer's entry: admins get Resources, BYON owners Transfer.
    settings: { kind: 'resources' | 'transfer'; onOpen: () => void } | null;
    migration: { active: boolean; cancelling: boolean; cancellable: boolean; onCancel: () => void } | null;
    directDomain: string;
    directIpPort: string;
    serverRoutes: GatewayRoute[];
    onOpenRoutes: () => void;
}) {
    const { user, refreshServers, gatewayEnabled, routingMode } = useAppData();
    const [isEditingName, setIsEditingName] = useState(false);
    const [editedName, setEditedName] = useState('');
    const [nameMsg, setNameMsg] = useState('');
    // Brief "copied" feedback for the owner-id click-to-copy.
    const [ownerCopied, setOwnerCopied] = useState(false);

    const { tier } = nodeConnectivity(server.nodeStatus, server.nodeLastSeenAt, now);
    // A node that is not reachable is explained by the shell's notice above; the dot shows it here.
    const word = stateWord(server.status);

    const startEdit = () => {
        setNameMsg('');
        setEditedName(server.name || '');
        setIsEditingName(true);
    };
    const saveName = async () => {
        const trimmed = editedName.replace(/\s{2,}/g, ' ').trim();
        if (!trimmed || !/^[a-zA-Z0-9\-+ ]{1,50}$/.test(trimmed)) { setIsEditingName(false); return; }
        setNameMsg('');
        // A refused rename (name taken, no permission) must say so: closing the
        // editor and letting refreshServers restore the old name reads as the
        // panel losing the edit.
        const res = await updateServerName(server.id, trimmed);
        if (res?.success === false) {
            setNameMsg(res.message || res.error || 'The name could not be changed.');
            setIsEditingName(false);
            return;
        }
        setIsEditingName(false);
        refreshServers();
    };
    const copyOwnerId = () => {
        navigator.clipboard.writeText(server.ownerId).then(() => {
            setOwnerCopied(true);
            setTimeout(() => setOwnerCopied(false), 1200);
        }).catch(() => { /* clipboard blocked: nothing to surface */ });
    };

    const proxy = server.proxyId ? servers.find(s => s.id === server.proxyId) : undefined;
    const hasDirect = routingMode !== 'gateway' && !!server.nodeAddress && (server.hostPort ?? 0) > 0;
    const showConnection = !isPendingSetup && (hasDirect || gatewayEnabled);
    const noRoute = routingMode === 'gateway' && gatewayEnabled && serverRoutes.length === 0;

    return (
        <div className="flex flex-col gap-3">
            <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
                <div className="min-w-0 flex-1">
                    <div className="mb-1 flex items-center gap-2 text-xs font-medium text-(--base-07)" aria-live="polite">
                        <span className={`size-2 shrink-0 rounded-full ${dotFor(tier, stateDot(server.status))}`} aria-hidden="true" />
                        <span title={server.status === 'migrating' ? 'Server is migrating to another node. Power actions are locked until it finishes.' : undefined}>
                            {word}
                        </span>
                    </div>
                    <div className="flex min-w-0 items-center gap-2">
                        {isEditingName ? (
                            <input
                                aria-label="Server name"
                                className="min-w-0 rounded-md border border-(--accent) bg-(--base-03) px-2 py-0.5 font-display text-2xl font-bold text-(--base-09) outline-none focus:[box-shadow:var(--focus-ring)]"
                                value={editedName}
                                onChange={e => setEditedName(e.target.value)}
                                onKeyDown={e => { if (e.key === 'Enter') saveName(); if (e.key === 'Escape') setIsEditingName(false); }}
                                onBlur={saveName}
                                maxLength={50}
                                autoFocus
                            />
                        ) : (
                            <>
                                <h1 className="min-w-0 truncate font-display text-2xl font-bold text-(--base-09)">{server.name}</h1>
                                {!isDemoView && (
                                    <Tooltip content="Rename server">
                                        <button type="button" onClick={startEdit} aria-label="Rename server" className="focus-ring grid size-8 shrink-0 place-items-center rounded-md text-(--base-06) transition-colors hover:bg-(--base-03) hover:text-(--base-09) active:bg-(--base-04)">
                                            <Pencil size={16} />
                                        </button>
                                    </Tooltip>
                                )}
                            </>
                        )}
                    </div>
                    {nameMsg && <p className="mt-1 text-xs text-(--error-light)" role="alert">{nameMsg}</p>}
                </div>

                <div className="flex flex-wrap items-center gap-2">
                    {cooldownSecondsLeft > 0 && (
                        <span className="srv-chip srv-num" title="Server is finishing install. Power actions unlock when this counter reaches 0.">
                            <Clock size={14} aria-hidden="true" />Settling {cooldownSecondsLeft}s
                        </span>
                    )}
                    {migration?.active && (
                        <Tooltip content="Cancel this migration and roll the server back to its current node. Only possible before cutover; the old node is kept until the move is verified.">
                            <button
                                type="button"
                                onClick={migration.onCancel}
                                disabled={migration.cancelling || !migration.cancellable}
                                className="btn btn-danger"
                            >
                                <Undo2 size={16} />
                                {migration.cancelling ? 'Cancelling...' : 'Cancel migration'}
                            </button>
                        </Tooltip>
                    )}
                    {settings && (
                        <Tooltip content={uploadLocked ? 'Upload in progress. Wait or cancel it first.' : settings.kind === 'transfer' ? 'Transfer this server to another of your nodes' : 'Resources, placement, storage and ports'}>
                            <button type="button" onClick={settings.onOpen} disabled={uploadLocked} className="btn btn-secondary">
                                {settings.kind === 'transfer' ? <Move size={16} /> : <SlidersHorizontal size={16} />}
                                {settings.kind === 'transfer' ? 'Transfer' : 'Resources'}
                            </button>
                        </Tooltip>
                    )}
                    {power}
                </div>
            </div>

            <div className="flex flex-wrap items-center gap-2">
                {server.installerType && (
                    <span className="srv-chip srv-num" title={server.buildNumber ? `Build ${server.buildNumber}` : undefined}>
                        {loaderName(server.installerType)}{server.minecraftVersion ? ` ${server.minecraftVersion}` : ''}
                    </span>
                )}
                {server.serverType === 'proxy' && <span className="srv-chip">Proxy</span>}
                {proxy && <Link href={`/servers/${proxy.id}`} className="srv-chip focus-ring">{proxy.name}</Link>}
                {server.activeSubServer && (
                    <span className="srv-chip"><span className="text-(--base-06)">Sub-server</span><span className="srv-num text-(--base-09)">{server.activeSubServer}</span></span>
                )}
                <Tooltip content={`Owner ID: ${server.ownerId}. Click to copy.`}>
                    <button type="button" onClick={copyOwnerId} className="srv-chip focus-ring">
                        <span className="text-(--base-06)">Owner</span>
                        <span className="text-(--base-09)">{server.owner === user?.username ? 'You' : (server.owner || server.ownerId)}</span>
                        {ownerCopied && <span className="text-(--success-light)">copied</span>}
                    </button>
                </Tooltip>
                {isDemoView && <span className="srv-chip text-(--accent-light)">Demo, read-only</span>}
                {server.role === 'admin' && !server.invitedHere && (
                    <span className="srv-chip text-(--warning-light)" title="You are here as an admin, not as the owner or an invitee.">
                        <ShieldCheck size={14} aria-hidden="true" />Admin access
                    </span>
                )}

                {showConnection && (
                    <>
                        {/* Routes are the one thing here a user acts on rather than copies. */}
                        {gatewayEnabled && (serverRoutes.length > 0 ? (
                            <Tooltip content="Manage routes">
                                <button type="button" onClick={onOpenRoutes} className="srv-chip focus-ring">
                                    <Globe size={14} aria-hidden="true" />Routes
                                </button>
                            </Tooltip>
                        ) : (
                            <Tooltip content="Add a gateway route so players can join">
                                <button type="button" onClick={onOpenRoutes} className="btn btn-secondary btn-sm focus-ring shrink-0">
                                    <Plus size={14} />Add route
                                </button>
                            </Tooltip>
                        ))}
                        {hasDirect && (
                            <>
                                {/* A domain is what players should type; IP:port stays as the secondary chip. */}
                                {directDomain !== directIpPort && (
                                    <CopyText
                                        value={directDomain}
                                        bubble
                                        label={<span className="inline-flex items-center gap-1.5"><Globe size={14} className="shrink-0 text-(--accent-light)" aria-hidden="true" />{directDomain}</span>}
                                        className="srv-chip srv-num cursor-copy text-(--base-09)"
                                    />
                                )}
                                <CopyText
                                    value={directIpPort}
                                    bubble
                                    label={<span className="inline-flex items-center gap-1.5"><Link2 size={14} className="shrink-0 text-(--base-06)" aria-hidden="true" />{directIpPort}</span>}
                                    className="srv-chip srv-num cursor-copy"
                                />
                                {/* Not admin-only, so it stays reachable here for owners; the
                                    drawer's Ports tab holds the same card for admins. */}
                                {!isDemoView && <ServerPortsCard key={server.id} serverId={server.id} />}
                            </>
                        )}
                        {gatewayEnabled && serverRoutes.map(route => (
                            <CopyText
                                key={route.domain}
                                value={route.domain}
                                bubble
                                label={<span className="inline-flex items-center gap-1.5"><Globe size={14} className="shrink-0 text-(--accent-light)" aria-hidden="true" />{route.domain}</span>}
                                className="srv-chip srv-num cursor-copy text-(--base-09)"
                            />
                        ))}
                        {noRoute && (
                            <span className="inline-flex items-center gap-1.5 text-xs font-medium text-(--warning)">
                                <AlertTriangle size={14} aria-hidden="true" />No gateway route configured
                            </span>
                        )}
                    </>
                )}
            </div>
        </div>
    );
}
