import { API_URL, getAuthHeader } from './core';
import { type PackLoaderVersion } from '@/lib/serverLoaderMetadata';

async function readMetadata(path: string, signal?: AbortSignal) {
    const res = await fetch(`${API_URL}${path}`, { headers: getAuthHeader(), signal });
    const data = await res.json();
    if (!res.ok || data?.success === false) {
        throw new Error(data?.message || `Version lookup failed (${res.status}).`);
    }
    return data;
}

export async function getPackMinecraftVersions(signal?: AbortSignal): Promise<string[]> {
    const data = await readMetadata('/versions?software=vanilla', signal);
    if (!Array.isArray(data.versions)) throw new Error('Minecraft version metadata is unavailable.');
    return [...new Set<string>(data.versions.map((v: { build: string }) => v.build).filter((v: unknown) => typeof v === 'string' && v))];
}

export async function getPackLoaderVersions(loader: string, minecraft: string, signal?: AbortSignal): Promise<PackLoaderVersion[]> {
    const query = new URLSearchParams({ software: loader, minecraft });
    const data = await readMetadata(`/versions/loaders?${query}`, signal);
    if (!Array.isArray(data.versions)) throw new Error('Loader version metadata is unavailable.');
    return data.versions.filter((v: PackLoaderVersion) => typeof v.version === 'string' && v.version.length > 0);
}
