// panel API client for per-server scheduled tasks (cron jobs).
//
// V1 task types:
//   - "restart" — payload unused, dispatches the same pipeline as the Restart power button
//   - "say"     — payload is the chat message, sent as `say <payload>` via console stdin
//
// Cron strings are validated server-side via /scheduled-tasks/validate so the
// panel can preview the next run timestamp without keeping a cron parser
// client-side.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export type ScheduledTaskType = 'restart' | 'say';

export interface ScheduledTask {
    id: number;
    serverId: number;
    name: string;
    taskType: ScheduledTaskType;
    scheduleCron: string;
    payload: string;
    enabled: boolean;
    nextRun?: string;
    lastRun?: string;
    // 'skipped': the server was in no state to take the firing (stopped,
    // in setup, suspended). Not a failure; lastError says why.
    lastStatus: '' | 'ok' | 'error' | 'skipped';
    lastError?: string;
    createdBy?: string;
    createdAt: string;
    updatedAt: string;
}

export interface ScheduledTaskInput {
    name?: string;
    taskType: ScheduledTaskType;
    scheduleCron: string;
    payload?: string;
    enabled?: boolean;
}

export async function listScheduledTasks(serverId: number): Promise<{ success: boolean; tasks?: ScheduledTask[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/scheduled-tasks`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function createScheduledTask(serverId: number, input: ScheduledTaskInput): Promise<{ success: boolean; task?: ScheduledTask; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/scheduled-tasks`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(input),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function updateScheduledTask(serverId: number, taskId: number, input: Partial<ScheduledTaskInput>): Promise<{ success: boolean; task?: ScheduledTask; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/scheduled-tasks/${taskId}`, {
            method: 'PATCH',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(input),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function deleteScheduledTask(serverId: number, taskId: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/scheduled-tasks/${taskId}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// validateCron returns whether the expression parses + the next run if so.
// Single-source-of-truth: the same parser as the executor. The panel uses
// this to surface "Next run: 2026-05-30 04:00 UTC" under the input field.
export async function validateCron(scheduleCron: string): Promise<{ success: boolean; valid: boolean; nextRun?: string; error?: string }> {
    try {
        const res = await fetch(`${API_URL}/scheduled-tasks/validate`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ scheduleCron }),
        });
        return handleResponse(res);
    } catch (err) { return { ...handleError(err), valid: false }; }
}
