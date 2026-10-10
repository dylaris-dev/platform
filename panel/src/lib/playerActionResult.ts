import { friendlyRconError, type RconResponse } from '@/lib/api/rcon';

// First line only, colour codes gone: 1.13+ follows a refusal with the command
// echo and a <--[HERE] marker, and plugins answer with section-sign colour codes,
// neither of which means anything in a toast.
function firstLine(s: string): string {
    return s.replace(/§./g, '').trim().split(/\r?\n/)[0].trim();
}

// What the toast after a player action says. The server's own reply is the
// truth here: "Player is already whitelisted" is a success at the HTTP level and
// still changed nothing, so a bare "Whitelist add" with a tick would have lied.
export function playerActionToast(label: string, res: RconResponse): { text: string; ok: boolean } {
    if (!res.success) return { text: `${label}: ${firstLine(friendlyRconError(res.error, 'failed'))}`, ok: false };
    return { text: `${label}: ${firstLine(res.output || '') || 'done'}`, ok: true };
}
