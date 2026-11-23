import { describe, it, expect } from 'vitest';
import { parseProperties, serializeProperties } from './properties-codec';

describe('parseProperties', () => {
    it('parses simple key=value pairs preserving order', () => {
        const doc = parseProperties('level-name=world\nmax-players=20\n');
        expect(doc.order).toEqual(['level-name', 'max-players']);
        expect(doc.values).toEqual({ 'level-name': 'world', 'max-players': '20' });
    });

    it('skips blank lines and comments', () => {
        const doc = parseProperties('# a comment\n\n!also a comment\nmotd=Hello\n');
        expect(doc.order).toEqual(['motd']);
        expect(doc.values.motd).toBe('Hello');
    });

    it('supports colon as a key/value separator', () => {
        const doc = parseProperties('level-name: world');
        expect(doc.values['level-name']).toBe('world');
    });

    it('unescapes \\n, \\t and \\\\ in values', () => {
        const doc = parseProperties('motd=line1\\nline2\\ttabbed\\\\literal');
        expect(doc.values.motd).toBe('line1\nline2\ttabbed\\literal');
    });

    it('unescapes the \\: Minecraft itself writes, so select values match their options', () => {
        // The server writes level-type=minecraft\:normal; read raw, no option matched it.
        const doc = parseProperties('level-type=minecraft\\:normal\ngenerator-settings=\\{\\}\nmotd=\\u00a7aHi');
        expect(doc.values['level-type']).toBe('minecraft:normal');
        expect(doc.values['generator-settings']).toBe('{}');
        expect(doc.values.motd).toBe('§aHi');
    });

    it('does not chain escapes: \\\\n is a backslash and an n, not a newline', () => {
        expect(parseProperties('a=x\\\\ny').values.a).toBe('x\\ny');
    });

    it('keeps the last value and the last line index when a key repeats', () => {
        const doc = parseProperties('a=1\na=2\n');
        expect(doc.values.a).toBe('2');
        expect(doc.order).toEqual(['a']);
        expect(doc.lineIndex.a).toBe(1);
    });

    it('records rawLines verbatim, including the trailing empty line from a final newline', () => {
        const doc = parseProperties('# header\nmotd=Hi\n');
        expect(doc.rawLines).toEqual(['# header', 'motd=Hi', '']);
    });

    it('handles CRLF line endings', () => {
        const doc = parseProperties('a=1\r\nb=2\r\n');
        expect(doc.order).toEqual(['a', 'b']);
        expect(doc.values).toEqual({ a: '1', b: '2' });
    });
});

describe('serializeProperties', () => {
    it('rewrites an existing key in place without reordering', () => {
        const doc = parseProperties('a=1\nb=2\n');
        expect(serializeProperties(doc, { a: '99' })).toBe('a=99\nb=2\n');
    });

    it('appends a new key at the end', () => {
        const doc = parseProperties('a=1\n');
        expect(serializeProperties(doc, { c: 'new' })).toBe('a=1\nc=new\n');
    });

    it('appends new keys before trailing blank lines', () => {
        const doc = parseProperties('a=1\n\n\n');
        expect(serializeProperties(doc, { c: 'new' })).toBe('a=1\nc=new\n\n\n');
    });

    it('escapes backslash, newline and tab when writing a value', () => {
        const doc = parseProperties('motd=old\n');
        const out = serializeProperties(doc, { motd: 'a\\b\nc\td' });
        expect(out).toBe('motd=a\\\\b\\nc\\td\n');
    });

    it('preserves comments and blank lines untouched', () => {
        const doc = parseProperties('# keep me\na=1\n\nb=2\n');
        expect(serializeProperties(doc, { a: '9' })).toBe('# keep me\na=9\n\nb=2\n');
    });

    it('round-trips parse -> serialize with no updates unchanged', () => {
        const text = 'a=1\nb=2\n# c\n';
        const doc = parseProperties(text);
        expect(serializeProperties(doc, {})).toBe(text);
    });
});
