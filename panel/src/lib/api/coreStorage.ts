// Typed client for /api/settings/core-storage. GET never returns the stored
// S3 secret (the backend blanks it and relies on the "omitempty" json tag to
// drop it entirely); s3SecretSet tells the UI whether one is already stored
// server-side so it can render "(unchanged)" instead of asking the admin to
// re-enter a secret that's still there.
import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';
import { handleUnauthorized } from '@/lib/api/session';
import type { CoreStorageConfig } from '@/lib/coreStorage';

export interface GetCoreStorageResponse {
  success: boolean;
  settings?: CoreStorageConfig;
  message?: string;
  /**
   * How many Core instances are currently heartbeating. 0 means the server
   * could not take the count (Redis unreachable), NOT that no Core is running.
   * Used as the expected total for the round-progress counter a save starts.
   */
  onlineCores?: number;
}

export async function getCoreStorage(): Promise<GetCoreStorageResponse> {
  try {
    const res = await fetch(`${API_URL}/settings/core-storage`, { headers: getAuthHeader() });
    return (await handleResponse(res)) as GetCoreStorageResponse;
  } catch (err) {
    return handleError(err) as GetCoreStorageResponse;
  }
}

export interface SaveCoreStorageResponse {
  success: boolean;
  message?: string;
  /** Present on a 409/503: the per-Core reachability round result. */
  round?: {
    roundId: string;
    confirmed: number;
    total: number;
    done: boolean;
    ok: boolean;
    results: import('@/lib/storageReach').CoreReachResult[];
  };
}

// Deliberately NOT routed through the shared handleResponse: its non-2xx
// branch keeps only `message` and drops every other field. A 409/503 refusal
// here carries `round` (the per-Core reachability result) alongside
// `message`, and the failure panel is unusable without it - so this endpoint
// parses its own body and keeps `round` on both branches instead.
export async function saveCoreStorage(s: CoreStorageConfig): Promise<SaveCoreStorageResponse> {
  try {
    const res = await fetch(`${API_URL}/settings/core-storage`, {
      method: 'POST',
      headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
      body: JSON.stringify(s),
    });
    if (handleUnauthorized(res)) return { success: false, message: 'Session expired' };
    const data = await res.json();
    if (res.ok) return { success: true, ...data };
    return { success: false, message: data.message || 'Unknown error', round: data.round };
  } catch (err) {
    return handleError(err) as SaveCoreStorageResponse;
  }
}

export interface TestCoreStorageResponse {
  success: boolean;
  ok?: boolean;
  /** How far the attempt got - see lib/connectionTest.ts. */
  stage?: string;
  message?: string;
  /**
   * Set when the probe SUCCEEDED but the configured location is not durable -
   * today: a path on the container's own filesystem rather than a mounted
   * volume. It rides along with ok:true on purpose, because the write/read
   * test really did pass; it is the durability of what was written that is in
   * doubt. Render it somewhere persistent, not in a toast that vanishes.
   */
  warning?: string;
}

// testCoreStorage posts the CANDIDATE config (the current, possibly-unsaved
// form state) so the admin can verify a backend works before committing to
// it. The backend builds a provider straight from this request body (falling
// back to the stored config only for an empty body) and never persists
// anything - it just writes/reads/deletes a throwaway probe object.
export async function testCoreStorage(candidate: CoreStorageConfig, signal?: AbortSignal): Promise<TestCoreStorageResponse> {
  try {
    const res = await fetch(`${API_URL}/settings/core-storage/test`, {
      method: 'POST',
      headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
      body: JSON.stringify(candidate),
      signal,
    });
    return (await handleResponse(res)) as TestCoreStorageResponse;
  } catch (err) {
    return handleError(err) as TestCoreStorageResponse;
  }
}
