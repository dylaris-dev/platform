'use client';

import { useState } from 'react';
import { Bell } from 'lucide-react';
import { usePackWorkspace } from '@/components/modpacks/PackWorkspace';
import { ModrinthLibraryPanel, ModrinthWatchDialog } from '@/components/mods/ModrinthLibrary';
import { newestPackBuild } from '@/lib/packWorkspace';
import styles from '@/components/modpacks/workspace.module.css';

export default function PackAlertsPage() {
    const { pack, builds, loading } = usePackWorkspace();
    const [open, setOpen] = useState(false);
    const [revision, setRevision] = useState(0);
    if (loading) return <p className="p-6" role="status">Loading pack...</p>;
    if (!pack) return <p className="p-6">Pack not found.</p>;
    const source = builds.find(build => build.id === pack.recommendedBuildId) || newestPackBuild(builds.filter(build => build.channel === 'release'));
    return <div className={styles.scroll}>
        <div className="space-y-4 max-w-4xl mx-auto">
            <header className="flex items-start justify-between gap-4 flex-wrap"><div><h2 className="text-lg font-medium">Minecraft availability for this pack</h2><p className="text-sm text-(--base-06) mt-2">Your personal alerts follow this pack, not the build currently open in the editor.</p></div><button className="btn btn-primary btn-sm" onClick={() => setOpen(true)}><Bell size={14} />Create pack alert</button></header>
            <div className="card p-4 text-sm text-(--base-07) space-y-2"><p>{source ? `Current source: ${source.versionString} · Minecraft ${source.minecraft} · ${source.loader}.` : 'No source yet. Recommend a build above or publish a release; alerts wait until then.'}</p><p>Checks run daily. A message arrives when all linked mods support your chosen Minecraft version and loader. Local files and gameplay still need testing. Reading a message does not delete it.</p><p>These alerts belong to your account. Other team members manage their own subscriptions.</p></div>
            <ModrinthLibraryPanel key={revision} packId={pack.id} />
        </div>
        {open && <ModrinthWatchDialog packId={pack.id} title={pack.internalName} loader={source?.loader} onClose={() => setOpen(false)} onSaved={() => setRevision(value => value + 1)} />}
    </div>;
}
