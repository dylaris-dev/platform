// panel client for RCON exec + config + the Players-tab actions
// (ban/kick/op/whitelist/list) which all funnel into the same RCON pipeline.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface RconResponse {
    success: boolean;
    output?: string;
    error?: string;
    durationMs?: number;
}

export interface RconConfig {
    success: boolean;
    enabled: boolean;
    port: number;
    hasSecret: boolean;
    password?: string;
    message?: string;
    // True when this SetConfig call rewrote server.properties. MC only opens
    // the RCON listener at JVM start, so the change is inert until the server
    // restarts - the panel uses this instead of guessing from a later
    // connection-refused error.
    restartRequired?: boolean;
    // Console filter: drops Minecraft's per-RCON-connection thread lines from
    // the log stream. Applies live (the log-shipper re-reads it from Redis on a
    // timer), so it is deliberately NOT covered by restartRequired.
    hideLogNoise?: boolean;
}

export async function execRcon(serverId: number, command: string, timeoutMs?: number): Promise<RconResponse> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/rcon`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ command, timeoutMs }),
        });
        const data = await res.json();
        // See players.ts: a refused command now carries a real status and its
        // reason in `error`.
        if (!res.ok) return { success: false, error: data.error || data.message || 'Request failed' };
        return data;
    } catch (err: any) {
        return { success: false, error: err?.message || 'Network error' };
    }
}

export async function getRconConfig(serverId: number): Promise<RconConfig> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/rcon/config`, { headers: getAuthHeader() });
        return handleResponse(res) as any;
    } catch (err) {
        return { ...(handleError(err) as any), enabled: false, port: 0, hasSecret: false };
    }
}

export async function setRconConfig(
    serverId: number,
    // hideLogNoise is OPTIONAL on purpose and the backend field is a pointer:
    // omitting it keeps the stored value, so an unrelated save (port, password)
    // cannot silently turn the console filter back off.
    payload: { enabled: boolean; port: number; password?: string; regenerate?: boolean; hideLogNoise?: boolean },
): Promise<RconConfig> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/rcon/config`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
        });
        return handleResponse(res) as any;
    } catch (err) {
        return { ...(handleError(err) as any), enabled: false, port: 0, hasSecret: false };
    }
}

// --- Player Management helpers (shorthand wrappers over execRcon) ---

export interface OnlinePlayer {
    name: string;
}

// parsePlayerList extracts usernames from vanilla /list output. Format varies
// slightly between server flavours but the common shape is:
//   "There are N of M players online: alice, bob, charlie"
// We split the segment after the colon by commas; whitespace tolerant.
export function parsePlayerList(out: string): OnlinePlayer[] {
    if (!out) return [];
    const idx = out.indexOf(':');
    if (idx === -1) return [];
    const tail = out.slice(idx + 1).trim();
    if (!tail) return [];
    return tail.split(',').map(s => s.trim()).filter(Boolean).map(name => ({ name }));
}

// A raw Go dial error (e.g. "dial mc_xxx:25575: dial tcp ...: connect:
// connection refused") is technical noise to an end user and leaks the
// internal container hostname. Almost always it means the RCON config was
// just saved but the server has not restarted yet (MC only opens the
// listener at JVM start), so any RCON call fails until then. Map that class
// of error to one clear, actionable message instead of showing the raw string.
const RCON_DIAL_ERROR_PATTERN = /dial tcp|connection refused|no such host|i\/o timeout/i;

export function isRconDialError(error?: string): boolean {
    return !!error && RCON_DIAL_ERROR_PATTERN.test(error);
}

export function friendlyRconError(error?: string, fallback = 'RCON unavailable'): string {
    if (isRconDialError(error)) return 'RCON not reachable yet - restart the server.';
    return error || fallback;
}

export const rconList = (serverId: number) => execRcon(serverId, 'list');
export const rconKick = (serverId: number, name: string, reason?: string) =>
    execRcon(serverId, `kick ${name}${reason ? ` ${reason}` : ''}`);
export const rconBan = (serverId: number, name: string, reason?: string) =>
    execRcon(serverId, `ban ${name}${reason ? ` ${reason}` : ''}`);
export const rconUnban = (serverId: number, name: string) =>
    execRcon(serverId, `pardon ${name}`);
export const rconOp = (serverId: number, name: string) => execRcon(serverId, `op ${name}`);
export const rconDeop = (serverId: number, name: string) => execRcon(serverId, `deop ${name}`);
export const rconWhitelistAdd = (serverId: number, name: string) =>
    execRcon(serverId, `whitelist add ${name}`);
export const rconWhitelistRemove = (serverId: number, name: string) =>
    execRcon(serverId, `whitelist remove ${name}`);
export const rconTell = (serverId: number, name: string, msg: string) =>
    execRcon(serverId, `tell ${name} ${msg}`);
