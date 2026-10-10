import { describe, expect, it } from 'vitest';
import { playerActionToast } from './playerActionResult';

describe('playerActionToast', () => {
    it.each([
        ['reply shown', { success: true, output: 'Kicked Steve' }, 'Kick: Kicked Steve', true],
        ['no-op reply shown as is', { success: true, output: 'Player is already whitelisted\n' }, 'Kick: Player is already whitelisted', true],
        ['colour codes stripped', { success: true, output: '§aKicked §lSteve' }, 'Kick: Kicked Steve', true],
        ['empty reply', { success: true, output: '' }, 'Kick: done', true],
        ['refusal reason shown', { success: false, error: 'That player does not exist' }, 'Kick: That player does not exist', false],
        ['multi-line refusal cut to its first line', { success: false, error: 'Unknown or incomplete command, see below for error\nkick X<--[HERE]' }, 'Kick: Unknown or incomplete command, see below for error', false],
        ['refusal without reason', { success: false }, 'Kick: failed', false],
    ])('%s', (_name, res, text, ok) => {
        expect(playerActionToast('Kick', res)).toEqual({ text, ok });
    });
});
