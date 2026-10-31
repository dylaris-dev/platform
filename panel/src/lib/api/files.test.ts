import { describe, it, expect } from 'vitest';
import { API_URL } from './core';
import { getDownloadUrl } from './files';

describe('getDownloadUrl', () => {
    it('builds URL without server UUID', () => {
        const url = getDownloadUrl('/server.jar');
        expect(url).toBe(`${API_URL}/files/download?path=%2Fserver.jar`);
    });

    it('appends server_uuid when provided', () => {
        const url = getDownloadUrl('/file.txt', 'abc-123');
        expect(url).toBe(`${API_URL}/files/download?path=%2Ffile.txt&server_uuid=abc-123`);
    });

    it('encodes special characters in path', () => {
        const url = getDownloadUrl('/my folder/file name.txt');
        expect(url).toContain(encodeURIComponent('/my folder/file name.txt'));
    });

    it('encodes special characters in server UUID', () => {
        const url = getDownloadUrl('/f.txt', 'uuid with spaces');
        expect(url).toContain('server_uuid=uuid%20with%20spaces');
    });
});
