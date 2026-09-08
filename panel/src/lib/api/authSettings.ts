import { API_URL, getAuthHeader, handleResponse, handleError } from './core';

export interface AuthPolicy {
    registrationEnabled: boolean;
    emailVerifyRequired: boolean;
    passwordMinLength: number;
    defaultNewUserAllRegions: boolean;
    // 2FA enforcement
    require2FAForAdmins: boolean;
    require2FAForAllUsers: boolean;
    // password reset link lifetime (minutes, 5–1440)
    passwordResetLinkTTLMinutes: number;
    // security questions
    securityQuestionsEnabled: boolean;
    securityQuestionsRequiredAtSignup: boolean;
    securityQuestionsRequiredAtReset: boolean;
    securityQuestionsCount: number;
    // auto-delete inactive users
    inactiveDeleteEnabled: boolean;
    inactiveDaysBeforeDelete: number;
    historyGraceExtraDays: number;
    deleteEmailWarningDays: number;
    deletionMode: 'anonymize' | 'hard_delete';
}

export type MailProvider = 'smtp' | 'resend';

export interface SMTPConfig {
    /** Which transport actually sends. Always present on a GET; on a PUT,
     *  omitting it means "leave the stored provider alone" - Core reads it as a
     *  pointer for exactly that reason. */
    provider: MailProvider;
    host: string;
    port: number;
    username: string;
    /** Write-only — never present on GET responses. */
    password?: string;
    /** Shared by BOTH providers: the sender is a property of the mail
     *  configuration, not of the protocol, so switching does not ask the
     *  operator to retype their own address. */
    fromEmail: string;
    fromName: string;
    /** "none" | "starttls" | "tls" */
    encryption: string;
    /** GET-only flag: true when a password is already stored.
     *  Lets the UI show "leave blank to keep" placeholder. */
    passwordSet?: boolean;
    /** Write-only, same rule as the password: blank keeps the stored one. */
    resendApiKey?: string;
    resendApiKeySet?: boolean;
}

export async function getAuthPolicy() {
    try {
        const res = await fetch(`${API_URL}/admin/settings/auth`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function saveAuthPolicy(policy: AuthPolicy) {
    try {
        const res = await fetch(`${API_URL}/admin/settings/auth`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(policy),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// Demo account: the single read-only user that sees the demo servers.
export async function getDemoAccount() {
    try {
        const res = await fetch(`${API_URL}/admin/settings/demo-account`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function setDemoAccount(username: string) {
    try {
        const res = await fetch(`${API_URL}/admin/settings/demo-account`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ username }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function getSMTPConfig() {
    try {
        const res = await fetch(`${API_URL}/admin/settings/smtp`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function saveSMTPConfig(config: SMTPConfig) {
    try {
        const res = await fetch(`${API_URL}/admin/settings/smtp`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(config),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function testSendSMTP(to?: string, signal?: AbortSignal) {
    try {
        const res = await fetch(`${API_URL}/admin/settings/smtp/test`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ to: to || '' }),
            signal,
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
