import { describe, it, expect, vi } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

// A mutating button that stays enabled while its own request is in flight can
// be clicked twice, and both clicks reach the server. Measured on the testbed,
// not assumed: two clicks on "Create key" in one tick produced two API keys one
// millisecond apart, and the modal only ever revealed one plaintext - so the
// second key was live and its secret was gone.
//
// The server wizard, backup save, backup trigger and restore already guarded
// this. These were the siblings that did not.

const SRC = join(__dirname, '..');

function tsxFiles(dir: string, out: string[] = []): string[] {
    for (const entry of readdirSync(dir)) {
        if (entry === 'node_modules' || entry === '.next') continue;
        const p = join(dir, entry);
        if (statSync(p).isDirectory()) tsxFiles(p, out);
        else if (p.endsWith('.tsx')) out.push(p);
    }
    return out;
}

// The opening <button ...> tag, multi-line included.
const BUTTON = /<button\b[\s\S]*?>/g;
const ON_CLICK = /onClick=\{\s*(?:async\s*)?(?:\([^)]*\)\s*=>\s*)?([A-Za-z_$][\w$]*)/;
const MUTATING = ['create', 'submit', 'add', 'save', 'send', 'invite', 'publish',
    'import', 'provision', 'generate', 'install', 'apply', 'delete', 'revoke',
    'remove', 'update', 'start', 'stop', 'restart', 'trigger', 'migrate',
    'transfer', 'enroll', 'rotate', 'reset'];
const OPENERS = ['open', 'show', 'toggle', 'set'];

// Handlers whose name reads like a mutation but which only touch local state,
// so there is no request to fire twice.
const LOCAL_ONLY = new Set([
    'dismissDeleteWarning', // clears a banner and navigates
    'startEdit',            // seeds an edit form
    'addHoster',            // appends an empty row to a form array
    'startCreate',          // opens an empty form dialog; the SAVE inside it is guarded
    // Sets the node the confirm dialog is about. The delete itself lives in
    // DeleteNodeModal and is disabled while it runs - and that dialog also
    // makes you type the node's name, so a double click cannot reach it.
    'onOpenDeleteDialog',
]);

describe('a mutating button cannot be fired twice', () => {
    it('every button that calls a mutation carries a disabled guard', () => {
        const unguarded: string[] = [];
        for (const file of tsxFiles(SRC)) {
            const src = readFileSync(file, 'utf8');
            for (const m of src.matchAll(BUTTON)) {
                const el = m[0];
                const oc = ON_CLICK.exec(el);
                if (!oc) continue;
                const handler = oc[1];
                const low = handler.toLowerCase();
                if (OPENERS.some(o => low.startsWith(o))) continue;
                if (!MUTATING.some(v => low.includes(v))) continue;
                if (LOCAL_ONLY.has(handler)) continue;
                if (el.includes('disabled')) continue;
                const line = src.slice(0, m.index).split('\n').length;
                unguarded.push(`${file.slice(SRC.length + 1).replace(/\\/g, '/')}:${line} ${handler}`);
            }
        }
        expect(unguarded, 'these fire their request again on a second click; ' +
            'use useBusy() and disable the button, or add the handler to LOCAL_ONLY ' +
            'if it never leaves the browser').toEqual([]);
    });
});

describe('useBusy blocks the second call in the same tick', () => {
    it('the ref, not the state, is what refuses it', async () => {
        // React state does not update until after the current tick, so a guard
        // that read `busy` would see false on both clicks. This is the whole
        // reason the hook keeps a ref.
        const mod = await import('./useBusy');
        const src = readFileSync(join(__dirname, 'useBusy.ts'), 'utf8');
        expect(src).toMatch(/useRef\(false\)/);
        expect(src).toMatch(/if \(inFlight\.current\) return;/);
        // and it must release in a finally, or one rejected request wedges the
        // button for the rest of the session
        expect(src).toMatch(/finally \{[\s\S]*inFlight\.current = false;/);
        expect(typeof mod.useBusy).toBe('function');
        expect(vi.isMockFunction(mod.useBusy)).toBe(false);
    });
});
