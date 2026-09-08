import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
    metricsDBIncomplete,
    emptyMetricsDBTarget,
    type MetricsDBTarget,
} from './metricsDb';

const target = (over: Partial<MetricsDBTarget> = {}): MetricsDBTarget => ({
    ...emptyMetricsDBTarget,
    host: 'metricsdb',
    dbName: 'dylaris_metrics',
    user: 'metrics',
    ...over,
});

describe('what the statistics-database form will accept', () => {
    // There is one target now, so an empty form is incomplete rather than a
    // valid default. A fresh install has to name a database before it can save
    // one, which is the whole reason the Core option was removed.
    it('an empty target is incomplete', () => {
        expect(metricsDBIncomplete(emptyMetricsDBTarget)).toMatch(/host/i);
    });

    // Switching recording OFF must not require a database. The card only
    // applies this check while the switch is on; the rule is asserted here
    // because it is the card that would otherwise strand an installation with
    // an empty form and no way to stop writing.
    it('the card only blocks the save while recording is on', () => {
        const card = readFileSync(
            join(__dirname, '..', '..', 'components', 'settings', 'MetricsDatabaseCard.tsx'),
            'utf8',
        );
        expect(card).toContain('saveBlockedReason={(enabled ? incomplete : null)');
    });

    it('names the field that is missing, so the message can point at it', () => {
        expect(metricsDBIncomplete(target({ host: '   ' }))).toMatch(/host/i);
        expect(metricsDBIncomplete(target({ dbName: '' }))).toMatch(/database name/i);
        expect(metricsDBIncomplete(target({ user: '' }))).toMatch(/user/i);
        expect(metricsDBIncomplete(target({ port: 'https' }))).toMatch(/port/i);
        expect(metricsDBIncomplete(target({ port: '0' }))).toMatch(/port/i);
        expect(metricsDBIncomplete(target({ port: '70000' }))).toMatch(/port/i);
    });
});

describe('the card that renders it', () => {
    const card = readFileSync(
        join(__dirname, '..', '..', 'components', 'settings', 'MetricsDatabaseCard.tsx'),
        'utf8',
    );

    // Every field edit has to clear the last test result. A green "connected"
    // banner sitting above a host that has since been retyped is a claim about
    // a connection nobody ever made.
    it('editing a field clears the previous test result', () => {
        expect(card).toMatch(/const set = \([^)]*\) => \{\s*\n\s*test\.clear\(\);/);
    });

    // The form uses the shared hook rather than its own snapshot ref. Hand-rolled
    // copies of that lifecycle are what left every feature switch on this page
    // unsavable - see featuresTabSnapshots.test.ts.
    it('uses the shared settings-form lifecycle', () => {
        expect(card).toContain('useSettingsForm');
        expect(card).not.toMatch(/useRef<[^>]*\| null>\(null\)/);
    });

    // The panel is the ONLY place this is configured - there is no environment
    // variable beside it any more, so nothing here may render read-only on the
    // grounds that something else owns the setting.
    it('the panel is the sole authority: no environment lock left', () => {
        expect(card).not.toContain('managedByEnv');
        expect(card).not.toContain('METRICS_DB_URL');
        expect(card).toContain('form={form}');
    });

    // Three severities. "Connected, but no TimescaleDB" is neither a pass nor a
    // failure, and a two-state banner would have to call it one of them. The
    // banner is shared now, so that is where the three tones are asserted.
    it('the shared banner renders warning as its own severity', () => {
        const note = readFileSync(
            join(__dirname, '..', '..', 'components', 'ui', 'ConnectionTest.tsx'),
            'utf8',
        );
        for (const sev of ['ok:', 'warning:', 'error:']) {
            expect(note).toContain(sev);
        }
        expect(note).toContain('--warning-border');
    });
});

describe('one setting, one writer', () => {
    const read = (rel: string) => readFileSync(join(__dirname, '..', '..', rel), 'utf8');

    // The recording switch used to live in the feature-flag bundle while the
    // database lived here. Two endpoints writing feature_metrics_enabled means
    // whichever saved last wins, and the feature card would have reverted a
    // change made on this one from its own stale copy. It is now written in
    // exactly one place, together with the target it needs.
    it('the feature-flag bundle no longer carries the metrics switch', () => {
        const flags = read('lib/api/featureFlags.ts');
        expect(flags).not.toMatch(/^\s*metrics:\s*boolean;/m);
    });

    it('the features tab renders no statistics switch of its own', () => {
        const tab = read('components/settings/FeaturesTab.tsx');
        expect(tab).not.toContain("editPlatformFlag('metrics'");
        expect(tab).not.toContain('platformFlags.metrics');
    });

    // And it is here, in the same form as the target, so one save commits both.
    it('the statistics card owns the switch', () => {
        const card = read('components/settings/MetricsDatabaseCard.tsx');
        expect(card).toContain("set({ enabled: v })");
        expect(card).toContain('SwitchRow');
    });
});

describe('taking a password back off', () => {
    const card = readFileSync(
        join(__dirname, '..', '..', 'components', 'settings', 'MetricsDatabaseCard.tsx'),
        'utf8',
    );

    // A blank field already means "keep what is stored", so it cannot also mean
    // "there is none". Without a second signal a password saved once could
    // never be removed - and none is the correct setting for a database
    // reachable only from Core.
    it('the checkbox is what says there is no password', () => {
        expect(card).toContain('This database has no password');
        expect(card).toContain('noPassword: v');
    });

    // Ticking it empties the field too, so the request cannot carry a stale
    // value alongside the flag that contradicts it.
    it('ticking it clears the field, so the two cannot disagree', () => {
        expect(card).toMatch(/noPassword: v,\s*\.\.\.\(v \? \{ password: '' \} : \{\}\)/);
        expect(card).toContain('disabled={!!target.noPassword}');
    });

    // The box describes what is SAVED, not what was last typed: it is derived
    // from the server's passwordSet on load and again after every save.
    it('reflects the stored state on load and after saving', () => {
        const derivations = card.match(/noPassword: !\w+(?:\.\w+)*\.passwordSet/g) ?? [];
        expect(
            derivations.length,
            'the checkbox must be re-derived after a save too, or it shows the pre-save state',
        ).toBe(2);
    });
});
