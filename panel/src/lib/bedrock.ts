// Pure helpers for the per-server Bedrock switch (RoutesModal).
import { setServerBedrock } from './api';

/** Hidden unless Bedrock can be turned on here, except while it is on: an
 *  owner must always be able to turn it off again. */
export function showBedrockSwitch(b: { available: boolean; enabled: boolean }): boolean {
    return b.available || b.enabled;
}

/** The Floodgate build that matches the server's loader. */
export function floodgateLink(loader: string): { label: string; href: string } {
    return loader === 'mods'
        ? { label: 'Floodgate-Modded', href: 'https://modrinth.com/mod/floodgate' }
        : { label: 'Floodgate for Spigot/Paper', href: 'https://geysermc.org/download?project=floodgate' };
}

/** Turns Bedrock on or off. Returns the error to show, or null on success.
 *  fetchAPI resolves on 4xx/5xx with {success:false}, so the 409 sentence
 *  from Core arrives as message/error. */
export async function changeBedrock(serverId: number, next: boolean): Promise<string | null> {
    try {
        const res = await setServerBedrock(serverId, next);
        if (res?.success === false) return res.message || res.error || 'Bedrock could not be changed.';
        return null;
    } catch {
        return 'The request could not be sent.';
    }
}
