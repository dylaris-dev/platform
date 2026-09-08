import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'fs';
import { join } from 'path';

/**
 * A class name that does not exist is invisible.
 *
 * globals.css is unlayered, so its rules beat every Tailwind utility - which is
 * exactly why a typo in one of its class names fails SILENTLY: the element keeps
 * whatever Tailwind utilities sit beside it and simply renders without the part
 * that was meant to style it. Nothing errors, nothing logs, and the review
 * screenshot looks plausible because the layout classes still applied.
 *
 * Measured, not hypothetical. `input` was written instead of `input-field` in
 * twenty places across five screens - every one of those inputs rendered as
 * browser chrome on a dark panel. `input-sm` and `btn-danger-ghost` never
 * existed at all.
 *
 * Scope is deliberately the design-system NAMESPACES below rather than every
 * class in the codebase: those prefixes are ours, so an unknown one is always a
 * mistake, while an unknown `flex-...` is Tailwind's business and not ours to
 * police.
 */

const SRC = join(process.cwd(), 'src');
const CSS = join(SRC, 'app', 'globals.css');

// Prefixes that belong to globals.css. A class starting with one of these and
// not defined there is a typo, every time.
const OWNED = ['btn-', 'input-', 'modal-', 'toggle-', 'alert-', 'card-', 'tab-'];

// Exact names we own that carry no separator.
const OWNED_EXACT = ['btn', 'input', 'modal', 'card', 'alert', 'toggle'];

function walk(dir: string, out: string[] = []): string[] {
    for (const entry of readdirSync(dir)) {
        const p = join(dir, entry);
        if (statSync(p).isDirectory()) walk(p, out);
        else if (/\.tsx?$/.test(p) && !/\.test\.tsx?$/.test(p)) out.push(p);
    }
    return out;
}

function definedClasses(css: string): Set<string> {
    const found = new Set<string>();
    // Every `.name` that opens a rule or joins a selector list. Pseudo-classes
    // and combinators end the name.
    for (const m of css.matchAll(/\.([a-zA-Z][\w-]*)(?=[\s,{:.>[])/g)) found.add(m[1]);
    return found;
}

describe('design-system class names', () => {
    it('every class in a globals.css namespace is actually defined there', () => {
        const defined = definedClasses(readFileSync(CSS, 'utf8'));
        const offenders: string[] = [];

        for (const file of walk(SRC)) {
            const src = readFileSync(file, 'utf8');
            for (const m of src.matchAll(/className=(?:"([^"]*)"|\{`([^`]*)`\})/g)) {
                const raw = m[1] ?? m[2] ?? '';
                for (const piece of raw.split(/\s+/)) {
                    // A template literal can hold a ternary, so a token arrives
                    // wrapped in the quotes and braces of the expression around
                    // it. Trim anything a class name cannot contain off both
                    // ends - keeping them made three correctly-named classes
                    // look undefined.
                    const token = piece.replace(/^[^\w-]+/, '').replace(/[^\w-]+$/, '');
                    // Template holes and Tailwind arbitrary values are not ours.
                    if (!token || piece.includes('$') || piece.includes('(') || piece.includes('[')) continue;
                    const owned = OWNED_EXACT.includes(token) || OWNED.some(p => token.startsWith(p));
                    if (!owned || defined.has(token)) continue;
                    offenders.push(`${token}  (${file.slice(SRC.length + 1)})`);
                }
            }
        }

        expect(
            [...new Set(offenders)].sort(),
            'these class names are in a globals.css namespace but defined nowhere, so they render as nothing'
        ).toEqual([]);
    });
});
