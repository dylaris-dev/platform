"use client";

import React, { useCallback, useEffect, useMemo, useState } from 'react';

import { Users, UserRound, Power, ShieldX, Skull, ShieldCheck, ShieldOff, Trash2, RefreshCw, Search, Send, AlertTriangle, Crown, ListChecks, CircleCheck, X, ListPlus, MessageSquare, Terminal, Lock, Info, MoreHorizontal } from 'lucide-react';
import { Dialog, DialogBackdrop, DialogClose, DialogContent, DialogTitle } from '@ui-sdk/react/dialog';
import { useAppData } from '@/lib/AppDataContext';
import PlayerHead from '@/components/PlayerHead';
import {
    getRconConfig, parsePlayerList, friendlyRconError, type OnlinePlayer,
} from '@/lib/api/rcon';
import {
    getPlayerLists, getOnlinePlayers, playerAction,
    type PlayerListEntry, type PlayerAction, type KnownPlayer,
} from '@/lib/api/players';
import { canManagePlayers, isServerLive, mergeAllPlayers, type AllPlayerRow } from '@/lib/playersView';
import { playerActionToast } from '@/lib/playerActionResult';
import RconConfigCard from '@/components/RconConfigCard';
import { Skeleton, SkeletonText, SkeletonCircle } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';
import Tabs, { type TabItem } from '@/components/ui/Tabs';
import Menu, { type MenuEntry } from '@/components/ui/Menu';
import Tooltip from '@/components/ui/Tooltip';
import Callout from '@/components/server/Callout';
import { confirmDialog } from '@/components/ui/ConfirmDialog';

// Player Management. Everything here goes through /servers/{id}/players,
// which is gated on players.read (the roster + the three list files) and
// players.manage (a fixed set of player commands). That is deliberate: this
// page used to call the raw RCON endpoint and the file browser, so doing this
// job needed rcon.exec - every command the server has, `stop` included - plus
// files.read over the whole filesystem.
//
// The online roster is polled every 10s while the server runs. Bans/whitelist/
// ops/usercache come from the JSON files MC keeps in the active sub-server dir,
// which stay the authoritative view of current state and are readable while the
// server is stopped; the actions mutate through the server, so they need it up.

type Section = 'all' | 'online' | 'bans' | 'whitelist' | 'ops' | 'rcon';

const SECTIONS: { id: Section; label: string; Icon: React.ComponentType<{ size?: number }> }[] = [
    { id: 'all',       label: 'All players', Icon: UserRound },
    { id: 'online',    label: 'Online',    Icon: Users },
    { id: 'bans',      label: 'Bans',      Icon: ShieldX },
    { id: 'whitelist', label: 'Whitelist', Icon: ListChecks },
    { id: 'ops',       label: 'Operators', Icon: Crown },
    { id: 'rcon',      label: 'RCON',      Icon: Terminal },
];

// Every section except RCON itself needs a live RCON connection. When RCON is
// off we force the RCON section and lock the rest until the operator enables it.
const RCON_DEPENDENT: Section[] = ['all', 'online', 'bans', 'whitelist', 'ops'];

// The sections that show who is online, and so ask RCON for the roster.
const ROSTER_SECTIONS: Section[] = ['all', 'online'];

const OFFLINE_HINT = 'Start the server to do this';

// How many All players rows render before "Show more": each row is an avatar
// request, and a long-running server's usercache holds up to 1000 names.
const ALL_PAGE = 100;

