import { describe, expect, it } from 'vitest';
import { detectUploadSoftware, readZipEntryNames, singleTopFolder, readPackMcVersion } from './uploadDetect';

// Three entries under one top folder, with an archive comment so the end
// record is not the last 22 bytes. Made with Python's zipfile.
const ZIP_B64 =
    'UEsDBBQAAAAAAMm8RF2DFtyMAQAAAAEAAAAmAAAAc3J2L3ZlcnNpb25zLzEuMjEuMTEvcGFwZXItMS4yMS4xMS5qYXJ4UEsDBBQAAAAAAMm8RF2DFtyMAQAAAAEAAAAVAAAAc3J2L3NlcnZlci5wcm9wZXJ0aWVzeFBLAwQUAAAAAADJvERdgxbcjAEAAAABAAAAEwAAAHNydi93b3JsZC9sZXZlbC5kYXR4UEsBAhQAFAAAAAAAybxEXYMW3IwBAAAAAQAAACYAAAAAAAAAAAAAAIABAAAAAHNydi92ZXJzaW9ucy8xLjIxLjExL3BhcGVyLTEuMjEuMTEuamFyUEsBAhQAFAAAAAAAybxEXYMW3IwBAAAAAQAAABUAAAAAAAAAAAAAAIABRQAAAHNydi9zZXJ2ZXIucHJvcGVydGllc1BLAQIUABQAAAAAAMm8RF2DFtyMAQAAAAEAAAATAAAAAAAAAAAAAACAAXkAAABzcnYvd29ybGQvbGV2ZWwuZGF0UEsFBgAAAAADAAMA2AAAAKsAAAAEAG5vdGU=';

describe('readZipEntryNames', () => {
    it('reads the names from the central directory', async () => {
        const names = await readZipEntryNames(new Blob([Buffer.from(ZIP_B64, 'base64')]));
        expect(names).toEqual(['srv/versions/1.21.11/paper-1.21.11.jar', 'srv/server.properties', 'srv/world/level.dat']);
    });
    it('answers null for something that is not a zip', async () => {
        expect(await readZipEntryNames(new Blob(['not a zip at all'.repeat(10)]))).toBeNull();
    });
});

describe('detectUploadSoftware', () => {
    it('the upload that never started: Paper with its jars only under versions/', () => {
        const d = detectUploadSoftware([
            'carlo-ben/versions/1.21.7/paper-1.21.7.jar',
            'carlo-ben/versions/1.21.11/paper-1.21.11.jar',
            'carlo-ben/versions/1.21.8/paper-1.21.8.jar',
            'carlo-ben/cache/mojang_1.21.11.jar',
            'carlo-ben/libraries/com/mojang/x.jar',
            'carlo-ben/plugins/WorldEdit.jar',
            'carlo-ben/version_history.json',
            'carlo-ben/world/level.dat',
        ], true);
        expect(d).toEqual({ software: 'paper', build: '1.21.11', launchable: false });
    });

    it('a root jar is launchable as it is', () => {
        expect(detectUploadSoftware(['paper-1.21.4-232.jar', 'world/level.dat'])).toEqual({ software: 'paper', build: '1.21.4', launchable: true });
        expect(detectUploadSoftware(['server.jar']).launchable).toBe(true);
        expect(detectUploadSoftware(['forge-1.20.1-47.2.0-installer.jar']).launchable).toBe(false);
        // a jar one folder down is not the node's start jar, unless the node moves that folder up
        expect(detectUploadSoftware(['srv/server.jar']).launchable).toBe(false);
        expect(detectUploadSoftware(['srv/server.jar'], true).launchable).toBe(true);
    });

    it('recognises the loaders by what they leave behind', () => {
        expect(detectUploadSoftware(['libraries/net/neoforged/neoforge/21.1.77/unix_args.txt', 'mods/a.jar']))
            .toEqual({ software: 'neoforge', build: '21.1.77', launchable: true });
        expect(detectUploadSoftware(['libraries/net/minecraftforge/forge/1.20.1-47.2.0/forge-1.20.1-47.2.0-server.jar']))
            .toEqual({ software: 'forge', build: '1.20.1', launchable: false });
        expect(detectUploadSoftware(['fabric-server-launch.jar', 'libraries/net/fabricmc/intermediary/1.21.1/i.jar']))
            .toEqual({ software: 'fabric', build: '1.21.1', launchable: true });
        expect(detectUploadSoftware(['versions/1.21.1/server-1.21.1.jar', 'libraries/com/mojang/x.jar']))
            .toEqual({ software: 'vanilla', build: '1.21.1', launchable: false });
    });

    // Cut down from real CurseForge server packs: mods plus the file that
    // names the loader, which the node installs.
    it('recognises a modpack server pack', () => {
        for (const declares of ['variables.txt', 'manifest.json', 'startserver.sh', 'neoforge-21.1.251-installer.jar']) {
            expect(detectUploadSoftware(['mods/a.jar', 'config/a.toml', declares])).toEqual({ launchable: false, serverPack: true });
        }
        // Inside its one top folder, as ATM9 ships it.
        expect(detectUploadSoftware(['Server-Files-1.1.1/mods/a.jar', 'Server-Files-1.1.1/startserver.sh'], true).serverPack).toBe(true);
        // A server that already starts is not offered the pack's loader.
        expect(detectUploadSoftware(['mods/a.jar', 'variables.txt', 'fabric-server-launch.jar']).serverPack).toBeUndefined();
        // A start script alone, without mods, is just a server.
        expect(detectUploadSoftware(['start.sh', 'world/level.dat']).serverPack).toBeUndefined();
    });

    it('nothing recognisable prefills nothing', () => {
        expect(detectUploadSoftware(['world/level.dat', 'server.properties'])).toEqual({ launchable: false });
    });
});

