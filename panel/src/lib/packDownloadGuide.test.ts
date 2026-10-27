import { describe, expect, it } from 'vitest';
import { packServerGuide } from './packDownloadGuide';

const build = { minecraft: '1.20.1', loader: 'forge', loaderVersion: '47.4.28' };
describe('pack server installation guides', () => {
    it('pins Fabric installation to the Minecraft and loader versions', () => {
        const guide = packServerGuide({ ...build, loader: 'fabric', loaderVersion: '0.16.10' }, 'linux');
        expect(guide.install).toContain('-mcversion 1.20.1 -loader 0.16.10 -downloadMinecraft');
        expect(guide.start).toContain('fabric-server-launch.jar');
        expect(guide.java).toBe('Java 17');
    });
    it('uses the generated launch script for each host OS', () => {
        expect(packServerGuide(build, 'windows').start).toBe('.\\run.bat nogui');
        expect(packServerGuide({ ...build, loader: 'neoforge' }, 'linux').start).toBe('bash ./run.sh nogui');
        expect(packServerGuide({ ...build, minecraft: '1.16.5' }, 'linux').start).toBe('');
    });
    it('recommends the matching Java runtime across Minecraft numbering schemes', () => {
        for (const [minecraft, java] of [['1.16.5', 'Java 8'], ['1.17.1', 'Java 16'], ['1.20.5', 'Java 21'], ['26.1', 'Java 25']]) {
            expect(packServerGuide({ ...build, minecraft }, 'windows').java).toBe(java);
        }
    });
    it('does not construct shell commands from untrusted version metadata', () => {
        for (const value of ['1.20.1; curl evil', '$(whoami)', '1.20.1\nstop', '../1.20.1', '1.20.1&calc', '1.20.1"']) {
            for (const field of ['minecraft', 'loaderVersion']) {
                const guide = packServerGuide({ ...build, [field]: value }, 'linux');
                expect(guide.supported).toBe(false);
                expect(guide.install).toBe('');
                expect(guide.start).toBe('');
            }
        }
        expect(packServerGuide({ ...build, loader: 'unknown' }, 'windows').supported).toBe(false);
    });
});
