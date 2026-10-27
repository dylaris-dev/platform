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
import { Badge } from '@/components/ui/Badge';
import { SkeletonList } from '@/components/Skeleton';
import styles from '@/components/modpacks/workspace.module.css';

export default function PackDetailPage() {
    const { pack, builds, permissions, loading, refresh } = usePackWorkspace();
    const { featureFlags } = useAppData();
    const [busy, run] = useBusy();
    const router = useRouter();
    if (loading) return <div className={styles.scroll}><SkeletonList rows={5} /></div>;
    if (!pack) return <p className="p-6 text-sm text-(--base-06)">Pack not found.</p>;
    const canDelete = permissions.canDelete && featureFlags.modpacks;
    return <div className={styles.scroll}>
        <div className="flex items-center justify-between gap-3 mb-5"><div><h2 className="text-base font-medium text-(--base-09)">Builds</h2><p className="text-xs text-(--base-06) mt-1">Each build pins its Minecraft environment and content.</p></div>{pack.modrinthProjectId && <a href={`https://modrinth.com/modpack/${encodeURIComponent(pack.modrinthProjectId)}`} target="_blank" rel="noopener noreferrer" className="btn btn-secondary btn-sm"><ExternalLink size={12} /> Modrinth</a>}</div>
        {builds.length === 0 && <div className="py-14 text-center text-(--base-06)"><Layers size={28} className="mx-auto mb-3" /><p className="text-sm">No builds yet.</p>{permissions.canEdit && featureFlags.modpacks && <Link href={`/modpacks/${pack.id}/builds/new`} className="btn btn-primary btn-sm mt-4"><Plus size={12} /> Create the first build</Link>}</div>}
        <div className="space-y-2">{builds.map(build => <div key={build.id} className="flex items-center gap-3 p-4 rounded-md border border-(--base-03) hover:bg-(--base-02)"><Link href={`/modpacks/${pack.id}/builds/${build.id}`} className="flex-1 min-w-0"><div className="flex items-center gap-2 flex-wrap"><strong className="font-mono text-sm text-(--base-09)">{build.versionString}</strong><Badge variant={build.channel === 'release' ? 'success' : 'neutral'}>{packBuildStatus(build, builds)}</Badge></div><p className="text-xs text-(--base-06) mt-2">MC {build.minecraft || 'Unspecified'} · {build.loader || 'Vanilla'} {build.loaderVersion} · {fmtDateTime(build.createdAt)}</p></Link>{canDelete && <button className="btn btn-secondary btn-sm" disabled={busy} aria-label={`Delete build ${build.versionString}`} onClick={() => run(async () => { if (!await confirmDialog({ title: `Delete build ${build.versionString}?`, message: 'This removes this build and its content links. Other builds remain available.' })) return; const result = await deleteBuild(pack.id, build.id); toast(result.success ? 'Build deleted.' : result.message || 'Delete failed.', !!result.success); if (result.success) refresh(); })}><Trash2 size={12} /></button>}</div>)}</div>
        {canDelete && <div className="pt-6 mt-7 border-t border-(--base-03)"><button className="btn btn-danger btn-sm" disabled={busy} onClick={() => run(async () => { if (!await confirmDialog({ title: `Delete ${pack.internalName}?`, message: 'This removes the local pack and all its builds. Any published Modrinth project remains on Modrinth.' })) return; const result = await deletePack(pack.id); toast(result.success ? 'Pack deleted.' : result.message || 'Delete failed.', !!result.success); if (result.success) router.push('/modpacks'); })}><Trash2 size={12} /> Delete pack</button></div>}
    </div>;
}
