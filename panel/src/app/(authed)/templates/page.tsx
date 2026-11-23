"use client";

import React, { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { AlertTriangle, CircleAlert, LayoutTemplate, Plus, RefreshCw } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { listTemplates, type SkippedFile, type Template } from '@/lib/api/templates';
import { formatTemplateSize, groupTemplates, templateCap } from '@/lib/templates';
import { SkeletonCard } from '@/components/Skeleton';
import PageHeader from '@/components/ui/PageHeader';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
import Callout from '@/components/server/Callout';
import '@/styles/modpacks.css';
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

    const onCreated = (template: Template, skipped: SkippedFile[], warning?: string) => {
        setCreating(false);
        toast('Template created.');
        if (warning) toast(warning, false);
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
        <main className="flex-1 min-h-0 overflow-y-auto app-page space-y-6">
            <PageHeader
                title="Templates"
                description="A template is a server folder you can start new servers from: configs, plugins and worlds included."
                actions={<>
                    <button onClick={reload} className="btn btn-secondary btn-sm" disabled={loading} aria-label="Refresh">
                        <RefreshCw size={13} className={loading ? 'animate-spin' : ''} />
                    </button>
                    <button onClick={() => setCreating(true)} className="btn btn-primary btn-sm">
                        <Plus size={13} /> New template
                    </button>
                </>}
            />

            {created && (
                <SkippedAlert skipped={created.skipped} onDismiss={() => setCreated(null)}>
                    <Link href={`/templates/${created.template.id}`} className="inline-block mt-2 text-(--accent-light)">
                        Open {created.template.name}
                    </Link>
                </SkippedAlert>
            )}

            {suspended && (
                <Callout tone="warning" role="status" icon={<AlertTriangle size={14} />} title="Your templates are read-only">
                    {suspended} You can still open, download and delete them.
                </Callout>
            )}

            {error && (
                <EmptyState tone="error" icon={<CircleAlert size={20} />} title={error} action={<button onClick={reload} className="btn btn-secondary btn-sm">Retry</button>} />
            )}

            {!groups ? (
                !error && (
                    <div className="mp-grid" aria-busy="true">
                        {Array.from({ length: 6 }).map((_, i) => <SkeletonCard key={i} height="h-28" />)}
                    </div>
                )
            ) : (
                <div className="space-y-8">
                    {sections.map(({ key, title, empty }) => {
                        const items = groups[key];
                        if (items.length === 0 && !empty) return null;
                        return (
                            <Section
                                key={key}
                                title={title}
                                description={key === 'mine' && maxPerUser !== null && !user?.isAdmin ? `${items.length} of ${maxPerUser}` : undefined}
                            >
                                {items.length === 0 ? (
                                    <EmptyState
                                        icon={<LayoutTemplate size={20} />}
                                        title={empty}
                                        action={<button onClick={() => setCreating(true)} className="btn btn-primary btn-sm"><Plus size={13} /> New template</button>}
                                    />
                                ) : (
                                    <div className="mp-grid">
                                        {items.map((t) => <TemplateCard key={t.id} t={t} maxSize={maxSize} showOwner={key === 'shared'} storage={storageLabel(t.backupStorageId, own, !!featureFlags.hosted)} />)}
                                    </div>
                                )}
                            </Section>
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
        <Link href={`/templates/${t.id}`} className="mp-card p-4 focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">
            <div className="text-sm font-medium text-(--base-09) truncate">{t.name}</div>
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
