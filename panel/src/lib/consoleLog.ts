// Level-based colouring for server console lines.
//
// Minecraft/Java servers usually log as `[HH:MM:SS] [Thread/LEVEL]: message`,
// and many emit it as plain text with no ANSI colour codes, so every line
// renders in the same colour. This maps the detected log level to a design
// token so warnings and errors stand out. ANSI-coloured segments still win,
// because parseAnsiLine sets inline colours on their spans, which override the
// line's className. Lines with no recognised level keep the default colour.
//
// Continuation lines (an exception header + `at ...` stack frames following
// an ERROR record) carry no level token of their own, so they inherit the
// level of the record they belong to - see computeLineLevels.

export type Level = 'error' | 'warn' | 'debug' | 'info';

const DEFAULT_LEVEL: Level = 'info';

// Pure regex detection only - no default fallback. Returns null when the
// line matches no known level token.
export function detectLevel(line: string): Level | null {
    const lvl = (line.match(/\b(ERROR|SEVERE|FATAL|WARN|WARNING|DEBUG|TRACE)\b/i)?.[1] || '').toUpperCase();
    if (lvl === 'ERROR' || lvl === 'SEVERE' || lvl === 'FATAL') return 'error';
    if (lvl === 'WARN' || lvl === 'WARNING') return 'warn';
    if (lvl === 'DEBUG' || lvl === 'TRACE') return 'debug';
    return null;
}

export function levelClass(level: Level): string {
    if (level === 'error') return 'text-(--error-light)';
    if (level === 'warn') return 'text-(--warning-light)';
    if (level === 'debug') return 'text-(--base-06)';
    return 'text-(--base-09)';
}

export function logLineClass(line: string): string {
    return levelClass(detectLevel(line) ?? DEFAULT_LEVEL);
}

const STACK_FRAME = /^\s*at\s/;
const EXCEPTION_SHAPE = /^[\w.$]+(Exception|Error)(:|$)/;
const CAUSED_BY = /^Caused by:/;

// A line with no level token of its own is a continuation of the previous
// record only when it has a recognised stack-trace shape: an indented line
// (`\tat ...`, `\t... N more`), a bare stack frame, an exception header
// (`java.x.FooException: ...`), or a `Caused by:` chain. We deliberately do
// NOT treat every line lacking a `[HH:MM:SS]` prefix as a continuation:
// proxy consoles (Velocity/Waterfall/BungeeCord) use different timestamp
// formats, so a broad "no timestamp" rule would bleed a record's colour into
// unrelated fresh lines for the rest of the stream.
function isContinuationLine(line: string): boolean {
    return /^\s/.test(line)
        || STACK_FRAME.test(line)
        || EXCEPTION_SHAPE.test(line)
        || CAUSED_BY.test(line);
}

// Single-pass fold over a batch of console lines: lines that match a level
// keep it, continuation-shaped lines inherit the previous line's computed
// level, everything else falls back to the default level. `previous` is the
// level of the line before the batch, so a live line can be levelled alone.
export function computeLineLevels(lines: string[], previous: Level = DEFAULT_LEVEL): Level[] {
    const levels: Level[] = [];
    for (const line of lines) {
        const detected = detectLevel(line);
        const level = detected ?? (isContinuationLine(line) ? previous : DEFAULT_LEVEL);
        levels.push(level);
        previous = level;
    }
    return levels;
}
