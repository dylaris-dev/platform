'use client';

import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { ArrowLeft } from 'lucide-react';
import { usePackWorkspace } from '@/components/modpacks/PackWorkspace';
import CreateBuildForm from '@/components/modpacks/CreateBuildForm';
import { currentPackBuild } from '@/lib/packWorkspace';
import { useAppData } from '@/lib/AppDataContext';
import { toast } from '@/components/ui/Toast';
import { SkeletonList } from '@/components/Skeleton';
import styles from '@/components/modpacks/workspace.module.css';

export default function NewBuildPage() {
    const { pack, builds, permissions, loading, refresh } = usePackWorkspace();
    const { featureFlags } = useAppData();
    const router = useRouter();
    if (loading) return <div className={styles.scroll}><SkeletonList rows={6} /></div>;
    if (!pack || !permissions.canEdit || !featureFlags.modpacks) return <p className="p-6 text-sm text-(--base-06)">You do not have access to create builds for this pack.</p>;
    return <div className={styles.scroll}>
        <Link href={`/modpacks/${pack.id}`} className="text-xs text-(--base-06) inline-flex gap-1 items-center mb-5"><ArrowLeft size={12} /> Back to builds</Link>
        <CreateBuildForm packId={pack.id} builds={builds} previous={currentPackBuild(builds)} existingVersions={builds.map(build => build.versionString)} onClose={() => router.push(`/modpacks/${pack.id}`)} onCreated={build => { toast('Build created.', true); refresh(); router.push(`/modpacks/${pack.id}/builds/${build.id}`); }} />
    </div>;
}
