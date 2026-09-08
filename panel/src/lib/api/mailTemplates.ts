import { API_URL, getAuthHeader } from '@/lib/api/core';

/**
 * Outgoing mail templates.
 *
 * Every call is settings.write, reading included: a template body is the exact
 * wording of a password-reset mail, which is a phishing kit for whoever can
 * read it.
 *
 * Each path is written out in full rather than assembled from a prefix. That is
 * not style: routesExist.test.ts reads these paths out of the source and checks
 * every one against API.md, and a path built by concatenation is invisible to
 * it - which is exactly how a typo becomes a 404 in front of a user.
 */

export interface MailVariable {
    name: string;
    description: string;
    example: string;
}

export interface MailTemplate {
    key: string;
    name: string;
    description: string;
    purpose: string;
    variables: MailVariable[];
    /** The wording this release ships with. */
    subject: string;
    body: string;
    /** Whether this install has changed it. */
    edited: boolean;
    /** What WILL be sent - the override if there is one, otherwise the default. */
    currentSubject: string;
    currentBody: string;
}

export interface MailPreview {
    subject: string;
    text: string;
    html: string;
}

async function send(url: string, init?: RequestInit) {
    try {
        const res = await fetch(url, {
            ...init,
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json', ...(init?.headers || {}) },
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) return { success: false, message: data.message || data.error || `Request failed (${res.status})` };
        return data;
    } catch (err) {
        return { success: false, message: err instanceof Error ? err.message : 'Network error' };
    }
}

export const listMailTemplates = () =>
    send(`${API_URL}/admin/mail/templates`);

export const saveMailTemplate = (key: string, subject: string, body: string) =>
    send(`${API_URL}/admin/mail/templates/${encodeURIComponent(key)}`, {
        method: 'PUT', body: JSON.stringify({ subject, body }),
    });

/** Reset is a delete: the row goes and the built-in wording applies again. */
export const resetMailTemplate = (key: string) =>
    send(`${API_URL}/admin/mail/templates/${encodeURIComponent(key)}`, { method: 'DELETE' });

/** Previews the text being EDITED, not the saved one. */
export const previewMailTemplate = (key: string, subject: string, body: string) =>
    send(`${API_URL}/admin/mail/templates/${encodeURIComponent(key)}/preview`, {
        method: 'POST', body: JSON.stringify({ subject, body }),
    });

/** Sends with EXAMPLE values, never a real link. Defaults to your own address. */
export const testSendMailTemplate = (key: string, to?: string) =>
    send(`${API_URL}/admin/mail/templates/${encodeURIComponent(key)}/test`, {
        method: 'POST', body: JSON.stringify({ to: to || '' }),
    });
