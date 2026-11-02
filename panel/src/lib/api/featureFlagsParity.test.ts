import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * The features endpoint overwrites EVERY flag on every save.
 *
 * That makes an omission silent and destructive rather than merely incomplete:
 * a flag the panel does not send arrives as the zero value, so saving any other
 * toggle switches it off. The operator gets no error, and the flag they turned
 * on last week is simply gone.
 *
 * So the panel's payload and Core's payload have to carry the same fields, and
 * that agreement cannot be checked in either language alone.
 */

const GO = readFileSync(
    join(__dirname, '../../../../core/handlers/settings/feature_settings.go'), 'utf8',
);
const TS = readFileSync(join(__dirname, 'featureFlags.ts'), 'utf8');

/** The json tags on Core's featureSettingsPayload struct. */
function goFields(): string[] {
    const start = GO.indexOf('type featureSettingsPayload struct {');
    expect(start, 'featureSettingsPayload not found - this test lost its subject').toBeGreaterThan(-1);
    const body = GO.slice(start, GO.indexOf('\n}', start));
    return [...body.matchAll(/`json:"([A-Za-z]+)"`/g)].map(m => m[1]);
}

/** The fields on the panel's FeatureFlagsAdminPayload interface. */
function tsFields(): string[] {
    const start = TS.indexOf('export interface FeatureFlagsAdminPayload {');
    expect(start, 'FeatureFlagsAdminPayload not found').toBeGreaterThan(-1);
    const body = TS.slice(start, TS.indexOf('\n}', start));
    return [...body.matchAll(/^\s{4}([A-Za-z]+)\??:/gm)].map(m => m[1]);
}

describe('feature flag payload parity', () => {
    it('the panel declares every field Core accepts', () => {
        const go = goFields();
        expect(go.length).toBeGreaterThan(3);
        const ts = new Set(tsFields());
        const missing = go.filter(f => !ts.has(f));
        expect(
            missing,
            'Core accepts these and the panel does not send them, so saving any other toggle sets them to false',
        ).toEqual([]);
    });

    it('the panel sends nothing Core will ignore', () => {
        // The other direction is not destructive, but it is a field an operator
        // can toggle that changes nothing - which is worse than no control.
        const go = new Set(goFields());
        const extra = tsFields().filter(f => !go.has(f));
        expect(extra, 'the panel sends fields Core does not read').toEqual([]);
    });

    it('the initial state names every field', () => {
        // A field missing from the useState default is undefined until the GET
        // lands, and a save in that window sends undefined - which JSON.stringify
        // drops, and the Go zero value is false.
        const tab = readFileSync(join(__dirname, '../../components/settings/FeaturesTab.tsx'), 'utf8');
        const init = tab.slice(tab.indexOf('useState<FeatureFlagsAdminPayload>('));
        const obj = init.slice(0, init.indexOf('}') + 1);
        for (const f of tsFields()) {
            // applyAuthoringToManual is a write-only instruction, not stored
            // state, so it has no place in the initial object.
            if (f === 'applyAuthoringToManual') continue;
            expect(obj, `${f} missing from the initial platformFlags state`).toContain(f + ':');
        }
    });
});
