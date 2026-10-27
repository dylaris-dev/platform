'use client';

import { useCallback, useEffect, useState } from 'react';
import { FileCode2, ImageIcon, Package, FileText } from 'lucide-react';
import { getModrinthProjectsByIds, type ModrinthProjectName } from '@/lib/api/modrinth';

const metadataCache = new Map<string, { project: ModrinthProjectName | null; expires: number }>();

/** Resolve a whole visible list in batches, rather than making one request per icon. */
export function useProjectMetadata(projectIds: string[]) {
    const key = [...new Set(projectIds.filter(Boolean))].sort().join(',');
    const [projects, setProjects] = useState<Record<string, ModrinthProjectName>>({});
    const [error, setError] = useState(false);
    const [attempt, setAttempt] = useState(0);
    useEffect(() => {
        let live = true;
        const ids = key ? key.split(',') : [];
        const cached: Record<string, ModrinthProjectName> = {};
        const missing = ids.filter(id => {
            const hit = metadataCache.get(id);
            if (!hit || hit.expires < Date.now()) return true;
            if (hit.project) cached[id] = hit.project;
            return false;
        });
        setProjects(cached);
        setError(false);
        if (missing.length) getModrinthProjectsByIds(missing).then(found => {
            if (!live) return;
            const byId = new Map(found.map(project => [project.id, project]));
            const next = { ...cached };
            for (const id of missing) {
                const project = byId.get(id) || null;
                metadataCache.set(id, { project, expires: Date.now() + 5 * 60_000 });
                if (project) next[id] = project;
            }
            if (metadataCache.size > 2000) metadataCache.clear();
            setProjects(next);
        }).catch(() => { if (live) setError(true); });
        return () => { live = false; };
    }, [key, attempt]);
    const retry = useCallback(() => setAttempt(value => value + 1), []);
    return { projects, error, retry };
}

export default function ProjectIcon({ url, name, contentType = 'mod', className = 'w-8 h-8' }: {
    url?: string;
    name: string;
    contentType?: string;
    className?: string;
}) {
    const [failed, setFailed] = useState('');
    const valid = !!url && /^https:\/\//i.test(url) && failed !== url;
    const Fallback = contentType === 'config' ? FileCode2 : contentType === 'resourcepack' || contentType === 'shaderpack' ? ImageIcon : contentType === 'file' ? FileText : Package;
    return valid ? (
        // Upstream icons are remote project artwork, not Next image assets.
        // eslint-disable-next-line @next/next/no-img-element
        <img src={url} alt={`${name} icon`} loading="lazy" referrerPolicy="no-referrer" onError={() => setFailed(url)} className={`${className} rounded-md object-cover shrink-0 bg-(--base-03)`} />
    ) : <span className={`${className} rounded-md bg-(--base-03) text-(--accent-light) flex items-center justify-center shrink-0`} role="img" aria-label={`${name} icon`}><Fallback size={17} /></span>;
}
