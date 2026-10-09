import { describe, expect, it } from 'vitest';
import { findMatches, isGzipName, isOpenableFile, stripGz } from '@dylaris/ui-filebrowser/src/utils';
import { detectLanguage } from '@dylaris/ui-filebrowser/src/CodeMirrorEditor';

describe('isOpenableFile', () => {
    it.each([
        ['latest.log', true],
        ['Latest.LOG', true],
        ['server.PROPERTIES', true],
        ['velocity.toml', true],
        ['README.md', true],
        ['start.sh', true],
        ['pack.json5', true],
        ['2026-10-09-1.log.gz', true],
        ['OLD.LOG.GZ', true],
        ['world.tar.gz', false],
        ['plugin.jar', false],
        ['region.mca', false],
        ['backup.gz', false],
    ])('%s -> %s', (name, want) => {
        expect(isOpenableFile(name)).toBe(want);
    });
});

describe('gzip names', () => {
    it('opens a .gz under the name inside it', () => {
        expect(isGzipName('a.log.GZ')).toBe(true);
        expect(isGzipName('a.log')).toBe(false);
        expect(stripGz('a.log.Gz')).toBe('a.log');
        expect(detectLanguage('config.json.gz')).toBe('json');
        expect(detectLanguage('latest.log.gz')).toBe('plain');
    });
});

describe('findMatches', () => {
    it('is literal and case-insensitive, non-overlapping', () => {
        expect(findMatches('Error a ERROR b error', 'error')).toEqual([0, 8, 16]);
        expect(findMatches('aaaa', 'aa')).toEqual([0, 2]);
        expect(findMatches('a.b(c', '(')).toEqual([3]);
        expect(findMatches('abc', '')).toEqual([]);
    });
});
