'use client';

import { useEffect, useState } from 'react';
import { Download, ExternalLink, Loader2, RefreshCw } from 'lucide-react';
import { getModrinthProject, type ModrinthProject } from '@/lib/api/modrinth';
import { ModDescription } from '@/components/mods/ModDescription';

/** Uses the same sanitized Markdown renderer as Server > Content. */
export default function ModProjectDetails({ projectId }: { projectId: string }) {
    const [project, setProject] = useState<ModrinthProject | null>(null);
    const [loading, setLoading] = useState(true);
    const [attempt, setAttempt] = useState(0);
    const [mode, setMode] = useState<'short' | 'full'>('short');
    useEffect(() => {
        let live = true;
        setLoading(true);
        setProject(null);
        getModrinthProject(projectId).then(result => { if (live) { setProject(result); setLoading(false); } });
        return () => { live = false; };
    }, [projectId, attempt]);
    if (loading) return <p role="status" className="flex gap-2 items-center text-xs text-(--base-06) py-4"><Loader2 size={13} className="animate-spin" /> Loading project details...</p>;
    if (!project) return <div role="alert" className="flex items-center flex-wrap gap-2 text-xs text-(--base-06) py-3">Project details could not be loaded.<button className="btn btn-secondary btn-sm" onClick={() => setAttempt(value => value + 1)}><RefreshCw size={12} /> Retry details</button></div>;
    return <section className="space-y-3" aria-label={`${project.title} details`}>
        <div className="flex items-center justify-between gap-3 flex-wrap">
            <div className="flex items-center gap-3 text-xs text-(--base-06)"><span className="inline-flex gap-1 items-center"><Download size={12} /> {project.downloads.toLocaleString()} downloads</span>{project.license && <span>{project.license.name || project.license.id}</span>}</div>
            <a href={`https://modrinth.com/project/${encodeURIComponent(project.id)}`} target="_blank" rel="noopener noreferrer" className="btn btn-secondary btn-sm">Open on Modrinth <ExternalLink size={12} /></a>
        </div>
        <div className="flex items-center justify-between gap-2"><span className="input-label mb-0">Description</span><div className="flex gap-1" role="group" aria-label="Description length">{(['short', 'full'] as const).map(value => <button key={value} onClick={() => setMode(value)} aria-pressed={mode === value} className={`btn btn-sm ${mode === value ? 'btn-primary' : 'btn-secondary'}`}>{value === 'short' ? 'Short' : 'Full'}</button>)}</div></div>
        {mode === 'full' ? <ModDescription body={project.body || project.description} /> : <p className="text-sm text-(--base-07) leading-relaxed">{project.description || 'No description provided.'}</p>}
    </section>;
}
