import { describe, it, expect } from 'vitest';
import { readFileSync } from 'fs';
import { join } from 'path';

/**
 * WCAG 2.x contrast for the colour pairs the panel actually renders.
 *
 * The owner's report was "grey on grey", and it was measurable: the most used
 * text token (--base-06, ~1000 call sites, also every .mono-label and
 * .input-label) sat near 3:1 on cards. This pins the token VALUES so a later
 * edit to globals.css cannot quietly slide back.
 *
 * The pairs come from grepping the call sites (text-(--x) next to bg-(--y)) and
 * the globals.css component classes, not from a theoretical matrix. Ghost
 * backgrounds are translucent, so they are composited over the surface they
 * normally sit on; both card levels are tried and the worse one counts.
 *
 * CONTRAST_TABLE=1 npx vitest run src/app/globalsContrast.test.ts --silent=false
 * prints the full table.
 */

const CSS = readFileSync(join(process.cwd(), 'src', 'app', 'globals.css'), 'utf8');

type RGBA = [number, number, number, number];

function block(selector: RegExp): string {
    const m = CSS.match(selector);
    if (!m || m.index === undefined) throw new Error(`block ${selector} not found in globals.css`);
    // Brace matching, so nested comments or a later block cannot end it early.
    let depth = 0;
    for (let i = m.index + m[0].length - 1; i < CSS.length; i++) {
        if (CSS[i] === '{') depth++;
        else if (CSS[i] === '}' && --depth === 0) return CSS.slice(m.index, i);
    }
    throw new Error(`unterminated block ${selector}`);
}

function tokens(src: string): Record<string, string> {
    const out: Record<string, string> = {};
    for (const m of src.matchAll(/--color-([a-z0-9-]+)\s*:\s*([^;]+);/g)) out[m[1]] = m[2].trim();
    return out;
}

function parse(v: string): RGBA {
    const hex = v.match(/^#([0-9a-f]{6})$/i);
    if (hex) {
        const n = parseInt(hex[1], 16);
        return [(n >> 16) & 255, (n >> 8) & 255, n & 255, 1];
    }
    const rgba = v.match(/^rgba?\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*(?:,\s*([\d.]+))?\s*\)$/);
    if (rgba) return [+rgba[1], +rgba[2], +rgba[3], rgba[4] === undefined ? 1 : +rgba[4]];
    throw new Error(`unparseable colour ${v}`);
}

function over(fg: RGBA, bg: RGBA): RGBA {
    const a = fg[3];
    return [fg[0] * a + bg[0] * (1 - a), fg[1] * a + bg[1] * (1 - a), fg[2] * a + bg[2] * (1 - a), 1];
}

function lum([r, g, b]: RGBA): number {
    const ch = (c: number) => {
        const s = c / 255;
        return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
    };
    return 0.2126 * ch(r) + 0.7152 * ch(g) + 0.0722 * ch(b);
}

function ratio(a: RGBA, b: RGBA): number {
    const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
    return (hi + 0.05) / (lo + 0.05);
}

