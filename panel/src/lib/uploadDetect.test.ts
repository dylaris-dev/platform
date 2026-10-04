import { describe, expect, it } from 'vitest';
import { detectUploadSoftware, readZipEntryNames, singleTopFolder } from './uploadDetect';

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
