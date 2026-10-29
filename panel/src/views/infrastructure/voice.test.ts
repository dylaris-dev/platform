import { describe, expect, it } from 'vitest';
import { bedrockRows, voiceRegionRows } from './VoicePanel';
import type { VoiceOverview, VoiceRegionStat } from '@/lib/api';

const region = (r: Partial<VoiceRegionStat>): VoiceRegionStat => ({
    region: 'eu', relays: 1, portsUsed: 0, portsTotal: 1000, rxBytes: 0, txBytes: 0, ...r,
});
const overview = (regions: VoiceRegionStat[]): VoiceOverview => ({ period: '2026-10', relays: [], regions, servers: [] });

describe('voiceRegionRows', () => {
    it('sorts regions by name', () => {
        const got = voiceRegionRows(overview([region({ region: 'us-east' }), region({ region: 'eu-central' }), region({ region: 'ap' })]));
        expect(got.map(r => r.region)).toEqual(['ap', 'eu-central', 'us-east']);
    });

    it('gives the share of the port pool held, rounded down', () => {
        const [r] = voiceRegionRows(overview([region({ portsUsed: 255, portsTotal: 1000 })]));
        expect(r.pct).toBe(25);
        expect(r.portsUsed).toBe(255);
    });

    it('is 0% of a pool with no size rather than NaN', () => {
        expect(voiceRegionRows(overview([region({ portsUsed: 3, portsTotal: 0 })]))[0].pct).toBe(0);
    });

    it('does not reorder the overview it was given', () => {
        const o = overview([region({ region: 'b' }), region({ region: 'a' })]);
        voiceRegionRows(o);
        expect(o.regions.map(r => r.region)).toEqual(['b', 'a']);
    });
});

describe('bedrockRows', () => {
    const row = (serverId: number, rxBytes: number, txBytes: number) => ({ serverId, serverName: `s${serverId}`, region: 'eu', rxBytes, txBytes });

    it('is empty when an older Core sent no bedrock list', () => {
        expect(bedrockRows(overview([]))).toEqual([]);
    });

    it('lists the busiest server first without reordering the input', () => {
        const o = { ...overview([]), bedrock: [row(1, 5, 5), row(2, 50, 1), row(3, 0, 20)] };
        expect(bedrockRows(o).map(r => r.serverId)).toEqual([2, 3, 1]);
        expect(o.bedrock.map(r => r.serverId)).toEqual([1, 2, 3]);
    });
});
