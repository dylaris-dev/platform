"use client";

import { useCallback, useEffect, useMemo, useState } from 'react';
import { Gauge, AlertTriangle, Trash2, Globe } from 'lucide-react';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard from '@/components/settings/SettingsCard';
import TrafficAllowanceFields, {
    emptyAllowance,
    sameAllowance,
    type TrafficAllowance,
} from '@/components/settings/TrafficAllowanceFields';
import { toast } from '@/components/ui/Toast';
import { getInfrastructureOverview } from '@/lib/api';
import {
    listTrafficLimits,
    setTrafficLimit,
    writeFor,
    TRAFFIC_REGION_ANY,
    TRAFFIC_KINDS,
    KIND_LABELS,
    KIND_HINTS,
    isRegionalKind,
    limitRegionFor,
    type TrafficLimit,
} from '@/lib/api/trafficLimits';
import DataThrottleCard from '@/components/settings/DataThrottleCard';

/**
 * The one platform-wide scope. There used to be a second, "global", asked after
 * this one - and for traffic it could answer nothing this one could not, since
 * every byte counted here belongs to a tenant. Two settings doing one job is a
 * screen where the number an operator typed stops applying the day somebody
 * fills in the other. Core folded the old rows into this scope on migration.
 */
const DEFAULT_SCOPE = 'user_default';

const cellKey = (region: string, kind: string) => `${limitRegionFor(region, kind)}|${kind}`;

function cellFrom(row: TrafficLimit | undefined): TrafficAllowance {
    if (!row) return emptyAllowance;
    return { set: true, includedGb: row.includedGb, maxPurchaseGb: row.maxPurchaseGb };
}

