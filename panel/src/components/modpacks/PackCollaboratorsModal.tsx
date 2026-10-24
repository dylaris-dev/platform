"use client";

import { useCallback, useEffect, useState } from 'react';
import { Trash2, UserPlus, Users, X } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import Checkbox from '@/components/ui/Checkbox';
import { toast } from '@/components/ui/Toast';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { useBusy } from '@/lib/useBusy';
import {
    invitePackCollaborator, listPackCollaborators, removePackCollaborator, updatePackCollaborator,
    type Pack, type PackCollaborator,
} from '@/lib/api/packs';

function rightsText(c: { canCreate: boolean; canEdit: boolean; canDelete: boolean }): string {
    const r = [c.canCreate && 'create', c.canEdit && 'edit & publish', c.canDelete && 'delete'].filter(Boolean);
    return r.length ? r.join(', ') : 'view only';
}

/**
 * Who may work on the owner's packs. An invite covers every pack (including
 * future ones) or the packs ticked; rights are per invite. Collaborators
 * publish through the owner's Modrinth token and never see it. The lower part
 * lists the invites the reader HOLDS, with "Leave".
 */
export default function PackCollaboratorsModal({ packs, onClose }: { packs: Pack[]; onClose: () => void }) {
    const [given, setGiven] = useState<PackCollaborator[] | null>(null);
    const [held, setHeld] = useState<PackCollaborator[]>([]);
    const [username, setUsername] = useState('');
    const [allPacks, setAllPacks] = useState(true);
    const [chosen, setChosen] = useState<Set<number>>(new Set());
    const [rights, setRights] = useState({ canCreate: false, canEdit: true, canDelete: false });
    const [inviting, runInvite] = useBusy();

    const load = useCallback(async () => {
        const r = await listPackCollaborators();
        setGiven(r.given);
        setHeld(r.held);
    }, []);
    useEffect(() => { load(); }, [load]);

    const invite = () => runInvite(async () => {
        if (!username.trim()) { toast('Enter a username', false); return; }
        if (!allPacks && chosen.size === 0) { toast('Pick at least one pack', false); return; }
        const res = await invitePackCollaborator({
            username: username.trim(), packIds: allPacks ? [] : [...chosen],
            canCreate: allPacks && rights.canCreate, canEdit: rights.canEdit, canDelete: rights.canDelete,
        });
        if (res.success) {
            toast(`${username.trim()} can now work on ${allPacks ? 'all your packs' : `${chosen.size} pack${chosen.size === 1 ? '' : 's'}`}.`, true);
            setUsername(''); setChosen(new Set());
            load();
        } else {
            toast(res.message || 'Could not invite', false);
        }
    });

    const toggleRight = async (c: PackCollaborator, key: 'canCreate' | 'canEdit' | 'canDelete') => {
        const next = { canCreate: c.canCreate, canEdit: c.canEdit, canDelete: c.canDelete, [key]: !c[key] };
        const res = await updatePackCollaborator(c.id, next);
        if (res.success) load(); else toast(res.message || 'Could not change the rights', false);
    };

    const remove = async (c: PackCollaborator, leaving: boolean) => {
        if (!await confirmDialog({
            title: leaving ? `Leave ${c.ownerName}'s packs?` : `Remove ${c.username}?`,
            message: leaving
                ? 'You lose access to these packs. The owner can invite you again.'
                : `${c.username} can no longer see or change ${c.packId ? c.packName : 'your packs'}. What they already changed stays.`,
            confirmLabel: leaving ? 'Leave' : 'Remove',
            destructive: true,
        })) return;
        const res = await removePackCollaborator(c.id);
        if (res.success) load(); else toast(res.message || 'Could not remove', false);
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel w-full max-w-2xl" onClick={e => e.stopPropagation()}>
                <div className="modal-header">
                    <h3 className="modal-title flex items-center gap-2"><Users size={16} /> Collaborators</h3>
                    <button onClick={onClose} className="text-(--base-06) hover:text-(--base-09)" aria-label="Close"><X size={16} /></button>
                </div>
                <div className="modal-body space-y-5">
                    <section className="space-y-3">
                        <p className="text-xs text-(--base-06)">
                            Collaborators work on your packs and publish through your Modrinth token; they never see the
                            token. Every change they make is recorded in the pack&apos;s activity.
                        </p>
                        <div>
                            <label className="input-label" htmlFor="collab-user">Username</label>
                            <input id="collab-user" value={username} onChange={e => setUsername(e.target.value)} className="input-field w-full" placeholder="Their panel username" autoComplete="off" />
                        </div>
                        <fieldset className="space-y-2">
                            <legend className="input-label">Packs</legend>
                            <Checkbox checked={allPacks} onChange={setAllPacks} label="All my packs, including ones created later" />
                            {!allPacks && (
                                <div className="grid grid-cols-1 sm:grid-cols-2 gap-1.5 pl-1">
                                    {packs.length === 0 && <p className="text-xs text-(--base-06)">You have no packs yet.</p>}
                                    {packs.map(p => (
                                        <Checkbox
                                            key={p.id}
                                            checked={chosen.has(p.id)}
                                            onChange={on => setChosen(prev => { const n = new Set(prev); if (on) n.add(p.id); else n.delete(p.id); return n; })}
                                            label={p.internalName}
                                        />
                                    ))}
                                </div>
                            )}
                        </fieldset>
                        <fieldset className="space-y-2">
                            <legend className="input-label">Rights</legend>
                            <Checkbox checked={rights.canEdit} onChange={v => setRights({ ...rights, canEdit: v })} label="Edit and publish" hint="Content, builds, settings, new versions on Modrinth." />
                            <Checkbox checked={rights.canDelete} onChange={v => setRights({ ...rights, canDelete: v })} label="Delete" hint="Packs, builds and content." />
                            <Checkbox checked={rights.canCreate && allPacks} disabled={!allPacks} onChange={v => setRights({ ...rights, canCreate: v })} label="Create new packs on my account" hint={allPacks ? undefined : 'Only with "All my packs".'} />
                        </fieldset>
                        <button onClick={invite} disabled={inviting} className="btn btn-primary btn-sm"><UserPlus size={13} /> Invite</button>
                    </section>

                    <section>
                        <h4 className="mono-label text-(--base-06) mb-2">Invited by you</h4>
                        {given === null ? null : given.length === 0 ? (
                            <p className="text-xs text-(--base-06)">Nobody yet.</p>
                        ) : (
                            <ul className="divide-y divide-(--base-03)">
                                {given.map(c => (
                                    <li key={c.id} className="py-2 flex items-center gap-3 flex-wrap">
                                        <span className="text-sm text-(--base-09) font-medium">{c.username}</span>
                                        <span className="text-xs text-(--base-06)">{c.packId ? c.packName : 'all packs'}</span>
                                        <span className="ml-auto flex items-center gap-3">
                                            {!c.packId && <Checkbox checked={c.canCreate} onChange={() => toggleRight(c, 'canCreate')} label="Create" />}
                                            <Checkbox checked={c.canEdit} onChange={() => toggleRight(c, 'canEdit')} label="Edit" />
                                            <Checkbox checked={c.canDelete} onChange={() => toggleRight(c, 'canDelete')} label="Delete" />
                                            <button onClick={() => remove(c, false)} className="btn btn-secondary btn-sm" aria-label={`Remove ${c.username}`}>
                                                <Trash2 size={12} className="text-(--error-light)" />
                                            </button>
                                        </span>
                                    </li>
                                ))}
                            </ul>
                        )}
                    </section>

                    {held.length > 0 && (
                        <section>
                            <h4 className="mono-label text-(--base-06) mb-2">Shared with you</h4>
                            <ul className="divide-y divide-(--base-03)">
                                {held.map(c => (
                                    <li key={c.id} className="py-2 flex items-center gap-3 flex-wrap">
                                        <span className="text-sm text-(--base-09) font-medium">{c.ownerName}</span>
                                        <span className="text-xs text-(--base-06)">{c.packId ? c.packName : 'all packs'} · {rightsText(c)}</span>
                                        <button onClick={() => remove(c, true)} className="btn btn-secondary btn-sm ml-auto">Leave</button>
                                    </li>
                                ))}
                            </ul>
                        </section>
                    )}
                </div>
                <div className="modal-footer">
                    <button onClick={onClose} className="btn btn-secondary">Close</button>
                </div>
            </ModalPanel>
        </div>
    );
}
