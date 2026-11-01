"use client";

import React, { useEffect, useState } from 'react';
import Link from 'next/link';
import { AlertTriangle, Check, LayoutTemplate } from 'lucide-react';
import { listTemplates, type Template } from '@/lib/api/templates';
import { Skeleton, SkeletonText } from '@/components/Skeleton';
import { templateSourceLabel } from './setupPreview';

export interface TemplateSelection {
    id: number;
    name: string;
    software: string;
    mcVersion: string;
    softwareVersion: string;
}

const versionLine = (t: { software: string; mcVersion: string; softwareVersion: string }) =>
    [t.software, t.mcVersion, t.softwareVersion !== t.mcVersion ? t.softwareVersion : ''].filter(Boolean).join(' · ');

// Template picker for the setup flow. Lists every template the caller may use
// of the kind this server runs; one without software cannot be installed
// (Core plans the install from it), so it is shown but not offered.
export default function TemplatePicker({ selection, onSelect, serverType }: {
    selection: TemplateSelection | null;
    onSelect: (s: TemplateSelection | null) => void;
    serverType?: 'game' | 'proxy';
}) {
    const [list, setList] = useState<Template[] | null>(null);
    const [error, setError] = useState('');

    const [attempt, setAttempt] = useState(0);

    useEffect(() => {
        let live = true;
        listTemplates().then(res => {
            if (!live) return;
            if (!res.success) { setError(res.message || 'Could not load templates.'); return; }
            const kind = serverType === 'proxy' ? 'proxy' : 'server';
            setList((res.templates ?? []).filter(t => t.kind === kind));
        });
        return () => { live = false; };
    }, [serverType, attempt]);

    const retry = () => { setError(''); setList(null); setAttempt(a => a + 1); };

    if (selection) {
        return (
            <div className="min-h-[220px]">
                <div className="card p-3 border-(--accent) bg-(--accent-ghost)/30 flex items-start gap-3">
                    <Check size={16} className="text-(--accent-light) shrink-0 mt-0.5" />
                    <div className="min-w-0 flex-1">
                        <div className="text-sm font-medium text-(--base-09) break-all">{templateSourceLabel(selection.name)}</div>
                        <div className="text-xs text-(--base-06) font-mono mt-0.5">{versionLine(selection)}</div>
                        <div className="text-[11px] text-(--base-06) mt-1">The template&apos;s files go into the server folder and its software is installed over them.</div>
                    </div>
                    <button type="button" onClick={() => onSelect(null)} className="text-xs text-(--base-06) hover:text-(--base-09)">Change</button>
                </div>
            </div>
        );
    }

    return (
        <div className="min-h-[220px] space-y-1.5">
            {error ? (
                <div className="text-center py-6 text-sm text-(--error-light) flex flex-col items-center gap-2" role="alert">
                    <AlertTriangle size={16} />
                    {error}
                    <button type="button" onClick={retry} className="btn btn-secondary btn-sm">Retry</button>
                </div>
            ) : list === null ? (
                <div className="space-y-1.5" aria-busy="true">
                    {Array.from({ length: 3 }).map((_, i) => (
                        <div key={i} className="card p-2 flex items-center gap-3">
                            <Skeleton className="w-8 h-8 rounded-md shrink-0" />
                            <div className="min-w-0 flex-1 space-y-1.5">
                                <SkeletonText width="w-1/3" className="h-3" />
                                <SkeletonText width="w-1/4" className="h-2" />
                            </div>
                        </div>
                    ))}
                </div>
            ) : list.length === 0 ? (
                <div className="text-center py-6 text-sm text-(--base-06)">
                    No {serverType === 'proxy' ? 'proxy ' : ''}templates yet. Create one under{' '}
                    <Link href="/templates" className="text-(--accent-light) hover:underline">Templates</Link>.
                </div>
            ) : (
                <div className="space-y-1.5 max-h-[280px] overflow-y-auto">
                    {list.map(t => {
                        const installable = !!t.software;
                        return (
                            <button
                                key={t.id}
                                type="button"
                                disabled={!installable}
                                onClick={() => onSelect({ id: t.id, name: t.name, software: t.software, mcVersion: t.mcVersion, softwareVersion: t.softwareVersion })}
                                className="w-full card p-2 flex items-center gap-3 text-left hover:border-(--accent-border) focus-visible:border-(--accent-border) disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:border-(--base-04)"
                            >
                                <div className="w-8 h-8 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                                    <LayoutTemplate size={12} className="text-(--base-05)" />
                                </div>
                                <div className="min-w-0 flex-1">
                                    <div className="text-sm font-medium text-(--base-09) truncate">{t.name}</div>
                                    <div className="text-[10px] font-mono text-(--base-06) truncate">
                                        {installable ? versionLine(t) : 'No software set. Set it on the template first.'}
                                        {t.visibility !== 'personal' ? ` · ${t.visibility}` : t.shareLevel ? ` · by ${t.ownerName || 'unknown'}` : ''}
                                    </div>
                                </div>
                            </button>
                        );
                    })}
                </div>
            )}
        </div>
    );
}
