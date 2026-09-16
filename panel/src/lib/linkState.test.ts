import { describe, it, expect } from 'vitest';
import { linkState, nodeLinkState } from './linkState';

describe('linkState', () => {
    it('says connected only when the link says so', () => {
        expect(linkState(true).tone).toBe('ok');
        expect(linkState(false).tone).toBe('bad');
    });

    // The one answer that must never be invented. Core sends nothing when it
    // could not ask, and a customer told "not connected" because OUR lookup
    // failed goes and debugs a machine that is fine.
    it('never turns "we could not ask" into "not connected"', () => {
        for (const v of [undefined, null]) {
            const s = linkState(v);
            expect(s.tone).toBe('unknown');
            expect(s.label).not.toMatch(/not connected/i);
        }
    });

    it('tells the reader what to do when it is not connected', () => {
        expect(linkState(false).hint).toMatch(/redeploy/i);
    });
});

describe('nodeLinkState', () => {
    // The state the release before this one was about: everything looks healthy
    // and no player can join, because the file carried no link.
    it('spells out the online machine whose link is missing', () => {
        const s = nodeLinkState(true, false);
        expect(s.tone).toBe('bad');
        expect(s.label).toBe('Link not connected');
        expect(s.hint).toMatch(/nobody can join/i);
        expect(s.hint).toMatch(/deploy file/i);
    });

    it('stays quiet about the link of a machine that is down', () => {
        const s = nodeLinkState(false, false);
        expect(s.tone).toBe('unknown');
        expect(s.hint).toMatch(/machine itself/i);
    });

    it('confirms a healthy pair and keeps "no answer" separate', () => {
        expect(nodeLinkState(true, true)).toMatchObject({ tone: 'ok', label: 'Link connected' });
        expect(nodeLinkState(true, undefined)).toMatchObject({ tone: 'unknown' });
    });
});
