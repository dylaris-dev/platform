"use client";

import { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { ShieldCheck, Shield, Plus, Pencil, Trash2 } from 'lucide-react';
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
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import '@/styles/tickets.css';

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
    const { servers, user, featureFlags } = useAppData();
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
            <div className="space-y-6">
                <PageHeader title="Access" description="Invite friends to help run your servers." />
                <SkeletonList rows={4} />
            </div>
        );
    }

    const fullCaps = fullAccessCaps(presets);
    const openInvite = () => setPanel({ kind: 'form', editing: null });

    return (
        <div className="space-y-6">
            <PageHeader
                title="Access"
                description={<>
                    <span className="tk-pill mr-2">{modeLabel(mode)}</span>
                    {user?.isAdmin ? (
                        <Link href="/settings/roles" className="text-(--accent-light) hover:underline focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">Managed in Settings -&gt; Roles</Link>
                    ) : (
                        <span>Managed in Settings -&gt; Roles</span>
                    )}
                </>}
                actions={mode !== 'off' ? <button type="button" onClick={openInvite} className="btn btn-primary"><Plus size={14} /> Invite</button> : undefined}
            />

            {featureFlags.modpacks && (
                <div className="srv-callout" data-tone="info">
                    <div>
                        <p className="font-medium text-(--base-09)">Modpack access</p>
                        <p className="mt-1 text-(--base-07)">Who may work on a modpack is its Modrinth team: invite people and set their rights on modrinth.com. They see the pack here once they connect their own Modrinth token. These rights are separate from server access below.</p>
                    </div>
                </div>
            )}

            {mode === 'off' && (
                <EmptyState
                    icon={<ShieldCheck size={20} />}
                    title="Delegation is off"
                    description="Only you and panel staff can act on your servers. An admin can enable it in Settings -> Roles."
                />
            )}

            {mode !== 'off' && (
                <div className="tk-split items-start">
                    <nav aria-label="Invited users" className="srv-surface">
                        <div className="srv-label px-4 pb-1 pt-4">Invited</div>
                        {grantsLoading ? (
                            <div className="px-4 pb-4"><SkeletonList rows={3} /></div>
                        ) : grants.length === 0 ? (
                            <p className="px-4 pb-4 text-[13px] text-(--base-06)">No one invited yet.</p>
                        ) : (
                            <RowList label="Invited users">
                                {grants.map(g => (
                                    <Row
                                        key={`${g.username}:${g.serverId ?? 'account'}`}
                                        onClick={() => setPanel({ kind: 'detail', grant: g })}
                                        active={panel.kind === 'detail' && sameGrant(panel.grant, g.username, g.serverId)}
                                        title={g.username}
                                        meta={<>{g.accountWide ? 'Account-wide' : g.serverName} &middot; {describeGrantAccess(g, fullCaps)}</>}
                                    />
                                ))}
                            </RowList>
                        )}
                        {mode === 'advanced' && (
                            <>
                                <div className="srv-label px-4 pb-1 pt-3">Manage</div>
                                <RowList label="Manage">
                                    <Row
                                        onClick={() => setPanel({ kind: 'roles' })}
                                        active={panel.kind === 'roles'}
                                        leading={<Shield size={14} />}
                                        title="Roles"
                                    />
                                </RowList>
                            </>
                        )}
                    </nav>

                    <div className="min-w-0">
                        {panel.kind === 'empty' && (
                            <EmptyState
                                icon={<ShieldCheck size={20} />}
                                title="Select an invited user"
                                description="Pick someone from the list, or invite a new person."
                                action={<button type="button" onClick={openInvite} className="btn btn-primary btn-sm"><Plus size={14} /> Invite a friend</button>}
                            />
                        )}

                        {panel.kind === 'detail' && (() => {
                            const g = panel.grant;
                            const editable = canEditInMode(g, mode, fullCaps);
                            return (
                                <div className="srv-surface max-w-xl space-y-3 p-6">
                                    <div className="flex flex-wrap items-center gap-2">
                                        <h2 className="text-[15px] font-semibold text-(--base-09)">{g.username}</h2>
                                        <span className="tk-pill">{g.accountWide ? 'Account-wide' : g.serverName}</span>
                                    </div>
                                    <p className="text-sm text-(--base-07)">Access: {describeGrantAccess(g, fullCaps)}</p>
                                    {isProxyScope(ownedServers, g.serverId) && g.inherit && (
                                        <p className="text-xs text-(--base-06)">Cascades to the servers behind this proxy</p>
                                    )}
                                    {!editable && (
                                        <p className="text-xs text-(--base-06)">This grant predates full-only mode; revoke and re-invite to change it.</p>
                                    )}
                                    <div className="flex items-center gap-2 pt-1">
                                        {editable && (
                                            <button type="button" onClick={() => setPanel({ kind: 'form', editing: g })} className="btn btn-secondary btn-sm">
                                                <Pencil size={12} /> Edit
                                            </button>
                                        )}
                                        <button type="button" onClick={() => handleRevoke(g)} className="btn btn-secondary btn-sm">
                                            <Trash2 size={12} className="text-(--error-light)" /> Revoke
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
        </div>
    );
}
