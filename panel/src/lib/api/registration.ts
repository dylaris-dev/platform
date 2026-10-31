import { API_URL, handleResponse, handleError } from './core';

export interface RegistrationStatus {
    success: boolean;
    registrationEnabled: boolean;
    emailVerifyRequired: boolean;
    passwordMinLength: number;
    // Base URL of the operator's legal pages; only on the hosted panel.
    legalBaseUrl?: string;
}

// Public — no auth header required. Login page calls this before render to
// decide whether to show the "Register" link.
export async function getRegistrationStatus() {
    try {
        const res = await fetch(`${API_URL}/auth/registration-status`);
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export interface RegisterPayload {
    username: string;
    email: string;
    password: string;
    securityQuestions?: { question: string; answer: string }[];
}

export async function register(payload: RegisterPayload) {
    try {
        const res = await fetch(`${API_URL}/auth/register`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function verifyEmail(token: string) {
    try {
        const res = await fetch(`${API_URL}/auth/verify-email`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ token }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function resendVerification(email: string) {
    try {
        const res = await fetch(`${API_URL}/auth/resend-verification`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ email }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// Password reset — all public, enumeration-safe at the API layer.

export async function forgotPassword(email: string) {
    try {
        const res = await fetch(`${API_URL}/auth/forgot-password`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ email }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function validateResetToken(token: string) {
    try {
        const res = await fetch(`${API_URL}/auth/validate-reset-token`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ token }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function resetPassword(token: string, password: string, securityAnswers?: string[]) {
    try {
        const body: Record<string, unknown> = { token, password };
        if (securityAnswers && securityAnswers.length > 0) body.securityAnswers = securityAnswers;
        const res = await fetch(`${API_URL}/auth/reset-password`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
