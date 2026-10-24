import type { UpdateComponent, UpdateInstance, Release, ReleaseEntry, ReleaseRequired } from '@/lib/api/updates';
import { fmtDateTime } from './time';

// Presentation helpers for the updates view. Pure functions, so the grouping
// rules can be tested without rendering anything.

// One version, and how many of a component's instances are on it. This is what
// turns "you have three nodes" into "two are current, one is not" - the shape a
// single fleet-wide number could never express, and the reason node versions are
// carried per node.
export interface VersionGroup {
    version: string;   // '' means the instances did not report one
    count: number;
    total: number;
    outdated: boolean;
    labels: string[];
}

// groupInstances collapses a component's instances by version.
//
// Ordering is deliberate: outdated groups first, then unknown, then current.
// The reason to open this view is to find what needs doing, so what needs doing
// goes at the top rather than wherever the fleet happens to sort.
export function groupInstances(instances: UpdateInstance[]): VersionGroup[] {
    const total = instances.length;
    const byVersion = new Map<string, VersionGroup>();

    for (const inst of instances) {
        const version = inst.version ?? '';
        const g = byVersion.get(version);
        if (g) {
            g.count += 1;
            g.labels.push(inst.label);
            // An instance is outdated on its own merits; the group inherits it
            // if any member has it. In practice every member of a version group
            // agrees, but deriving it rather than assuming keeps the display
            // honest if they ever do not.
            g.outdated = g.outdated || inst.outdated;
        } else {
            byVersion.set(version, {
                version,
                count: 1,
                total,
                outdated: inst.outdated,
                labels: [inst.label],
            });
        }
    }

    const rank = (g: VersionGroup) => (g.outdated ? 0 : g.version === '' ? 1 : 2);
    return [...byVersion.values()].sort((a, b) => {
        const r = rank(a) - rank(b);
        if (r !== 0) return r;
        // Newest first within a rank. Versions are CalVer with an optional
        // same-day counter, so a plain string compare gets .10 vs .2 wrong.
        return compareVersions(b.version, a.version);
    });
}

// compareVersions orders CalVer versions numerically per part, so 2026.08.28.10
// sorts after 2026.08.28.2. An empty version sorts lowest, which is only ever
// used for a stable order and never to call something outdated.
export function compareVersions(a: string, b: string): number {
    if (a === b) return 0;
    if (!a) return -1;
    if (!b) return 1;
    const pa = a.split('.').map(n => parseInt(n, 10) || 0);
    const pb = b.split('.').map(n => parseInt(n, 10) || 0);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
        const d = (pa[i] ?? 0) - (pb[i] ?? 0);
        if (d !== 0) return d < 0 ? -1 : 1;
    }
    return 0;
}

const SERVICE_LABELS: Record<string, string> = {
    core: 'Core',
    panel: 'Panel',
    node: 'Nodes',
    'log-shipper': 'Log shipper',
    edge: 'Edge',
    link: 'Link',
    hub: 'Hub',
    warp: 'Warp',
};

export function serviceLabel(service: string): string {
    return SERVICE_LABELS[service] ?? service;
}

// anythingOutdated is what decides whether the button advertises work. It reads
// the components rather than comparing the newest release to what was seen,
// because those answer different questions: "something was published" and
// "something of yours is behind" are not the same, and only the second is
// actionable.
export function anythingOutdated(components: UpdateComponent[]): boolean {
    return components.some(c => c.outdated);
}

export type BellState = 'attention' | 'unseen' | 'idle';

// bellState decides how loud the trigger is.
//
//   attention  something of yours is behind, or a deadline applies
//   unseen     nothing to do, but a release you have not acknowledged exists
//   idle       nothing to say
//
// The two are separate on purpose. A release that does not touch anything you
// run is worth reading and not worth alarming you about, and collapsing the two
// is how a notification badge becomes something people learn to ignore.
export function bellState(opts: {
    outdated: boolean;
    required: boolean;
    latest?: string;
    seen?: string;
}): BellState {
    if (opts.outdated || opts.required) return 'attention';
    if (opts.latest && opts.latest !== opts.seen) return 'unseen';
    return 'idle';
}

export interface Category {
    key: 'breaking' | 'security' | 'features' | 'fixes';
    label: string;
    entries: ReleaseEntry[];
}

