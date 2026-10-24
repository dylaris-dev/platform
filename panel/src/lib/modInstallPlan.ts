// Turning Core's install plan (POST /servers/{id}/mods/plan) into what the
// Content tab does: which installs to start, in which order, and what to ask
// the reader before it starts them. No I/O here.

import type { InstallModPayload, ModInstallPlan, ModPlanProblem } from './api/modrinth';

// Plan answers that refuse the build or the caller: the install would be
// refused the same way, so there is nothing to offer instead. A 401 is the
// session, which the API layer already sends to the login.
const PLAN_REFUSED = new Set([400, 401, 403, 404, 409]);

/** Whether a failed plan ends the install rather than offering one without dependencies. */
export function planRefused(status?: number): boolean {
    return status !== undefined && PLAN_REFUSED.has(status);
}

/**
 * The installs to start, dependencies first and the requested mod LAST, so
 * nothing is queued on the node before what it needs. Core already orders the
 * plan that way; the root is taken out and re-added at the end regardless, so
 * it is never installed twice or ahead of a dependency.
 */
export function installQueue(plan: ModInstallPlan, root: InstallModPayload): InstallModPayload[] {
    const deps = plan.install
        .filter(e => e.versionId !== root.versionId && e.projectId !== root.projectId)
        .map(e => ({
            projectId: e.projectId,
            projectSlug: e.slug,
            versionId: e.versionId,
            title: e.title,
            fileName: e.fileName,
            downloadUrl: e.downloadUrl,
        }));
    return [...deps, root];
}

/**
 * What Update all does with one mod's plan: the installs to run, dependencies
 * first, or why the mod is left on its current build. There is nobody to ask
 * mid-run, so a build that would leave a dependency unresolved or clash with an
 * installed mod is not installed: the run must not leave a server that cannot
 * start.
 */
export function updateQueue(
    plan: ModInstallPlan, root: InstallModPayload, held: Map<string, string> = new Map(),
): { queue: InstallModPayload[] } | { skip: string } {
    const blockers = [...plan.unresolved, ...plan.incompatible];
    if (blockers.length) {
        return { skip: `the new build needs or clashes with ${blockers.map(p => p.title || p.reason).join(', ')}` };
    }
    const queue = installQueue(plan, root);
    // A dependency its members paused or switched off is not replaced behind
    // their back by another mod's update.
    const touched = queue.filter(p => p.projectId !== root.projectId && held.has(p.projectId));
    if (touched.length) {
        return { skip: `it would replace ${touched.map(p => held.get(p.projectId)).join(', ')}, which is paused or switched off` };
    }
    return { queue };
}

export interface PlanConfirm {
    title: string;
    message: string;
    confirmLabel: string;
    destructive: boolean;
}

const problem = (p: ModPlanProblem) => (p.title ? `${p.title}: ${p.reason}` : p.reason);

/**
 * The one confirm before an install, or null when there is nothing to say:
 * no extra mods and no warnings means it installs straight away, the way the
 * Modrinth app does.
 */
export function planConfirm(plan: ModInstallPlan, root: InstallModPayload): PlanConfirm | null {
    const extras = plan.install
        .filter(e => e.versionId !== root.versionId && e.projectId !== root.projectId)
        .map(e => (e.versionNumber ? `${e.title} ${e.versionNumber}` : e.title));
    if (!extras.length && !plan.unresolved.length && !plan.incompatible.length) return null;
    const parts: string[] = [];
    if (extras.length) parts.push(`Also installs: ${extras.join(', ')}.`);
    if (plan.unresolved.length) {
        parts.push(`Not installed, so the server may not start without them:\n${plan.unresolved.map(problem).join('\n')}`);
    }
    if (plan.incompatible.length) {
        parts.push(`Clashes with mods already installed:\n${plan.incompatible.map(problem).join('\n')}`);
    }
    const warn = plan.unresolved.length > 0 || plan.incompatible.length > 0;
    return {
        title: extras.length
            ? `Install "${root.title}" with ${extras.length} ${extras.length === 1 ? 'dependency' : 'dependencies'}?`
            : `Install "${root.title}"?`,
        message: parts.join('\n\n'),
        confirmLabel: warn ? 'Install anyway' : `Install ${extras.length + 1} mods`,
        destructive: warn,
    };
}
