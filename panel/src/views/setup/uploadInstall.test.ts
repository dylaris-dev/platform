import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const SRC = path.resolve(path.dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')), '../..');
const read = (p: string) => readFileSync(path.join(SRC, p), 'utf8').replace(/\r\n/g, '\n');

describe('upload install from the Setup tab', () => {
    const setup = read('views/SetupView.tsx');
    const branch = setup.slice(
        setup.indexOf("} else if (installTab === 'upload' && uploadFile) {"),
        setup.indexOf("installer.type = 'upload';"),
    );

    // Uploaded when picked, so Install only queues the old request: an old
    // node takes it unchanged.
    it('does not upload again on Install and sends the old request', () => {
        expect(branch).toContain("installer.type = 'upload-zip';");
        expect(branch).toContain('installer.structure = uploadStructure;');
        expect(branch).toContain('if (!uploadStaged || uploadTarget !== sanitized)');
        expect(branch).not.toContain('uploadFiles');
        expect(branch).not.toContain('installer.path');
    });

    it('uploads straight to <sub>/.upload.zip', () => {
        expect(setup).toContain('dt.items.add(new File([file], UPLOAD_ZIP_NAME, { type: file.type }));');
        expect(setup).toContain('return createBeamAdapter().uploadFiles(sub, dt.files, onProgress, undefined, undefined, server.uuid);');
    });

    it('locks the name while an upload goes to it, and Change discards it', () => {
        expect(setup).toContain('subNameLocked: !!uploadTarget,');
        expect(setup).toContain('onSubNameUnlock: clearUpload,');
        expect(setup).toMatch(/const clearUpload = \(\) => \{\s*stager\.discard\(\);/);
        const wizard = read('views/setup/SetupNewWizard.tsx');
        expect(wizard).toContain('disabled={props.subNameLocked}');
        expect(wizard).toMatch(/\{props\.subNameLocked && \(\s*<button type="button" onClick=\{props\.onSubNameUnlock\}/);
    });

    it('holds the archive while the install request is out', () => {
        const hold = setup.indexOf('if (holdsUpload) stager.beginInstall();');
        const send = setup.indexOf('res = await setupServer(server.id, {');
        expect(hold).toBeGreaterThan(-1);
        expect(send).toBeGreaterThan(hold);
        expect(setup).toContain('if (holdsUpload) stager.endInstall(!!res.success);');
        expect(setup).toContain('if (holdsUpload) stager.endInstall(false);');
    });
});
