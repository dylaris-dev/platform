import { describe, expect, it } from 'vitest';
import { EditorState } from '@codemirror/state';
import { ensureSyntaxTree, type LanguageSupport } from '@codemirror/language';
import { highlightTree } from '@lezer/highlight';
import { json } from '@codemirror/lang-json';
import { yaml } from '@codemirror/lang-yaml';
import { javascript } from '@codemirror/lang-javascript';
import { detectLanguage } from '@dylaris/ui-filebrowser/src/CodeMirrorEditor';
import { dylarisHighlightStyle } from '@dylaris/ui-filebrowser/src/codemirror-theme';
import { propertiesLanguage } from '@dylaris/ui-filebrowser/src/codemirror-properties';

describe('detectLanguage', () => {
    it.each([
        ['server.properties', 'properties'],
        ['paper-global.yml', 'yaml'],
        ['forge-common.TOML', 'properties'],
        ['en_us.lang', 'properties'],
        ['start.sh', 'properties'],
        ['start.bat', 'properties'],
        ['setup.ini', 'properties'],
        ['pack.mcmeta', 'json'],
        ['pack.json5', 'json'],
        ['quests/chapter.snbt', 'javascript'],
        ['scripts/recipes.zs', 'javascript'],
        ['log4j2.xml', 'xml'],
        ['items.csv', 'plain'],
        ['README.md', 'plain'],
        ['latest.log', 'plain'],
    ])('%s -> %s', (name, want) => {
        expect(detectLanguage(name)).toBe(want);
    });
});

// The text of every highlighted token the editor's style colours.
function coloured(lang: LanguageSupport, doc: string): string[] {
    const state = EditorState.create({ doc, extensions: [lang] });
    const tree = ensureSyntaxTree(state, doc.length, 5000);
    if (!tree) throw new Error('parse did not finish');
    const out: string[] = [];
    highlightTree(tree, dylarisHighlightStyle, (from, to) => out.push(doc.slice(from, to)));
    return out;
}

describe('dylarisHighlightStyle colours the values, not just the keys', () => {
    it.each([
        ['yaml plain scalars', yaml(), 'motd: Hello world\nmax: 20\n', ['Hello world', '20']],
        ['toml section and values', propertiesLanguage(), '[general]\nenabled = true\nname = "x"\n', ['[general]', 'true', '"x"']],
        ['json', json(), '{"a": "b", "n": 1}', ['"b"', '1']],
        ['snbt via js', javascript(), '{ id: "0A1B", x: 2.5d }', ['id', '"0A1B"', '2.5']],
    ])('%s', (_name, lang, doc, want) => {
        const got = coloured(lang, doc);
        for (const w of want) expect(got).toContain(w);
    });
});