export default function ServerPlayersPage() {
    const paramId = useRouteId('servers');
    const { servers } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);

    const [section, setSection] = useState<Section>('all');
    const [search, setSearch] = useState('');
    const [online, setOnline] = useState<OnlinePlayer[]>([]);
    const [bans, setBans] = useState<PlayerListEntry[]>([]);
    const [whitelist, setWhitelist] = useState<PlayerListEntry[]>([]);
    const [ops, setOps] = useState<PlayerListEntry[]>([]);
    const [known, setKnown] = useState<KnownPlayer[]>([]);
    const [whitelistState, setWhitelistState] = useState<{ enabled?: boolean; enforced?: boolean }>({});
    // True from the start: the first render must show the skeleton, not
    // "Nobody has joined yet." for the moment before the first fetch begins.
    const [loading, setLoading] = useState(true);
    const [actionError, setActionError] = useState<string | null>(null);
    const [rconEnabled, setRconEnabled] = useState(false);
    const [rconLoaded, setRconLoaded] = useState(false);
    // False once the RCON config read comes back 403: this viewer may manage
    // players but not look at the server's configuration.
    const [rconConfigVisible, setRconConfigVisible] = useState(true);

    // The kit's confirm dialog; the handlers below keep their shape.
    const setConfirm = (c: { title: string; message: string; onConfirm: () => void | Promise<void>; danger?: boolean }) => {
        void confirmDialog({ title: c.title, message: c.message, confirmLabel: 'Confirm', destructive: !!c.danger })
            .then(ok => (ok ? c.onConfirm() : undefined));
    };
    const [tellPrompt, setTellPrompt] = useState<{ player: string; message: string } | null>(null);
    const [addPrompt, setAddPrompt] = useState<{ target: 'whitelist' | 'ops'; name: string } | null>(null);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    // server.status follows the servers.changed SSE, so this flips without a
    // reload and the refresh below re-runs on it.
    const live = useMemo(() => isServerLive(server?.status), [server?.status]);
    const canManage = canManagePlayers(server);
    const actionsBlocked = !live || !canManage;
    const blockedHint = !canManage
        ? (server?.role === 'demo' ? 'Read-only: you cannot manage players on this server' : 'You need the Manage players permission')
        : OFFLINE_HINT;
    const [allLimit, setAllLimit] = useState(ALL_PAGE);

    // With RCON off, the effective section is always 'rcon' (the only usable
    // one); the user's chosen section resumes once RCON is enabled. This is
    // also what gates the auto roster poll below against a server that has been
    // enabled but not yet restarted: RconConfigCard only reports enabled=true
    // through onEnabledChange once it has confirmed (post-restart) that RCON
    // is actually reachable, so rconEnabled here stays false - and this
    // section stays forced to 'rcon' - until then.
    const effectiveSection: Section = rconEnabled ? section : 'rcon';
    const visibleSections = rconConfigVisible ? SECTIONS : SECTIONS.filter(x => x.id !== 'rcon');

    // Load the RCON state once so we know whether to lock the other sections.
    // RconConfigCard also reports flips live via onEnabledChange below.
    useEffect(() => {
        let cancelled = false;
        (async () => {
            const res = await getRconConfig(serverId);
            if (cancelled) return;
            if (res.success) {
                // Keep the RCON-dependent tabs locked when a restart is still
                // pending (enabled in the DB but server.properties not yet
                // re-read on a boot): treat it as not-yet-live, matching the
                // comment above and surviving a page reload.
                setRconEnabled(res.enabled && !res.restartRequired);
            } else if ((res as { status?: number }).status === 403) {
                // Reading the RCON CONFIG needs config.read, which is not the
                // capability this page runs on any more. Someone holding only
                // players.read would otherwise be locked out of the very
                // sections they are allowed to see, by a call about a setting
                // they are not allowed to look at. Hide the RCON section and
                // let the player routes answer for themselves - they carry
                // their own gate and their own "rcon not enabled" message.
                setRconConfigVisible(false);
                setRconEnabled(true);
            }
            setRconLoaded(true);
        })();
        return () => { cancelled = true; };
    }, [serverId]);

    // background=true is the 10s poll: it must not touch `loading`, or the whole
    // list is swapped for six skeleton cards every ten seconds. Only a first load
    // and an explicit Refresh show a loading state; a poll replaces the data in
    // place, which is what makes it invisible.
    const refresh = useCallback(async (background = false, rosterOnly = false) => {
        if (!serverId || effectiveSection === 'rcon') { setLoading(false); return; }
        if (!background) setLoading(true);
        setActionError(null);

        if (ROSTER_SECTIONS.includes(effectiveSection)) {
            if (!live) {
                // A stopped server cannot answer RCON; asking would only put a
                // dial error on screen for a state the page already explains.
                setOnline([]);
            } else {
                const res = await getOnlinePlayers(serverId);
                if (!res.success) {
                    // A dial/connection error here means RCON was enabled but the
                    // server has not restarted since (or crashed after) - never
                    // show the raw Go dial string (it also leaks the internal
                    // mc_<uuid> container hostname).
                    setActionError(friendlyRconError(res.error, 'RCON unavailable'));
                    setOnline([]);
                } else {
                    setOnline(parsePlayerList(res.output || ''));
                }
            }
        }
        // rosterOnly is the 10s poll: re-reading five files off the node every
        // ten seconds per open tab would buy nothing, they change on actions.
        if (effectiveSection !== 'online' && !rosterOnly) {
            // One call for all three lists. `unavailable` names any list the
            // server could not read, which is a different fact from an empty
            // one - showing "nobody is banned" for a list nobody could open is
            // the failure this endpoint exists to make impossible.
            const res = await getPlayerLists(serverId);
            setBans(res.bans);
            setWhitelist(res.whitelist);
            setOps(res.ops);
            setKnown(res.known);
            setWhitelistState({ enabled: res.whitelistEnabled, enforced: res.whitelistEnforced });
            const listKey = effectiveSection === 'all' ? 'known' : effectiveSection;
            if (!res.success) {
                setActionError(res.message || 'Player lists could not be loaded.');
            } else if (res.unavailable && res.unavailable[listKey]) {
                setActionError(`${effectiveSection === 'all' ? 'usercache' : effectiveSection}: ${res.unavailable[listKey]}`);
            } else if (effectiveSection === 'whitelist' && res.unavailable?.properties) {
                setActionError(`server.properties: ${res.unavailable.properties}`);
            }
        }

        setLoading(false);
    }, [serverId, effectiveSection, live]);

    // Section switches DO show the skeleton: the previous section's rows would
    // otherwise sit there looking like this section's data.
    useEffect(() => { refresh(); }, [refresh]);

    // Online list auto-poll every 10s, only while the server runs. Other
    // sections are file-backed and change only on user-initiated actions -
    // refresh on action.
    useEffect(() => {
        if (!live || !ROSTER_SECTIONS.includes(effectiveSection)) return;
        const id = setInterval(() => refresh(true, true), 10_000);
        return () => clearInterval(id);
    }, [effectiveSection, refresh, live]);

    // One entry point for every mutation: the panel names an ACTION and Core
    // builds the command. Nothing here can construct one.
    const run = async (
        label: string,
        action: PlayerAction,
        player: string,
        extra?: { reason?: string; message?: string },
    ) => {
        const res = await playerAction(serverId, action, player, extra);
        const { text, ok } = playerActionToast(label, res);
        showToast(text, ok);
        if (ok) refresh(true);
    };

    // ---- Action handlers ----

    const handleKick = (name: string) => {
        setConfirm({
            title: `Kick ${name}?`,
            message: `Disconnect ${name} from the server. They can rejoin immediately.`,
            onConfirm: () => run('Kick', 'kick', name),
        });
    };
    const handleBan = (name: string) => {
        setConfirm({
            title: `Ban ${name}?`,
            message: `Permanently ban ${name}. Adds an entry to banned-players.json.`,
            danger: true,
            onConfirm: () => run('Ban', 'ban', name),
        });
    };
    const handleUnban = (name: string) => {
        setConfirm({
            title: `Unban ${name}?`,
            message: `Remove ${name} from banned-players.json.`,
            onConfirm: () => run('Unban', 'unban', name),
        });
    };
    const handleOp = (name: string) => run('Op', 'op', name);
    const handleDeop = (name: string) => {
        setConfirm({
            title: `De-op ${name}?`,
            message: `Removes operator privileges from ${name}.`,
            onConfirm: () => run('De-op', 'deop', name),
        });
    };
    const handleWhitelistAdd = (name: string) => run('Whitelist add', 'whitelist_add', name);
    const handleWhitelistToggle = (on: boolean) => {
        if (on) return run('Whitelist on', 'whitelist_on', '');
        setConfirm({
            title: 'Turn the whitelist off?',
            message: 'Anyone who is not banned can join again.',
            onConfirm: () => run('Whitelist off', 'whitelist_off', ''),
        });
    };
    const handleWhitelistRemove = (name: string) => {
        setConfirm({
            title: `Remove ${name} from whitelist?`,
            message: `${name} will be kicked when whitelist is enforced.`,
            onConfirm: () => run('Whitelist remove', 'whitelist_remove', name),
        });
    };

    // ---- Current list (filtered by search) ----

    const currentList = useMemo(() => {
        const q = search.trim().toLowerCase();
        const filter = <T extends { name: string }>(rows: T[]) =>
            q ? rows.filter(p => p.name.toLowerCase().includes(q)) : rows;
        if (effectiveSection === 'all') return filter(mergeAllPlayers(known, online));
        if (effectiveSection === 'online') return filter(online);
        if (effectiveSection === 'bans') return filter(bans);
        if (effectiveSection === 'whitelist') return filter(whitelist);
        if (effectiveSection === 'ops') return filter(ops);
        return [];
    }, [effectiveSection, search, online, bans, whitelist, ops, known]);

    const whitelisted = useMemo(() => new Set(whitelist.map(r => r.name.toLowerCase())), [whitelist]);
    const opped = useMemo(() => new Set(ops.map(r => r.name.toLowerCase())), [ops]);
    const banned = useMemo(() => new Set(bans.map(r => r.name.toLowerCase())), [bans]);

    if (!server) return null;

    const sectionItems: TabItem<Section>[] = visibleSections.map(({ id, label, Icon }) => {
        const locked = !rconEnabled && RCON_DEPENDENT.includes(id);
        return {
            id,
            label: (id === 'online' || id === 'all') && !locked && online.length > 0 ? `${label} (${online.length})` : label,
            icon: locked ? Lock : Icon,
            disabled: locked,
            title: locked ? 'Enable RCON to unlock this section' : undefined,
        };
    });

    // One trigger for every row menu. Wrapped in a span only when blocked, so
    // the reason is reachable even though a disabled button takes no hover.
    const rowMenu = (p: { name: string }, items: MenuEntry[]) => {
        const menu = (
            <Menu
                trigger={
                    <button type="button" aria-label={`Actions for ${p.name}`} disabled={actionsBlocked} className="btn btn-ghost btn-icon btn-sm">
                        <MoreHorizontal size={16} />
                    </button>
                }
                items={items}
            />
        );
        return actionsBlocked ? <Tooltip content={blockedHint}><span className="inline-flex">{menu}</span></Tooltip> : menu;
    };

    // Single-action sections get the action itself, not a one-item menu.
    const rowButton = (label: string, icon: React.ReactNode, onClick: () => void, name: string, tone?: string) => {
        const btn = (
            <button type="button" aria-label={`${label} ${name}`} onClick={onClick} disabled={actionsBlocked} className={`btn btn-secondary btn-sm ${tone ?? ''}`}>
                {icon}
                {label}
            </button>
        );
        return actionsBlocked ? <Tooltip content={blockedHint}><span className="inline-flex">{btn}</span></Tooltip> : btn;
    };

    const rowActions = (p: any): React.ReactNode => {
        const key = String(p.name).toLowerCase();
        if (section === 'all') {
            const items: MenuEntry[] = [];
            if ((p as AllPlayerRow).online) items.push({ label: 'Kick', icon: <ShieldOff size={14} />, onSelect: () => handleKick(p.name) });
            if (!whitelisted.has(key)) items.push({ label: 'Add to whitelist', icon: <ListChecks size={14} />, onSelect: () => handleWhitelistAdd(p.name) });
            if (!opped.has(key)) items.push({ label: 'Make operator', icon: <ShieldCheck size={14} />, onSelect: () => handleOp(p.name) });
            if (items.length) items.push({ type: 'separator', id: 'sep' });
            items.push(banned.has(key)
                ? { label: 'Unban', icon: <CircleCheck size={14} />, onSelect: () => handleUnban(p.name) }
                : { label: 'Ban', icon: <Skull size={14} />, danger: true, onSelect: () => handleBan(p.name) });
            return rowMenu(p, items);
        }
        if (section === 'online') {
            return rowMenu(p, [
                { label: 'Whisper', icon: <MessageSquare size={14} />, onSelect: () => setTellPrompt({ player: p.name, message: '' }) },
                { label: 'Make operator', icon: <ShieldCheck size={14} />, onSelect: () => handleOp(p.name) },
                { label: 'Kick', icon: <ShieldOff size={14} />, onSelect: () => handleKick(p.name) },
                { type: 'separator', id: 'sep' },
                { label: 'Ban', icon: <Skull size={14} />, danger: true, onSelect: () => handleBan(p.name) },
            ]);
        }
        if (section === 'bans') return rowButton('Unban', <CircleCheck size={14} />, () => handleUnban(p.name), p.name);
        if (section === 'whitelist') return rowButton('Remove', <Trash2 size={14} />, () => handleWhitelistRemove(p.name), p.name, 'text-(--error-light)');
        if (section === 'ops') return rowButton('Remove operator', <Crown size={14} />, () => handleDeop(p.name), p.name, 'text-(--error-light)');
        return null;
    };

    return (
        // h-full, not flex-1: ServerShell's children box is a scrolling BLOCK, so
        // flex-1 sizes nothing here and the player list below never reached its own
        // overflow - the whole window scrolled instead. The padding goes with it;
        // the shell already applies one.
        <main className="h-full flex flex-col gap-5 overflow-hidden">
            <header className="flex shrink-0 flex-wrap items-end justify-between gap-x-6 gap-y-3">
                <div className="min-w-0">
                    <h1 className="font-display text-xl font-bold text-(--base-09)">Players</h1>
                    <p className="mt-1 text-sm text-(--base-06)">
                        Who is online and who may join.
                        {live && ROSTER_SECTIONS.includes(effectiveSection) && ' The online list refreshes every 10 seconds.'}
                    </p>
                </div>
                {effectiveSection !== 'rcon' && (
                    <div className="flex items-center gap-2">
                        <div className="relative">
                            <Search size={14} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-(--base-05)" aria-hidden="true" />
                            <input
                                type="text"
                                value={search}
                                onChange={e => setSearch(e.target.value)}
                                placeholder="Search players"
                                aria-label="Search players"
                                className="input-field input-icon-left w-44 sm:w-56"
                            />
                        </div>
                        {/* Not onClick={refresh}: the handler would receive the
                            MouseEvent as `background` and every manual refresh
                            would silently become a background one. */}
                        <button onClick={() => refresh()} className="btn btn-secondary btn-sm" disabled={loading}>
                            <RefreshCw size={14} className={loading ? 'animate-spin motion-reduce:animate-none' : ''} />
                            Refresh
                        </button>
                    </div>
                )}
            </header>

            <div className="shrink-0">
                <Callout tone="info" icon={<Info size={16} />}>
                    Player actions run as Minecraft commands over RCON. What you see is the server&apos;s own reply; mods and
                    plugins can change or delay it.
                </Callout>
            </div>

            <div className="shrink-0">
                <Tabs<Section>
                    items={sectionItems}
                    active={effectiveSection}
                    onChange={setSection}
                    ariaLabel="Player sections"
                />
            </div>

            {actionError && (
                <div className="shrink-0">
                    <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{actionError}</Callout>
                </div>
            )}

            {/* Content */}
            <div className="flex-1 overflow-y-auto">
                {!rconLoaded ? (
                    <div className="space-y-1.5">
                        {Array.from({ length: 4 }).map((_, i) => (
                            <Skeleton key={i} className="h-14 rounded-xl" />
                        ))}
                    </div>
                ) : effectiveSection === 'rcon' ? (
                    <div className="space-y-4">
                        {!rconEnabled && (
                            <Callout tone="info" icon={<Lock size={16} />}>
                                RCON is off. Enable it below to unlock All players, Online, Bans, Whitelist and Operators. Live
                                player management runs over RCON.
                            </Callout>
                        )}
                        <RconConfigCard serverId={serverId} onEnabledChange={setRconEnabled} />
                    </div>
                ) : (
                    <>
                        {!live && (
                            <div className="mb-4">
                                <Callout tone="info" icon={<Power size={16} />}>
                                    The server is offline. Start it to see who is online.
                                    {effectiveSection !== 'online' && ' The lists below are read from its files; changing them needs the server running.'}
                                </Callout>
                            </div>
                        )}

                        {effectiveSection === 'whitelist' && (
                            <section className="srv-surface mb-4 space-y-2 p-5" aria-label="Whitelist status">
                                <div className="flex flex-wrap items-center gap-3">
                                    <span className="text-sm font-medium text-(--base-09)">Whitelist</span>
                                    {whitelistState.enabled === undefined ? (
                                        <span className="srv-chip bg-(--base-03)">Unknown</span>
                                    ) : whitelistState.enabled ? (
                                        <span className="srv-chip bg-(--success-ghost) text-(--success-light)">On</span>
                                    ) : (
                                        <span className="srv-chip bg-(--warning-ghost) text-(--warning-light)">Off, anyone can join</span>
                                    )}
                                    {whitelistState.enforced !== undefined && (
                                        <Tooltip content="enforce-whitelist has no RCON command. Change it under Configuration (server.properties); it applies after a restart.">
                                            <span className="srv-chip bg-(--base-03)" tabIndex={0}>
                                                {whitelistState.enforced ? 'Enforced' : 'Not enforced'}
                                            </span>
                                        </Tooltip>
                                    )}
                                    {whitelistState.enabled !== undefined && (() => {
                                        const hint = !canManage ? blockedHint : live ? undefined : 'Start the server to change this, or change white-list under Configuration';
                                        const btn = (
                                            <button
                                                onClick={() => handleWhitelistToggle(!whitelistState.enabled)}
                                                disabled={actionsBlocked}
                                                className="btn btn-secondary btn-sm"
                                            >
                                                {whitelistState.enabled ? 'Turn off' : 'Turn on'}
                                            </button>
                                        );
                                        return (
                                            <span className="ml-auto inline-flex">
                                                {hint && actionsBlocked ? <Tooltip content={hint}><span className="inline-flex">{btn}</span></Tooltip> : btn}
                                            </span>
                                        );
                                    })()}
                                </div>
                                <p className="text-xs text-(--base-06)">
                                    Operators can always join, even when they are not on the whitelist.
                                    {' '}Enforced kicks online players who are removed from the whitelist; change it under Configuration.
                                </p>
                            </section>
                        )}

                        {/* "Add to whitelist/ops" affordance for those sections */}
                        {(effectiveSection === 'whitelist' || effectiveSection === 'ops') && (
                            <div className="mb-4">
                                {(() => {
                                    const btn = (
                                        <button
                                            onClick={() => setAddPrompt({ target: effectiveSection as 'whitelist' | 'ops', name: '' })}
                                            disabled={actionsBlocked}
                                            className="btn btn-secondary btn-sm"
                                        >
                                            <ListPlus size={14} />
                                            Add player to {effectiveSection === 'whitelist' ? 'whitelist' : 'operators'}
                                        </button>
                                    );
                                    return actionsBlocked ? <Tooltip content={blockedHint}><span className="inline-flex">{btn}</span></Tooltip> : btn;
                                })()}
                            </div>
                        )}

                        {loading ? (
                            <div className="space-y-1.5">
                                {Array.from({ length: 6 }).map((_, i) => (
                                    <article key={i} className="srv-surface flex items-center gap-3 px-4 py-2.5">
                                        <SkeletonCircle size="w-8 h-8 rounded-md" />
                                        <div className="min-w-0 flex-1">
                                            <SkeletonText width="w-32" className="h-3.5" />
                                        </div>
                                        <Skeleton className="h-8 w-8 rounded-lg" />
                                    </article>
                                ))}
                            </div>
                        ) : currentList.length === 0 ? (
                            // The offline note above already says why the roster is empty.
                            effectiveSection === 'online' && !live ? null : (
                                <div className="srv-surface flex flex-col items-center gap-2 px-6 py-14 text-center">
                                    <Users size={24} className="text-(--base-05)" aria-hidden="true" />
                                    <p className="text-sm text-(--base-07)">
                                        {section === 'online' ? 'Nobody is online.' : section === 'all' ? 'Nobody has joined yet.' : 'No entries.'}
                                    </p>
                                    {search.trim() && <p className="text-xs text-(--base-06)">Nothing matches &ldquo;{search.trim()}&rdquo;.</p>}
                                </div>
                            )
                        ) : (
                            <ul className="space-y-1.5">
                                {(section === 'all' ? currentList.slice(0, allLimit) : currentList).map((p: any) => (
                                    <li key={`${section}-${p.name}`}>
                                        <article className="flex items-center gap-3 rounded-xl bg-(--base-02) px-4 py-2.5 transition-colors hover:bg-(--base-03)">
                                            <PlayerHead
                                                name={p.name}
                                                className="size-8 shrink-0 rounded-md bg-(--base-03)"
                                                fallback={
                                                    <div className="flex size-8 shrink-0 items-center justify-center rounded-md bg-(--base-03) text-xs font-semibold text-(--base-07)">
                                                        {String(p.name).charAt(0).toUpperCase()}
                                                    </div>
                                                }
                                            />
                                            <div className="min-w-0 flex-1">
                                                <div className="flex flex-wrap items-center gap-1.5 text-sm font-medium text-(--base-09)">
                                                    <span className="truncate">{p.name}</span>
                                                    {section === 'all' && (p as AllPlayerRow).online && <span className="srv-chip h-5 bg-(--success-ghost) text-(--success-light)">Online</span>}
                                                    {section === 'all' && opped.has(p.name.toLowerCase()) && <span className="srv-chip h-5 bg-(--accent-ghost) text-(--accent-light)">Op</span>}
                                                    {section === 'all' && whitelisted.has(p.name.toLowerCase()) && <span className="srv-chip h-5 bg-(--base-03)">Whitelisted</span>}
                                                    {section === 'all' && banned.has(p.name.toLowerCase()) && <span className="srv-chip h-5 bg-(--error-ghost) text-(--error-light)">Banned</span>}
                                                </div>
                                                {section === 'bans' && (p as PlayerListEntry).reason && (
                                                    <div className="truncate text-xs text-(--base-06)">Reason: {(p as PlayerListEntry).reason}</div>
                                                )}
                                            </div>
                                            {/* Every action goes through RCON, so all of them are disabled together. */}
                                            <div className="flex shrink-0 items-center gap-1">{rowActions(p)}</div>
                                        </article>
                                    </li>
                                ))}
                                {section === 'all' && currentList.length > allLimit && (
                                    <li>
                                        <button onClick={() => setAllLimit(n => n + ALL_PAGE)} className="btn btn-secondary btn-sm w-full">
                                            Show more ({currentList.length - allLimit} more)
                                        </button>
                                    </li>
                                )}
                            </ul>
                        )}
                    </>
                )}
            </div>

            {/* Whisper */}
            <PromptDialog
                open={!!tellPrompt}
                onClose={() => setTellPrompt(null)}
                icon={<MessageSquare size={16} />}
                title={`Whisper to ${tellPrompt?.player ?? ''}`}
                value={tellPrompt?.message ?? ''}
                onChange={message => setTellPrompt(t => (t ? { ...t, message } : t))}
                placeholder="Message…"
                label="Message"
                maxLength={200}
                submitLabel="Send"
                submitIcon={<Send size={14} />}
                onSubmit={async () => {
                    if (!tellPrompt) return;
                    const msg = tellPrompt.message.trim();
                    const target = tellPrompt.player;
                    setTellPrompt(null);
                    if (msg) await run('Whisper', 'tell', target, { message: msg });
                }}
            />

            {/* Add to whitelist/ops */}
            <PromptDialog
                open={!!addPrompt}
                onClose={() => setAddPrompt(null)}
                icon={<ListPlus size={16} />}
                title={`Add to ${addPrompt?.target === 'ops' ? 'operators' : 'whitelist'}`}
                value={addPrompt?.name ?? ''}
                onChange={name => setAddPrompt(a => (a ? { ...a, name } : a))}
                placeholder="Player name"
                label="Player name"
                maxLength={32}
                submitLabel="Add"
                onSubmit={async () => {
                    if (!addPrompt) return;
                    const name = addPrompt.name.trim();
                    const target = addPrompt.target;
                    setAddPrompt(null);
                    if (!name) return;
                    if (target === 'whitelist') await handleWhitelistAdd(name);
                    else await handleOp(name);
                }}
            />
        </main>
    );
}

