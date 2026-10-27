'use client';

import { useState } from 'react';
import { type Pack, type PackBuild, setRecommendedBuild } from '@/lib/api/packs';
import Select from '@/components/ui/Select';
import { toast } from '@/components/ui/Toast';

export default function PackRecommendation({
    pack,
    builds,
    canEdit,
    onChanged,
}: {
    pack: Pack;
    builds: PackBuild[];
    canEdit: boolean;
    onChanged: () => Promise<void>;
}) {
    const [busy, setBusy] = useState(false);
    const explicit = builds.find((build) => build.id === pack.recommendedBuildId);
    const release = [...builds]
        .filter((build) => build.channel === 'release')
        .sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt) || b.id - a.id)[0];
    const selected = explicit || release;
    return (
        <section className="space-y-2 pt-4 border-t border-(--base-03)">
            <label className="input-label" htmlFor="pack-recommended-build">
                Recommended build
            </label>
            <Select
                id="pack-recommended-build"
                ariaLabel="Recommended build"
                value={String(pack.recommendedBuildId || 0)}
                disabled={!canEdit || busy}
                options={[
                    { value: '0', label: 'Automatic: newest release' },
                    ...builds.map((build) => ({
                        value: String(build.id),
                        label: `${build.versionString} · ${build.channel}`,
                    })),
                ]}
                onChange={async (value) => {
                    setBusy(true);
                    try {
                        const result = await setRecommendedBuild(pack.id, Number(value));
                        if (!result.success) throw new Error(result.message || 'Could not update recommendation.');
                        await onChanged();
                        toast('Pack recommendation updated.');
                    } catch (error) {
                        toast(error instanceof Error ? error.message : 'Could not update recommendation.', false);
                    } finally {
                        setBusy(false);
                    }
                }}
            />
            <p className="text-xs text-(--base-06)">
                {selected
                    ? `Pack alerts follow ${selected.versionString}.`
                    : 'Alerts wait until you recommend a build or publish a release.'}{' '}
                This does not publish or update running servers.
            </p>
            {explicit && !explicit.frozen && (
                <p className="text-xs text-(--warning-light)">
                    This recommended build is still editable. Alerts use its current mod list.
                </p>
            )}
        </section>
    );
}
