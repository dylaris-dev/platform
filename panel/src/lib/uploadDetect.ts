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
    const read = async (start: number, end: number) => new DataView(await file.slice(start, end).arrayBuffer());
    const tailStart = Math.max(0, file.size - 65557);
    const tail = await read(tailStart, file.size);
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
        const rec = await read(recOffset, recOffset + 56);
        if (rec.byteLength < 56 || rec.getUint32(0, true) !== 0x06064b50) return null;
        cdSize = Number(rec.getBigUint64(40, true));
        cdOffset = Number(rec.getBigUint64(48, true));
    }
    // ponytail: a directory over 64 MB (~500k entries) is not read; the form then just isn't prefilled.
    if (cdSize > 64 << 20 || cdOffset + cdSize > file.size) return null;
    const cd = await read(cdOffset, cdOffset + cdSize);
    const decoder = new TextDecoder();
    const names: string[] = [];
    for (let p = 0; p + 46 <= cd.byteLength && cd.getUint32(p, true) === 0x02014b50;) {
        const n = cd.getUint16(p + 28, true);
        const extra = cd.getUint16(p + 30, true);
        const comment = cd.getUint16(p + 32, true);
        names.push(decoder.decode(new Uint8Array(cd.buffer, cd.byteOffset + p + 46, n)));
        p += 46 + n + extra + comment;
    }
    return names;
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
