import { describe, expect, it } from 'vitest';
import { packTargetPathError, packUploadTarget } from './packUpload';

describe('config and KubeJS upload paths', () => {
    it('preserves recognized folders rather than flattening or doubling config paths', () => {
        expect(packUploadTarget({ name: 'recipes.js', webkitRelativePath: 'kubejs/server_scripts/recipes.js' }, 'config')).toBe('kubejs/server_scripts/recipes.js');
        expect(packUploadTarget({ name: 'tconstruct.toml', webkitRelativePath: 'pack/defaultconfigs/tconstruct.toml' }, 'config')).toBe('defaultconfigs/tconstruct.toml');
        expect(packUploadTarget({ name: 'example.toml', webkitRelativePath: 'config/forge/example.toml' }, 'config')).toBe('config/forge/example.toml');
    });

    it('uses an explicit prefix for individual files or subfolders without a Minecraft root', () => {
        expect(packUploadTarget({ name: 'recipes.js' }, 'kubejs/server_scripts/')).toBe('kubejs/server_scripts/recipes.js');
        expect(packUploadTarget({ name: 'test.toml', webkitRelativePath: 'forge/test.toml' }, 'config')).toBe('config/forge/test.toml');
    });

    it.each(['../secret', '/etc/passwd', 'C:/world/file', 'config\\file', 'config/../mods/file.jar', 'config//file', 'config/./file', 'config/\x00file'])('rejects unsafe target %j', path => {
        expect(packTargetPathError(path)).not.toBeNull();
    });

    it('accepts a Minecraft-relative KubeJS destination', () => {
        expect(packTargetPathError('kubejs/server_scripts/recipes.js')).toBeNull();
    });
});