// categories returns all four, always, in the order that matters to a reader:
// what forces action, what affects safety, what is new, what was repaired.
//
// The FILE stores them Features/Breaking/Security/Fixes because that is the
// order they are written in; the reader is served by a different one, and an
// empty category is still shown so "nothing this time" is visible rather than
// inferred from an absence.
export function categories(r: Release): Category[] {
    return [
        { key: 'breaking', label: 'Breaking', entries: r.breaking ?? [] },
        { key: 'security', label: 'Security', entries: r.security ?? [] },
        { key: 'features', label: 'Features', entries: r.features ?? [] },
        { key: 'fixes', label: 'Fixes', entries: r.fixes ?? [] },
    ];
}

// MergedReleases is several releases read as one.
//
// The bell used to draw a block per version, stacked. With four releases in a
// day that is four headings, sixteen category labels and a dozen "Nothing this
// time." lines between the reader and the two entries that matter. Nobody reads
// a changelog to find out how it was cut into blocks - they read it to find out
// what changed since they last looked, which is one question with one answer.
export interface MergedReleases {
    /** "2026.08.30.4" for one, "2026.08.30.1 - 2026.08.30.4" for several. */
    range: string;
    /** How many releases were folded in. 1 renders as a plain version. */
    count: number;
    /** The most urgent mandatory-update line among them, if any. */
    required: ReleaseRequired | null;
    categories: Category[];
}

// mergeReleases folds a list into one block, newest first within each category.
//
// The list arrives newest-first, which is the order the entries keep: within
// Breaking, the newest release's breaking changes are read before an older
// one's. Duplicates are NOT collapsed - two releases fixing "the same" thing
// wrote two sentences, and deciding they are the same sentence is a judgement
// this cannot make from the text.
export function mergeReleases(releases: Release[]): MergedReleases | null {
    if (releases.length === 0) return null;

    const versions = releases.map(r => r.version).filter(Boolean);
    // The list is newest-first, so the range reads oldest to newest - the
    // direction a person reads a span, not the direction the array is in.
    const range = versions.length > 1
        ? `${versions[versions.length - 1]} - ${versions[0]}`
        : (versions[0] ?? '');

    // The requirement that binds is the most urgent one: an immediate beats any
    // date, and otherwise the EARLIEST deadline wins. A reader shown the latest
    // would plan for a date another release has already passed.
    let required: ReleaseRequired | null = null;
    for (const r of releases) {
        if (!r.required) continue;
        if (!required) { required = r.required; continue; }
        if (required.immediate) continue;
        if (r.required.immediate || r.required.deadline < required.deadline) required = r.required;
    }

    const merged: Category[] = categories(releases[0]).map(c => ({ ...c, entries: [] }));
    for (const r of releases) {
        categories(r).forEach((c, i) => { merged[i].entries.push(...c.entries); });
    }
    return { range, count: releases.length, required, categories: merged };
}

// formatDeadline renders a mandatory-update deadline in the reader's own zone.
// A UTC timestamp in a warning about when their server stops working is exactly
// the wrong place to make somebody do arithmetic.
export function formatDeadline(iso: string): string {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    return fmtDateTime(d, {
        year: 'numeric', month: 'short', day: 'numeric',
        hour: '2-digit', minute: '2-digit',
    });
}

// releasesSince narrows the list to what the reader has NOT acknowledged.
//
// The bell used to render everything Core sent - twenty releases - regardless of
// the seen marker sitting right beside them in the same response. An operator
// who was fully current still opened it to a span reaching back days. The block
// that rendered them already said "everything the reader has not seen"; that was
// the intent, written as though it were the implementation.
//
// An unknown marker shows EVERYTHING rather than nothing. The two failure
// directions are not equal: one release too many costs a moment, one too few
// costs the release - and a marker naming a version that has aged out of the
// window is exactly the case where guessing wrong is easy.
export function releasesSince(releases: Release[], seen?: string): Release[] {
    if (!seen) return releases;
    const at = releases.findIndex(r => r.version === seen);
    if (at < 0) return releases;
    return releases.slice(0, at);
}

// mandatoryApplies decides whether the red badge is earned.
//
// It reads what Core computed per COMPONENT, not what the release notes carry. A
// deadline in the window means someone once had to act; a `required` entry means
// something this reader runs is below the floor right now. Deriving the badge
// from the notes kept it lit for people who had already updated - which is how a
// red badge stops meaning anything.
export function mandatoryApplies(required: { service: string }[] | undefined): boolean {
    return (required?.length ?? 0) > 0;
}
