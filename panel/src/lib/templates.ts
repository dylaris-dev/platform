// Pure helpers for the Templates module.

import type { Template, TemplateFile } from '@/lib/api/templates';
import type { FileEntry } from '@dylaris/ui-filebrowser';
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

/** "12.0 MB of 512 MB", or "12.0 MB, no limit" when there is none (null). */
export function formatTemplateSize(bytes: number, maxBytes: number | null): string {
    const size = `${(bytes / MiB).toFixed(1)} MB`;
    if (maxBytes === null) return `${size}, no limit`;
    const max = maxBytes / MiB;
    return `${size} of ${Number.isInteger(max) ? max : max.toFixed(1)} MB`;
}

/**
 * One template's size cap as Core reports it on the item (admin cap, user cap,
 * or null on the owner's own storage). An older Core sends none; then the
 * list-wide cap is the best there is.
 */
export function templateCap(t: { maxSizeBytes?: number | null }, fallback: number | null): number | null {
    return t.maxSizeBytes === undefined ? fallback : t.maxSizeBytes;
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

/** The software a template can install: Core's services.TemplateSoftware,
 *  which is what the node installs. Change both together. */
export const TEMPLATE_SOFTWARE = ['paper', 'vanilla', 'fabric', 'forge', 'neoforge', 'velocity', 'waterfall', 'bungeecord'] as const;

/** What a template's softwareVersion means for its software, or null where it
 *  has none: Paper and vanilla install the newest build of the Minecraft
 *  version, so a build number there would only be mistaken for one. */
export function softwareVersionField(software: string): { label: string; placeholder: string; required: boolean } | null {
    switch (software.trim().toLowerCase()) {
        case 'forge': return { label: 'Forge build', placeholder: '47.3.0, empty = recommended', required: false };
        case 'fabric': return { label: 'Fabric loader', placeholder: '0.16.10, empty = newest', required: false };
        case 'neoforge': return { label: 'NeoForge version', placeholder: '21.1.265 (for 1.21.1)', required: true };
        case 'velocity': return { label: 'Velocity version', placeholder: '3.4.0', required: true };
        case 'waterfall': return { label: 'Waterfall version', placeholder: '1.21', required: true };
        case 'bungeecord': return { label: 'BungeeCord build', placeholder: '#2104', required: true };
        case 'paper': case 'vanilla': return null;
        default: return { label: 'Software version', placeholder: '', required: false };
    }
}

/** Whether the Minecraft version field applies: proxies have none. */
export function usesMinecraftVersion(software: string): boolean {
    return !['velocity', 'waterfall', 'bungeecord'].includes(software.trim().toLowerCase());
}

/** Top-level entries a capture leaves unticked at first: logs, crash reports
 *  and the name cache hold player names and IP addresses, and a template can
 *  be shared. They can still be ticked; Core then warns. */
export function captureExcludedByDefault(name: string, isDir: boolean): boolean {
    if (isDir) return name === 'logs' || name === 'crash-reports';
    return name === 'usercache.json' || /\.log(\.[0-9]+)?(\.gz)?$/.test(name);
}

/** Paths one capture may name: Core's maxCaptureSelected in
 *  core/handlers/templates/templates_capture.go. Change both together. */
export const MAX_CAPTURE_PATHS = 500;

// Core's defaultTargetDirForLoader (core/handlers/servers/server_mods.go): these load plugins/, the rest mods/.
const PLUGIN_SOFTWARE = ['paper', 'spigot', 'bukkit', 'purpur', 'pufferfish', 'velocity', 'waterfall', 'bungeecord'];

/** The Modrinth search filter for a template's software, or null when it loads
 *  no mods or plugins (vanilla, or not set). Software is free text, so it is
 *  normalised the way Core reads it. */
export function templateModrinthTarget(software: string): { loader: string; projectType: 'mod' | 'plugin'; dir: 'mods' | 'plugins' } | null {
    const s = software.trim().toLowerCase();
    if (!s || s === 'vanilla') return null;
    // Pufferfish is not a Modrinth loader and many Paper plugins do not list purpur; both search as paper.
    if (PLUGIN_SOFTWARE.includes(s)) return { loader: s === 'pufferfish' || s === 'purpur' ? 'paper' : s, projectType: 'plugin', dir: 'plugins' };
    return { loader: s, projectType: 'mod', dir: 'mods' };
}

/** The entries directly inside prefix of a template's flat file list: a folder
 *  exists while it holds a file. A Modrinth reference has no stored bytes, so it
 *  is tagged and the browser offers it for deletion only. */
export function templateFolderEntries(files: TemplateFile[], prefix: string): FileEntry[] {
    const names = new Map<string, FileEntry>();
    for (const f of files) {
        if (prefix && !f.name.startsWith(prefix + '/')) continue;
        const rest = prefix ? f.name.slice(prefix.length + 1) : f.name;
        const name = rest.split('/')[0];
        if (!name) continue;
        const isDir = rest.includes('/');
        const prev = names.get(name);
        const entry: FileEntry = { name, is_dir: isDir, size: (prev?.size ?? 0) + f.size };
        if (!isDir && f.modrinth) entry.tag = 'Modrinth';
        names.set(name, entry);
    }
    return [...names.values()];
}
