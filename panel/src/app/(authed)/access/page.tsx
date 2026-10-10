"use client";

import { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { ShieldCheck, Plus, Pencil, Trash2 } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import {
    getPermissionsMode, getCatalog, getPresets,
    type PermissionsMode, type CatalogScope, type Preset,
} from '@/lib/api/authzCatalog';
import { listGrants, revokeGrant, type Grant } from '@/lib/api/grants';
import { listServerRoles, type ServerRole } from '@/lib/api/serverRoles';
import { SkeletonList } from '@/components/Skeleton';
import AccessServerRoles from '@/components/access/AccessServerRoles';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import AccessGrantForm from '@/components/access/AccessGrantForm';
import { modeLabel, describeGrantAccess, fullAccessCaps, canEditInMode, isProxyScope } from '@/lib/access/accessMode';
import { toast } from '@/components/ui/Toast';

// Owner-facing delegation UI, master-detail layout. Behavior branches on the
// account's permissions_mode:
//   off      - delegation is disabled platform-wide; informational only.
//   simple   - "Full-only": invite a friend as a complete server admin.
//   advanced - "Admin-roles": named per-server roles plus per-friend
//              grant/deny capability overrides.
// The sidebar mirrors settings/layout.tsx's grouped-nav style; the right
// pane is a single detail panel driven by the `panel` state machine below.

type Panel =
    | { kind: 'empty' }
    | { kind: 'detail'; grant: Grant }
    | { kind: 'form'; editing: Grant | null }
    | { kind: 'roles' };

function sameGrant(a: Grant, username: string, serverId: number | null): boolean {
    return a.username === username && a.serverId === serverId;
}

export default function AccessPage() {
    const { servers, user } = useAppData();
    const ownedServers = servers.filter(s => s.role === 'owner');

    const [mode, setMode] = useState<PermissionsMode | null>(null);
    const [catalog, setCatalog] = useState<CatalogScope[]>([]);
    const [presets, setPresets] = useState<Preset[]>([]);

    const [grants, setGrants] = useState<Grant[]>([]);
    const [grantsLoading, setGrantsLoading] = useState(true);

    const [serverRoles, setServerRoles] = useState<ServerRole[]>([]);
    const [serverRolesLoading, setServerRolesLoading] = useState(true);

    const [panel, setPanel] = useState<Panel>({ kind: 'empty' });

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refreshGrants = useCallback(async () => {
        const res = await listGrants();
        if (res.success && res.grants) setGrants(res.grants);
        setGrantsLoading(false);
    }, []);

    const refreshServerRoles = useCallback(async () => {
        const res = await listServerRoles();
        if (res.success && res.roles) setServerRoles(res.roles);
        setServerRolesLoading(false);
    }, []);

    useEffect(() => {
        (async () => {
            const modeRes = await getPermissionsMode();
            // Fail closed: if the mode can't be read, treat delegation as
            // disabled rather than getting stuck on the loading skeleton.
            setMode(modeRes.success && modeRes.mode ? modeRes.mode : 'off');
        })();
        (async () => {
            const res = await getCatalog();
            if (res.success && res.catalog) setCatalog(res.catalog);
        })();
        (async () => {
            const res = await getPresets();
            if (res.success && res.presets) setPresets(res.presets);
        })();
        refreshGrants();
        refreshServerRoles();
    }, [refreshGrants, refreshServerRoles]);

    const handleRevoke = async (g: Grant) => {
        // One-click trash sitting next to Edit in the same row. Revoking drops
        // the grant and the capabilities configured on it, so re-granting starts
        // from a preset again - a misclick is not a free mistake.
        if (!(await confirmDialog({
            title: 'Revoke access',
            message: `Revoke ${g.username}'s access to this server? The grant and the capabilities set on it are removed.`,
            confirmLabel: 'Revoke',
        }))) return;
        const res = await revokeGrant(g.username, g.serverId);
        if (res.success) {
            showToast('Grant revoked.', true);
            refreshGrants();
            setPanel({ kind: 'empty' });
        } else {
            showToast(res.message || 'Revoke failed', false);
        }
    };

    if (mode === null) {
        return (
            <main className="flex-1 overflow-y-auto p-6 max-w-4xl">
                <h1 className="h-page mb-6">Access</h1>
                <SkeletonList rows={4} />
            </main>
        );
    }

    const fullCaps = fullAccessCaps(presets);

    return (
        <main className="flex-1 overflow-y-auto p-6">
            <div className="flex items-center gap-3 mb-6">
                <h1 className="h-page">Access</h1>
                <span className="mono-label bg-(--base-03) text-(--base-07) px-1.5 py-0.5 rounded-sm">
                    {modeLabel(mode)}
                </span>
                {user?.isAdmin ? (
                    <Link
                        href="/settings/roles"
                        className="mono-label text-(--base-05) hover:text-(--accent-light) focus-visible:text-(--accent-light) outline-none transition-colors"
                    >
                        Managed in Settings -&gt; Roles
                    </Link>
                ) : (
                    <span className="mono-label text-(--base-05)">Managed in Settings -&gt; Roles</span>
                )}
            </div>

            {mode === 'off' && (
                <div className="card p-8 flex flex-col items-center text-center gap-2 max-w-4xl">
                    <ShieldCheck size={28} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">
                        Delegation is off. Only you and panel staff can act on your servers. An admin can enable it in Settings -&gt; Roles.
                    </p>
                </div>
            )}

            {mode !== 'off' && (
                <div className="flex gap-6">
                    <nav className="w-52 shrink-0 border-r border-(--base-03) pr-3 flex flex-col gap-7">
                        <div className="flex flex-col gap-1">
                            <div className="flex items-center justify-between px-3">
                                <span className="mono-label">Invited</span>
                                <button
                                    onClick={() => setPanel({ kind: 'form', editing: null })}
                                    className="flex items-center gap-1 text-xs text-(--accent-light) hover:text-(--base-09)"
                                    aria-label="Invite"
                                >
                                    <Plus size={14} />
                                    Invite
                                </button>
                            </div>

                            {grantsLoading ? (
                                <div className="px-3">
                                    <SkeletonList rows={3} />
                                </div>
                            ) : grants.length === 0 ? (
                                <p className="px-3 text-xs text-(--base-06)">No one invited yet.</p>
                            ) : (
                                grants.map(g => {
                                    const isActive = panel.kind === 'detail' && sameGrant(panel.grant, g.username, g.serverId);
                                    return (
                                        <button
                                            key={`${g.username}:${g.serverId ?? 'account'}`}
                                            onClick={() => setPanel({ kind: 'detail', grant: g })}
                                            className={`px-3 py-2 rounded-r-md text-left border-l-[3px] transition-colors ${
                                                isActive
                                                    ? 'border-(--accent) bg-(--accent)/10 text-(--accent-light)'
                                                    : 'border-transparent text-(--base-07) hover:text-(--base-09) hover:bg-(--base-03)'
                                            }`}
                                        >
                                            <div className="text-sm font-medium truncate">{g.username}</div>
                                            <div className="mono-label text-(--base-06) truncate">
                                                {g.accountWide ? 'Account-wide' : g.serverName} &middot; {describeGrantAccess(g, fullCaps)}
                                            </div>
                                        </button>
                                    );
                                })
                            )}
                        </div>

                        {mode === 'advanced' && (
                            <div className="flex flex-col gap-1">
                                <span className="mono-label px-3">Manage</span>
                                <button
                                    onClick={() => setPanel({ kind: 'roles' })}
                                    className={`px-3 py-2 rounded-r-md text-left text-sm font-medium border-l-[3px] transition-colors ${
                                        panel.kind === 'roles'
                                            ? 'border-(--accent) bg-(--accent)/10 text-(--accent-light)'
                                            : 'border-transparent text-(--base-07) hover:text-(--base-09) hover:bg-(--base-03)'
                                    }`}
                                >
                                    Roles
                                </button>
                            </div>
                        )}
                    </nav>

                    <div className="flex-1 min-w-0">
                        {panel.kind === 'empty' && (
                            <div className="card p-8 flex flex-col items-center text-center gap-3">
                                <ShieldCheck size={28} className="text-(--base-05)" />
                                <p className="text-sm text-(--base-07)">
                                    Select an invited user on the left, or invite a new one.
                                </p>
                                <button
                                    onClick={() => setPanel({ kind: 'form', editing: null })}
                                    className="btn btn-primary btn-sm"
                                >
                                    <Plus size={14} />
                                    Invite a friend
                                </button>
                            </div>
                        )}

                        {panel.kind === 'detail' && (() => {
                            const g = panel.grant;
                            const editable = canEditInMode(g, mode, fullCaps);
                            return (
                                <div className="card p-6 max-w-xl">
                                    <div className="flex items-center gap-2 flex-wrap mb-1">
                                        <h2 className="text-sm font-medium text-(--base-09)">{g.username}</h2>
                                        <span className="mono-label bg-(--base-03) text-(--base-07) px-1.5 rounded-sm">
                                            {g.accountWide ? 'Account-wide' : g.serverName}
                                        </span>
                                    </div>
                                    <p className="text-sm text-(--base-07) mb-1">
                                        Access: {describeGrantAccess(g, fullCaps)}
                                    </p>
                                    {isProxyScope(ownedServers, g.serverId) && g.inherit && (
                                        <p className="text-xs text-(--base-06) mb-1">
                                            Cascades to the servers behind this proxy
                                        </p>
                                    )}
                                    {!editable && (
                                        <p className="text-xs text-(--base-06) mb-3">
                                            This grant predates full-only mode; revoke and re-invite to change it.
                                        </p>
                                    )}
                                    <div className="flex items-center gap-2 mt-4">
                                        {editable && (
                                            <button
                                                onClick={() => setPanel({ kind: 'form', editing: g })}
                                                className="btn btn-secondary btn-sm"
                                            >
                                                <Pencil size={12} />
                                                Edit
                                            </button>
                                        )}
                                        <button
                                            onClick={() => handleRevoke(g)}
                                            className="btn btn-secondary btn-sm"
                                        >
                                            <Trash2 size={12} className="text-(--error-light)" />
                                            Revoke
                                        </button>
                                    </div>
                                </div>
                            );
                        })()}

                        {panel.kind === 'form' && (mode === 'simple' || mode === 'advanced') && (
                            <AccessGrantForm
                                key={panel.editing ? `edit-${panel.editing.username}-${panel.editing.serverId ?? 'acct'}` : 'new'}
                                mode={mode}
                                ownedServers={ownedServers}
                                catalog={catalog}
                                roles={serverRoles}
                                presets={presets}
                                editing={panel.editing}
                                showToast={showToast}
                                onSaved={() => { refreshGrants(); setPanel({ kind: 'empty' }); }}
                                onCancel={() => setPanel({ kind: 'empty' })}
                            />
                        )}

                        {panel.kind === 'roles' && (
                            <AccessServerRoles
                                catalog={catalog}
                                roles={serverRoles}
                                loading={serverRolesLoading}
                                onRolesChanged={refreshServerRoles}
                                showToast={showToast}
                            />
                        )}
                    </div>
                </div>
            )}

        </main>
    );
}
