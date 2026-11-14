import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import {
    USERNAME, EMAIL, SERVER_NAME, SUB_SERVER_NAME,
    PACK_SLUG, NODE_LABEL, LOCATION_NAME, MC_VERSION,
} from './validation';
import { DOCKER_NETWORK_NAME, DIGITS_AND_PUNCT } from './gatewayProvider';

/**
 * validation.ts says it is "kept in lockstep with the Go source of truth in
 * platform/pkg/validate". Nothing enforced that, and a claim of equality with
 * no check is a claim about the day it was written.
 *
 * Drift here is quiet in both directions. Looser on the panel and the form
 * accepts what Core rejects, so the user gets a server-side error on a field
 * that showed a green tick. Stricter and the panel refuses input the backend
 * would have taken. Neither shows up in a type check or a build.
 *
 * The comparison is textual because it can be: every mirrored pattern is
 * byte-identical between the Go backtick literal and the JS regex body today.
 * If a future pattern legitimately needs different escaping, normalise it here
 * rather than dropping it from the map.
 */

const VALIDATE_GO = path.resolve(__dirname, '../../..', 'pkg', 'validate', 'validate.go');

/** Go identifier -> the panel constant that mirrors it. */
const MIRRORED: Record<string, RegExp> = {
    Username: USERNAME,
    Email: EMAIL,
    ServerName: SERVER_NAME,
    SubServerName: SUB_SERVER_NAME,
    Slug: PACK_SLUG,
    Label: NODE_LABEL,
    LocationName: LOCATION_NAME,
    McVersion: MC_VERSION,
    // The managed link's Docker network (gatewayProvider.ts isDockerNetworkName).
    dockerNetworkName: DOCKER_NETWORK_NAME,
    digitsAndPunct: DIGITS_AND_PUNCT,
};

/**
 * Patterns the panel deliberately does not mirror, each with the reason. This
 * exists so that ADDING a regex to validate.go fails here until someone decides
 * which list it belongs on - the alternative is a new backend rule the panel
 * silently never learns about.
 */
const NOT_MIRRORED: Record<string, string> = {
    ServerUUID: 'server ids are minted by the backend, never typed into a form',
    MinecraftUsername: 'no form takes a Mojang name; player management reads them from the server',
    serverNameStrip: 'the complement of ServerName, inlined in sanitizeServerName (whose behaviour is tested separately)',
};

function goPatterns(): Map<string, string> {
    let src: string;
    try {
        src = readFileSync(VALIDATE_GO, 'utf8');
    } catch (e) {
        throw new Error(
            `cannot read ${VALIDATE_GO}: ${e}. This test compares the panel's rules against the Go ones; ` +
            `if the layout moved, fix the path rather than skipping the comparison.`,
        );
    }
    const out = new Map<string, string>();
    const re = /^\s*(\w+)\s*=\s*regexp\.MustCompile\(`([^`]*)`\)/gm;
    let m: RegExpExecArray | null;
    while ((m = re.exec(src)) !== null) out.set(m[1], m[2]);
    return out;
}

describe('validation.ts mirrors pkg/validate', () => {
    const patterns = goPatterns();

    it('finds the Go patterns at all', () => {
        // A silent zero-match extraction would make every assertion below pass
        // while comparing nothing.
        expect(patterns.size).toBeGreaterThanOrEqual(Object.keys(MIRRORED).length);
    });

    for (const [goName, panelRe] of Object.entries(MIRRORED)) {
        it(`${goName} matches`, () => {
            const goPattern = patterns.get(goName);
            expect(goPattern, `validate.go no longer defines ${goName}`).toBeDefined();
            expect(panelRe.source).toBe(goPattern);
        });
    }

    it('every Go pattern is either mirrored or listed as deliberately not', () => {
        const accounted = new Set([...Object.keys(MIRRORED), ...Object.keys(NOT_MIRRORED)]);
        const strays = [...patterns.keys()].filter(n => !accounted.has(n));
        expect(
            strays,
            `validate.go gained ${strays.join(', ')}. Mirror it in validation.ts or add it to NOT_MIRRORED with the reason.`,
        ).toEqual([]);
    });
});
