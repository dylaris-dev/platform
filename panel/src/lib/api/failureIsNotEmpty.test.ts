import { afterEach, describe, expect, it, vi } from 'vitest';

import { listInstalledMods, getServerModpackContents } from './modrinth';
import { getUnmanagedMods } from './modcompat';

afterEach(() => {
    vi.unstubAllGlobals();
});

function respond(status: number, body: unknown) {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
    })));
}

// The two mod lists are deliberately NOT alike, and this pins the difference so
// that "make them consistent" cannot quietly remove it.
describe('the two mod lists fail differently on purpose', () => {
    it('listInstalledMods raises: the tab reads it to decide what is already there', async () => {
        respond(500, { success: false, message: 'Could not list mods' });
        await expect(listInstalledMods(1)).rejects.toThrow('Could not list mods');
    });

    it('listInstalledMods returns the mods on success', async () => {
        respond(200, { success: true, mods: [{ id: 1, fileName: 'spark.jar' }] });
        await expect(listInstalledMods(1)).resolves.toHaveLength(1);
    });

    // getServerModpackContents stays fail-open, and its own comment says why:
    // the cross-check is advisory, so the tab has to keep working without the
    // snapshot. An advisory decoration missing is not a claim about anything.
    it('getServerModpackContents still fails open', async () => {
        respond(500, { success: false, message: 'no snapshot' });
        await expect(getServerModpackContents(1)).resolves.toEqual([]);
    });
});

// The strongest instance of the shape, because Core deliberately built the
// other half of it. Its handler comment says the node answers a MISSING
// directory with an empty list, so an error there is a real failure to LOOK,
// and "reporting that as nothing unmanaged would hide exactly the thing this
// endpoint exists to reveal" - so it returns 502.
//
// The wrapper then flattened that 502 back into an empty list. The guard
// existed on one side of the boundary only, and a server whose node could not
// be reached read as a server with nothing out of place.
describe('getUnmanagedMods', () => {
    it('raises rather than reporting that nothing is out of place', async () => {
        respond(502, { success: false, message: "Could not read this server's files from its node" });
        await expect(getUnmanagedMods(1)).rejects.toThrow('files from its node');
    });

    it('returns the files on success', async () => {
        respond(200, { success: true, files: [{ directory: 'mods', name: 'stray.jar', size: 10 }] });
        await expect(getUnmanagedMods(1)).resolves.toHaveLength(1);
    });

    // A tidy server is a real answer and must stay one.
    it('a server with nothing unmanaged is an empty list', async () => {
        respond(200, { success: true, files: [] });
        await expect(getUnmanagedMods(1)).resolves.toEqual([]);
    });
});
