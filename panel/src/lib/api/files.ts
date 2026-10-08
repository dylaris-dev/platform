import { API_URL, getAuthHeader, handleResponse, handleError } from './core';

function uuidParam(serverUuid?: string): string {
    return serverUuid ? `&server_uuid=${encodeURIComponent(serverUuid)}` : '';
}

function uuidQuery(serverUuid?: string): string {
    return serverUuid ? `?server_uuid=${encodeURIComponent(serverUuid)}` : '';
}

export async function getFiles(path: string, serverUuid?: string) {
    try {
        const res = await fetch(`${API_URL}/files?path=${encodeURIComponent(path)}${uuidParam(serverUuid)}`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function getFileContent(path: string, serverUuid?: string) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 30000);
    try {
        const res = await fetch(`${API_URL}/files/content?path=${encodeURIComponent(path)}${uuidParam(serverUuid)}`, {
            headers: getAuthHeader(),
            signal: controller.signal,
        });
        clearTimeout(timer);
        return handleResponse(res);
    } catch (err) {
        clearTimeout(timer);
        if (err instanceof Error && err.name === 'AbortError') {
            return { success: false, message: 'Timeout: file could not be loaded.' };
        }
        return handleError(err);
    }
}

export async function saveFile(path: string, content: string, serverUuid?: string) {
    try {
        const res = await fetch(`${API_URL}/files/save${uuidQuery(serverUuid)}`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ path, content }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function createFile(path: string, isDir: boolean, serverUuid?: string) {
    try {
        const res = await fetch(`${API_URL}/files/create${uuidQuery(serverUuid)}`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ path, is_dir: isDir }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function deleteFile(path: string, serverUuid?: string) {
    try {
        const res = await fetch(`${API_URL}/files/delete${uuidQuery(serverUuid)}`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ path }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function renameFile(oldPath: string, newPath: string, serverUuid?: string) {
    try {
        const res = await fetch(`${API_URL}/files/rename${uuidQuery(serverUuid)}`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ oldPath, newPath }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function copyFile(oldPath: string, newPath: string, serverUuid?: string) {
    try {
        const res = await fetch(`${API_URL}/files/copy${uuidQuery(serverUuid)}`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ oldPath, newPath }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export function uploadFiles(path: string, files: FileList, onProgress: (p: number) => void, strategy?: string, mergeStrategy?: string, serverUuid?: string) {
    return new Promise<{ success: boolean; message: string }>((resolve) => {
        const xhr = new XMLHttpRequest();
        const formData = new FormData();
        formData.append('path', path);
        if (strategy) formData.append('strategy', strategy);
        if (mergeStrategy) formData.append('merge_conflict_strategy', mergeStrategy);
        if (serverUuid) formData.append('server_uuid', serverUuid);
        for (let i = 0; i < files.length; i++) formData.append('files', files[i]);

        xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(Math.round((e.loaded / e.total) * 100)); };
        xhr.onload = () => {
            if (xhr.status >= 200 && xhr.status < 300) {
                try { resolve(JSON.parse(xhr.responseText)); }
                catch { resolve({ success: false, message: 'Bad upload response' }); }
            } else {
                // Surface the server's reason (a quota, size-cap or disk-limit
                // rejection carries a {success:false, message} body with a real
                // status like 413). Without this the user only ever sees the
                // generic text and never learns why the upload was refused.
                try {
                    const body = JSON.parse(xhr.responseText);
                    resolve({ success: false, message: body.message || 'Upload failed' });
                } catch {
                    resolve({ success: false, message: 'Upload failed' });
                }
            }
        };
        xhr.onerror = () => resolve({ success: false, message: 'Connection error' });

        // server_uuid in the query too: Core checks the caller before it reads
        // the body, and the form is the body.
        const query = serverUuid ? `?server_uuid=${encodeURIComponent(serverUuid)}` : '';
        xhr.open('POST', `${API_URL}/files/upload${query}`, true);
        // No Authorization header: XMLHttpRequest sends cookies on same-origin
        // requests by default, and the session is a cookie now.
        xhr.send(formData);
    });
}

export function uploadLibraryFiles(path: string, files: FileList, onProgress: (p: number) => void) {
    return new Promise<{ success: boolean; message: string }>((resolve) => {
        const xhr = new XMLHttpRequest();
        const formData = new FormData();
        formData.append('path', path);
        for (let i = 0; i < files.length; i++) formData.append('files', files[i]);

        xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(Math.round((e.loaded / e.total) * 100)); };
        xhr.onload = () => {
            if (xhr.status >= 200 && xhr.status < 300) {
                try { resolve(JSON.parse(xhr.responseText)); }
                catch { resolve({ success: false, message: 'Bad upload response' }); }
            } else {
                // Surface the server's reason (e.g. the storage-reach gate's
                // taxonomy remedy on a 503) instead of a generic message; see
                // the identical handling in uploadFiles above.
                try {
                    const body = JSON.parse(xhr.responseText);
                    resolve({ success: false, message: body.message || 'Upload failed' });
                } catch {
                    resolve({ success: false, message: 'Upload failed' });
                }
            }
        };
        xhr.onerror = () => resolve({ success: false, message: 'Connection error' });

        xhr.open('POST', `${API_URL}/library/upload`, true);
        // No Authorization header: XMLHttpRequest sends cookies on same-origin
        // requests by default, and the session is a cookie now.
        xhr.send(formData);
    });
}

export function getDownloadUrl(path: string, serverUuid?: string): string {
    const base = `${API_URL}/files/download?path=${encodeURIComponent(path)}`;
    return serverUuid ? `${base}&server_uuid=${encodeURIComponent(serverUuid)}` : base;
}

export async function downloadFile(
    path: string,
    serverUuid?: string,
    _isDir?: boolean,
    onProgress?: (loaded: number, total: number) => void
): Promise<void> {
    const url = getDownloadUrl(path, serverUuid);
    // No ?token=: the download is same-origin, so the session cookie is sent
    // with it. Putting a JWT in a URL was always the thing to avoid - it lands
    // in access logs and in the Referer header - and there is no longer a
    // reason to.
    const res = await fetch(url);
    if (!res.ok) throw new Error(`Download failed: ${res.status}`);

    const total = parseInt(res.headers.get('Content-Length') || '0', 10);
    const disposition = res.headers.get('Content-Disposition') || '';
    const filenameMatch = disposition.match(/filename="([^"]+)"/);
    const filename = filenameMatch?.[1] || path.split('/').pop() || 'download';

    const reader = res.body!.getReader();
    const chunks: BlobPart[] = [];
    let loaded = 0;
    while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        if (value) {
            chunks.push(value);
            loaded += value.length;
            onProgress?.(loaded, total);
        }
    }

    const blob = new Blob(chunks);
    const blobUrl = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = blobUrl;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(blobUrl);
}

export async function selectiveDownload(
    basePath: string,
    selectedPaths: string[],
    selectAll: boolean,
    serverUuid?: string,
    onProgress?: (loaded: number, total: number) => void
): Promise<void> {
    const params = new URLSearchParams();
    if (serverUuid) params.set('server_uuid', serverUuid);
    params.set('base_path', basePath);
    if (selectAll) {
        params.set('select_all', 'true');
    } else {
        for (const p of selectedPaths) params.append('selected', p);
    }

    const url = `${API_URL}/files/download/selective?${params.toString()}`;

    const res = await fetch(url);
    if (!res.ok) throw new Error(`Download failed: ${res.status}`);

    const total = parseInt(res.headers.get('Content-Length') || '0', 10);
    const reader = res.body!.getReader();
    const chunks: BlobPart[] = [];
    let loaded = 0;
    while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        if (value) {
            chunks.push(value);
            loaded += value.length;
            onProgress?.(loaded, total);
        }
    }

    const blob = new Blob(chunks);
    const blobUrl = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = blobUrl;
    a.download = 'download.zip';
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(blobUrl);
}

export function getLibraryDownloadUrl(path: string): string {
    return `${API_URL}/library/download?path=${encodeURIComponent(path)}`;
}