describe('singleTopFolder', () => {
    it('a zipped server folder, and only that', () => {
        expect(singleTopFolder(['my-server/', 'my-server/world/level.dat', 'my-server/versions/1.21/paper-1.21.jar'])).toBe(true);
        // the node would move the only folder up even with files beside it
        expect(singleTopFolder(['server.jar', 'world/level.dat'])).toBe(false);
        expect(singleTopFolder(['a/x', 'b/y'])).toBe(false);
        expect(singleTopFolder(['server.jar'])).toBe(false);
        // a zip made by macOS Finder
        expect(singleTopFolder(['srv/world/level.dat', '__MACOSX/srv/._level.dat', '.DS_Store'])).toBe(true);
        expect(detectUploadSoftware(['srv/server.jar', '__MACOSX/srv/._server.jar'], true).launchable).toBe(true);
        expect(singleTopFolder([])).toBe(false);
    });
});

describe('case', () => {
    it('matches the node: Server.jar does not start on Linux', () => {
        expect(detectUploadSoftware(['Server.jar']).launchable).toBe(false);
        expect(detectUploadSoftware(['Paper-1.21.jar']).launchable).toBe(false);
    });
});

// Made with Python's zipfile: a CurseForge server pack with its manifest
// deflated, a ServerPackCreator one with variables.txt stored, and a pack that
// names no version anywhere.
const CF_PACK_B64 = 'UEsDBBQAAAAIAHqSSV1fqyHuOAEAAJUFAAANAAAAbWFuaWZlc3QuanNvbm3TwWqDQBDG8VdZ9mwlM2Ni4rHkUugblBwkrq0latjYQgi+e5HmsB+fx+U/7OHHzMP33RDOsW4nX7mH/w3x1o2Dr5yXXDe5+Mz5fmzex7oJ8eYr9/HwXbP0doyf4aUo8yJXXcausevrePeVm+JPmE9z5vxQ92EZfg33cWjcMVynr2W27S7h+ds1jt/hPL0dfeU2z/T/mDOHWdIslDXNStnSbJSLNBeUt2neUt6leUe5THNJeZ/mPeVDmg/MAmyy4oZwLCdAJ2wngCesJ8An7CcAKCwoQChsKIAorCjAKOwoACksqSCpLKkgqSs7iEvIkgqSypIKksqSCpLKkgqSypIKksqSCpLKkgqSypIGksaSBpLGkgaStnLPeNAsaSBpLGkgaSxpIGksaSBpLGkgaSxpIGmH+TT/AVBLAwQUAAAAAAB6kkldgxbcjAEAAAABAAAACgAAAG1vZHMvYS5qYXJ4UEsBAhQAFAAAAAgAepJJXV+rIe44AQAAlQUAAA0AAAAAAAAAAAAAAIABAAAAAG1hbmlmZXN0Lmpzb25QSwECFAAUAAAAAAB6kkldgxbcjAEAAAABAAAACgAAAAAAAAAAAAAAgAFjAQAAbW9kcy9hLmphclBLBQYAAAAAAgACAHMAAACMAQAAAAA=';
const SPC_PACK_B64 = 'UEsDBBQAAAAAAHqSSV2b/HErMAAAADAAAAANAAAAdmFyaWFibGVzLnR4dCMgeApNSU5FQ1JBRlRfVkVSU0lPTj0xLjIxLjEKTU9ETE9BREVSPU5lb0ZvcmdlClBLAwQUAAAAAAB6kkldgxbcjAEAAAABAAAACgAAAG1vZHMvYS5qYXJ4UEsBAhQAFAAAAAAAepJJXZv8cSswAAAAMAAAAA0AAAAAAAAAAAAAAIABAAAAAHZhcmlhYmxlcy50eHRQSwECFAAUAAAAAAB6kkldgxbcjAEAAAABAAAACgAAAAAAAAAAAAAAgAFbAAAAbW9kcy9hLmphclBLBQYAAAAAAgACAHMAAACEAAAAAAA=';
// Windows, 7-Zip and Info-ZIP put an extra field (here an extended timestamp)
// in the local header, ahead of the data; Python's zipfile writes none.
const EXTRA_PACK_B64 = 'UEsDBBQAAAAIAAAAIQATP7WCWAAAAF0AAAANAAkAbWFuaWZlc3QuanNvblVUBQABAPFTZRXKQQqAIBAF0KsMf12S1SbP0A2iheQULsyYLAjx7uH6vYzgT97E7gmGMl6W28cThqCVnlSPhhCim6N1LDcMLRneVd+jHNyOgxpUV9clPlj5YCjJw2Ut5QdQSwMEFAAAAAAArJJJXYMW3IwBAAAAAQAAAAoAAABtb2RzL2EuamFyeFBLAQIUABQAAAAIAAAAIQATP7WCWAAAAF0AAAANAAkAAAAAAAAAAACAAQAAAABtYW5pZmVzdC5qc29uVVQFAAEA8VNlUEsBAhQAFAAAAAAArJJJXYMW3IwBAAAAAQAAAAoAAAAAAAAAAAAAAIABjAAAAG1vZHMvYS5qYXJQSwUGAAAAAAIAAgB8AAAAtQAAAAAA';
// A pack zipped as its folder, as many CurseForge server files are.
const FOLDER_PACK_B64 = 'UEsDBBQAAAAAABGTSV3q/pHZGQAAABkAAAAWAAAAUGFjay0xLjIvdmFyaWFibGVzLnR4dE1JTkVDUkFGVF9WRVJTSU9OPTEuMTguMgpQSwMEFAAAAAAAEZNJXYMW3IwBAAAAAQAAABMAAABQYWNrLTEuMi9tb2RzL2EuamFyeFBLAQIUABQAAAAAABGTSV3q/pHZGQAAABkAAAAWAAAAAAAAAAAAAACAAQAAAABQYWNrLTEuMi92YXJpYWJsZXMudHh0UEsBAhQAFAAAAAAAEZNJXYMW3IwBAAAAAQAAABMAAAAAAAAAAAAAAIABTQAAAFBhY2stMS4yL21vZHMvYS5qYXJQSwUGAAAAAAIAAgCFAAAAfwAAAAAA';
const BARE_PACK_B64 = 'UEsDBBQAAAAAAHqSSV2DFtyMAQAAAAEAAAAKAAAAbW9kcy9hLmphcnhQSwMEFAAAAAAAepJJXQi7mcALAAAACwAAAAgAAABzdGFydC5zaGphdmEgLWphciB4UEsBAhQAFAAAAAAAepJJXYMW3IwBAAAAAQAAAAoAAAAAAAAAAAAAAIABAAAAAG1vZHMvYS5qYXJQSwECFAAUAAAAAAB6kkldCLuZwAsAAAALAAAACAAAAAAAAAAAAAAAgAEpAAAAc3RhcnQuc2hQSwUGAAAAAAIAAgBuAAAAWgAAAAAA';

// The setup form recommended the Java of the server being replaced over a
// pack for another Minecraft version; the pack names its own.
describe('readPackMcVersion', () => {
    const blob = (b64: string) => new Blob([Buffer.from(b64, 'base64')]);
    it('reads a deflated CurseForge manifest', async () => {
        expect(await readPackMcVersion(blob(CF_PACK_B64))).toBe('1.20.1');
    });
    it('skips a local extra field before the data', async () => {
        expect(await readPackMcVersion(blob(EXTRA_PACK_B64))).toBe('1.19.2');
    });
    it('reads a pack zipped as its folder', async () => {
        expect(await readPackMcVersion(blob(FOLDER_PACK_B64))).toBe('1.18.2');
    });
    it('reads a stored ServerPackCreator variables.txt', async () => {
        expect(await readPackMcVersion(blob(SPC_PACK_B64))).toBe('1.21.1');
    });
    it('answers nothing for a pack that names no version, or no zip', async () => {
        expect(await readPackMcVersion(blob(BARE_PACK_B64))).toBeUndefined();
        expect(await readPackMcVersion(new Blob(['not a zip'.repeat(10)]))).toBeUndefined();
    });
});
