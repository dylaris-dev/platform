"use client";

import { LayoutTemplate } from 'lucide-react';
import EmptyState from '@/components/ui/EmptyState';
import { useAppData } from '@/lib/AppDataContext';
import { templatesVisible } from '@/lib/templates';

// One gate for every Templates page, matching Core's audience gate: a page
// reached by URL while the feature is closed explains itself instead of
// rendering a list of 403s.
export default function TemplatesLayout({ children }: { children: React.ReactNode }) {
    const { featureFlags, user } = useAppData();
    if (templatesVisible(featureFlags.templates, !!user?.isAdmin)) return <>{children}</>;
    return (
        <main className="flex-1 min-h-0 overflow-y-auto app-page">
            <EmptyState
                icon={<LayoutTemplate size={20} />}
                title="Templates are not available"
                description="Templates are not open to your account on this platform. Ask an administrator if you need them."
                className="mx-auto mt-10 max-w-lg"
            />
        </main>
    );
}
