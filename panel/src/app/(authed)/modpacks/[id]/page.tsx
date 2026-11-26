'use client';

import Link from 'next/link';
import { Plus, Layers, Trash2, ExternalLink } from 'lucide-react';
import { usePackWorkspace } from '@/components/modpacks/PackWorkspace';
import { deleteBuild, deletePack } from '@/lib/api/packs';
import { useAppData } from '@/lib/AppDataContext';
import { useRouter } from 'next/navigation';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { useBusy } from '@/lib/useBusy';
import { packBuildStatus } from '@/lib/packWorkspace';
import { fmtDateTime } from '@/lib/time';
import PackBuildBadges from '@/components/modpacks/PackBuildBadges';
import { SkeletonList } from '@/components/Skeleton';
import DeleteBuildButton from '@/components/modpacks/DeleteBuildButton';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import styles from '@/components/modpacks/workspace.module.css';

export default function PackDetailPage() {
    const { pack, builds, permissions, loading, refresh } = usePackWorkspace();
    const { featureFlags } = useAppData();
    const [busy, run] = useBusy();
    const router = useRouter();
    if (loading) return <div className={styles.scroll}><SkeletonList rows={5} /></div>;
    if (!pack) return <p className="p-6 text-sm text-(--base-06)">Pack not found.</p>;
    const canDelete = permissions.canDelete && featureFlags.modpacks;
    return <div className={styles.scroll}><div className="mx-auto max-w-5xl space-y-6">
        <Section
            title="Builds"
            description="Each build pins its Minecraft environment and content."
            actions={pack.modrinthProjectId ? <a href={`https://modrinth.com/modpack/${encodeURIComponent(pack.modrinthProjectId)}`} target="_blank" rel="noopener noreferrer" className="btn btn-secondary btn-sm"><ExternalLink size={12} /> Modrinth</a> : undefined}
        >
            {builds.length === 0 ? <EmptyState icon={<Layers size={20} />} title="No builds yet" action={permissions.canEdit && featureFlags.modpacks ? <Link href={`/modpacks/${pack.id}/builds/new`} className="btn btn-primary btn-sm"><Plus size={12} /> Create the first build</Link> : undefined} /> : <div className="srv-surface"><RowList label="Builds">{builds.map(build => <Row
                key={build.id}
                href={`/modpacks/${pack.id}/builds/${build.id}`}
                title={<span className="flex flex-wrap items-center gap-2"><strong className="srv-num text-sm">{build.versionString}</strong><PackBuildBadges build={build} builds={builds} recommendedBuildId={pack.recommendedBuildId} /></span>}
                meta={`MC ${build.minecraft || 'Unspecified'} · ${build.loader || 'Vanilla'} ${build.loaderVersion} · ${fmtDateTime(build.createdAt)}`}
                actions={canDelete ? <DeleteBuildButton packId={pack.id} build={build} onDeleted={() => { void refresh(); }} /> : undefined}
            />)}</RowList></div>}
        </Section>
        {canDelete && <div className="pt-2"><button className="btn btn-danger btn-sm" disabled={busy} onClick={() => run(async () => { if (!await confirmDialog({ title: `Delete ${pack.internalName}?`, message: 'This removes the local pack and all its builds. Any published Modrinth project remains on Modrinth.' })) return; const result = await deletePack(pack.id); toast(result.success ? 'Pack deleted.' : result.message || 'Delete failed.', !!result.success); if (result.success) router.push('/modpacks'); })}><Trash2 size={12} /> Delete pack</button></div>}
    </div></div>;
}
