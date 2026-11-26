"use client";

import { useState } from 'react';
import { Plus, Check } from 'lucide-react';
import Select, { type SelectOption } from '@/components/ui/Select';
import Checkbox from '@/components/ui/Checkbox';
import type { CatalogScope, Preset } from '@/lib/api/authzCatalog';
import type { Server } from '@/lib/api';
import { assignGrant, type Grant } from '@/lib/api/grants';
import type { ServerRole } from '@/lib/api/serverRoles';
import { fullAccessCaps, isProxyScope } from '@/lib/access/accessMode';
import CapabilityPicker from '@/components/access/CapabilityPicker';
import { useBusy } from '@/lib/useBusy';

// Invite/edit form for a per-friend delegation grant, shared by both
// permissions_mode variants: 'simple' (Full-only, one preset, no picking)
// and 'advanced' (Admin-roles, server role + granular grant/deny overrides).
// Subsumes the old AccessAdvancedGrants (advanced-only) - the parent Access
// page (task 6) renders this once for both modes and also for editing an
// existing grant (editing != null), where the username is locked since
// identity is not editable: revoke + re-invite to change it.

interface AccessGrantFormProps {
    mode: 'simple' | 'advanced';
    ownedServers: Server[];
    catalog: CatalogScope[];
    roles: ServerRole[];
    presets: Preset[];
    editing: Grant | null;
    showToast: (msg: string, ok?: boolean) => void;
    onSaved: () => void;
    onCancel: () => void;
}

// State is seeded once from `editing` via the useState initializers below.
// A caller that reuses one instance across edit targets (edit A, then edit B,
// then a new invite) MUST pass a `key` that changes per target
// (e.g. key={editing ? `edit-${editing.username}-${editing.serverId ?? 'acct'}` : 'new'})
// so React remounts the form and the fields refresh; otherwise it keeps
// showing the previous target's values.
export default function AccessGrantForm({ mode, ownedServers, catalog, roles, presets, editing, showToast, onSaved, onCancel }: AccessGrantFormProps) {
    const [username, setUsername] = useState(editing?.username ?? '');
    const [submittingGrant, runSubmit] = useBusy();
    const [serverId, setServerId] = useState(editing?.serverId != null ? String(editing.serverId) : '');
    const [serverRoleId, setServerRoleId] = useState(editing?.serverRoleId != null ? String(editing.serverRoleId) : '');
    const [grantCaps, setGrantCaps] = useState<string[]>(editing?.grantCaps ?? []);
    const [denyCaps, setDenyCaps] = useState<string[]>(editing?.denyCaps ?? []);
    const [inherit, setInherit] = useState(editing?.inherit ?? false);

    const showInherit = isProxyScope(ownedServers, serverId ? Number(serverId) : null);

    const scopeOptions: SelectOption[] = [
        { value: '', label: 'Account-wide (all servers)' },
        ...ownedServers.map(s => ({ value: String(s.id), label: s.name, badge: s.serverType === 'proxy' ? 'proxy' : undefined })),
    ];

    const roleOptions: SelectOption[] = [
        { value: '', label: 'None - custom' },
        ...roles.map(r => ({ value: String(r.id), label: r.name })),
    ];

    const handleScopeChange = (value: string) => {
        setServerId(value);
        // The inherit flag only means something for a proxy scope; reset it
        // whenever the scope moves off a proxy so a stale true never ships.
        if (!isProxyScope(ownedServers, value ? Number(value) : null)) {
            setInherit(false);
        }
    };

    const handleSubmit = async () => {
        const trimmedUsername = username.trim();
        if (!trimmedUsername) { showToast('Username required', false); return; }

        let submitGrantCaps: string[];
        let submitDenyCaps: string[];
        let submitServerRoleId: number | null;

        if (mode === 'simple') {
            const fullCaps = fullAccessCaps(presets);
            if (fullCaps.length === 0) {
                showToast('Presets not loaded yet, try again', false);
                return;
            }
            submitGrantCaps = fullCaps;
            submitDenyCaps = [];
            submitServerRoleId = null;
        } else {
            submitGrantCaps = grantCaps;
            submitDenyCaps = denyCaps;
            submitServerRoleId = serverRoleId ? Number(serverRoleId) : null;
        }

        const res = await assignGrant({
            username: trimmedUsername,
            serverId: serverId ? Number(serverId) : null,
            serverRoleId: submitServerRoleId,
            grantCaps: submitGrantCaps,
            denyCaps: submitDenyCaps,
            inherit: showInherit ? inherit : false,
        });

        if (res.success) {
            showToast(editing ? 'Grant updated.' : 'Grant created.', true);
            onSaved();
        } else {
            showToast(res.message || 'Assign failed', false);
        }
    };

    return (
        <div className="srv-surface max-w-xl p-6">
            <h2 className="mb-4 text-[15px] font-semibold text-(--base-09)">{editing ? 'Edit grant' : 'New grant'}</h2>
            <div className="space-y-4">
                <div>
                    <label className="input-label">Friend&apos;s username</label>
                    <input
                        type="text"
                        value={username}
                        onChange={e => setUsername(e.target.value)}
                        className="input-field w-full"
                        placeholder="friend-username"
                        maxLength={64}
                        disabled={!!editing}
                    />
                </div>

                <div>
                    <label className="input-label">Scope</label>
                    <Select
                        value={serverId}
                        onChange={handleScopeChange}
                        options={scopeOptions}
                        ariaLabel="Scope"
                        disabled={!!editing}
                    />
                </div>

                {showInherit && (
                    <Checkbox
                        checked={inherit}
                        onChange={setInherit}
                        label="Also apply to the servers behind this proxy"
                    />
                )}

                {mode === 'simple' ? (
                    <div>
                        <label className="input-label">Access</label>
                        <p className="text-sm text-(--base-09)">Full access</p>
                    </div>
                ) : (
                    <>
                        <div>
                            <label className="input-label">Server role</label>
                            <Select
                                value={serverRoleId}
                                onChange={setServerRoleId}
                                options={roleOptions}
                                ariaLabel="Server role"
                            />
                        </div>

                        <div>
                            <label className="input-label">Grant (add on top of the role)</label>
                            <div className="mt-1">
                                <CapabilityPicker
                                    catalog={catalog}
                                    scopes={['server', 'owner']}
                                    selected={grantCaps}
                                    onChange={setGrantCaps}
                                />
                            </div>
                        </div>

                        <div>
                            <label className="input-label">Deny (remove from the role)</label>
                            <div className="mt-1">
                                <CapabilityPicker
                                    catalog={catalog}
                                    scopes={['server', 'owner']}
                                    selected={denyCaps}
                                    onChange={setDenyCaps}
                                />
                            </div>
                        </div>
                    </>
                )}

                <div className="flex items-center gap-2">
                    <button onClick={() => runSubmit(handleSubmit)} disabled={submittingGrant} className="btn btn-primary btn-sm disabled:opacity-40">
                        {editing ? <Check size={12} /> : <Plus size={12} />}
                        {editing ? 'Save' : 'Add grant'}
                    </button>
                    <button onClick={onCancel} className="btn btn-secondary btn-sm">
                        Cancel
                    </button>
                </div>
            </div>
        </div>
    );
}
