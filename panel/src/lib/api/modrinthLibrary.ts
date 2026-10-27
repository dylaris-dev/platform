import { API_URL, getAuthHeader, handleResponse } from './core';

export type Favorite = { projectId: string; title: string };
export type AvailabilityWatch = {
	 followPack?: boolean;
	 checkedBuildId?: number;
	 checkedBuildVersion?: string;
    id: number;
    packId?: number;
    buildId?: number;
    title: string;
    loader: string;
    current: string;
    projects: Favorite[];
    unlinked: number;
    targets: string[];
    future: boolean;
    delivered: string[];
    versions: { minecraft: string; available: number; missing: string[] }[];
    lastError?: string;
    lastChecked?: string;
    nextCheck: string;
};
export async function libraryRequest(path: string, method = 'GET', body?: unknown) {
    const response = await fetch(`${API_URL}${path}`, {
        method,
        headers: { ...getAuthHeader(), ...(body ? { 'Content-Type': 'application/json' } : {}) },
        body: body ? JSON.stringify(body) : undefined,
    });
    const data = (await handleResponse(response)) as {
        success?: boolean;
        message?: string;
        favorites?: Favorite[];
        watches?: AvailabilityWatch[];
    };
    if (!response.ok || data.success === false) throw new Error(data.message || 'The request failed.');
    return data;
}