const DEFAULT = tokens(block(/@theme\s*\{/));
const HIGH = { ...DEFAULT, ...tokens(block(/:root\[data-contrast="high"\]\s*\{/)) };

type Kind = 'text' | 'ui';
// [foreground, background, kind]. Backgrounds ending in -ghost or -border are
// translucent and get composited over base-02 and base-03.
const PAIRS: [string, string, Kind][] = [];
const SURFACES = ['base-00', 'base-01', 'base-02', 'base-03'];
for (const fg of ['base-05', 'base-06', 'base-07', 'base-08', 'base-09']) {
    for (const bg of SURFACES) PAIRS.push([fg, bg, 'text']);
}
for (const fg of ['base-07', 'base-08', 'base-09']) PAIRS.push([fg, 'base-04', 'text']);
for (const s of ['accent', 'error', 'warning', 'success', 'primary']) {
    for (const bg of SURFACES) PAIRS.push([`${s}-light`, bg, 'text']);
    PAIRS.push([`${s}-light`, `${s}-ghost`, 'text']);
}
// text-(--warning) is used as text at ~40 sites, deliberately kept.
for (const bg of ['base-02', 'base-03', 'warning-ghost']) PAIRS.push(['warning', bg, 'text']);
// Text on solid fills: buttons, banners, the accent-light pill.
PAIRS.push(['white', 'accent', 'text'], ['white', 'error', 'text'], ['white', 'success', 'text']);
PAIRS.push(['black', 'warning', 'text'], ['base-00', 'accent-light', 'text'], ['base-09', 'accent', 'text']);
PAIRS.push(['base-08', 'accent-ghost', 'text'], ['base-08', 'warning-ghost', 'text']);
// Non-text (1.4.11): the focus ring, input and checkbox borders, the error
// border on an invalid input, the on/off toggle track.
for (const bg of ['base-00', 'base-02', 'base-03']) PAIRS.push(['accent-light', bg, 'ui']);
for (const bg of ['base-01', 'base-02', 'base-03']) PAIRS.push(['base-05', bg, 'ui']);
PAIRS.push(['error-light', 'base-03', 'ui'], ['success', 'base-02', 'ui']);
// Read off the input rules themselves, so reverting a border in globals.css
// fails here rather than only in the token pairs above. Each border is measured
// on its own fill and on the surfaces an input sits on (page, card, sunken).
function cssToken(selector: RegExp, prop: string): string {
    const m = block(selector).match(new RegExp(`\\n\\s*${prop}\\s*:[^;]*var\\(--([a-z0-9-]+)\\)`));
    if (!m) throw new Error(`${prop} in ${selector} is not a var(--token)`);
    return m[1];
}
for (const cls of ['input-field', 'input-mono']) {
    const sel = new RegExp(`\\n\\.${cls}\\s*\\{`);
    const fill = cssToken(sel, 'background');
    for (const bg of new Set([fill, 'base-00', 'base-01', 'base-02', 'base-03'])) {
        PAIRS.push([cssToken(sel, 'border'), bg, 'ui']);
    }
}
const hoverBorder = cssToken(/:is\(\.input-field, \.input-mono\):hover[^{]*\{/, 'border-color');
for (const bg of ['base-01', 'base-03']) PAIRS.push([hoverBorder, bg, 'ui']);

const FIXED: Record<string, string> = { white: '#ffffff', black: '#000000' };

function resolve(set: Record<string, string>, name: string): string {
    const v = FIXED[name] ?? set[name];
    if (!v) throw new Error(`token --color-${name} not defined`);
    return v;
}

function measure(set: Record<string, string>, fg: string, bg: string): number {
    const f = parse(resolve(set, fg));
    const b = parse(resolve(set, bg));
    if (b[3] === 1) return ratio(over(f, b), b);
    return Math.min(...['base-02', 'base-03'].map((s) => {
        const under = parse(resolve(set, s));
        const flat = over(b, under);
        return ratio(over(f, flat), flat);
    }));
}

function rows(set: Record<string, string>) {
    return PAIRS.map(([fg, bg, kind]) => ({ pair: `${fg} on ${bg}`, kind, ratio: measure(set, fg, bg) }));
}

function failures(name: string, set: Record<string, string>) {
    const need: Record<Kind, number> = { text: 4.5, ui: 3 };
    const all = rows(set).map((r) => ({ ...r, ratio: +r.ratio.toFixed(2), need: need[r.kind], ok: r.ratio >= need[r.kind] }));
    if (process.env.CONTRAST_TABLE) {
        console.log(name);
        console.table(all);
    }
    return all.filter((r) => !r.ok);
}

describe('globals.css contrast', () => {
    it('default tokens meet WCAG AA (4.5:1 text, 3:1 UI)', () => {
        expect(failures('default', DEFAULT)).toEqual([]);
    });

    it('high-contrast tokens meet WCAG AA', () => {
        expect(failures('high', HIGH)).toEqual([]);
    });

    // A high-contrast set that is lower than the default anywhere is a bug in
    // the mode, even when both clear AA.
    it('high contrast is never lower than the default for any pair', () => {
        const base = rows(DEFAULT);
        const worse = rows(HIGH).filter((r, i) => r.ratio < base[i].ratio - 0.005)
            .map((r) => `${r.pair}: ${r.ratio.toFixed(2)}`);
        expect(worse).toEqual([]);
    });

    it('the surface scale stays ordered, darkest to lightest', () => {
        for (const set of [DEFAULT, HIGH]) {
            const ls = ['00', '01', '02', '03', '04', '05', '06', '07', '08', '09'].map((n) => lum(parse(set[`base-${n}`])));
            for (let i = 1; i < ls.length; i++) expect(ls[i]).toBeGreaterThan(ls[i - 1]);
        }
    });
});
