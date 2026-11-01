// Pure helpers for the Templates module.

import type { Template } from '@/lib/api/templates';
import type { FeatureFlags } from '@/lib/api/featureFlags';

export interface TemplateGroups {
    mine: Template[];
    shared: Template[];
    admin: Template[];
    platform: Template[];
}

/** Splits a list into the four groups the Templates page shows. Admin and
 *  platform templates belong to the platform, whoever created them. */
export function groupTemplates(list: Template[], myUserId: string): TemplateGroups {
    const g: TemplateGroups = { mine: [], shared: [], admin: [], platform: [] };
    for (const t of list) {
        if (t.visibility === 'platform') g.platform.push(t);
        else if (t.visibility === 'admin') g.admin.push(t);
        else if (t.ownerId === myUserId) g.mine.push(t);
        else g.shared.push(t);
    }
    return g;
}

const MiB = 1024 * 1024;

/** "12.0 MB of 512 MB", or "12.0 MB" when there is no limit (null). */
export function formatTemplateSize(bytes: number, maxBytes: number | null): string {
    const size = `${(bytes / MiB).toFixed(1)} MB`;
    if (maxBytes === null) return size;
    const max = maxBytes / MiB;
    return `${size} of ${Number.isInteger(max) ? max : max.toFixed(1)} MB`;
}

/** Mirrors Core's RequireTemplatesAudience: admins always pass, everyone else
 *  only when the audience is "all". An older Core sends no audience at all. */
export function templatesVisible(audience: FeatureFlags['templates'], isAdmin: boolean): boolean {
    return isAdmin || audience === 'all';
}

/** The setup installer for a template. Core refuses any other field beside
 *  these two (wipePaths aside), so nothing else may ride along. */
export function templateInstallPayload(templateId: number): { type: 'template'; templateId: number } {
    return { type: 'template', templateId };
}

/** A Templates limit setting as LimitField holds it: "unlimited" is null, a
 *  number is the cap (0 = none), anything else the product default. */
export function limitFromSetting(raw: string | undefined, productDefault: number): number | null {
    if (raw === 'unlimited') return null;
    return raw !== undefined && /^[0-9]+$/.test(raw) ? Number(raw) : productDefault;
}

export function limitToSetting(v: number | null): string {
    return v === null ? 'unlimited' : String(v);
}

/** What a template's softwareVersion means for its software. Fabric and Forge
 *  record a loader there only when it differs from the Minecraft version. */
export function softwareVersionLabel(software: string): string {
    if (software === 'fabric' || software === 'forge') return 'Loader version';
    if (software === 'neoforge') return 'NeoForge version';
    return 'Software build';
}

/** Paths one capture may name: Core's maxCaptureSelected in
 *  core/handlers/templates/templates_capture.go. Change both together. */
export const MAX_CAPTURE_PATHS = 500;
