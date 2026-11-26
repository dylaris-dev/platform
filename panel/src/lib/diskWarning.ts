import type { DiskUsage } from './api/types';
import { formatBytes } from './format';

// The node sends reason "files" when the file count, not the bytes, set the
// warning: a server out of files fails every create and is stopped like one
// out of space, while its bytes can read far from full. Without the reason
// (an older node) the warning is about bytes, as it always was.
export function diskWarningText(u: DiskUsage): { title: string; detail: string } {
    const files = u.reason === 'files' && (u.inodeLimit ?? 0) > 0;
    const level = u.warning === 'full' ? 'reached' : u.warning === '90' ? 'critical' : 'low';
    if (files) {
        const used = (u.inodes ?? 0).toLocaleString('en-US');
        const cap = u.inodeLimit!.toLocaleString('en-US');
        return {
            title: { reached: 'File limit reached', critical: 'File limit critical', low: 'Files running low' }[level],
            detail: level === 'reached'
                ? `${used} of ${cap} files. Delete files (map renders often hold millions) to start it again.`
                : `${Math.round(((u.inodes ?? 0) / u.inodeLimit!) * 100)}% of files used: ${used} of ${cap} files`,
        };
    }
    return {
        title: { reached: 'Storage limit reached', critical: 'Storage limit critical', low: 'Storage running low' }[level],
        detail: `${Math.round((u.total / u.limit) * 100)}% used — ${formatBytes(u.total)} of ${formatBytes(u.limit)}`,
    };
}
