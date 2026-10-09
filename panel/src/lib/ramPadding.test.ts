import { describe, it, expect } from 'vitest';
import {
    parseRequiredRamPadding, resolveRamPadding, inheritedRamPadding, ramPaddingPatch, parseRamPaddingInput, resetLabel, isValidRamPadding,
} from './ramPadding';

describe('resolveRamPadding', () => {
    it('takes the first level that is set, 0 included', () => {
        expect(resolveRamPadding(1024, 768, 640)).toEqual({ value: 1024, source: 'server' });
        expect(resolveRamPadding(0, 768, 640)).toEqual({ value: 0, source: 'server' });
        expect(resolveRamPadding(null, 768, 640)).toEqual({ value: 768, source: 'node' });
        expect(resolveRamPadding(undefined, 0, 640)).toEqual({ value: 0, source: 'node' });
        expect(resolveRamPadding(null, null, 640)).toEqual({ value: 640, source: 'global' });
        expect(resolveRamPadding(null, null, 0)).toEqual({ value: 0, source: 'global' });
        expect(resolveRamPadding(null, undefined, undefined)).toEqual({ value: 512, source: 'global' });
    });

    it('inherits without the server level', () => {
        expect(inheritedRamPadding(null, 300)).toEqual({ value: 300, source: 'global' });
    });
});

describe('ramPaddingPatch', () => {
    it('sends nothing when unchanged', () => {
        expect(ramPaddingPatch(null, null)).toEqual({});
        expect(ramPaddingPatch(1024, 1024)).toEqual({});
    });
    it('sets an override, 0 included', () => {
        expect(ramPaddingPatch(null, 1024)).toEqual({ ramPaddingMb: 1024 });
        expect(ramPaddingPatch(1024, 0)).toEqual({ ramPaddingMb: 0 });
    });
    it('resets to inherit, never both fields', () => {
        expect(ramPaddingPatch(1024, null)).toEqual({ resetRamPadding: true });
    });
});

describe('parseRamPaddingInput', () => {
    it('reads empty as inherit and rejects what Core refuses', () => {
        expect(parseRamPaddingInput('')).toBeNull();
        expect(parseRamPaddingInput(' 0 ')).toBe(0);
        expect(parseRamPaddingInput('16384')).toBe(16384);
        expect(parseRamPaddingInput('16385')).toBeNaN();
        expect(parseRamPaddingInput('-1')).toBeNaN();
        expect(parseRamPaddingInput('1.5')).toBeNaN();
        expect(parseRamPaddingInput('abc')).toBeNaN();
        expect(isValidRamPadding(512)).toBe(true);
    });
});

describe('parseRequiredRamPadding', () => {
    it('never turns an empty global field into 0', () => {
        expect(parseRequiredRamPadding('')).toBeNaN();
        expect(parseRequiredRamPadding('  ')).toBeNaN();
        expect(parseRequiredRamPadding('abc')).toBeNaN();
        expect(parseRequiredRamPadding('0')).toBe(0);
        expect(parseRequiredRamPadding('768')).toBe(768);
    });
});

describe('resetLabel', () => {
    it('names the level and value', () => {
        expect(resetLabel('node', 768)).toBe('Use node default (768 MB)');
        expect(resetLabel('global', 512)).toBe('Use global default (512 MB)');
    });
});