export default function TrafficLimitsTab() {
    const [rows, setRows] = useState<TrafficLimit[]>([]);
    const [liveRegions, setLiveRegions] = useState<string[]>([]);
    const [cells, setCells] = useState<Record<string, TrafficAllowance>>({});
    const [baseline, setBaseline] = useState<Record<string, TrafficAllowance>>({});
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    const [loadFailed, setLoadFailed] = useState(false);

    const load = useCallback(async () => {
        const res = await listTrafficLimits();
        if (!res.success) {
            setLoadFailed(true);
            setLoading(false);
            return;
        }
        setLoadFailed(false);
        setRows(res.limits || []);
        try {
            // The regions the edges actually REPORT. Without them the screen can
            // only show regions somebody already configured, which is exactly
            // the set that does not need attention - the gap is the regions
            // nobody has decided anything about.
            const infra = await getInfrastructureOverview();
            const found = new Set<string>();
            for (const e of (infra?.edges || [])) if (e.region) found.add(e.region);
            setLiveRegions([...found].sort());
        } catch {
            // A missing edge list is not a reason to hide the stored rows.
        }
        setLoading(false);
    }, []);

    useEffect(() => { void load(); }, [load]);

    const rowFor = useCallback(
        (region: string, kind: string) =>
            rows.find(r => r.scope === DEFAULT_SCOPE && r.region === limitRegionFor(region, kind) && r.kind === kind),
        [rows],
    );

    /**
     * Every region worth a row: what the edges report, plus anything already
     * configured. A region can be retired while its limit still exists, and the
     * limit has to stay visible - and has to keep applying if that region ever
     * comes back, which is why the row is never cleaned up on our side.
     */
    const regions = useMemo(() => {
        const s = new Set<string>(liveRegions);
        for (const r of rows) {
            if (r.scope === DEFAULT_SCOPE && isRegionalKind(r.kind)) s.add(r.region);
        }
        return [...s].sort();
    }, [rows, liveRegions]);

    // Rebuild the edit buffer from what is stored. This runs on load and after a
    // save, and it is safe in both because a save writes EVERY dirty cell before
    // reloading. It used to run after a per-cell save, which discarded whatever
    // the operator had typed into the cells they had not saved yet.
    useEffect(() => {
        const next: Record<string, TrafficAllowance> = {};
        for (const region of regions) {
            for (const kind of TRAFFIC_KINDS) {
                if (!isRegionalKind(kind)) continue;
                next[cellKey(region, kind)] = cellFrom(rowFor(region, kind));
            }
        }
        for (const kind of TRAFFIC_KINDS) {
            if (isRegionalKind(kind)) continue;
            next[cellKey(TRAFFIC_REGION_ANY, kind)] = cellFrom(rowFor(TRAFFIC_REGION_ANY, kind));
        }
        setCells(next);
        setBaseline(next);
    }, [regions, rowFor]);

    const update = (region: string, kind: string, patch: Partial<TrafficAllowance>) => {
        const k = cellKey(region, kind);
        setCells(c => ({ ...c, [k]: { ...c[k], ...patch } }));
    };

    const dirtyKeys = useMemo(
        () => Object.keys(cells).filter(k => !baseline[k] || !sameAllowance(cells[k], baseline[k])),
        [cells, baseline],
    );

    /**
     * One save for the whole screen.
     *
     * The card rule here is "one card is one save", and this card's payload is
     * every allowance on it. Splitting it per traffic kind is what produced the
     * defect this replaced: each box saved alone and then reloaded, so filling
     * in player traffic and file transfers and pressing one of the two buttons
     * silently threw the other away.
     */
    const save = async (): Promise<boolean> => {
        setSaving(true);
        let ok = true;
        for (const k of dirtyKeys) {
            const [region, kind] = k.split('|');
            const c = cells[k];
            const included = writeFor(c.set, c.includedGb);
            const purchase = writeFor(c.set, c.maxPurchaseGb);
            const res = await setTrafficLimit({
                scope: DEFAULT_SCOPE,
                region,
                kind,
                includedMode: included.mode,
                includedGb: included.gb,
                purchaseMode: purchase.mode,
                purchaseGb: purchase.gb,
            });
            if (!res.success) {
                ok = false;
                toast(res.message || `Could not save ${KIND_LABELS[kind] ?? kind} for ${region}.`, false);
                break;
            }
        }
        await load();
        setSaving(false);
        if (ok) toast(dirtyKeys.length === 1 ? 'Allowance saved' : `${dirtyKeys.length} allowances saved`);
        return ok;
    };

    const form = {
        dirty: dirtyKeys.length > 0,
        saving,
        loadFailed,
        save,
        discard: () => setCells(baseline),
    };

    const overrides = rows.filter(r => r.scope.startsWith('user:'));
    const nothingConfigured = rows.filter(r => r.scope === DEFAULT_SCOPE).length === 0;

    const cellEditor = (region: string, kind: string) => {
        const k = cellKey(region, kind);
        const c = cells[k];
        if (!c) return null;
        return (
            <TrafficAllowanceFields
                value={c}
                onChange={patch => update(region, kind, patch)}
                // Not a limit of zero, and the copy has to say so: the
                // difference between "nothing decided" and "decided, no limit"
                // is invisible in a number field.
                unsetNote="Nothing decided. Traffic here is not limited and nobody is stopped or billed for it."
            />
        );
    };

    return (
        <SettingsPage
            title="Traffic limits"
            description="What every tenant may use, and may buy on top. Player traffic and file transfers hold SEPARATE allowances: a tenant past one of them is not past the other."
            icon={Gauge}
            width="5xl"
            loading={loading}
            skeletonCards={2}
        >
            {nothingConfigured && (
                // Load-bearing, not decorative. The account-wide fallback that
                // used to stop people is gone: an allowance nobody has set does
                // not cap anyone and does not bill anyone.
                <div className="card p-4 flex items-start gap-3 border-(--warning)/40">
                    <AlertTriangle size={16} className="text-(--warning) mt-0.5 shrink-0" />
                    <div className="text-sm text-(--base-08)">
                        <p className="font-medium text-(--base-09)">No allowance is configured anywhere.</p>
                        <p className="text-xs text-(--base-06) mt-1">
                            Nothing is capped and nothing is billed until something below is set. There is no
                            account-wide fallback behind this screen.
                        </p>
                    </div>
                </div>
            )}

            <SettingsCard
                title="Allowances for all tenants"
                description="Per unit held: a tenant with a node and a route-only location gets each of these twice. A per-tenant override replaces the whole row, never half of it."
                icon={Gauge}
                form={form}
                loadFailedMessage="The stored allowances could not be read. Saving now would write the empty defaults on screen over them."
            >
                <div className="space-y-5">
                    <section className="space-y-3">
                        <div>
                            <h3 className="text-sm font-medium text-(--base-09)">Per region</h3>
                            <p className="text-xs text-(--base-06) mt-0.5 max-w-2xl">
                                Set per region because a terabyte does not cost the same everywhere, and because
                                a tenant may use their allowance in each region they route through. Each of
                                these is its own pool: being past one is not being past another.
                            </p>
                        </div>
                        {regions.length === 0 ? (
                            <p className="text-xs text-(--base-06) rounded-md border border-(--base-03) p-3">
                                No regions yet. One appears here as soon as an edge reports it, and a region that
                                later disappears keeps its allowance so it applies again if the region returns.
                            </p>
                        ) : (
                            <div className="space-y-3">
                                {regions.map(region => (
                                    <div key={region} className="rounded-md border border-(--base-03) p-3 space-y-3">
                                        <div className="flex flex-wrap items-center gap-2">
                                            <span className="mono-label text-(--accent-light)">{region}</span>
                                            {!liveRegions.includes(region) && (
                                                <span className="text-xs text-(--base-06)">
                                                    No edge is reporting this region right now. Its allowance still
                                                    applies to traffic recorded there, and again if it comes back.
                                                </span>
                                            )}
                                        </div>
                                        {TRAFFIC_KINDS.filter(isRegionalKind).map(kind => (
                                            <div key={kind} className="rounded-md border border-(--base-03)/60 p-3">
                                                <div className="text-sm font-medium text-(--base-09)">
                                                    {KIND_LABELS[kind] ?? kind}
                                                </div>
                                                <p className="text-xs text-(--base-06) mt-0.5 mb-2 max-w-2xl">
                                                    {KIND_HINTS[kind]}
                                                </p>
                                                {cellEditor(region, kind)}
                                            </div>
                                        ))}
                                    </div>
                                ))}
                            </div>
                        )}
                    </section>

                    <section className="space-y-3">
                        <div>
                            <h3 className="text-sm font-medium text-(--base-09) flex items-center gap-2">
                                <Globe size={14} className="text-(--base-06)" />
                                {KIND_LABELS.relay}
                            </h3>
                            <p className="text-xs text-(--base-06) mt-0.5 max-w-2xl">
                                {KIND_HINTS.relay} One allowance, not one per region: every relay is in
                                eu-central, so a per-region cap here would be a row per region answering a
                                question with a single possible answer.
                            </p>
                        </div>
                        <div className="rounded-md border border-(--base-03) p-3">
                            {cellEditor(TRAFFIC_REGION_ANY, 'relay')}
                        </div>
                    </section>
                </div>
            </SettingsCard>

            {overrides.length > 0 && (
                <SettingsCard
                    title="Per-tenant overrides"
                    description="Set on the tenant, shown here so none of them is invisible. An override answers on its own - it does not inherit the half it leaves empty."
                    icon={AlertTriangle}
                >
                    <div className="rounded-md border border-(--base-03) divide-y divide-(--base-03)">
                        {overrides.map(o => (
                            <div key={o.id} className="flex flex-wrap items-center gap-3 p-3 text-sm">
                                <code className="input-mono text-xs text-(--base-08)">{o.scope.slice(5)}</code>
                                <span className="mono-label text-(--accent-light)">
                                    {o.region === TRAFFIC_REGION_ANY ? 'all regions' : o.region}
                                </span>
                                <span className="mono-label text-(--base-06)">{KIND_LABELS[o.kind] ?? o.kind}</span>
                                <span className="text-(--base-07)">
                                    included {o.includedGb === null ? 'unlimited' : `${o.includedGb} GB`}
                                    {' · '}
                                    may buy {o.maxPurchaseGb === null ? 'unlimited' : `${o.maxPurchaseGb} GB`}
                                </span>
                                <button
                                    type="button"
                                    className="btn btn-secondary btn-sm ml-auto"
                                    onClick={async () => {
                                        const res = await setTrafficLimit({
                                            scope: o.scope,
                                            region: o.region,
                                            kind: o.kind,
                                            includedMode: 'default',
                                            purchaseMode: 'default',
                                        });
                                        if (res.success) { toast('Override removed'); await load(); }
                                        else toast(res.message || 'Could not remove the override.', false);
                                    }}
                                >
                                    <Trash2 size={13} /> Remove
                                </button>
                            </div>
                        ))}
                    </div>
                </SettingsCard>
            )}
            <DataThrottleCard />
        </SettingsPage>
    );
}
