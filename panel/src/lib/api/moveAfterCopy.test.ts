import { afterEach, describe, expect, it, vi } from 'vitest';
import { updateServerVersionAfterCopy } from '@/lib/api/modcompat';

afterEach(() => { vi.unstubAllGlobals(); });

const json = (status: number, body: unknown) =>
    new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

// "Copy first" queues the copy and then the move. The move used to go out at
// once and came back as a busy failure while the copy ran; it now waits for
// the copy and goes out once it has landed.
describe('updateServerVersionAfterCopy', () => {
    it('waits while the copy runs, then moves', async () => {
        const fetchMock = vi.fn()
            .mockResolvedValueOnce(json(409, { success: false, code: 'copy_running', message: 'still copying' }))
            .mockResolvedValueOnce(json(409, { success: false, code: 'copy_running', message: 'still copying' }))
            .mockResolvedValueOnce(json(200, { success: true, message: 'Moving' }));
        vi.stubGlobal('fetch', fetchMock);
        const waits: number[] = [];
        const res = await updateServerVersionAfterCopy(7, { minecraft: '1.21.1' }, 'main-old', async ms => { waits.push(ms); });
        expect(res.success).toBe(true);
        expect(fetchMock).toHaveBeenCalledTimes(3);
        expect(waits.length).toBe(2);
        for (const call of fetchMock.mock.calls) {
            expect(JSON.parse(call[1].body).afterCopy).toBe('main-old');
        }
    });

    it('stops on any other answer', async () => {
        const fetchMock = vi.fn().mockResolvedValueOnce(json(409, { success: false, code: 'copy_failed', message: 'did not finish' }));
        vi.stubGlobal('fetch', fetchMock);
        const res = await updateServerVersionAfterCopy(7, { minecraft: '1.21.1' }, 'main-old', async () => {});
        expect(res.success).toBe(false);
        expect(res.code).toBe('copy_failed');
        expect(fetchMock).toHaveBeenCalledTimes(1);
    });
});
