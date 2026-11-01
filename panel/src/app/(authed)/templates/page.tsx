"use client";

import React, { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { AlertTriangle, CircleAlert, LayoutTemplate, Plus, RefreshCw } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { listTemplates, type SkippedFile, type Template } from '@/lib/api/templates';
import { formatTemplateSize, groupTemplates, templateCap } from '@/lib/templates';
import { SkeletonCard } from '@/components/Skeleton';
import { Badge } from '@/components/ui/Badge';
import NewTemplateDialog from '@/components/templates/NewTemplateDialog';
import SkippedAlert from '@/components/templates/SkippedAlert';
import { OwnerShares } from '@/components/templates/TemplateShares';
import { toast } from '@/components/ui/Toast';
import { listOwnBackupStorages, storageLabel, type OwnBackupStorage } from '@/lib/api/ownBackupStorage';

export default function TemplatesPage() {
    const { user, featureFlags } = useAppData();
    const router = useRouter();
    const [list, setList] = useState<Template[] | null>(null);
    const [maxSize, setMaxSize] = useState<number | null>(null);
    const [maxPerUser, setMaxPerUser] = useState<number | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [loading, setLoading] = useState(true);
    const [creating, setCreating] = useState(false);
    const [created, setCreated] = useState<{ template: Template; skipped: SkippedFile[] } | null>(null);
    const [own, setOwn] = useState<OwnBackupStorage[]>([]);

    const refresh = useCallback(async () => {
        // Own targets only name the storage badges; without the capability the
        // list is empty and own-storage templates read as the owner's storage.
        const [res, ownRes] = await Promise.all([listTemplates(), listOwnBackupStorages()]);
        setLoading(false);
        setOwn(ownRes.success ? ownRes.storages ?? [] : []);
        if (!res.success) { setError(res.message || 'Could not load templates.'); return; }
        setError(null);
        setList(res.templates ?? []);
        setMaxSize(res.maxSizeBytes ?? null);
        setMaxPerUser(res.maxPerUser ?? null);
    }, []);

    useEffect(() => { refresh(); }, [refresh]);
    const reload = () => { setLoading(true); refresh(); };

    const onCreated = (template: Template, skipped: SkippedFile[]) => {
        setCreating(false);
        toast('Template created.');
        // Skipped files are worth reading before moving on, so the list page
        // keeps the alert and links to the template instead of navigating away.
        if (skipped.length === 0) { router.push(`/templates/${template.id}`); return; }
        setCreated({ template, skipped });
        reload();
    };

    const groups = list ? groupTemplates(list, user?.id ?? '') : null;
    const suspended = groups?.mine.find(t => t.writeRefusal)?.writeRefusal;
    const sections: { key: keyof NonNullable<typeof groups>; title: string; empty: string }[] = [
        { key: 'mine', title: 'My templates', empty: 'You have no templates yet. Create one from a zip, from a server, or start empty.' },
        { key: 'shared', title: 'Shared with me', empty: '' },
        { key: 'admin', title: 'Admin', empty: '' },
        { key: 'platform', title: 'Platform', empty: '' },
    ];

    return (
        <main className="flex-1 min-h-0 overflow-y-auto p-5">
            <header className="flex items-center gap-3 mb-4">
                <LayoutTemplate size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">Templates</h1>
                <div className="ml-auto flex items-center gap-2">
                    <button onClick={reload} className="btn btn-secondary btn-sm" disabled={loading} aria-label="Refresh">
                        <RefreshCw size={12} className={loading ? 'animate-spin' : ''} />
                    </button>
                    <button onClick={() => setCreating(true)} className="btn btn-primary btn-sm">
                        <Plus size={13} /> New template
                    </button>
                </div>
            </header>

            <p className="text-xs text-(--base-07) mb-4">
                A template is a server folder you can start new servers from: configs, plugins and worlds included.
            </p>

            {created && (
                <SkippedAlert skipped={created.skipped} onDismiss={() => setCreated(null)}>
                    <Link href={`/templates/${created.template.id}`} className="inline-block mt-2 text-(--accent-light)">
                        Open {created.template.name}
                    </Link>
                </SkippedAlert>
            )}

            {suspended && (
                <div className="alert alert-warning mb-4" role="status">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <div className="text-xs">
                        <p className="font-medium">Your templates are read-only</p>
                        <p>{suspended} You can still open, download and delete them.</p>
                    </div>
                </div>
            )}

            {error && (
                <div className="card p-3 border border-(--error) bg-(--error)/10 mb-4 flex items-start gap-2" role="alert">
                    <CircleAlert size={16} className="text-(--error-light) mt-0.5 shrink-0" />
                    <div className="text-xs text-(--base-09) flex-1">{error}</div>
                    <button onClick={reload} className="btn btn-secondary btn-sm">Retry</button>
                </div>
            )}

            {!groups ? (
                !error && (
                    <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3" aria-busy="true">
                        {Array.from({ length: 6 }).map((_, i) => <SkeletonCard key={i} height="h-28" />)}
                    </div>
                )
            ) : (
                <div className="space-y-6">
                    {sections.map(({ key, title, empty }) => {
                        const items = groups[key];
                        if (items.length === 0 && !empty) return null;
                        return (
                            <section key={key} aria-labelledby={`tpl-group-${key}`}>
                                <h2 id={`tpl-group-${key}`} className="mono-label text-(--base-06) mb-2">
                                    {title}
                                    {key === 'mine' && maxPerUser !== null && !user?.isAdmin && (
                                        <span className="ml-2 normal-case">{items.length} of {maxPerUser}</span>
                                    )}
                                </h2>
                                {items.length === 0 ? (
                                    <div className="card p-8 flex flex-col items-center text-center gap-2">
                                        <LayoutTemplate size={28} className="text-(--base-05)" />
                                        <p className="text-sm text-(--base-07)">{empty}</p>
                                        <button onClick={() => setCreating(true)} className="btn btn-primary btn-sm mt-2"><Plus size={13} /> New template</button>
                                    </div>
                                ) : (
                                    <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
                                        {items.map((t) => <TemplateCard key={t.id} t={t} maxSize={maxSize} showOwner={key === 'shared'} storage={storageLabel(t.backupStorageId, own, !!featureFlags.hosted)} />)}
                                    </div>
                                )}
                            </section>
                        );
                    })}
                    <OwnerShares />
                </div>
            )}

            {creating && <NewTemplateDialog onClose={() => setCreating(false)} onCreated={onCreated} />}
        </main>
    );
}

function TemplateCard({ t, maxSize, showOwner, storage }: { t: Template; maxSize: number | null; showOwner: boolean; storage: string }) {
    const version = [t.software, t.mcVersion, t.softwareVersion].filter(Boolean).join(' ');
    return (
        <Link href={`/templates/${t.id}`} className="card p-4 hover:border-(--accent-border) focus-visible:border-(--accent-border) transition-colors group">
            <div className="text-sm font-medium text-(--base-09) truncate group-hover:text-(--accent-light)">{t.name}</div>
            <div className="text-xs text-(--base-06) truncate mt-0.5">{version || 'No software set'}</div>
            <div className="mt-3 flex items-center gap-2 flex-wrap">
                <Badge variant="neutral">{t.kind === 'proxy' ? 'proxy' : 'server'}</Badge>
                {showOwner && t.shareLevel && <Badge variant="accent">{t.shareLevel === 'edit' ? 'can edit' : 'can use'}</Badge>}
                <Badge variant="neutral">{storage}</Badge>
                <span className="text-[11px] text-(--base-06) ml-auto">{formatTemplateSize(t.sizeBytes, templateCap(t, maxSize))}</span>
            </div>
            {showOwner && <div className="text-[11px] text-(--base-06) mt-2 truncate">by {t.ownerName || 'unknown'}</div>}
        </Link>
    );
}
