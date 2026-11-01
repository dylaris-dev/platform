"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { Trash2, UserPlus, Users } from 'lucide-react';
import {
    deleteOwnerShare, deleteTemplateShare, listOwnerShares, listTemplateShares, putOwnerShare, putTemplateShare,
    type ShareLevel, type TemplateShare,
} from '@/lib/api/templates';
import { SkeletonList } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';
import { useBusy } from '@/lib/useBusy';

type Res = { success: boolean; message?: string };

const LEVELS: { value: ShareLevel; label: string }[] = [
    { value: 'use', label: 'Use: install it on their servers' },
    { value: 'edit', label: 'Edit: also change its files' },
];

/** Shares of one template. Rendered only for a caller with canShare. */
export function TemplateShares({ templateId }: { templateId: number }) {
    return (
        <ShareManager
            idPrefix={`tpl-${templateId}`}
            title="Shared with"
            description="People you share this template with see it under Shared with me."
            load={() => listTemplateShares(templateId)}
            put={(u, l) => putTemplateShare(templateId, u, l)}
            remove={(id) => deleteTemplateShare(templateId, id)}
        />
    );
}

/** Owner-wide shares: every personal template of the caller, present and future. */
export function OwnerShares() {
    return (
        <ShareManager
            idPrefix="owner"
            title="Share all my templates"
            description="Gives someone access to every personal template you own, including ones you create later."
            load={listOwnerShares}
            put={putOwnerShare}
            remove={deleteOwnerShare}
        />
    );
}

function ShareManager({
    idPrefix, title, description, load, put, remove,
}: {
    idPrefix: string;
    title: string;
    description: string;
    load: () => Promise<Res & { shares?: TemplateShare[] }>;
    put: (username: string, level: ShareLevel) => Promise<Res>;
    remove: (userId: string) => Promise<Res>;
}) {
    const [shares, setShares] = useState<TemplateShare[] | null>(null);
    const [loadError, setLoadError] = useState<string | null>(null);
    const [username, setUsername] = useState('');
    const [level, setLevel] = useState<ShareLevel>('use');
    const [formError, setFormError] = useState<string | null>(null);
    const [adding, runAdd] = useBusy();
    const [removing, runRemove] = useBusy();

    const refresh = useCallback(async () => {
        const res = await load();
        if (res.success) {
            setShares(res.shares ?? []);
            setLoadError(null);
        } else {
            setLoadError(res.message || 'Could not load the shares.');
        }
        // load is a fresh closure each render; idPrefix names what it loads.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [idPrefix]);

    useEffect(() => { refresh(); }, [refresh]);

    const add = async (e: React.FormEvent) => {
        e.preventDefault();
        if (!username.trim()) { setFormError('Enter a username.'); return; }
        const res = await put(username.trim(), level);
        if (!res.success) { setFormError(res.message || 'Could not share.'); return; }
        setFormError(null);
        setUsername('');
        toast('Shared.');
        refresh();
    };

    const drop = async (s: TemplateShare) => {
        const res = await remove(s.userId);
        if (!res.success) { toast(res.message || 'Could not remove the share.', false); return; }
        toast(`${s.username} no longer has access.`);
        refresh();
    };

    return (
        <section className="card p-4 space-y-3" aria-labelledby={`${idPrefix}-shares-title`}>
            <div>
                <h2 id={`${idPrefix}-shares-title`} className="text-sm font-medium text-(--base-09) flex items-center gap-2">
                    <Users size={14} className="text-(--accent-light)" /> {title}
                </h2>
                <p className="text-xs text-(--base-06) mt-1">{description}</p>
            </div>

            <form onSubmit={(e) => runAdd(() => add(e))} className="flex flex-wrap items-end gap-2">
                <div className="flex-1 min-w-40">
                    <label htmlFor={`${idPrefix}-share-user`} className="input-label">Username</label>
                    <input
                        id={`${idPrefix}-share-user`}
                        className="input-field w-full"
                        value={username}
                        onChange={(e) => setUsername(e.target.value)}
                        autoComplete="off"
                        aria-invalid={!!formError}
                        aria-describedby={formError ? `${idPrefix}-share-error` : undefined}
                    />
                </div>
                <div>
                    <label htmlFor={`${idPrefix}-share-level`} className="input-label">Access</label>
                    <select
                        id={`${idPrefix}-share-level`}
                        className="input-field"
                        value={level}
                        onChange={(e) => setLevel(e.target.value as ShareLevel)}
                    >
                        {LEVELS.map((l) => <option key={l.value} value={l.value}>{l.label}</option>)}
                    </select>
                </div>
                <button type="submit" className="btn btn-primary btn-sm disabled:opacity-40" disabled={adding}>
                    <UserPlus size={13} /> Share
                </button>
            </form>
            {formError && <p id={`${idPrefix}-share-error`} className="text-xs text-(--error-light)" role="alert">{formError}</p>}

            {loadError ? (
                <p className="text-xs text-(--error-light)" role="alert">{loadError}</p>
            ) : shares === null ? (
                <SkeletonList rows={2} />
            ) : shares.length === 0 ? (
                <p className="text-xs text-(--base-06)">Not shared with anyone yet.</p>
            ) : (
                <ul className="divide-y divide-(--base-03)">
                    {shares.map((s) => (
                        <li key={s.userId} className="flex items-center gap-3 py-2">
                            <span className="text-sm text-(--base-09) flex-1 truncate">{s.username}</span>
                            <span className="text-xs text-(--base-06)">{s.level === 'edit' ? 'Can edit' : 'Can use'}</span>
                            <button
                                type="button"
                                onClick={() => runRemove(() => drop(s))}
                                disabled={removing}
                                aria-label={`Remove access for ${s.username}`}
                                className="btn btn-secondary btn-sm disabled:opacity-40"
                            >
                                <Trash2 size={12} />
                            </button>
                        </li>
                    ))}
                </ul>
            )}
        </section>
    );
}
