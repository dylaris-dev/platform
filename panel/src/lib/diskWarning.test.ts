import { describe, it, expect } from 'vitest';
import { diskWarningText } from './diskWarning';

const GB = 1024 ** 3;

describe('diskWarningText', () => {
  it('names the bytes when the bytes are full', () => {
    expect(diskWarningText({ total: 10 * GB, limit: 10 * GB, subServers: {}, warning: 'full' })).toEqual({
      title: 'Storage limit reached',
      detail: '100% used — 10 GB of 10 GB',
    });
  });

  it('names the file count when the node says files are full', () => {
    const t = diskWarningText({
      total: 3 * GB, limit: 10 * GB, subServers: {}, warning: 'full',
      reason: 'files', inodes: 1000000, inodeLimit: 1000000,
    });
    expect(t.title).toBe('File limit reached');
    expect(t.detail).toBe('1,000,000 of 1,000,000 files. Delete files (map renders often hold millions) to start it again.');
  });

  it('warns about files running low', () => {
    const t = diskWarningText({
      total: GB, limit: 10 * GB, subServers: {}, warning: '90',
      reason: 'files', inodes: 900000, inodeLimit: 1000000,
    });
    expect(t.title).toBe('File limit critical');
    expect(t.detail).toBe('90% of files used: 900,000 of 1,000,000 files');
  });

  it('reads a node without the reason as bytes', () => {
    expect(diskWarningText({ total: 8 * GB, limit: 10 * GB, subServers: {}, warning: '80' }).title).toBe('Storage running low');
  });
});
