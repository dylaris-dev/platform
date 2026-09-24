import { API_URL, getAuthHeader, handleResponse, handleError } from './core';

export interface Notification {
    id: number;
    userId: string;
    type: string;
    title: string;
    body: string;
    link?: string;
    readAt?: string;
    createdAt: string;
}

export async function listNotifications(unreadOnly = false, limit = 50) {
    try {
        const params = new URLSearchParams();
        if (unreadOnly) params.set('unread_only', '1');
        if (limit) params.set('limit', String(limit));
        const res = await fetch(`${API_URL}/notifications?${params.toString()}`, { headers: getAuthHeader() });
        // A 503 used to mean "the ticket feature is off" and was reported as an
        // empty inbox. The inbox is no longer gated on that feature, so a 503
        // here is Core being unreachable - reporting that as "you have no
        // notifications" would hide an outage behind a quiet, plausible answer.
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function getUnreadCount() {
    try {
        const res = await fetch(`${API_URL}/notifications/unread-count`, { headers: getAuthHeader() });
        // See listNotifications: a 503 is no longer "the feature is off", so it
        // is not answered with a confident zero.
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function markNotificationRead(id: number) {
    try {
        const res = await fetch(`${API_URL}/notifications/${id}/read`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function markAllNotificationsRead() {
    try {
        const res = await fetch(`${API_URL}/notifications/read-all`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
