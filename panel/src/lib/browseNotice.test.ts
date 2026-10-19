import { describe, it, expect } from 'vitest';
import { browseNotice, envFilterName } from './browseNotice';

const none = { client: false, server: false };
const client = { client: true, server: false };
const server = { client: false, server: true };
const both = { client: true, server: true };

describe('envFilterName', () => {
    it('names the toggles that are on', () => {
        expect(envFilterName(none)).toBeNull();
        expect(envFilterName(client)).toBe('Client');
        expect(envFilterName(server)).toBe('Server');
        expect(envFilterName(both)).toBe('Client and Server');
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
        const n = browseNotice({ failed: false, hits: 20, visible: 0, env: server });
        expect(n?.kind).toBe('empty');
        expect(n?.message).toContain('Server filter');
        expect(n?.message).toContain('20 results');
    });

    it('names the filter when the search itself came back empty with it on', () => {
        expect(browseNotice({ failed: false, hits: 0, visible: 0, env: both })?.message)
            .toContain('Client and Server filter on');
    });

    it('says nothing when there are rows', () => {
        expect(browseNotice({ failed: false, hits: 20, visible: 3, env: client })).toBeNull();
    });
});
