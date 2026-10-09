import { compareVersionsDesc } from '@/views/setup/VersionPicker';

/**
 * Reading what server an uploaded archive holds, so the setup form can prefill
 * the server software and version it then installs over the upload.
 *
 * An upload used to be extracted and started as-is. A Paper server keeps its
 * jar under versions/, not in the top folder, so such an upload installed
 * "successfully" and then failed every start with "no runnable server found".
 */

/**
 * The entry names of a zip, read from its central directory only: the tail of
 * the file and the directory itself, never the whole archive (uploads run to
 * gigabytes). Null when the file is not a zip this can read.
 */
export async function readZipEntryNames(file: Blob): Promise<string[] | null> {
    const entries = await readZipEntries(file);
    return entries ? entries.map(e => e.name) : null;
}

interface ZipEntry { name: string; method: number; compSize: number; size: number; offset: number }

const read = async (file: Blob, start: number, end: number) => new DataView(await file.slice(start, end).arrayBuffer());

async function readZipEntries(file: Blob): Promise<ZipEntry[] | null> {
    const tailStart = Math.max(0, file.size - 65557);
    const tail = await read(file, tailStart, file.size);
    let eocd = -1;
    for (let i = tail.byteLength - 22; i >= 0; i--) {
        if (tail.getUint32(i, true) === 0x06054b50) { eocd = i; break; }
    }
    if (eocd < 0) return null;
    let cdSize = tail.getUint32(eocd + 12, true);
    let cdOffset = tail.getUint32(eocd + 16, true);
    if (cdSize === 0xffffffff || cdOffset === 0xffffffff || tail.getUint16(eocd + 10, true) === 0xffff) {
        // ZIP64: the locator sits right before the classic end record.
        const loc = eocd - 20;
        if (loc < 0 || tail.getUint32(loc, true) !== 0x07064b50) return null;
        const recOffset = Number(tail.getBigUint64(loc + 8, true));
        const rec = await read(file, recOffset, recOffset + 56);
        if (rec.byteLength < 56 || rec.getUint32(0, true) !== 0x06064b50) return null;
        cdSize = Number(rec.getBigUint64(40, true));
        cdOffset = Number(rec.getBigUint64(48, true));
    }
    // ponytail: a directory over 64 MB (~500k entries) is not read; the form then just isn't prefilled.
    if (cdSize > 64 << 20 || cdOffset + cdSize > file.size) return null;
    const cd = await read(file, cdOffset, cdOffset + cdSize);
    const decoder = new TextDecoder();
    const entries: ZipEntry[] = [];
    for (let p = 0; p + 46 <= cd.byteLength && cd.getUint32(p, true) === 0x02014b50;) {
        const n = cd.getUint16(p + 28, true);
        const extra = cd.getUint16(p + 30, true);
        const comment = cd.getUint16(p + 32, true);
        entries.push({
            name: decoder.decode(new Uint8Array(cd.buffer, cd.byteOffset + p + 46, n)),
            method: cd.getUint16(p + 10, true),
            compSize: cd.getUint32(p + 20, true),
            size: cd.getUint32(p + 24, true),
            offset: cd.getUint32(p + 42, true),
        });
        p += 46 + n + extra + comment;
    }
    return entries;
}

/**
 * One small text entry of a zip, by exact name: stored or deflated, at most
 * 4 MB. Null for anything else.
 */
async function readZipText(file: Blob, entries: ZipEntry[], name: string): Promise<string | null> {
    const e = entries.find(x => x.name === name);
    // ponytail: ZIP64 sizes/offsets (0xffffffff) are not followed; such a pack is just not read.
    if (!e || e.size > 4 << 20 || e.compSize === 0xffffffff || e.offset === 0xffffffff) return null;
    const local = await read(file, e.offset, e.offset + 30);
    if (local.byteLength < 30 || local.getUint32(0, true) !== 0x04034b50) return null;
    const start = e.offset + 30 + local.getUint16(26, true) + local.getUint16(28, true);
    const raw = file.slice(start, start + e.compSize);
    if (e.method === 0) return raw.text();
    if (e.method !== 8 || typeof DecompressionStream === 'undefined') return null;
    return new Response(raw.stream().pipeThrough(new DecompressionStream('deflate-raw'))).text();
}

/**
 * The Minecraft version a server pack declares: CurseForge's manifest.json,
 * else ServerPackCreator's variables.txt. Read so the setup form recommends
 * the pack's Java, not the one of the server it replaces.
 */
