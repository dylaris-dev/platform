"use client";

import React, { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { AlertTriangle, ArrowLeft, CircleAlert, Download, LayoutTemplate, Loader2, Trash2 } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { useRouteId } from '@/lib/routeParams';
import {
    deleteTemplate, getTemplate, listTemplates, templateDownloadUrl, updateTemplate,
    type SkippedFile, type Template, type TemplateVisibility,
} from '@/lib/api/templates';
import { formatTemplateSize, softwareVersionLabel, templateCap } from '@/lib/templates';
import { SkeletonHeader, SkeletonList } from '@/components/Skeleton';
import { Badge } from '@/components/ui/Badge';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { useBusy } from '@/lib/useBusy';
import TemplateFiles from '@/components/templates/TemplateFiles';
import { TemplateShares } from '@/components/templates/TemplateShares';
import SkippedAlert from '@/components/templates/SkippedAlert';
import { listOwnBackupStorages, storageLabel, type OwnBackupStorage } from '@/lib/api/ownBackupStorage';

type Draft = { name: string; visibility: TemplateVisibility; software: string; mcVersion: string; softwareVersion: string };
const draftOf = (t: Template): Draft => ({ name: t.name, visibility: t.visibility, software: t.software, mcVersion: t.mcVersion, softwareVersion: t.softwareVersion });

export default function TemplateDetailPage() {
    const id = Number(useRouteId('templates'));
    const { user, featureFlags } = useAppData();
    const router = useRouter();
    const [t, setT] = useState<Template | null>(null);
    const [maxSize, setMaxSize] = useState<number | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [draft, setDraft] = useState<Draft | null>(null);
    const [saveError, setSaveError] = useState<string | null>(null);
    const [skipped, setSkipped] = useState<SkippedFile[]>([]);
    const [saving, runSave] = useBusy();
    const [deleting, runDelete] = useBusy();
    const [own, setOwn] = useState<OwnBackupStorage[]>([]);

    useEffect(() => {
        if (!id) return;
        let live = true;
        Promise.all([getTemplate(id), listTemplates(), listOwnBackupStorages()]).then(([res, list, ownRes]) => {
            if (!live) return;
            if (ownRes.success) setOwn(ownRes.storages ?? []);
            if (!res.success) { setError(res.message || 'Could not load the template.'); return; }
            const tpl = res as Template;
            setT(tpl);
            setDraft(draftOf(tpl));
            // The limit rides on the list response; a failed list only hides it.
            if (list.success) setMaxSize(list.maxSizeBytes ?? null);
        });
        return () => { live = false; };
    }, [id]);

    const onChanged = useCallback((sizeBytes?: number) => {
        if (typeof sizeBytes === 'number') setT((prev) => (prev ? { ...prev, sizeBytes } : prev));
    }, []);
    const onSkipped = useCallback((s: SkippedFile[]) => setSkipped(s), []);

    if (error || !id) {
        return (
            <main className="flex-1 min-h-0 overflow-y-auto p-5">
                <BackLink />
                <div className="card p-8 flex flex-col items-center text-center gap-2 max-w-lg mx-auto mt-6" role="alert">
                    <CircleAlert size={28} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">{error || 'Template not found'}</p>
                </div>
            </main>
        );
    }
    if (!t || !draft) {
        return (
            <main className="flex-1 min-h-0 overflow-y-auto p-5 space-y-4" aria-busy="true">
                <SkeletonHeader />
                <SkeletonList rows={6} />
            </main>
        );
    }

    const isAdmin = !!user?.isAdmin;
    // Core lets an edit sharer change files but not delete the owner's template.
    const canDelete = t.canEdit && (isAdmin || t.ownerId === user?.id);
    // While the owner is suspended Core refuses every write, so the editors are
    // shown read-only instead of failing on save. Delete still works.
    const canWrite = t.canEdit && !t.writeRefusal;
    const dirty = (Object.keys(draft) as (keyof Draft)[]).some((k) => draft[k] !== t[k]);

    const save = async (e: React.FormEvent) => {
        e.preventDefault();
        const patch: Partial<Draft> = {};
        for (const k of Object.keys(draft) as (keyof Draft)[]) {
            if (draft[k] !== t[k]) (patch as Record<string, string>)[k] = draft[k];
        }
        const res = await updateTemplate(t.id, patch);
        if (!res.success) { setSaveError(res.message || 'Save failed.'); return; }
        setSaveError(null);
        const next = { ...t, ...(res as Template) };
        setT(next);
        setDraft(draftOf(next));
        toast('Template saved.');
    };

    const remove = async () => {
        if (!await confirmDialog({ title: `Delete ${t.name}?`, message: 'This removes the template and all its files. Servers made from it are not affected.' })) return;
        const res = await deleteTemplate(t.id);
        if (!res.success) { toast(res.message || 'Delete failed.', false); return; }
        toast(res.warning || 'Template deleted.');
        router.push('/templates');
    };

    const field = (k: keyof Draft, label: string) => (
        <div>
            <label htmlFor={`tpl-${k}`} className="input-label">{label}</label>
            <input
                id={`tpl-${k}`}
                className="input-field w-full"
                value={draft[k]}
                maxLength={64}
                readOnly={!canWrite}
                onChange={(e) => setDraft({ ...draft, [k]: e.target.value })}
            />
        </div>
    );

    return (
        <main className="flex-1 min-h-0 overflow-y-auto p-5 space-y-4">
            <BackLink />
            <header className="flex items-center gap-3 flex-wrap">
                <LayoutTemplate size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09) truncate">{t.name}</h1>
                <Badge variant="neutral">{t.kind}</Badge>
                <Badge variant="neutral">{t.visibility}</Badge>
                <Badge variant="neutral">{storageLabel(t.backupStorageId, own, !!featureFlags.hosted)}</Badge>
                {t.ownerId !== user?.id && t.visibility === 'personal' && <span className="text-xs text-(--base-06)">by {t.ownerName || 'unknown'}</span>}
                <span className="text-xs text-(--base-06) ml-auto">{formatTemplateSize(t.sizeBytes, templateCap(t, maxSize))}</span>
                <a href={templateDownloadUrl(t.id)} download className="btn btn-secondary btn-sm">
                    <Download size={12} /> Download
                </a>
            </header>

            {t.writeRefusal && (
                <div className="alert alert-warning" role="status">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <div className="text-xs">
                        <p className="font-medium">This template is read-only</p>
                        <p>{t.writeRefusal} You can still download{canDelete ? ' and delete' : ''} it.</p>
                    </div>
                </div>
            )}

            {skipped.length > 0 && <SkippedAlert skipped={skipped} onDismiss={() => setSkipped([])} />}

            <form onSubmit={(e) => runSave(() => save(e))} className="card p-4 space-y-3">
                <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
                    {field('name', 'Name')}
                    {field('software', 'Software')}
                    {field('mcVersion', 'Minecraft version')}
                    {field('softwareVersion', softwareVersionLabel(draft.software.trim()))}
                    {isAdmin && (
                        <div>
                            <label htmlFor="tpl-visibility" className="input-label">Visibility</label>
                            <select id="tpl-visibility" className="input-field w-full" value={draft.visibility} onChange={(e) => setDraft({ ...draft, visibility: e.target.value as TemplateVisibility })}>
                                <option value="personal">Personal</option>
                                <option value="admin">Admin</option>
                                <option value="platform">Platform</option>
                            </select>
                        </div>
                    )}
                </div>
                {saveError && <p className="text-xs text-(--error-light)" role="alert">{saveError}</p>}
                {canWrite && (
                    <div className="flex items-center gap-2">
                        <button type="submit" disabled={!dirty || saving} className="btn btn-primary btn-sm disabled:opacity-40 disabled:cursor-not-allowed">
                            {saving ? <Loader2 size={12} className="animate-spin" aria-label="Saving" /> : 'Save'}
                        </button>
                        {dirty && <button type="button" onClick={() => { setDraft(draftOf(t)); setSaveError(null); }} className="btn btn-secondary btn-sm">Discard</button>}
                    </div>
                )}
            </form>

            <section className="card p-4 flex flex-col min-h-112" aria-label="Template files">
                <TemplateFiles templateId={t.id} canEdit={canWrite} maxSizeBytes={templateCap(t, maxSize)} onChanged={onChanged} onSkipped={onSkipped} />
            </section>

            {t.canShare && <TemplateShares templateId={t.id} />}

            {canDelete && (
                <div className="pt-4 border-t border-(--base-03)">
                    <button className="btn btn-danger btn-sm disabled:opacity-40" disabled={deleting} onClick={() => runDelete(remove)}>
                        <Trash2 size={12} /> Delete template
                    </button>
                </div>
            )}
        </main>
    );
}

function BackLink() {
    return (
        <Link href="/templates" className="inline-flex items-center gap-1 text-xs text-(--base-06) hover:text-(--base-09)">
            <ArrowLeft size={12} /> Templates
        </Link>
    );
}
