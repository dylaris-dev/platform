'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import Link from 'next/link';
import { ExternalLink, RefreshCw, Send, Upload, AlertTriangle, CheckCircle2, Lightbulb } from 'lucide-react';
import { usePackWorkspace } from '@/components/modpacks/PackWorkspace';
import { getModrinthPage, editModrinthPage, submitModrinthReview, uploadModrinthIcon, type ModrinthPageEdit, type ModrinthPageResult } from '@/lib/api/packs';
import { getModrinthCategories, type ModrinthCategory } from '@/lib/api/modrinth';
import { nagText, blockingNags, canSubmitFrom, MODRINTH_STATUS_LABEL } from '@/lib/modrinthChecklist';
import { categoryLabel } from '@/lib/modrinthCategories';
import Select from '@/components/ui/Select';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { SkeletonList } from '@/components/Skeleton';
import styles from '@/components/modpacks/workspace.module.css';

const LICENSES = [
    { value: 'LicenseRef-All-Rights-Reserved', label: 'All rights reserved' },
    { value: 'MIT', label: 'MIT' },
    { value: 'Apache-2.0', label: 'Apache 2.0' },
    { value: 'GPL-3.0-only', label: 'GPL 3.0' },
    { value: 'LGPL-3.0-only', label: 'LGPL 3.0' },
    { value: 'CC-BY-4.0', label: 'CC BY 4.0' },
    { value: 'CC-BY-NC-4.0', label: 'CC BY-NC 4.0' },
    { value: 'CC0-1.0', label: 'CC0 (public domain)' },
];
const ENVIRONMENTS = [
    { value: 'client_and_server', label: 'Client and server' },
    { value: 'client_only', label: 'Client only (singleplayer)' },
];
// Modrinth's own minimums, from its publishing checklist.
const MIN_SUMMARY = 25;
const MIN_DESCRIPTION = 125;

type Form = { title: string; description: string; body: string; categories: string[]; licenseId: string; sourceUrl: string; issuesUrl: string; wikiUrl: string; discordUrl: string; environment: string };

function formFrom(result: ModrinthPageResult): Form {
    const p = result.project!;
    return {
        title: p.title, description: p.description, body: p.body,
        categories: [...p.categories, ...p.additional_categories],
        licenseId: p.license.id, sourceUrl: p.source_url || '', issuesUrl: p.issues_url || '', wikiUrl: p.wiki_url || '', discordUrl: p.discord_url || '',
        environment: result.environment && result.environment !== 'unknown' ? result.environment : '',
    };
}

/** Only what changed goes to Modrinth, so an untouched field is never rewritten. */
function changes(before: Form, after: Form): ModrinthPageEdit {
    const edit: ModrinthPageEdit = {};
    if (after.title !== before.title) edit.title = after.title;
    if (after.description !== before.description) edit.description = after.description;
    if (after.body !== before.body) edit.body = after.body;
    if (after.categories.join() !== before.categories.join()) { edit.categories = after.categories; edit.additionalCategories = []; }
    if (after.licenseId !== before.licenseId) edit.licenseId = after.licenseId;
    if (after.sourceUrl !== before.sourceUrl) edit.sourceUrl = after.sourceUrl;
    if (after.issuesUrl !== before.issuesUrl) edit.issuesUrl = after.issuesUrl;
    if (after.wikiUrl !== before.wikiUrl) edit.wikiUrl = after.wikiUrl;
    if (after.discordUrl !== before.discordUrl) edit.discordUrl = after.discordUrl;
    if (after.environment && after.environment !== before.environment) edit.environment = after.environment;
    return edit;
}

