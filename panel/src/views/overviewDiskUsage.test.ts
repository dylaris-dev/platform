import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const SRC = path.resolve(path.dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')), '..');
const read = (p: string) => readFileSync(path.join(SRC, p), 'utf8').replace(/\r\n/g, '\n');

describe('overview disk usage with backups folded into the server quota', () => {
    // .dylaris-backups sits inside the server directory, so the node's disk
    // total (quota project or du) already holds the archives. Adding the backup
    // usage on top counted every archive twice and shrank "Free" by as much.
    it('does not add the backup bytes to a total that already holds them', () => {
        const view = read('views/OverviewView.tsx');
        expect(view).toContain('const total = diskUsage.total;');
        expect(view).not.toMatch(/diskUsage\.total \+ foldedBackupBytes/);
    });
});
