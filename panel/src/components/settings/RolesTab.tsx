"use client";

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { CircleCheck, Plus, Pencil, Trash2, X, ShieldCheck, UserCog, Eye, Search } from 'lucide-react';
import { getCatalog, getPermissionsMode, type CatalogScope, type PermissionsMode } from '@/lib/api/authzCatalog';
import {
    listPanelRoles,
    createPanelRole,
    updatePanelRole,
    deletePanelRole,
    assignUserPanelRole,
    getUserPanelRole,
    listPanelAssignments,
    setPermissionsMode,
    type PanelRole,
    type PanelAssignment,
} from '@/lib/api/panelRoles';
import { getUsers, type User } from '@/lib/api';
import CapabilityPicker from '@/components/access/CapabilityPicker';
import { SkeletonCard } from '@/components/Skeleton';
import { MODE_LABELS, MODE_HELP } from '@/lib/access/accessMode';
import { privilegedUsers, searchUsers } from '@/lib/access/panelAssignments';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard from '@/components/settings/SettingsCard';

// Panel-admin Settings tab (F6): (A) the global permissions_mode 3-state
// switch, (B) panel-role CRUD, (C) assigning a panel role + grant/deny
// overrides to a user. Settings/layout.tsx already admin-gates this route,
// so no extra gate is needed here. Every capability list renders from the
// catalog endpoint - no hardcoded permission arrays.

const MODE_OPTIONS: { value: PermissionsMode; label: string }[] = [
    { value: 'off', label: MODE_LABELS.off },
    { value: 'simple', label: MODE_LABELS.simple },
    { value: 'advanced', label: MODE_LABELS.advanced },
];


