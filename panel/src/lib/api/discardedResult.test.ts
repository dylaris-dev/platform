import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

// fetchAPI does NOT throw on 4xx/5xx - it RESOLVES with {success:false} (see
// src/lib/api/types.ts). So `await deleteThing(id)` as a bare statement is not
// "errors bubble up", it is "every refusal Core has is discarded", and the UI
// then does whatever it was going to do next: reload a list that still contains
// the row, close a dialog, or toast success outright.
//
// NetworkView already carries the fix with the reasoning written out - "every
// refusal Core has here ... ended with the dropdown clearing and no word to the
// user, which reads exactly like it worked". These are the call sites that had
// not caught up.
//
// Guarded here rather than per-component because the defect is a shape, not a
// place: any new caller that drops the result reintroduces it.

const MUTATING = [
    'deleteUser',
    'deleteModule',
    'updateModulePosition',
    'deleteBackupRun',
    'deleteServerRoute',
    'updateServerName',
    'updateServerResources',
    'setServerAutoMove',
    'sendConsoleCommand',
    'setServerProxyProtocol',
    'setServerProxyMode',
    'setLinkRouteProxyProtocol',
    'setLinkRouteVoice',
    'setRouteEdition',
];

function walk(dir: string, out: string[] = []): string[] {
    for (const entry of readdirSync(dir)) {
        const p = join(dir, entry);
        if (statSync(p).isDirectory()) {
            walk(p, out);
        } else if (/\.tsx?$/.test(p) && !/\.test\.tsx?$/.test(p)) {
            out.push(p);
        }
    }
    return out;
}

const SRC = join(__dirname, '..', '..');
const FILES = walk(SRC);

describe('a mutating API call never has its result discarded', () => {
    for (const fn of MUTATING) {
        it(`${fn} is never awaited as a bare statement`, () => {
            // A bare `await fn(` statement - not `const x = await fn(`, not
            // `arr.push(await fn(` - is the discarded-result shape.
            const bare = new RegExp(String.raw`^[ \t]*await ${fn}\(`, 'm');
            const offenders = FILES.filter(f => bare.test(readFileSync(f, 'utf8')))
                .map(f => f.slice(SRC.length + 1).replace(/\\/g, '/'));
            expect(offenders).toEqual([]);
        });
    }

    it('the helper that made this shape dangerous still behaves that way', () => {
        // If fetchAPI ever starts throwing on non-2xx, the rule above becomes
        // unnecessary and this test should be revisited rather than worked
        // around. Pin the current contract so that change is deliberate.
        const source = readFileSync(join(__dirname, 'types.ts'), 'utf8');
        expect(source).toMatch(/return \{ success: false, error: msg, message: msg \}/);
        expect(source).not.toMatch(/throw new Error\(`Request failed/);
    });
});