export default function PackModrinthPage() {
    const { pack, permissions, loading } = usePackWorkspace();
    const [result, setResult] = useState<ModrinthPageResult | null>(null);
    const [error, setError] = useState('');
    const [form, setForm] = useState<Form | null>(null);
    const [saved, setSaved] = useState<Form | null>(null);
    const [busy, setBusy] = useState<'' | 'load' | 'save' | 'icon' | 'submit'>('load');
    const [categories, setCategories] = useState<ModrinthCategory[]>([]);
    const iconInput = useRef<HTMLInputElement>(null);

    const apply = useCallback((res: ModrinthPageResult) => {
        if (!res.success || !res.project) { setError(res.message || 'Could not load the Modrinth project.'); return false; }
        setError(''); setResult(res);
        const next = formFrom(res); setForm(next); setSaved(next);
        return true;
    }, []);
    const load = useCallback(async () => {
        if (!pack?.modrinthProjectId) { setBusy(''); return; }
        setBusy('load');
        try { apply(await getModrinthPage(pack.id)); } finally { setBusy(''); }
    }, [pack?.id, pack?.modrinthProjectId, apply]);
    useEffect(() => { void load(); }, [load]);
    useEffect(() => { let live = true; getModrinthCategories().then(all => { if (live) setCategories(all.filter(c => c.project_type === 'modpack' && c.header === 'categories')); }); return () => { live = false; }; }, []);

    const edit = useMemo(() => form && saved ? changes(saved, form) : {}, [form, saved]);
    const dirty = Object.keys(edit).length > 0;
    useEffect(() => {
        if (!dirty) return;
        const warn = (e: BeforeUnloadEvent) => { e.preventDefault(); };
        window.addEventListener('beforeunload', warn);
        return () => window.removeEventListener('beforeunload', warn);
    }, [dirty]);

    if (loading) return <div className={styles.scroll}><SkeletonList rows={6} /></div>;
    if (!pack) return <p className="p-6 text-sm text-(--base-06)">Pack not found.</p>;
    if (!pack.modrinthProjectId) return <div className={styles.scroll}><div className="max-w-2xl mx-auto card p-6 space-y-3 text-sm text-(--base-07)">
        <h2 className="text-base font-medium text-(--base-09)">No Modrinth project yet</h2>
        <p>Publishing the first build creates this pack&apos;s Modrinth project. Its page, tags and review submission are managed here afterwards.</p>
        <Link href={`/modpacks/${pack.id}`} className="btn btn-secondary btn-sm">Back to builds</Link>
    </div></div>;
    if (!permissions.canEdit) return <p className="p-6 text-sm text-(--base-06)">Editing the Modrinth page needs edit access to this pack.</p>;
    if (busy === 'load' && !result) return <div className={styles.scroll}><SkeletonList rows={6} /></div>;
    if (!result?.project || !form) return <div className={styles.scroll}><div role="alert" className="max-w-2xl mx-auto card p-5 text-sm space-y-3"><p>{error || 'Could not load the Modrinth project.'}</p><button className="btn btn-secondary btn-sm" onClick={() => void load()}><RefreshCw size={12} />Retry</button></div></div>;

    const project = result.project;
    const nags = result.nags || [];
    const blocking = blockingNags(nags);
    const set = <K extends keyof Form>(key: K, value: Form[K]) => setForm(prev => prev ? { ...prev, [key]: value } : prev);
    const summaryLen = [...form.description.trim()].length;
    const bodyLen = [...form.body.trim()].length;
    const repeatsSummary = form.body.trim() !== '' && form.body.trim() === form.description.trim();
    const pageUrl = `https://modrinth.com/modpack/${encodeURIComponent(project.slug || project.id)}`;
    const licenseOptions = LICENSES.some(l => l.value === form.licenseId) ? LICENSES : [{ value: form.licenseId, label: form.licenseId }, ...LICENSES];

    const save = async () => {
        if (!dirty) return;
        setBusy('save');
        try {
            const res = await editModrinthPage(pack.id, edit);
            if (apply(res)) toast(res.message || 'Saved to Modrinth.', true);
            else toast(res.message || 'Saving failed.', false);
        } finally { setBusy(''); }
    };
    const uploadIcon = async (file: File | undefined) => {
        if (!file) return;
        if (file.size > 256 * 1024) { toast('The icon must be at most 256 KB.', false); return; }
        setBusy('icon');
        try {
            const res = await uploadModrinthIcon(pack.id, file);
            if (res.success && res.project) { setResult(res); toast('Icon updated.', true); } else toast(res.message || 'Icon upload failed.', false);
        } finally { setBusy(''); if (iconInput.current) iconInput.current.value = ''; }
    };
    const submit = async () => {
        if (dirty) { toast('Save your changes first.', false); return; }
        if (!await confirmDialog({ title: 'Submit for review?', message: 'Modrinth moderators review the project before it becomes public. Changes during the review are limited; this usually takes a few days.', confirmLabel: 'Submit' })) return;
        setBusy('submit');
        try {
            const res = await submitModrinthReview(pack.id);
            if (apply(res)) toast(res.message || 'Submitted for review.', true);
            else toast(res.message || 'Submission failed.', false);
        } finally { setBusy(''); }
    };

    return <div className={styles.scroll}>
        <div className="max-w-6xl mx-auto grid gap-5 lg:grid-cols-[minmax(0,1fr)_340px] items-start">
            <div className="space-y-5 min-w-0">
                <header className="flex items-start justify-between gap-3 flex-wrap">
                    <div><h2 className="text-lg font-medium text-(--base-09)">Modrinth page</h2><p className="text-xs text-(--base-06) mt-1">What players see on Modrinth. Changes are saved to Modrinth through the pack owner&apos;s token.</p></div>
                    <div className="flex gap-2"><button className="btn btn-ghost btn-sm" disabled={!!busy} onClick={() => void load()}><RefreshCw size={12} className={busy === 'load' ? 'animate-spin' : ''} />Refresh</button><a href={pageUrl} target="_blank" rel="noopener noreferrer" className="btn btn-secondary btn-sm"><ExternalLink size={12} />Open on Modrinth</a></div>
                </header>

                <section className="card p-5 space-y-4" aria-label="Texts">
                    <div><label htmlFor="mr-title" className="input-label">Name</label><input id="mr-title" className="input-field w-full" value={form.title} maxLength={64} onChange={e => set('title', e.target.value)} /></div>
                    <div>
                        <label htmlFor="mr-summary" className="input-label">Summary</label>
                        <input id="mr-summary" className="input-field w-full" value={form.description} maxLength={256} onChange={e => set('description', e.target.value)} />
                        <p className={`text-xs mt-1 ${summaryLen < MIN_SUMMARY ? 'text-(--warning-light)' : 'text-(--base-06)'}`}>{summaryLen}/256 characters. One line under the name in search results; at least {MIN_SUMMARY}, no links.</p>
                    </div>
                    <div>
                        <label htmlFor="mr-body" className="input-label">Description (Markdown)</label>
                        <textarea id="mr-body" className="input-field input-mono w-full min-h-[22rem] text-xs leading-relaxed" value={form.body} onChange={e => set('body', e.target.value)} />
                        <p className={`text-xs mt-1 ${bodyLen < MIN_DESCRIPTION || repeatsSummary ? 'text-(--warning-light)' : 'text-(--base-06)'}`}>{bodyLen} characters, at least {MIN_DESCRIPTION}. {repeatsSummary ? 'It must not repeat the summary. ' : ''}Say what the pack adds, why to play it and what players need to know.</p>
                    </div>
                </section>

                <section className="card p-5 space-y-3" aria-label="Tags">
                    <h3 className="text-sm font-medium text-(--base-09)">Tags</h3>
                    <p className="text-xs text-(--base-06)">Pick the tags that describe the pack, up to 8. The Minecraft version and loader come from the uploaded builds.</p>
                    <div className="flex flex-wrap gap-1.5">{categories.length === 0 ? <p className="text-xs text-(--base-06)">Loading tags...</p> : categories.map(c => { const on = form.categories.includes(c.name); return <button key={c.name} type="button" aria-pressed={on} disabled={!on && form.categories.length >= 8} onClick={() => set('categories', on ? form.categories.filter(x => x !== c.name) : [...form.categories, c.name])} className={`btn btn-sm capitalize ${on ? 'bg-(--accent-ghost) border-(--accent-border) text-(--accent-light)' : 'btn-secondary'} disabled:opacity-40`}>{categoryLabel(c.name)}</button>; })}</div>
                </section>

                <section className="card p-5 grid gap-4 sm:grid-cols-2" aria-label="Links and license">
                    <h3 className="text-sm font-medium text-(--base-09) sm:col-span-2">Links, license and environment</h3>
                    {([['sourceUrl', 'Source code'], ['issuesUrl', 'Issue tracker'], ['wikiUrl', 'Wiki'], ['discordUrl', 'Discord invite']] as const).map(([key, label]) => <div key={key}><label htmlFor={`mr-${key}`} className="input-label">{label}</label><input id={`mr-${key}`} type="url" placeholder="https://" className="input-field w-full" value={form[key]} onChange={e => set(key, e.target.value)} /></div>)}
                    <div><label htmlFor="mr-license" className="input-label">License</label><Select id="mr-license" value={form.licenseId} onChange={value => set('licenseId', value)} options={licenseOptions} /></div>
                    <div><label htmlFor="mr-env" className="input-label">Environment</label><Select id="mr-env" value={form.environment} placeholder="Not set" onChange={value => set('environment', value)} options={ENVIRONMENTS} /><p className="text-xs text-(--base-06) mt-1">Applies to every uploaded version; new builds take it over.</p></div>
                </section>

                <div className="flex items-center justify-end gap-3 sticky bottom-0 py-3 border-t border-(--base-03) bg-(--base-00)">
                    {dirty && <span className="text-xs text-(--base-06)">Unsaved changes</span>}
                    <button className="btn btn-secondary btn-sm" disabled={!dirty || !!busy} onClick={() => saved && setForm(saved)}>Discard</button>
                    <button className="btn btn-primary btn-sm" disabled={!dirty || !!busy} onClick={() => void save()}>{busy === 'save' ? 'Saving...' : 'Save to Modrinth'}</button>
                </div>
            </div>

            <aside className="space-y-5 lg:sticky lg:top-0" aria-label="Review">
                <section className="card p-5 space-y-3">
                    <div className="flex items-center gap-3">
                        {project.icon_url ? <img src={project.icon_url} alt="" className="w-14 h-14 rounded-lg object-cover" /> : <div className="w-14 h-14 rounded-lg bg-(--base-03)" aria-hidden="true" />}
                        <div className="min-w-0"><p className="font-medium text-(--base-09) truncate">{project.title}</p><p className="text-xs text-(--base-06)">{MODRINTH_STATUS_LABEL[project.status] || project.status}</p></div>
                    </div>
                    <input ref={iconInput} type="file" accept="image/png,image/jpeg,image/webp,image/gif" className="hidden" onChange={e => void uploadIcon(e.target.files?.[0])} />
                    <button className="btn btn-secondary btn-sm w-full" disabled={!!busy} onClick={() => iconInput.current?.click()}><Upload size={12} />{busy === 'icon' ? 'Uploading...' : project.icon_url ? 'Replace icon' : 'Upload icon'}</button>
                    <p className="text-[11px] text-(--base-06)">PNG, JPEG, WebP or GIF, square, at most 256 KB.</p>
                </section>

                {project.moderator_message && <section role="status" className="card p-5 space-y-2 border-(--warning)"><h3 className="text-sm font-medium text-(--warning-light)">Message from Modrinth&apos;s moderators</h3><p className="text-sm text-(--base-07) whitespace-pre-wrap">{project.moderator_message.message}</p>{project.moderator_message.body && <p className="text-xs text-(--base-06) whitespace-pre-wrap">{project.moderator_message.body}</p>}</section>}

                <section className="card p-5 space-y-3" aria-label="Publishing checklist">
                    <h3 className="text-sm font-medium text-(--base-09)">Publishing checklist</h3>
                    <p className="text-xs text-(--base-06)">Checked by Modrinth itself. Required items block the review.</p>
                    {nags.length === 0 && <p className="text-xs text-(--success-light) flex items-center gap-1.5"><CheckCircle2 size={13} />Nothing left to do.</p>}
                    <ul className="space-y-2">{[...blocking, ...nags.filter(n => n.severity !== 'required')].map(n => <li key={n.kind} className="flex gap-2 text-xs">{n.severity === 'required' ? <AlertTriangle size={13} className="shrink-0 mt-0.5 text-(--error-light)" /> : <Lightbulb size={13} className="shrink-0 mt-0.5 text-(--base-06)" />}<span className={n.severity === 'required' ? 'text-(--base-09)' : 'text-(--base-07)'}>{nagText(n)}{n.severity !== 'required' && <span className="text-(--base-06)"> Optional.</span>}</span></li>)}</ul>
                    {canSubmitFrom(project.status)
                        ? <button className="btn btn-primary btn-sm w-full" disabled={blocking.length > 0 || dirty || !!busy} title={blocking.length ? 'Complete the required items first' : dirty ? 'Save your changes first' : undefined} onClick={() => void submit()}><Send size={12} />{busy === 'submit' ? 'Submitting...' : 'Submit for review'}</button>
                        : <p className="text-xs text-(--base-07)">{project.status === 'processing' ? 'Submitted. Modrinth\'s moderators review it; the result appears here.' : 'This project is past review.'}</p>}
                </section>
            </aside>
        </div>
    </div>;
}
