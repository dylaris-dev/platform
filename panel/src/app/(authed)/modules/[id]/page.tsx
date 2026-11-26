"use client";

import { useAppData } from '@/lib/AppDataContext';
import EmptyState from '@/components/ui/EmptyState';
import PlaceholderView from '@/views/PlaceholderView';
import { useRouteId } from '@/lib/routeParams';

export default function CustomModulePage() {
    const paramId = useRouteId('modules');
    const { modules } = useAppData();
    const m = modules.find(x => String(x.id) === String(paramId));

    return (
        <main className="flex-1 flex flex-col overflow-hidden relative z-10 app-page">
            {!m ? (
                <EmptyState title="Module not found" description="This module does not exist or was removed." />
            ) : m.type === 'iframe' && m.url ? (
                <div className="w-full h-full rounded-(--radius-lg) overflow-hidden bg-(--base-02)">
                    {/* Sandboxed like a custom tab: a third-party page must not
                        navigate the panel or open windows unasked. */}
                    <iframe
                        src={m.url}
                        className="w-full h-full"
                        title={m.name}
                        sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"
                        referrerPolicy="no-referrer"
                    />
                </div>
            ) : (
                <PlaceholderView viewName={m.name} />
            )}
        </main>
    );
}
