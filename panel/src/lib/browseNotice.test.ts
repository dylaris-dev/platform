import { describe, it, expect } from 'vitest';
import { browseNotice, envFilterName } from './browseNotice';

const none = null;
const serverOnly = 'server_only' as const;
const clientServer = 'client_server' as const;

describe('envFilterName', () => {
    it('names the toggles that are on', () => {
        expect(envFilterName(none)).toBeNull();
        expect(envFilterName(serverOnly)).toBe('Server only');
        expect(envFilterName(clientServer)).toBe('Client + Server');
    });
});

describe('browseNotice', () => {
    it('a failed search is an error, never "No projects match."', () => {
        // A 429 from Core used to arrive as null and render as an empty result.
        const n = browseNotice({ failed: true, hits: 0, visible: 0, env: none });
        expect(n?.kind).toBe('failed');
        expect(n?.message).not.toBe('No projects match.');
    });

    it('a real empty result says so', () => {
        expect(browseNotice({ failed: false, hits: 0, visible: 0, env: none }))
            .toEqual({ kind: 'empty', message: 'No projects match.' });
    });

    it('names the filter that hid every hit', () => {
        const n = browseNotice({ failed: false, hits: 20, visible: 0, env: serverOnly });
        expect(n?.kind).toBe('empty');
        expect(n?.message).toContain('Server only filter');
        expect(n?.message).toContain('20 results');
    });

    it('names the filter when the search itself came back empty with it on', () => {
        expect(browseNotice({ failed: false, hits: 0, visible: 0, env: clientServer })?.message)
            .toContain('Client + Server filter on');
    });

    it('with no button on, says the hidden rows were client-only', () => {
        const n = browseNotice({ failed: false, hits: 5, visible: 0, env: none });
        expect(n?.kind).toBe('empty');
        expect(n?.message).toContain('client-only');
        expect(n?.message).not.toBe('No projects match.');
    });

    it('says nothing when there are rows', () => {
        expect(browseNotice({ failed: false, hits: 20, visible: 3, env: clientServer })).toBeNull();
    });
});
