// Java runtime selection for a Minecraft version.
//
// Pure logic, kept out of JavaVersionPicker.tsx so the vitest suite (src/lib
// only, no DOM stack) can cover it. The picker re-exports these so existing
// imports keep working.

export interface JavaImage {
    id: string;
    label: string;
    /** Version range shown under the label. */
    note: string;
    proxyNote: string;
}

export const JAVA_IMAGES: JavaImage[] = [
    { id: 'ghcr.io/dylaris-dev/platform-mc-java25:latest', label: 'Java 25', note: '26.1+', proxyNote: 'Velocity 4+' },
    { id: 'ghcr.io/dylaris-dev/platform-mc-java21:latest', label: 'Java 21', note: '1.20.5 - 26.0', proxyNote: 'Velocity 3, BungeeCord, Waterfall' },
    { id: 'ghcr.io/dylaris-dev/platform-mc-java17:latest', label: 'Java 17', note: '1.18+', proxyNote: 'Minimum for BungeeCord' },
    { id: 'ghcr.io/dylaris-dev/platform-mc-java8:latest', label: 'Java 8', note: '1.7 - 1.16', proxyNote: 'Too old for current proxies' },
];

export const JAVA_25 = JAVA_IMAGES[0].id;
export const JAVA_21 = JAVA_IMAGES[1].id;
export const JAVA_17 = JAVA_IMAGES[2].id;
export const JAVA_8 = JAVA_IMAGES[3].id;

/**
 * Returns the Java image a Minecraft version needs, or null if the string is not
 * a version we recognise.
 *
 * Two numbering schemes have to be told apart, and getting that wrong is what
 * shipped: everything up to 1.21.x is "1.MINOR.PATCH", where the leading 1 is
 * decoration and MINOR carries the meaning, while 26.x onwards is
 * "MAJOR.MINOR". The old code read parts[1] unconditionally, so "26.2" was
 * judged as minor=2 and fell through every branch to null - no recommendation at
 * all for exactly the versions the picker lists first.
 *
 * Minecraft 26.1 refuses to boot on anything below Java 25 and says so:
 * "Minecraft 26.1 and newer requires running the server with Java 25 or above."
 */
export function recommendJavaForVersion(version: string): string | null {
    const parts = version.split('.').map(Number);
    if (parts.some((n) => !Number.isFinite(n)) || parts.length === 0) return null;

    const first = parts[0];

    // New scheme: 26.1 and up need Java 25, 26.0 is still Java 21.
    if (first >= 26) {
        if (first > 26 || (parts[1] ?? 0) >= 1) return JAVA_25;
        return JAVA_21;
    }
    // Anything between the two schemes is not a real Minecraft version.
    if (first !== 1) return null;

    const minor = parts[1] ?? 0;
    const patch = parts[2] ?? 0;
    if (minor >= 21 || (minor === 20 && patch >= 5)) return JAVA_21;
    if (minor >= 18) return JAVA_17;
    // 1.7, not 1.8: the version picker offers 1.7 and 1.7.10 runs on Java 8, so
    // starting the range at 1.8 left the oldest selectable version with no
    // recommendation - the same gap as the 26.x one, at the other end.
    if (minor >= 7) return JAVA_8;
    return null;
}

/**
 * Returns the Java image a proxy release needs, or null for software that is
 * not a proxy or a release we cannot read.
 *
 * A proxy's version is its own, not a Minecraft version, so
 * recommendJavaForVersion read "4.2.0" as nothing and the form kept Java 21,
 * on which Velocity 4 does not start. The minimums are PaperMC's own
 * (java.version.minimum in the Fill API: Velocity 4 needs 25, 3.5 needs 21)
 * and BungeeCord's build (Java 17). Java 21 covers every 3.x Velocity, every
 * BungeeCord build and Waterfall.
 */
export function recommendJavaForProxy(software: string, build: string): string | null {
    switch (software) {
        case 'velocity': {
            const major = Number(build.split('.')[0]);
            if (!build || !Number.isInteger(major)) return null;
            return major >= 4 ? JAVA_25 : JAVA_21;
        }
        case 'waterfall':
        case 'bungeecord':
            return JAVA_21;
        default:
            return null;
    }
}

/**
 * The Java recommendation for the setup form. A proxy reads its release from
 * the version pickers, which only describe the install on the tabs that use
 * them; an upload says nothing about the proxy inside it.
 */
export function recommendJavaForSetup(i: { isProxy: boolean; tab: string; software: string; build: string; mcVersion: string }): string | null {
    if (!i.isProxy) return recommendJavaForVersion(i.mcVersion);
    if (i.tab !== 'online' && i.tab !== 'library') return null;
    return recommendJavaForProxy(i.software, i.build);
}

/**
 * For Paper/Vanilla the "build" is a patch version like "1.20.11" - more precise
 * than the major "1.20" and worth using for the Java recommendation. For
 * Fabric/Forge the build is a loader version ("0.15.0", "47.2.0") that does not
 * start with the major, so fall back to the major there.
 */
export function effectiveMcVersion(major: string, build: string): string {
    if (build && (build === major || build.startsWith(major + '.'))) return build;
    return major;
}
