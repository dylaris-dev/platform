import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

// confirmDialog replaced window.confirm across the panel and its own doc comment
// counts "~19 call sites spread over 14 files". These three were missed by that
// sweep, and they are one-click trash icons with no undo:
//
//   Warp region  - warp_leaders.region is REFERENCES warp_regions(region) ON
//                  DELETE CASCADE, so deleting a region deletes every leader
//                  endpoint in it. The button's title already said so.
//   Warp leader  - the endpoint has to be typed back in by hand.
//   Access grant - the trash sits next to Edit in the same row, and revoking
//                  drops the capabilities configured on the grant.
//
// Deliberately NOT listed here, so the omissions stay visible as decisions:
// revokeEnrollToken and revokeShareLink. Both are re-mintable in one click and
// revocation is the safe direction, so an accidental one costs nothing
// permanent.

const SRC = join(__dirname, '..', '..');

const MUST_CONFIRM: { file: string; call: string }[] = [
    { file: 'components/settings/WarpTab.tsx', call: 'deleteWarpRegion(' },
    { file: 'components/settings/WarpTab.tsx', call: 'deleteWarpLeader(' },
    { file: 'app/(authed)/access/page.tsx', call: 'revokeGrant(' },
];

describe('a destructive one-click action asks first', () => {
    for (const { file, call } of MUST_CONFIRM) {
        it(`${call.replace('(', '')} is guarded in ${file}`, () => {
            const source = readFileSync(join(SRC, file), 'utf8');
            const at = source.indexOf(`await ${call}`);
            expect(at, `${call} not found in ${file}`).toBeGreaterThan(-1);
            // The confirm has to be in the same handler, above the call - not
            // merely somewhere in the file, which a neighbouring handler's
            // dialog would satisfy.
            //
            // Match the CALL, not the identifier: the first version of this
            // test looked for the string "confirmDialog" and stayed green when
            // the guard was deleted, because the comment left behind above the
            // call mentions it by name. A control that leaves the test green
            // means the test is weak.
            const handlerHead = source.slice(Math.max(0, at - 700), at);
            expect(handlerHead).toMatch(/await confirmDialog\(\{/);
        });
    }

    it('the warp region dialog names the cascade rather than asking a bare "are you sure"', () => {
        // The whole point of the message is that the region is not the only
        // thing that disappears.
        const source = readFileSync(join(SRC, 'components/settings/WarpTab.tsx'), 'utf8');
        expect(source).toMatch(/Every leader endpoint in it is deleted with it/);
    });

    it('confirmDialog still fails closed when nothing is mounted', () => {
        // Every guard above is only worth as much as this: with no
        // ConfirmDialogRoot mounted it must resolve false, not true.
        const dialog = readFileSync(join(__dirname, 'ConfirmDialog.tsx'), 'utf8');
        expect(dialog).toMatch(/if \(!publish\)[\s\S]{0,200}Promise\.resolve\(false\)/);
    });

    it('only the confirm button resolves true, never Escape, Cancel or the backdrop', () => {
        // The Alert Dialog reports how it closed through returnValue; anything
        // but the one button carrying value="confirm" must read as a refusal.
        const dialog = readFileSync(join(__dirname, 'ConfirmDialog.tsx'), 'utf8');
        expect(dialog).toMatch(/onOpenChange=\{\(open, details\) => \{ if \(!open\) close\(details\.returnValue === 'confirm'\); \}\}/);
        expect(dialog.match(/value="confirm"/g)).toHaveLength(1);
    });
});
