// Stable round-trip codec for Java .properties files. Preserves blank
// lines, comments, key ordering and trailing newline so saves don't show
// up as massive diffs. Updates rewrite a single line, additions append at
// the bottom.

export interface PropertiesDoc {
    /** Ordered keys as they appear in the file. New keys append. */
    order: string[];
    /** Parsed key→value map. */
    values: Record<string, string>;
    /** Original lines (verbatim) so unknown keys / comments survive. */
    rawLines: string[];
    /** Maps a key to the line index in rawLines that holds it. */
    lineIndex: Record<string, number>;
}

const KEY_VALUE_REGEX = /^([^=:#!]+?)\s*[=:]\s*(.*)$/;

export function parseProperties(text: string): PropertiesDoc {
    const lines = text.split(/\r?\n/);
    const order: string[] = [];
    const values: Record<string, string> = {};
    const lineIndex: Record<string, number> = {};

    lines.forEach((line, i) => {
        const trimmed = line.trim();
        if (!trimmed || trimmed.startsWith('#') || trimmed.startsWith('!')) return;
        const m = trimmed.match(KEY_VALUE_REGEX);
        if (!m) return;
        const key = m[1].trim();
        // Java's rule in one pass: \n \t \r \f and \uXXXX are special, any other
        // escaped char is itself. Minecraft writes `minecraft\:normal`.
        const value = m[2].replace(/\\(u[0-9a-fA-F]{4}|[\s\S])/g, (_, c: string) => {
            if (c.length === 5) return String.fromCharCode(parseInt(c.slice(1), 16));
            return ({ n: '\n', t: '\t', r: '\r', f: '\f' } as Record<string, string>)[c] ?? c;
        });
        if (!(key in values)) order.push(key);
        values[key] = value;
        lineIndex[key] = i;
    });

    return { order, values, rawLines: lines, lineIndex };
}

function escapeValue(v: string): string {
    return v.replace(/\\/g, '\\\\').replace(/\n/g, '\\n').replace(/\t/g, '\\t');
}

export function serializeProperties(doc: PropertiesDoc, updates: Record<string, string>): string {
    const lines = [...doc.rawLines];

    // Existing keys: rewrite in place.
    for (const key of Object.keys(updates)) {
        const newVal = updates[key];
        if (key in doc.lineIndex) {
            const lineNum = doc.lineIndex[key];
            lines[lineNum] = `${key}=${escapeValue(newVal)}`;
        } else {
            // New key — append before any trailing blank lines so the file
            // doesn't grow extra whitespace on every add.
            let appendAt = lines.length;
            while (appendAt > 0 && lines[appendAt - 1].trim() === '') appendAt -= 1;
            lines.splice(appendAt, 0, `${key}=${escapeValue(newVal)}`);
        }
    }

    return lines.join('\n');
}
