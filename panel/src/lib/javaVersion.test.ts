import { describe, it, expect } from 'vitest';
import { recommendJavaForVersion, recommendJavaForProxy, recommendJavaForSetup, effectiveMcVersion, JAVA_25, JAVA_21, JAVA_17, JAVA_8, JAVA_IMAGES } from './javaVersion';

describe('recommendJavaForVersion', () => {
    // The bug this exists for: the old parser read parts[1] unconditionally, so
    // "26.2" was judged as minor=2, fell through every branch and returned null.
    // The picker lists 26.x first, so the newest selectable version got no
    // recommendation and installed onto Java 21, where Paper refuses to start:
    // "Minecraft 26.1 and newer requires running the server with Java 25 or
    // above." Verified live - the container crash-looped on exit 1.
    it.each([
        ['26.1', JAVA_25],
        ['26.2', JAVA_25],
        ['27.0', JAVA_25],
        // 26.0 predates the Java 25 requirement, which starts at 26.1.
        ['26.0', JAVA_21],
    ])('new scheme %s -> %s', (v, want) => {
        expect(recommendJavaForVersion(v)).toBe(want);
    });

    it.each([
        ['1.21', JAVA_21],
        ['1.21.4', JAVA_21],
        ['1.20.5', JAVA_21],
        ['1.20.4', JAVA_17],
        ['1.18', JAVA_17],
        ['1.17', JAVA_8],
        ['1.16.5', JAVA_8],
        ['1.8', JAVA_8],
        // The picker offers 1.7 and 1.7.10 runs on Java 8; starting the range at
        // 1.8 left the oldest selectable version unmapped.
        ['1.7', JAVA_8],
        ['1.7.10', JAVA_8],
    ])('old scheme %s -> %s', (v, want) => {
        expect(recommendJavaForVersion(v)).toBe(want);
    });

    it.each(['', 'latest', 'snapshot-23w45a', '1.6', '2.0', 'abc.def'])('rejects %s', (v) => {
        expect(recommendJavaForVersion(v)).toBeNull();
    });
});

describe('JAVA_IMAGES', () => {
    it('offers a Java 25 image, without which no 26.x server can boot', () => {
        expect(JAVA_IMAGES.some((j) => j.id === JAVA_25)).toBe(true);
    });

    it('has no duplicate ids', () => {
        expect(new Set(JAVA_IMAGES.map((j) => j.id)).size).toBe(JAVA_IMAGES.length);
    });

    it('exposes every named constant', () => {
        for (const id of [JAVA_25, JAVA_21, JAVA_17, JAVA_8]) {
            expect(JAVA_IMAGES.some((j) => j.id === id)).toBe(true);
        }
    });
});

describe('effectiveMcVersion', () => {
    it('prefers the build when it refines the major', () => {
        expect(effectiveMcVersion('1.20', '1.20.4')).toBe('1.20.4');
        expect(effectiveMcVersion('26.2', '26.2')).toBe('26.2');
    });

    it('falls back to the major for loader versions', () => {
        expect(effectiveMcVersion('1.20', '47.2.0')).toBe('1.20');
        expect(effectiveMcVersion('1.20', '')).toBe('1.20');
    });
});

describe('recommendJavaForProxy', () => {
    // Velocity 4 needs Java 25 (Fill API java.version.minimum); the form used to
    // keep Java 21 for it because "4.2.0" is not a Minecraft version.
    it.each([
        ['velocity', '4.2.0', JAVA_25],
        ['velocity', '5.0.0', JAVA_25],
        ['velocity', '3.5.1', JAVA_21],
        ['velocity', '3.4.0', JAVA_21],
        ['waterfall', '1.21', JAVA_21],
        ['bungeecord', '#2104', JAVA_21],
        ['bungeecord', '', JAVA_21],
    ])('%s %s -> %s', (software, build, want) => {
        expect(recommendJavaForProxy(software, build)).toBe(want);
    });

    it.each([
        ['velocity', ''],
        ['velocity', 'latest'],
        ['paper', '1.21.4'],
        ['', '4.2.0'],
    ])('no recommendation for %s %s', (software, build) => {
        expect(recommendJavaForProxy(software, build)).toBeNull();
    });
});

describe('recommendJavaForSetup', () => {
    it('a game server follows its Minecraft version', () => {
        expect(recommendJavaForSetup({ isProxy: false, tab: 'online', software: 'paper', build: '26.2', mcVersion: '26.2' })).toBe(JAVA_25);
    });
    it('a proxy follows its release on the online and library tabs', () => {
        expect(recommendJavaForSetup({ isProxy: true, tab: 'online', software: 'velocity', build: '4.2.0', mcVersion: '4.2.0' })).toBe(JAVA_25);
        expect(recommendJavaForSetup({ isProxy: true, tab: 'library', software: 'velocity', build: '3.5.1', mcVersion: '3.5.1' })).toBe(JAVA_21);
    });
    it('an upload says nothing about the proxy inside it', () => {
        expect(recommendJavaForSetup({ isProxy: true, tab: 'upload', software: 'velocity', build: '4.2.0', mcVersion: '' })).toBeNull();
    });
});