export default function RolesTab() {
    const [mode, setMode] = useState<PermissionsMode>('off');
    const [deletingPanelRole, runDeleteRole] = useBusy();
    const [modeSaving, setModeSaving] = useState(false);

    const [catalog, setCatalog] = useState<CatalogScope[]>([]);
    const [roles, setRoles] = useState<PanelRole[]>([]);
    const [users, setUsers] = useState<User[]>([]);
    const [assignments, setAssignments] = useState<PanelAssignment[]>([]);
    const [loading, setLoading] = useState(true);
    const [userQuery, setUserQuery] = useState('');

    const [roleModal, setRoleModal] = useState<{ role: PanelRole | null } | null>(null);
    const [deletingRole, setDeletingRole] = useState<PanelRole | null>(null);
    const [assignUser, setAssignUser] = useState<User | null>(null);
    const [viewingRole, setViewingRole] = useState<PanelRole | null>(null);

    const showToast = useCallback((msg: string, ok = true) => toast(msg, ok), []);

    const loadRoles = useCallback(async () => {
        const res = await listPanelRoles();
        if (res.success && res.roles) setRoles(res.roles);
        else if (!res.success) showToast(res.message || 'Failed to load panel roles', false);
    }, [showToast]);

    // Who holds what. Kept separate from the role list because it changes for a
    // different reason - saving an assignment reloads this, editing a role does
    // not - and because a failure here must not empty the privilege list: an
    // empty one reads as "nobody has anything", which is the worst thing to be
    // wrong about on this screen.
    const loadAssignments = useCallback(async () => {
        const res = await listPanelAssignments();
        if (res.success && res.assignments) setAssignments(res.assignments);
        else if (!res.success) showToast(res.message || 'Failed to load panel role assignments', false);
    }, [showToast]);

    const privileged = useMemo(
        () => privilegedUsers(users, assignments, roles), [users, assignments, roles]);
    const matchingUsers = useMemo(() => searchUsers(users, userQuery), [users, userQuery]);

    useEffect(() => {
        (async () => {
            const [modeRes, catalogRes, usersRes] = await Promise.all([
                getPermissionsMode(),
                getCatalog(),
                getUsers(),
            ]);
            if (modeRes.success && modeRes.mode) setMode(modeRes.mode);
            else showToast(modeRes.message || 'Failed to load permissions mode - shown value is unconfirmed', false);
            if (catalogRes.success && catalogRes.catalog) setCatalog(catalogRes.catalog);
            if (usersRes.success && usersRes.users) setUsers(usersRes.users);
            await Promise.all([loadRoles(), loadAssignments()]);
            setLoading(false);
        })();
    }, [loadRoles, loadAssignments, showToast]);

    const handleSetMode = async (next: PermissionsMode) => {
        if (modeSaving || next === mode) return;
        const prev = mode;
        setMode(next);
        setModeSaving(true);
        const res = await setPermissionsMode(next);
        if (!res.success) {
            setMode(prev);
            showToast(res.message || 'Failed to update permissions mode', false);
        } else {
            showToast('Permissions mode updated');
        }
        setModeSaving(false);
    };

    const handleDeleteRole = async () => {
        if (!deletingRole) return;
        const res = await deletePanelRole(deletingRole.id);
        setDeletingRole(null);
        if (res.success) {
            showToast('Panel role deleted');
            loadRoles();
            // Everyone who held it is back to no role, so the list beside it is
            // now wrong until this returns.
            loadAssignments();
        } else {
            showToast(res.message || 'Failed to delete panel role', false);
        }
    };

    return (
        // The loading state goes through SettingsPage rather than a branch of
        // its own: the skeleton has to be as wide as the page, and this page
        // has two columns now, so a hand-rolled one drew a narrow placeholder
        // and then jumped. SettingsPage is the only thing that knows the width.
        <SettingsPage
            title="Roles and permissions"
            icon={ShieldCheck}
            width="5xl"
            loading={loading}
            skeletonCards={3}
            description="Who may delegate access on their own servers, which capability bundles panel staff can hold, and who holds them."
        >
            {/* Section A - permissions_mode */}
            <SettingsCard
                title="Permissions mode"
                description="Whether and how server owners can delegate access to invited friends on their Access page. Panel roles and per-user overrides below are unaffected and always enforced."
            >
                    <div className="flex items-center gap-2">
                        {MODE_OPTIONS.map(opt => (
                            <button
                                key={opt.value}
                                type="button"
                                disabled={modeSaving}
                                onClick={() => handleSetMode(opt.value)}
                                className={`btn btn-sm ${
                                    opt.value === mode
                                        ? 'bg-(--accent-ghost) text-(--accent-light) border border-(--accent-border)'
                                        : 'btn-secondary'
                                } disabled:opacity-40`}
                            >
                                {opt.label}
                            </button>
                        ))}
                    </div>
                    <p className="text-xs text-(--base-06)">{MODE_HELP[mode]}</p>
            </SettingsCard>

            {/* Section B - panel roles CRUD */}
            <SettingsCard
                title="Panel roles"
                bodySpacing="none"
                description="Capability bundles for panel staff. Always available to admins, independent of the permissions mode above."
                actions={
                    <button type="button" onClick={() => setRoleModal({ role: null })} className="btn btn-primary btn-sm shrink-0">
                        <Plus size={13} />
                        New role
                    </button>
                }
            >

                {roles.length === 0 ? (
                    <div className="card p-6 flex flex-col items-center text-center gap-2">
                        <ShieldCheck size={24} className="text-(--base-05)" />
                        <p className="text-sm text-(--base-07)">No panel roles yet.</p>
                    </div>
                ) : (
                    <div className="space-y-2">
                        {roles.map(role => (
                            <div key={role.id} className="card p-3.5 flex items-center gap-3">
                                <div className="min-w-0 flex-1">
                                    <div className="flex items-center gap-2">
                                        <span className="font-medium text-sm text-(--base-09)">{role.name}</span>
                                        {role.isSystem && <span className="badge badge-neutral">system</span>}
                                    </div>
                                    <span className="mono-label">{role.capabilities.length} capabilities</span>
                                </div>
                                <div className="flex items-center gap-2 shrink-0">
                                    <button
                                        type="button"
                                        onClick={() => setViewingRole(role)}
                                        className="btn btn-secondary btn-sm"
                                        title="View capabilities"
                                    >
                                        <Eye size={12} />
                                        View
                                    </button>
                                    {!role.isSystem && (
                                        <>
                                            <button
                                                type="button"
                                                onClick={() => setRoleModal({ role })}
                                                className="btn btn-secondary btn-sm"
                                                title="Edit role"
                                            >
                                                <Pencil size={12} />
                                                Edit
                                            </button>
                                            <button
                                                type="button"
                                                onClick={() => setDeletingRole(role)}
                                                className="btn btn-danger btn-sm"
                                                title="Delete role"
                                            >
                                                <Trash2 size={12} />
                                                Delete
                                            </button>
                                        </>
                                    )}
                                </div>
                            </div>
                        ))}
                    </div>
                )}
            </SettingsCard>

            {/* Section C - who holds what, and everybody else */}
            <div className="grid grid-cols-1 lg:grid-cols-2 gap-5 items-start">
                <SettingsCard
                    title="Who holds a panel role"
                    bodySpacing="none"
                    description="Everyone the panel has given something: a role, a capability override, or platform admin. Admins are listed because admin short-circuits every capability."
                >
                    {privileged.length === 0 ? (
                        <div className="card p-6 flex flex-col items-center text-center gap-2">
                            <ShieldCheck size={22} className="text-(--base-05)" />
                            <p className="text-sm text-(--base-07)">Nobody holds a panel role yet.</p>
                            <p className="text-xs text-(--base-06)">Find someone in the list beside this one and assign one.</p>
                        </div>
                    ) : (
                        <div className="table-wrapper">
                            <table className="w-full">
                                <thead>
                                    <tr>
                                        <th className="table-th text-left">User</th>
                                        <th className="table-th text-left">Panel role</th>
                                        <th className="table-th text-right">Edit</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {privileged.map(p => (
                                        <tr key={p.user.id} className="table-tr table-tr-hover">
                                            <td className="table-td">
                                                <span className="font-mono font-medium text-(--base-09)">{p.user.username}</span>
                                                {p.isAdmin && (
                                                    <span className="badge badge-accent ml-2" title="Platform admin: holds every panel capability regardless of the role beside it">
                                                        admin
                                                    </span>
                                                )}
                                            </td>
                                            <td className="table-td">
                                                {p.roleName
                                                    ? <span className="badge badge-neutral">{p.roleName}</span>
                                                    : <span className="text-xs text-(--base-05)">no role</span>}
                                                {(p.grantCount > 0 || p.denyCount > 0) && (
                                                    <span className="mono-label ml-2" title="Per-user capability overrides on top of the role">
                                                        {p.grantCount > 0 && `+${p.grantCount}`}
                                                        {p.grantCount > 0 && p.denyCount > 0 && ' '}
                                                        {p.denyCount > 0 && `-${p.denyCount}`}
                                                    </span>
                                                )}
                                            </td>
                                            <td className="table-td text-right">
                                                <button
                                                    type="button"
                                                    onClick={() => setAssignUser(p.user)}
                                                    className="btn px-2.5 py-1 text-xs bg-(--base-03) border border-(--base-04) text-(--base-07) hover:text-(--base-09) transition-colors"
                                                    title={`Edit ${p.user.username}'s panel role`}
                                                >
                                                    <UserCog size={13} />
                                                </button>
                                            </td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </div>
                    )}
                </SettingsCard>

                <SettingsCard
                    title="All users"
                    bodySpacing="none"
                    description="Search anyone and give them a panel role, plus optional per-user grant and deny overrides."
                >
                    <div className="relative mb-3">
                        <Search size={14} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-(--base-05) pointer-events-none" />
                        <input
                            type="search"
                            value={userQuery}
                            onChange={e => setUserQuery(e.target.value)}
                            placeholder="Search by username or email"
                            aria-label="Search users"
                            className="input-field w-full pl-8"
                            spellCheck={false}
                        />
                    </div>
                    <div className="table-wrapper max-h-[420px] overflow-y-auto">
                        <table className="w-full">
                            <thead>
                                <tr>
                                    <th className="table-th text-left">User</th>
                                    <th className="table-th text-right">Assign</th>
                                </tr>
                            </thead>
                            <tbody>
                                {matchingUsers.map(u => (
                                    <tr key={u.id} className="table-tr table-tr-hover">
                                        <td className="table-td min-w-0">
                                            <span className="font-mono font-medium text-(--base-09)">{u.username}</span>
                                            {u.email && <span className="block text-xs text-(--base-06) truncate">{u.email}</span>}
                                        </td>
                                        <td className="table-td text-right">
                                            <button
                                                type="button"
                                                onClick={() => setAssignUser(u)}
                                                className="btn px-2.5 py-1 text-xs bg-(--base-03) border border-(--base-04) text-(--base-07) hover:text-(--base-09) transition-colors"
                                                title={`Assign a panel role to ${u.username}`}
                                            >
                                                <UserCog size={13} />
                                            </button>
                                        </td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                        {matchingUsers.length === 0 && (
                            <p className="p-6 text-center text-sm text-(--base-06)">
                                {users.length === 0 ? 'No users.' : `Nobody matches "${userQuery}".`}
                            </p>
                        )}
                    </div>
                </SettingsCard>
            </div>

            {/* Create/Edit role modal */}
            {roleModal && (
                <RoleModal
                    role={roleModal.role}
                    catalog={catalog}
                    onClose={() => setRoleModal(null)}
                    onSaved={() => { setRoleModal(null); loadRoles(); showToast(roleModal.role ? 'Panel role updated' : 'Panel role created'); }}
                    onError={msg => showToast(msg, false)}
                />
            )}

            {/* Delete role confirm */}
            {deletingRole && (
                <div className="modal-overlay animate-fade-in" onClick={() => setDeletingRole(null)}>
                    <div className="modal-panel w-full max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title text-(--error-light)">Delete &quot;{deletingRole.name}&quot;?</h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">Users assigned this role fall back to no panel role. This cannot be undone.</p>
                        </div>
                        <div className="modal-footer">
                            <button type="button" onClick={() => setDeletingRole(null)} className="btn btn-secondary">Cancel</button>
                            <button type="button" onClick={() => runDeleteRole(handleDeleteRole)} disabled={deletingPanelRole} className="btn btn-danger disabled:opacity-40">Delete</button>
                        </div>
                    </div>
                </div>
            )}

            {/* Read-only capability viewer (system + custom roles) */}
            {viewingRole && (
                <ViewCapabilitiesModal
                    role={viewingRole}
                    catalog={catalog}
                    onClose={() => setViewingRole(null)}
                />
            )}

            {/* Assign panel role to user */}
            {assignUser && (
                <AssignRoleModal
                    user={assignUser}
                    roles={roles}
                    catalog={catalog}
                    onClose={() => setAssignUser(null)}
                    onSaved={() => { setAssignUser(null); loadAssignments(); showToast('User panel role updated'); }}
                    onError={msg => showToast(msg, false)}
                />
            )}

        </SettingsPage>
    );
}

// ---------------------------------------------
// Read-only capability viewer (system + custom roles)
// ---------------------------------------------
function ViewCapabilitiesModal({
    role,
    catalog,
    onClose,
}: {
    role: PanelRole;
    catalog: CatalogScope[];
    onClose: () => void;
}) {
    const capSet = new Set(role.capabilities);
    // The seeded 'admin' system role carries every panel capability; flag it so
    // we can add the "full access" note. The resolver still short-circuits admin
    // via the JWT claim, so this list is descriptive, not the source of power.
    const isAdmin = role.isSystem && role.name.toLowerCase() === 'admin';

    // Group the role's held capabilities by scope -> category using the catalog
    // labels. Only scopes/categories with at least one held capability render.
    const groups = catalog
        .map(scope => ({
            scope: scope.scope,
            categories: scope.categories
                .map(cat => ({ category: cat.category, caps: cat.capabilities.filter(c => capSet.has(c.id)) }))
                .filter(cat => cat.caps.length > 0),
        }))
        .filter(scope => scope.categories.length > 0);

    // Capabilities held by the role but absent from the catalog (e.g. a cap
    // retired from the registry). Surfaced verbatim so nothing is hidden.
    const known = new Set(catalog.flatMap(s => s.categories.flatMap(c => c.capabilities.map(cap => cap.id))));
    const unknown = role.capabilities.filter(id => !known.has(id));
    const multiScope = groups.length > 1;

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <div className="modal-panel w-full max-w-lg max-h-[85vh] flex flex-col" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <div>
                        <h3 className="modal-title flex items-center gap-2">
                            {role.name}
                            {role.isSystem && <span className="badge badge-neutral">system</span>}
                        </h3>
                        <p className="text-xs font-mono text-(--base-06)">{role.capabilities.length} capabilities</p>
                    </div>
                    <button onClick={onClose} className="p-1 rounded hover:bg-(--base-03) text-(--base-06)">
                        <X size={16} />
                    </button>
                </div>
                <div className="modal-body overflow-y-auto flex-1 space-y-5">
                    {isAdmin && (
                        <div className="alert alert-info text-xs flex items-start gap-2">
                            <ShieldCheck size={14} className="shrink-0 mt-0.5" />
                            <span>Admins have full, unrestricted access to every panel capability. The complete list is shown below.</span>
                        </div>
                    )}
                    {groups.length === 0 && unknown.length === 0 ? (
                        <p className="text-sm text-(--base-06)">This role holds no capabilities.</p>
                    ) : (
                        groups.map(scope => (
                            <div key={scope.scope} className="space-y-3">
                                {multiScope && (
                                    <div className="text-sm font-display font-semibold text-(--base-08) capitalize">{scope.scope}</div>
                                )}
                                {scope.categories.map(cat => (
                                    <div key={`${scope.scope}.${cat.category}`}>
                                        <div className="mono-label text-(--base-06) mb-1.5">{cat.category}</div>
                                        <ul className="space-y-1">
                                            {cat.caps.map(c => (
                                                <li key={c.id} className="flex items-center gap-2 text-sm text-(--base-09)">
                                                    <CircleCheck size={13} className="shrink-0 text-(--success-light)" />
                                                    {c.label}
                                                </li>
                                            ))}
                                        </ul>
                                    </div>
                                ))}
                            </div>
                        ))
                    )}
                    {unknown.length > 0 && (
                        <div>
                            <div className="mono-label text-(--base-06) mb-1.5">Other</div>
                            <ul className="space-y-1">
                                {unknown.map(id => (
                                    <li key={id} className="text-sm font-mono text-(--base-07)">{id}</li>
                                ))}
                            </ul>
                        </div>
                    )}
                </div>
                <div className="modal-footer">
                    <button type="button" onClick={onClose} className="btn btn-primary">Close</button>
                </div>
            </div>
        </div>
    );
}

// ---------------------------------------------
// Create/Edit panel-role modal
// ---------------------------------------------
function RoleModal({
    role,
    catalog,
    onClose,
    onSaved,
    onError,
}: {
    role: PanelRole | null;
    catalog: CatalogScope[];
    onClose: () => void;
    onSaved: () => void;
    onError: (msg: string) => void;
}) {
    const [name, setName] = useState(role?.name ?? '');
    const [caps, setCaps] = useState<string[]>(role?.capabilities ?? []);
    const [saving, setSaving] = useState(false);

    const handleSave = async () => {
        const trimmed = name.trim();
        if (!trimmed) { onError('Role name is required'); return; }
        setSaving(true);
        const res = role
            ? await updatePanelRole(role.id, trimmed, caps)
            : await createPanelRole(trimmed, caps);
        setSaving(false);
        if (res.success) {
            onSaved();
        } else {
            onError(res.message || 'Failed to save panel role');
        }
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <div className="modal-panel w-full max-w-lg max-h-[85vh] flex flex-col" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <h3 className="modal-title">{role ? 'Edit Panel Role' : 'New Panel Role'}</h3>
                    <button onClick={onClose} className="p-1 rounded hover:bg-(--base-03) text-(--base-06)">
                        <X size={16} />
                    </button>
                </div>
                <div className="modal-body overflow-y-auto flex-1 space-y-4">
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">Name</label>
                        <input
                            type="text"
                            value={name}
                            onChange={e => setName(e.target.value)}
                            maxLength={64}
                            className="input-field w-full"
                            disabled={saving}
                        />
                    </div>
                    <div>
                        <label className="input-label">Capabilities</label>
                        <div className="mt-2">
                            <CapabilityPicker catalog={catalog} scopes={['panel']} selected={caps} onChange={setCaps} disabled={saving} />
                        </div>
                    </div>
                </div>
                <div className="modal-footer">
                    <button type="button" onClick={onClose} className="btn btn-secondary" disabled={saving}>Cancel</button>
                    <button type="button" onClick={handleSave} className="btn btn-primary disabled:opacity-40" disabled={saving}>
                        {saving ? 'Saving...' : 'Save'}
                    </button>
                </div>
            </div>
        </div>
    );
}

// ---------------------------------------------
// Assign panel role + grant/deny overrides to a user
// ---------------------------------------------
function AssignRoleModal({
    user,
    roles,
    catalog,
    onClose,
    onSaved,
    onError,
}: {
    user: User;
    roles: PanelRole[];
    catalog: CatalogScope[];
    onClose: () => void;
    onSaved: () => void;
    onError: (msg: string) => void;
}) {
    const [loading, setLoading] = useState(true);
    const [panelRoleId, setPanelRoleId] = useState<number | null>(null);
    const [grantCaps, setGrantCaps] = useState<string[]>([]);
    const [denyCaps, setDenyCaps] = useState<string[]>([]);
    const [saving, setSaving] = useState(false);
    // The GET exists only to pre-fill this editor, and the PUT is an
    // unconditional full replace of all three fields. So a failed pre-fill used
    // to render "role: None, no grants, no denies" - indistinguishable from a
    // user who genuinely has none - with Save enabled and no error anywhere.
    // One click then wrote that: the role and grants are LOST, and dropping the
    // deny overrides silently GRANTS back everything they were removing. A read
    // that failed must not be able to change anyone's privileges.
    const [loadFailed, setLoadFailed] = useState(false);

    useEffect(() => {
        getUserPanelRole(user.id).then(res => {
            if (res.success) {
                setPanelRoleId(res.panelRoleId ?? null);
                setGrantCaps(res.grantCaps ?? []);
                setDenyCaps(res.denyCaps ?? []);
                setLoadFailed(false);
            } else {
                setLoadFailed(true);
            }
            setLoading(false);
        });
    }, [user.id]);

    const handleSave = async () => {
        setSaving(true);
        const res = await assignUserPanelRole(user.id, panelRoleId, grantCaps, denyCaps);
        setSaving(false);
        if (res.success) {
            onSaved();
        } else {
            onError(res.message || 'Failed to assign panel role');
        }
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <div className="modal-panel w-full max-w-lg max-h-[85vh] flex flex-col" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <div>
                        <h3 className="modal-title">Assign Panel Role</h3>
                        <p className="text-xs font-mono text-(--base-06)">{user.username}</p>
                    </div>
                    <button onClick={onClose} className="p-1 rounded hover:bg-(--base-03) text-(--base-06)">
                        <X size={16} />
                    </button>
                </div>
                {loading ? (
                    <div className="modal-body">
                        <SkeletonCard height="h-24" />
                    </div>
                ) : loadFailed ? (
                    <div className="modal-body">
                        <div className="alert alert-error text-xs" role="alert">
                            This user&apos;s current panel role and capability overrides could not be loaded.
                            The editor stays closed rather than showing an empty assignment, because saving
                            it would clear whatever they actually have. Close this and try again.
                        </div>
                    </div>
                ) : (
                    <div className="modal-body overflow-y-auto flex-1 space-y-5">
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">Panel role</label>
                            <select
                                value={panelRoleId ?? ''}
                                onChange={e => setPanelRoleId(e.target.value === '' ? null : Number(e.target.value))}
                                className="input-field w-full"
                                disabled={saving}
                            >
                                <option value="">None</option>
                                {roles.map(r => (
                                    <option key={r.id} value={r.id}>{r.name}</option>
                                ))}
                            </select>
                        </div>
                        <div>
                            <label className="input-label">Grant overrides</label>
                            <p className="text-xs text-(--base-06) mb-2">Additional capabilities on top of the role, or on top of nothing if no role is assigned.</p>
                            <CapabilityPicker catalog={catalog} scopes={['panel']} selected={grantCaps} onChange={setGrantCaps} disabled={saving} />
                        </div>
                        <div>
                            <label className="input-label">Deny overrides</label>
                            <p className="text-xs text-(--base-06) mb-2">Capabilities removed even if the role or a grant above would include them.</p>
                            <CapabilityPicker catalog={catalog} scopes={['panel']} selected={denyCaps} onChange={setDenyCaps} disabled={saving} />
                        </div>
                    </div>
                )}
                <div className="modal-footer">
                    <button type="button" onClick={onClose} className="btn btn-secondary" disabled={saving}>Cancel</button>
                    <button type="button" onClick={handleSave} className="btn btn-primary disabled:opacity-40 disabled:cursor-not-allowed" disabled={saving || loading || loadFailed}>
                        {saving ? 'Saving...' : 'Save'}
                    </button>
                </div>
            </div>
        </div>
    );
}
