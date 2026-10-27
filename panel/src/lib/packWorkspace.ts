import type { PackBuild } from '@/lib/api/packs';

export function newestPackBuild(builds: PackBuild[]): PackBuild | undefined {
    return [...builds].sort((a, b) => {
        const difference = Date.parse(b.createdAt) - Date.parse(a.createdAt);
        return Number.isFinite(difference) && difference !== 0 ? difference : b.id - a.id;
    })[0];
}

export function currentPackBuild(builds: PackBuild[],recommendedBuildId?:number): PackBuild | undefined {
    const recommended=builds.find(build=>build.id===recommendedBuildId);
    if(recommended)return recommended;
    return newestPackBuild(builds.filter(build => build.channel === 'release')) || newestPackBuild(builds);
}

export function packBuildStatus(build: PackBuild, builds: PackBuild[],recommendedBuildId?:number): string {
    if(build.id===recommendedBuildId)return 'Recommended';
    if (currentPackBuild(builds)?.id === build.id) return build.channel === 'release' ? 'Latest release' : 'Latest build';
    if (build.channel === 'release') return 'Release';
    if (build.channel === 'beta') return 'Beta';
    return build.frozen ? 'Frozen draft' : 'Draft';
}

export function packContentGroup(type: string): 'mods' | 'resources' | 'configs' {
    if (type === 'config') return 'configs';
    if (type === 'resourcepack' || type === 'shaderpack') return 'resources';
    return 'mods';
}
