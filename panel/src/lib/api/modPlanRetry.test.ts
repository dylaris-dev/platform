import { afterEach, describe, expect, it, vi } from 'vitest';
import { planModInstall } from '@/lib/api/modrinth';

afterEach(() => { vi.unstubAllGlobals(); });

// Update all plans every mod it updates, on the same 30/min bucket the update
// check already used. A 429 must be waited out, not reported as a failed plan:
// that left the rest of a large run on its old builds with "Too many requests".
describe('planModInstall', () => {
    it('waits out a rate limit and returns the plan', async () => {
        const ok = { success: true, install: [{ projectId: 'p', versionId: 'v' }], unresolved: [], incompatible: [] };
        const fetchMock = vi.fn()
            .mockResolvedValueOnce(new Response('{"success":false,"message":"Too many requests"}', { status: 429, headers: { 'Retry-After': '1' } }))
            .mockResolvedValueOnce(new Response(JSON.stringify(ok), { status: 200, headers: { 'Content-Type': 'application/json' } }));
        vi.stubGlobal('fetch', fetchMock);
        const res = await planModInstall(7, 'v');
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(res.ok).toBe(true);
    });
});
