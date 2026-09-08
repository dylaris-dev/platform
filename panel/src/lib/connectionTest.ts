"use client";

import { useCallback, useRef, useState } from 'react';

/**
 * One lifecycle for every "Test connection" button in the panel.
 *
 * Each of them used to own its own: a boolean, an await, and a toast. That is
 * three lines to copy and four things to forget, and all four were forgotten
 * somewhere - a button that stayed clickable while its request was in flight, a
 * spinner with no result, a result with no spinner, and a request with no
 * timeout at all, which leaves a button spinning until the tab is closed
 * because `fetch` has no default deadline.
 *
 * The other half of the job is on the server (core/services/conncheck.go): a
 * test now happens in two steps, dial the host first and only then authenticate,
 * so a failure can say WHICH of the two failed. That distinction is the reason
 * this file exists - "Connection failed" is the same sentence for a hostname
 * typo and a wrong password, and only one of them is fixed by retyping the
 * password.
 */

/** How far the attempt got. Mirrors the constants in services/conncheck.go. */
export type ConnStage = 'unreachable' | 'rejected' | 'ok' | 'timeout';

/** The shape every test endpoint answers with. */
export interface ConnTestResponse {
    success: boolean;
    ok?: boolean;
    stage?: string;
    severity?: 'ok' | 'warning' | 'error';
    message?: string;
    warning?: string;
}

export interface ConnTestResult {
    severity: 'ok' | 'warning' | 'error';
    stage: ConnStage;
    /** Three or four words. The message says the rest. */
    heading: string;
    message: string;
}

/**
 * The client's hard stop.
 *
 * Deliberately NOT three seconds, even though three seconds is what the
 * reachability step is bounded by. That budget lives on the server, where the
 * dial happens: a dead host is answered in three seconds and comes back as
 * "not reachable", which is the fast failure worth having. This timeout is the
 * different, rarer case - Core itself stopped answering - and cutting a
 * successful login off at three seconds would report a working database as
 * broken every time it is opening a cold pool or crossing a slow link.
 */
export const CONN_TEST_TIMEOUT_MS = 15000;

/** Matches services.dialBudget, for wording only. */
export const REACH_BUDGET_SECONDS = 3;

/**
 * The mail test is not a connection test: it hands a message to a relay and
 * waits for it to be accepted. A relay that greylists or checks the sender's
 * domain first is slow and correct, so cutting it off at the same deadline as a
 * database handshake would report working configurations as broken.
 */
export const SMTP_TEST_TIMEOUT_MS = 30000;

const HEADINGS: Record<ConnStage, string> = {
    unreachable: 'Not reachable',
    rejected: 'Reached, but refused',
    ok: 'Connected',
    timeout: 'No answer',
};

function stageOf(raw: string | undefined, ok: boolean): ConnStage {
    if (raw === 'unreachable' || raw === 'rejected' || raw === 'ok') return raw;
    // An endpoint that has not been given a stage yet, or an older Core: the
    // honest fallback is "it answered and said no", never "not reachable" -
    // claiming a host is down when nobody checked is worse than saying less.
    return ok ? 'ok' : 'rejected';
}

/**
 * Turns a test endpoint's answer into something renderable.
 *
 * `severity` is not derived from `ok`, because the third state is real: the
 * metrics database can connect perfectly and still be missing TimescaleDB,
 * which is neither a pass to tick green nor a failure to refuse. Endpoints that
 * have no such state simply never send `warning`.
 */
export function readConnTest(res: ConnTestResponse, okMessage: string): ConnTestResult {
    const ok = res.success !== false && res.ok !== false;
    const stage = stageOf(res.stage, ok);
    const severity = res.severity ?? (ok ? (res.warning ? 'warning' : 'ok') : 'error');
    return {
        severity,
        stage,
        heading: severity === 'warning' ? 'Connected, with a caveat' : HEADINGS[stage],
        message: res.warning || res.message || (ok ? okMessage : 'The test failed without saying why.'),
    };
}

export interface ConnectionTest {
    running: boolean;
    result: ConnTestResult | null;
    run: () => void;
    clear: () => void;
}

/**
 * Runs one test at a time, with a deadline and an abort.
 *
 * A second click while one is in flight is ignored rather than queued: the
 * button is disabled for exactly that reason, and a caller that renders it
 * some other way should not be able to stack requests behind it.
 */
export function useConnectionTest(
    run: (signal: AbortSignal) => Promise<ConnTestResult>,
    timeoutMs: number = CONN_TEST_TIMEOUT_MS,
): ConnectionTest {
    const [running, setRunning] = useState(false);
    const [result, setResult] = useState<ConnTestResult | null>(null);
    const inFlight = useRef(false);

    const clear = useCallback(() => setResult(null), []);

    const start = useCallback(() => {
        if (inFlight.current) return;
        inFlight.current = true;
        setRunning(true);
        setResult(null);

        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), timeoutMs);

        run(controller.signal)
            .then(setResult)
            .catch((err: unknown) => {
                // An abort is the timeout firing, and it is the one failure
                // whose message must not blame the target: nothing was learned
                // about it. Anything else is the panel's own fetch failing.
                const aborted = err instanceof DOMException && err.name === 'AbortError';
                setResult(aborted
                    ? {
                        severity: 'error',
                        stage: 'timeout',
                        heading: HEADINGS.timeout,
                        message: `No answer within ${Math.round(timeoutMs / 1000)} seconds, so the test was ` +
                            `stopped. This says nothing about the target - it is Core that did not come back.`,
                    }
                    : {
                        severity: 'error',
                        stage: 'rejected',
                        heading: 'The test could not be run',
                        message: err instanceof Error ? err.message : String(err),
                    });
            })
            .finally(() => {
                clearTimeout(timer);
                inFlight.current = false;
                setRunning(false);
            });
    }, [run, timeoutMs]);

    return { running, result, run: start, clear };
}
