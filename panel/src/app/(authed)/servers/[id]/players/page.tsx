"use client";

import React, { useCallback, useEffect, useMemo, useState } from 'react';

import { Users, UserRound, Power, ShieldX, Skull, ShieldCheck, ShieldOff, Trash2, RefreshCw, Search, Send, AlertTriangle, Crown, ListChecks, CircleCheck, X, ListPlus, MessageSquare, Terminal, Lock } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import PlayerHead from '@/components/PlayerHead';
import {
    getRconConfig, parsePlayerList, friendlyRconError, type OnlinePlayer,
} from '@/lib/api/rcon';
import {
    getPlayerLists, getOnlinePlayers, playerAction,
    type PlayerListEntry, type PlayerAction, type KnownPlayer,
} from '@/lib/api/players';
import { isServerLive, mergeAllPlayers, type AllPlayerRow } from '@/lib/playersView';
import RconConfigCard from '@/components/RconConfigCard';
import { Skeleton, SkeletonText, SkeletonCircle } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';
import ModalPanel from '@/components/ui/ModalPanel';

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
    const [loading, setLoading] = useState(false);
    const [actionError, setActionError] = useState<string | null>(null);
    const [rconEnabled, setRconEnabled] = useState(false);
    const [rconLoaded, setRconLoaded] = useState(false);
    // False once the RCON config read comes back 403: this viewer may manage
    // players but not look at the server's configuration.
    const [rconConfigVisible, setRconConfigVisible] = useState(true);

    const [confirm, setConfirm] = useState<{
        title: string;
        message: string;
        onConfirm: () => void | Promise<void>;
        danger?: boolean;
    } | null>(null);
    const [tellPrompt, setTellPrompt] = useState<{ player: string; message: string } | null>(null);
    const [addPrompt, setAddPrompt] = useState<{ target: 'whitelist' | 'ops'; name: string } | null>(null);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    // server.status follows the servers.changed SSE, so this flips without a
    // reload and the refresh below re-runs on it.
    const live = useMemo(() => isServerLive(server?.status), [server?.status]);
    // The server list carries no players.manage bit, so the one viewer known not
    // to hold it is a demo visitor. A member without it is refused by Core and
    // sees that refusal as a toast.
    const canManage = server?.role !== 'demo';
    const actionsBlocked = !live || !canManage;
    const blockedHint = !canManage ? 'Read-only: you cannot manage players on this server' : OFFLINE_HINT;
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
        if (!serverId || effectiveSection === 'rcon') return;
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
        if (!res.success) {
            showToast(`${label}: ${friendlyRconError(res.error, 'failed')}`, false);
        } else {
            showToast(`${label} ✓`, true);
            refresh(true);
        }
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

    return (
        // h-full, not flex-1: ServerShell's children box is a scrolling BLOCK, so
        // flex-1 sizes nothing here and the player list below never reached its own
        // overflow - the whole window scrolled instead. The p-6 goes with it; the
        // shell already applies one.
        <main className="h-full flex flex-col gap-4 overflow-hidden">
            <header className="flex items-center gap-3 shrink-0">
                <Users size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">Players</h1>
                {live && ROSTER_SECTIONS.includes(effectiveSection) && (
                    <span className="text-xs text-(--base-06)">Online list polls every 10s</span>
                )}
                {effectiveSection !== 'rcon' && (
                    <div className="ml-auto flex items-center gap-2">
                        <div className="relative">
                            <Search size={12} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-(--base-05)" />
                            <input
                                type="text"
                                value={search}
                                onChange={e => setSearch(e.target.value)}
                                placeholder="Search…"
                                className="input-field pl-7 w-48 text-xs py-1"
                            />
                        </div>
                        {/* Not onClick={refresh}: the handler would receive the
                            MouseEvent as `background` and every manual refresh
                            would silently become a background one. */}
                        <button onClick={() => refresh()} className="btn btn-secondary btn-sm" disabled={loading}>
                            <RefreshCw size={12} className={loading ? 'animate-spin' : ''} />
                            Refresh
                        </button>
                    </div>
                )}
            </header>

            {/* Section strip */}
            <nav className="flex gap-1 shrink-0 border-b border-(--base-03)">
                {visibleSections.map(({ id, label, Icon }) => {
                    const locked = !rconEnabled && RCON_DEPENDENT.includes(id);
                    const active = effectiveSection === id;
                    return (
                        <button
                            key={id}
                            onClick={() => { if (!locked) setSection(id); }}
                            disabled={locked}
                            title={locked ? 'Enable RCON to unlock this section' : undefined}
                            className={`flex items-center gap-1.5 px-3 py-2 text-xs font-medium border-b-2 transition-colors ${
                                active
                                    ? 'border-(--accent) text-(--accent-light)'
                                    : locked
                                        ? 'border-transparent text-(--base-05) opacity-50 cursor-not-allowed'
                                        : 'border-transparent text-(--base-07) hover:text-(--base-09) hover:border-(--base-04)'
                            }`}
                        >
                            <Icon size={12} />
                            {label}
                            {locked && <Lock size={10} className="opacity-70" />}
                            {(id === 'online' || id === 'all') && !locked && online.length > 0 && (
                                <span className="ml-1 mono-label bg-(--base-03) px-1 rounded-sm">{online.length}</span>
                            )}
                        </button>
                    );
                })}
            </nav>

            {actionError && (
                <div className="shrink-0 flex items-start gap-2 px-3 py-2 rounded-md bg-(--error-ghost) border border-(--error)/30 text-(--error-light) text-sm">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <span>{actionError}</span>
                </div>
            )}

            {/* Content */}
            <div className="flex-1 overflow-y-auto">
                {!rconLoaded ? (
                    <div className="space-y-1.5">
                        {Array.from({ length: 4 }).map((_, i) => (
                            <Skeleton key={i} className="h-12 rounded-md" />
                        ))}
                    </div>
                ) : effectiveSection === 'rcon' ? (
                    <div className="space-y-3">
                        {!rconEnabled && (
                            <div className="flex items-start gap-2 px-3 py-2 rounded-md bg-(--accent-ghost) border border-(--accent)/30 text-(--base-08) text-xs">
                                <Lock size={13} className="shrink-0 mt-0.5 text-(--accent-light)" />
                                <span>
                                    RCON is off. Enable it below to unlock All players, Online, Bans, Whitelist and Operators - live
                                    player management runs over RCON.
                                </span>
                            </div>
                        )}
                        <RconConfigCard serverId={serverId} onEnabledChange={setRconEnabled} />
                    </div>
                ) : (
                    <>
                {!live && (
                    <div className="flex items-start gap-2 px-3 py-2 mb-3 rounded-md bg-(--base-02) border border-(--base-03) text-(--base-07) text-xs">
                        <Power size={13} className="shrink-0 mt-0.5" />
                        <span>
                            The server is offline - start it to see who is online.
                            {effectiveSection !== 'online' && ' The lists below are read from its files; changing them needs the server running.'}
                        </span>
                    </div>
                )}

                {effectiveSection === 'whitelist' && (
                    <section className="card p-3 mb-3 space-y-2" aria-label="Whitelist status">
                        <div className="flex items-center gap-3 flex-wrap">
                            <span className="text-sm font-medium text-(--base-09)">Whitelist</span>
                            {whitelistState.enabled === undefined ? (
                                <span className="badge badge-neutral">Unknown</span>
                            ) : whitelistState.enabled ? (
                                <span className="badge badge-success">On</span>
                            ) : (
                                <span className="badge badge-warning">Off - anyone can join</span>
                            )}
                            {whitelistState.enforced !== undefined && (
                                <span
                                    className="badge badge-neutral"
                                    title="enforce-whitelist has no RCON command. Change it under Configuration (server.properties); it applies after a restart."
                                >
                                    {whitelistState.enforced ? 'Enforced' : 'Not enforced'}
                                </span>
                            )}
                            {whitelistState.enabled !== undefined && (
                                <button
                                    onClick={() => handleWhitelistToggle(!whitelistState.enabled)}
                                    disabled={actionsBlocked}
                                    title={!canManage ? blockedHint : live ? undefined : 'Start the server to change this, or change white-list under Configuration'}
                                    className="btn btn-secondary btn-sm ml-auto"
                                >
                                    {whitelistState.enabled ? 'Turn off' : 'Turn on'}
                                </button>
                            )}
                        </div>
                        <p className="text-xs text-(--base-06)">
                            Operators can always join, even when they are not on the whitelist.
                            {' '}Enforced kicks online players who are removed from the whitelist; change it under Configuration.
                        </p>
                    </section>
                )}

                {/* "Add to whitelist/ops" affordance for those sections */}
                {(effectiveSection === 'whitelist' || effectiveSection === 'ops') && (
                    <button
                        onClick={() => setAddPrompt({ target: effectiveSection as 'whitelist' | 'ops', name: '' })}
                        disabled={actionsBlocked}
                        title={actionsBlocked ? blockedHint : undefined}
                        className="btn btn-secondary btn-sm mb-3"
                    >
                        <ListPlus size={12} />
                        Add player to {effectiveSection === 'whitelist' ? 'whitelist' : 'operators'}
                    </button>
                )}

                {loading ? (
                    <div className="space-y-1.5">
                        {Array.from({ length: 6 }).map((_, i) => (
                            <article key={i} className="card p-2 flex items-center gap-3">
                                <SkeletonCircle size="w-8 h-8 rounded-sm" />
                                <div className="min-w-0 flex-1">
                                    <SkeletonText width="w-32" className="h-3.5" />
                                </div>
                                <div className="flex items-center gap-1">
                                    <Skeleton className="w-7 h-7 rounded-md" />
                                    <Skeleton className="w-7 h-7 rounded-md" />
                                    <Skeleton className="w-7 h-7 rounded-md" />
                                </div>
                            </article>
                        ))}
                    </div>
                ) : currentList.length === 0 ? (
                    // The offline note above already says why the roster is empty.
                    effectiveSection === 'online' && !live ? null : (
                        <div className="text-center py-12 text-sm text-(--base-06)">
                            {section === 'online' ? 'Nobody is online.' : section === 'all' ? 'Nobody has joined yet.' : 'No entries.'}
                        </div>
                    )
                ) : (
                    <div className="space-y-1.5">
                        {(section === 'all' ? currentList.slice(0, allLimit) : currentList).map((p: any) => (
                            <article key={`${section}-${p.name}`} className="card p-2 flex items-center gap-3">
                                {/* Player head */}
                                <PlayerHead
                                    name={p.name}
                                    className="w-8 h-8 rounded-sm shrink-0 bg-(--base-03)"
                                    fallback={
                                        <div className="w-8 h-8 rounded-sm shrink-0 bg-(--base-03) flex items-center justify-center text-xs font-semibold text-(--base-07)">
                                            {String(p.name).charAt(0).toUpperCase()}
                                        </div>
                                    }
                                />
                                <div className="min-w-0 flex-1">
                                    <div className="text-sm font-medium text-(--base-09) flex items-center gap-1.5 flex-wrap">
                                        {p.name}
                                        {section === 'all' && (p as AllPlayerRow).online && <span className="badge badge-success">Online</span>}
                                        {section === 'all' && opped.has(p.name.toLowerCase()) && <span className="badge badge-accent">Op</span>}
                                        {section === 'all' && whitelisted.has(p.name.toLowerCase()) && <span className="badge badge-neutral">Whitelisted</span>}
                                        {section === 'all' && banned.has(p.name.toLowerCase()) && <span className="badge badge-error">Banned</span>}
                                    </div>
                                    {section === 'bans' && (p as PlayerListEntry).reason && (
                                        <div className="text-xs text-(--base-06) truncate">Reason: {(p as PlayerListEntry).reason}</div>
                                    )}
                                </div>
                                {/* Per-section actions. A disabled fieldset disables every
                                    button in it: they all go through RCON. */}
                                <fieldset disabled={actionsBlocked} title={actionsBlocked ? blockedHint : undefined} className="flex items-center gap-1 border-0 p-0 m-0 min-w-0">
                                    {section === 'all' && (() => {
                                        const key = p.name.toLowerCase();
                                        return (
                                            <>
                                                {(p as AllPlayerRow).online && (
                                                    <button onClick={() => handleKick(p.name)} className="btn btn-secondary btn-sm" title="Kick" aria-label={`Kick ${p.name}`}>
                                                        <ShieldOff size={12} />
                                                    </button>
                                                )}
                                                {!whitelisted.has(key) && (
                                                    <button onClick={() => handleWhitelistAdd(p.name)} className="btn btn-secondary btn-sm" title="Add to whitelist" aria-label={`Add ${p.name} to whitelist`}>
                                                        <ListChecks size={12} />
                                                    </button>
                                                )}
                                                {!opped.has(key) && (
                                                    <button onClick={() => handleOp(p.name)} className="btn btn-secondary btn-sm" title="Op" aria-label={`Op ${p.name}`}>
                                                        <ShieldCheck size={12} className="text-(--accent-light)" />
                                                    </button>
                                                )}
                                                {banned.has(key) ? (
                                                    <button onClick={() => handleUnban(p.name)} className="btn btn-secondary btn-sm" title="Unban" aria-label={`Unban ${p.name}`}>
                                                        <CircleCheck size={12} className="text-(--success-light)" />
                                                    </button>
                                                ) : (
                                                    <button onClick={() => handleBan(p.name)} className="btn btn-secondary btn-sm" title="Ban" aria-label={`Ban ${p.name}`}>
                                                        <Skull size={12} className="text-(--error-light)" />
                                                    </button>
                                                )}
                                            </>
                                        );
                                    })()}
                                    {section === 'online' && (
                                        <>
                                            <button onClick={() => setTellPrompt({ player: p.name, message: '' })} className="btn btn-secondary btn-sm" title="Whisper">
                                                <MessageSquare size={12} />
                                            </button>
                                            <button onClick={() => handleOp(p.name)} className="btn btn-secondary btn-sm" title="Op">
                                                <ShieldCheck size={12} className="text-(--accent-light)" />
                                            </button>
                                            <button onClick={() => handleKick(p.name)} className="btn btn-secondary btn-sm" title="Kick">
                                                <ShieldOff size={12} />
                                            </button>
                                            <button onClick={() => handleBan(p.name)} className="btn btn-secondary btn-sm" title="Ban">
                                                <Skull size={12} className="text-(--error-light)" />
                                            </button>
                                        </>
                                    )}
                                    {section === 'bans' && (
                                        <button onClick={() => handleUnban(p.name)} className="btn btn-secondary btn-sm">
                                            <CircleCheck size={12} className="text-(--success-light)" />
                                            Unban
                                        </button>
                                    )}
                                    {section === 'whitelist' && (
                                        <button onClick={() => handleWhitelistRemove(p.name)} className="btn btn-secondary btn-sm" title="Remove">
                                            <Trash2 size={12} className="text-(--error-light)" />
                                        </button>
                                    )}
                                    {section === 'ops' && (
                                        <button onClick={() => handleDeop(p.name)} className="btn btn-secondary btn-sm" title="De-op">
                                            <Crown size={12} className="text-(--error-light)" />
                                        </button>
                                    )}
                                </fieldset>
                            </article>
                        ))}
                        {section === 'all' && currentList.length > allLimit && (
                            <button onClick={() => setAllLimit(n => n + ALL_PAGE)} className="btn btn-secondary btn-sm w-full">
                                Show more ({currentList.length - allLimit} more)
                            </button>
                        )}
                    </div>
                )}
                    </>
                )}
            </div>

            {/* Confirm modal */}
            {confirm && (
                <div className="modal-overlay animate-fade-in" onClick={() => setConfirm(null)}>
                    <ModalPanel onClose={() => setConfirm(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className={`modal-title flex items-center gap-2 ${confirm.danger ? 'text-(--error-light)' : ''}`}>
                                {confirm.danger && <AlertTriangle size={18} />}
                                {confirm.title}
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">{confirm.message}</p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setConfirm(null)} className="btn btn-secondary">Cancel</button>
                            <button
                                onClick={async () => {
                                    const fn = confirm.onConfirm;
                                    setConfirm(null);
                                    await fn();
                                }}
                                className={confirm.danger ? 'btn btn-danger' : 'btn btn-primary'}
                            >
                                Confirm
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Tell prompt */}
            {tellPrompt && (
                <div className="modal-overlay animate-fade-in" onClick={() => setTellPrompt(null)}>
                    <ModalPanel onClose={() => setTellPrompt(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <MessageSquare size={16} />
                                Whisper to {tellPrompt.player}
                            </h3>
                            <button onClick={() => setTellPrompt(null)} className="text-(--base-06)"><X size={16} /></button>
                        </div>
                        <div className="modal-body">
                            <input
                                type="text"
                                value={tellPrompt.message}
                                onChange={e => setTellPrompt({ ...tellPrompt, message: e.target.value })}
                                placeholder="Message…"
                                className="input-field w-full"
                                maxLength={200}
                                autoFocus
                            />
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setTellPrompt(null)} className="btn btn-secondary">Cancel</button>
                            <button
                                onClick={async () => {
                                    const msg = tellPrompt.message.trim();
                                    const target = tellPrompt.player;
                                    setTellPrompt(null);
                                    if (msg) await run('Whisper', 'tell', target, { message: msg });
                                }}
                                className="btn btn-primary"
                                disabled={!tellPrompt.message.trim()}
                            >
                                <Send size={12} />
                                Send
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Add to whitelist/ops */}
            {addPrompt && (
                <div className="modal-overlay animate-fade-in" onClick={() => setAddPrompt(null)}>
                    <ModalPanel onClose={() => setAddPrompt(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <ListPlus size={16} />
                                Add to {addPrompt.target === 'whitelist' ? 'whitelist' : 'operators'}
                            </h3>
                            <button onClick={() => setAddPrompt(null)} className="text-(--base-06)"><X size={16} /></button>
                        </div>
                        <div className="modal-body">
                            <input
                                type="text"
                                value={addPrompt.name}
                                onChange={e => setAddPrompt({ ...addPrompt, name: e.target.value })}
                                placeholder="Player name"
                                className="input-field w-full"
                                maxLength={32}
                                autoFocus
                            />
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setAddPrompt(null)} className="btn btn-secondary">Cancel</button>
                            <button
                                onClick={async () => {
                                    const name = addPrompt.name.trim();
                                    const target = addPrompt.target;
                                    setAddPrompt(null);
                                    if (!name) return;
                                    if (target === 'whitelist') await handleWhitelistAdd(name);
                                    else await handleOp(name);
                                }}
                                className="btn btn-primary"
                                disabled={!addPrompt.name.trim()}
                            >
                                Add
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}

        </main>
    );
}