export async function readPackMcVersion(file: Blob): Promise<string | undefined> {
    try {
        const entries = await readZipEntries(file);
        if (!entries) return undefined;
        // A pack zipped as its folder is moved up by the node; read it there too.
        const names = entries.map(e => e.name);
        const top = singleTopFolder(names) ? withoutMacJunk(names)[0].split('/')[0] + '/' : '';
        const manifest = await readZipText(file, entries, top + 'manifest.json');
        try {
            const v = manifest && JSON.parse(manifest)?.minecraft?.version;
            if (typeof v === 'string' && /^\d[\w.-]{0,31}$/.test(v)) return v;
        } catch {
            // not the CurseForge shape; variables.txt may still say it
        }
        const vars = await readZipText(file, entries, top + 'variables.txt');
        const m = vars && /^\s*MINECRAFT_VERSION\s*=\s*["']?([\d][\w.-]{0,31})/m.exec(vars);
        if (m) return m[1];
    } catch {
        // an unreadable pack only means no recommendation
    }
    return undefined;
}

/**
 * Every entry sits under one top folder: what a zipped server folder looks
 * like. Extracted as it is, the server would land one level down.
 */
export function singleTopFolder(names: string[]): boolean {
    const clean = withoutMacJunk(names);
    return clean.length > 0 && clean.every(n => n.includes('/')) && new Set(clean.map(n => n.split('/')[0])).size === 1;
}

// What macOS adds to a zip it makes. The node ignores it the same way when it
// decides which folder to move up (node/installer.go uploadMacJunk).
function withoutMacJunk(names: string[]): string[] {
    return names
        .map(n => n.replace(/\\/g, '/').replace(/^\.\//, ''))
        .filter(n => n && !/^(__MACOSX|\.DS_Store)(\/|$)/.test(n));
}

export interface UploadDetection {
    /** paper | vanilla | fabric | forge | neoforge, when recognised. */
    software?: string;
    /** The version picker's build value: the MC version, or NeoForge's own version. */
    build?: string;
    /** The upload starts as it is: a server jar in its top folder, or a Forge/NeoForge argfile. */
    launchable: boolean;
    /**
     * A modpack server pack (CurseForge, ServerPackCreator): mods plus a file
     * declaring its loader, which the node installs (node/installer_serverpack.go).
     */
    serverPack?: boolean;
    /** The Minecraft version the upload runs, where it says so. */
    mcVersion?: string;
}

const highest = (vs: string[]) => [...vs].sort(compareVersionsDesc)[0];

function captures(names: string[], re: RegExp): string[] {
    const out: string[] = [];
    for (const n of names) {
        const m = re.exec(n);
        if (m) out.push(m[1]);
    }
    return out;
}

/**
 * Mirrors the node's start resolution (node/launch.go) for `launchable`, and
 * reads the version from the files each loader leaves behind.
 */
export function detectUploadSoftware(rawNames: string[], subfolder = false): UploadDetection {
    let names = withoutMacJunk(rawNames);
    // A dropped folder ("subfolder") arrives as one top folder, which the node
    // moves up; a zip is extracted as it is.
    const tops = new Set(names.map(n => n.split('/')[0]));
    if (subfolder && tops.size === 1) {
        const prefix = [...tops][0] + '/';
        names = names.map(n => n.startsWith(prefix) ? n.slice(prefix.length) : n).filter(Boolean);
    }
    const root = names.filter(n => !n.includes('/'));

    const launchable =
        // Case-sensitive like the node's globs on Linux: "Server.jar" does not start.
        root.some(n => /^(forge-.+|neoforge-.+|paper-.+|purpur-.+)\.jar$/.test(n) && !/-installer\.jar$/.test(n)) ||
        root.some(n => /^(fabric-server-launch|server)\.jar$/.test(n)) ||
        names.some(n => /^libraries\/net\/(minecraftforge\/forge|neoforged\/neoforge)\/[^/]+\/unix_args\.txt$/.test(n));

    const serverPack = names.some(n => n.startsWith('mods/')) &&
        root.some(n => /^(variables\.txt|manifest\.json|startserver\.sh|start\.sh|run\.sh|.+-installer\.jar)$/.test(n));
    if (serverPack && !launchable) return { launchable, serverPack };

    const neo = captures(names, /^libraries\/net\/neoforged\/neoforge\/([^/]+)\//);
    if (neo.length) return { software: 'neoforge', build: highest(neo), launchable };

    const forge = [
        ...captures(names, /^libraries\/net\/minecraftforge\/forge\/(\d[^/-]*)-[^/]+\//),
        ...captures(root, /^forge-(\d[^-]*)-.+\.jar$/i),
    ];
    if (forge.length) return { software: 'forge', build: highest(forge), launchable };

    const vanillaVersions = captures(names, /^versions\/([^/]+)\/server-[^/]+\.jar$/);
    if (root.includes('fabric-server-launch.jar') || names.some(n => n.startsWith('.fabric/') || n.startsWith('libraries/net/fabricmc/'))) {
        const mc = [...captures(names, /^libraries\/net\/fabricmc\/intermediary\/([^/]+)\//), ...vanillaVersions];
        return { software: 'fabric', build: mc.length ? highest(mc) : undefined, launchable };
    }

    const paper = [
        ...captures(names, /^versions\/([^/]+)\/paper-[^/]+\.jar$/),
        ...captures(root, /^paper-(\d[^-]*?)(?:-\d+)?\.jar$/i),
    ];
    if (paper.length) return { software: 'paper', build: highest(paper), launchable };
    if (names.some(n => n.startsWith('libraries/io/papermc/'))) {
        const mc = captures(names, /^cache\/mojang_([^/]+)\.jar$/);
        return { software: 'paper', build: mc.length ? highest(mc) : undefined, launchable };
    }

    if (vanillaVersions.length) return { software: 'vanilla', build: highest(vanillaVersions), launchable };
    return { launchable };
}