// A one-field prompt driven by `open`.
function PromptDialog({ open, onClose, icon, title, value, onChange, placeholder, label, maxLength, submitLabel, submitIcon, onSubmit }: {
    open: boolean;
    onClose: () => void;
    icon: React.ReactNode;
    title: string;
    value: string;
    onChange: (v: string) => void;
    placeholder: string;
    label: string;
    maxLength: number;
    submitLabel: string;
    submitIcon?: React.ReactNode;
    onSubmit: () => void | Promise<void>;
}) {
    return (
        <Dialog open={open} onOpenChange={o => { if (!o) onClose(); }}>
            <DialogBackdrop className="modal-overlay animate-fade-in" />
            <DialogContent className="modal-panel fixed inset-0 m-auto h-fit w-[calc(100%-2rem)] max-w-sm">
                <div className="modal-header flex items-center justify-between gap-3">
                    <DialogTitle className="modal-title flex min-w-0 items-center gap-2">
                        {icon}
                        <span className="truncate">{title}</span>
                    </DialogTitle>
                    <DialogClose aria-label="Close" className="btn btn-ghost btn-icon btn-sm shrink-0">
                        <X size={16} />
                    </DialogClose>
                </div>
                <div className="modal-body">
                    <input
                        type="text"
                        value={value}
                        onChange={e => onChange(e.target.value)}
                        placeholder={placeholder}
                        aria-label={label}
                        className="input-field w-full"
                        maxLength={maxLength}
                        autoFocus
                    />
                </div>
                <div className="modal-footer">
                    <DialogClose className="btn btn-secondary">Cancel</DialogClose>
                    <button onClick={() => { void onSubmit(); }} className="btn btn-primary" disabled={!value.trim()}>
                        {submitIcon}
                        {submitLabel}
                    </button>
                </div>
            </DialogContent>
        </Dialog>
    );
}
