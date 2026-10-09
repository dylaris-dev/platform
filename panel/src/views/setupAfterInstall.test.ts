import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const SRC = path.resolve(path.dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')), '..');
const read = (p: string) => readFileSync(path.join(SRC, p), 'utf8').replace(/\r\n/g, '\n');

describe('after an install from the Setup tab', () => {
    // The header keeps its own route list and loaded it only on a server change,
    // so it said "No gateway route configured" about the route just created.
    it('tells the server header about a route it created', () => {
        const setup = read('views/SetupView.tsx');
        expect(setup).toMatch(/if \(routeCreated\) \{\s*await loadRoutes\(\);\s*window\.dispatchEvent\(new CustomEvent\(ROUTES_CHANGED_EVENT, \{ detail: \{ serverId: server\.id \} \}\)\);/);
        const shell = read('app/(authed)/servers/[id]/ServerShell.tsx');
        expect(shell).toContain('window.addEventListener(ROUTES_CHANGED_EVENT, onChanged);');
        expect(shell).toContain('if ((e as CustomEvent).detail?.serverId === sid) load();');
        // The Setup tab's own routes dialog is the second way to make one.
        expect(read('views/SetupView.tsx')).toMatch(/onRoutesChanged=\{\(rs\) => \{\s*setExistingRoutes\(rs\);\s*window\.dispatchEvent\(new CustomEvent\(ROUTES_CHANGED_EVENT/);
        // Every route, side by side: the header showed the first three only.
        expect(shell).toContain('{serverRoutes.map(route => (');
    });

    // Only a successful install moves on; a failed route keeps its error on screen.
    it('opens the console after a successful install only', () => {
        // An upload with no file only prepares the slot for SFTP; it stays.
        expect(read('views/SetupView.tsx')).toMatch(/\} else \{\s*onSetupComplete\(\);[^}]*if \(installer\.type !== 'upload'\) onInstalled\?\.\(\);\s*\}/);
        expect(read('app/(authed)/servers/[id]/setup/page.tsx')).toContain('router.push(`/servers/${server.id}/console`)');
    });

    // Seven sources in a max-w-md strip overflowed their buttons.
    it('lets the install-method strip wrap instead of overflowing', () => {
        for (const f of ['views/setup/SetupNewWizard.tsx', 'views/setup/SetupEditMode.tsx']) {
            const s = read(f);
            expect(s).toContain('flex flex-wrap gap-1 bg-(--base-03) p-1 rounded-md">');
            expect(s).not.toContain('btn flex-1 py-2 text-sm border-0 rounded-md ${props.installTab');
        }
    });

    // The Java was keyed on the online pickers alone, which still held the
    // server being replaced ("Minecraft 26.3 needs Java 25" over a 1.20.1 pack).
    it('recommends the Java of what is installed, and says when it cannot tell', () => {
        const setup = read('views/SetupView.tsx');
        expect(setup).toContain(": keepsUpload ? (uploadDetection?.mcVersion || '')");
        expect(setup).toContain('const javaVersionUnknown = keepsUpload && !isProxy && !!uploadFile && uploadDetection !== null && !targetMcVersion;');
        expect(setup).toContain('const mc = d.serverPack ? await readPackMcVersion(uploadFile)');
        for (const f of ['views/setup/SetupNewWizard.tsx', 'views/setup/SetupEditMode.tsx']) {
            const s = read(f);
            expect(s).toMatch(/useMemo\(\s*\(\) => props\.targetMcVersion,/);
            expect(s).toContain('versionUnknown={props.javaVersionUnknown}');
        }
        expect(read('views/setup/JavaVersionPicker.tsx')).toMatch(/\{versionUnknown && !showMismatchWarning && \(\s*<div className="alert alert-warning/);
    });

    // The console jumped back down on every line, so nothing above could be read.
    it('follows console output only while the reader is at the bottom', () => {
        const c = read('views/ConsoleView.tsx');
        expect(c).toContain('if (el && followRef.current) el.scrollTop = el.scrollHeight;');
        expect(c).toContain('const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40;');
        expect(c).toMatch(/\{!following && lines\.length > 0 && \(\s*<button/);
        expect(c).not.toContain('scrollIntoView');
        // At once, or the smooth scroll's own events switch following off.
        expect(c).not.toContain("behavior: 'smooth'");
    });

    // Stuck at the scroller's padding edge, the search bar let rows show above it.
    it('pins the properties search bar flush with the top', () => {
        expect(read('views/ServerPropertiesView.tsx')).toContain('sticky -top-5 bg-(--base-02) z-10 -mx-5 -mt-5');
    });
});
