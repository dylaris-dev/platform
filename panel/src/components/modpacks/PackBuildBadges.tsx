import { Lock, Star } from 'lucide-react';
import type { PackBuild } from '@/lib/api/packs';
import { newestPackBuild, packBuildStatus } from '@/lib/packWorkspace';
import { Badge } from '@/components/ui/Badge';

export default function PackBuildBadges({ build, builds, recommendedBuildId }: { build: PackBuild; builds: PackBuild[]; recommendedBuildId?: number }) {
    const recommended=build.id===recommendedBuildId;
    const latestRelease=newestPackBuild(builds.filter(candidate=>candidate.channel==='release'))?.id===build.id;
    return <span className="inline-flex gap-1.5 items-center flex-wrap">
        {recommended && <Badge variant="accent" icon={<Star size={10}/>}>Recommended</Badge>}
        {latestRelease && <Badge variant="success">Latest release</Badge>}
        {!recommended&&!latestRelease && <Badge variant="neutral">{packBuildStatus(build,builds,recommendedBuildId)}</Badge>}
        {(build.frozen||build.modrinthPublished)&&<Lock size={11} className="text-(--base-06)" aria-label="Read-only"/>}
    </span>;
}
