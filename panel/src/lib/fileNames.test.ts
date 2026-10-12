import { describe, expect, it } from 'vitest';
import { compareNames, validFilenameRegex } from '@dylaris/ui-filebrowser/src/utils';

describe('file names', () => {
  it('accepts the names Minecraft and mod authors write', () => {
    for (const n of ['New World', 'fabric-api-0.92.0+1.20.1.jar', 'Übersetzung.txt', 'a&b (copy).txt', '.hidden']) {
      expect(validFilenameRegex.test(n), n).toBe(true);
    }
  });

  it('refuses a path, a control character and the dot names', () => {
    for (const n of ['', '.', '..', 'a/b', String.raw`a\b`, 'tab\there', 'nul\u0000', 'x'.repeat(256)]) {
      expect(validFilenameRegex.test(n), JSON.stringify(n)).toBe(false);
    }
  });

  it('sorts numbers by value and ignores case', () => {
    expect(['world10', 'World2', 'world1'].sort(compareNames)).toEqual(['world1', 'World2', 'world10']);
  });
});
