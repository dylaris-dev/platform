'use client';

import { useState } from 'react';
import { Bell } from 'lucide-react';
import { SkeletonList } from '@/components/Skeleton';
import { usePackWorkspace } from '@/components/modpacks/PackWorkspace';
import { ModrinthLibraryPanel, ModrinthWatchDialog } from '@/components/mods/ModrinthLibrary';
import { newestPackBuild } from '@/lib/packWorkspace';
import PageHeader from '@/components/ui/PageHeader';
import styles from '@/components/modpacks/workspace.module.css';

export default function PackAlertsPage() {
    const { pack, builds, loading } = usePackWorkspace();
    const [open, setOpen] = useState(false);
    const [revision, setRevision] = useState(0);
    if (loading) return <div className={styles.scroll} role="status"><SkeletonList rows={4} /></div>;
    if (!pack) return <p className="p-6">Pack not found.</p>;
    const source = builds.find(build => build.id === pack.recommendedBuildId) || newestPackBuild(builds.filter(build => build.channel === 'release'));
    return <div className={styles.scroll}>
        <div className="space-y-4 max-w-4xl mx-auto">
            <PageHeader as="h2" title="Minecraft availability for this pack" description="Your personal alerts follow this pack, not the build currently open in the editor." actions={<button className="btn btn-primary btn-sm" onClick={() => setOpen(true)}><Bell size={14} />Create pack alert</button>} />
            <div className="srv-surface p-4 text-sm text-(--base-07) space-y-2"><p>{source ? `Current source: ${source.versionString} · Minecraft ${source.minecraft} · ${source.loader}.` : 'No source yet. Recommend a build above or publish a release; alerts wait until then.'}</p><p>Checks run daily. A message arrives when all linked mods support your chosen Minecraft version and loader. Local files and gameplay still need testing. Reading a message does not delete it.</p><p>These alerts belong to your account. Other team members manage their own subscriptions.</p></div>
            <ModrinthLibraryPanel key={revision} packId={pack.id} />
        </div>
        {open && <ModrinthWatchDialog packId={pack.id} title={pack.internalName} loader={source?.loader} onClose={() => setOpen(false)} onSaved={() => setRevision(value => value + 1)} />}
    </div>;
}
