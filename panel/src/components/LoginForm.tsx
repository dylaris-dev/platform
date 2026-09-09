"use client";

import React, { useState, useRef, useEffect } from 'react';
import { useRouter } from 'next/navigation';
import Link from 'next/link';
import { login } from '../lib/api/auth';
import { getRegistrationStatus, resendVerification } from '../lib/api/registration';
import { ShieldCheck, ArrowLeft, MailCheck } from 'lucide-react';
import { navigateAfterLogin, popLoginRedirect } from '@/lib/postLogin';

type Step = 'credentials' | '2fa' | 'verify-needed';

export default function LoginForm() {
  const [step, setStep] = useState<Step>('credentials');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [totpCode, setTotpCode] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [registrationEnabled, setRegistrationEnabled] = useState(false);
  const [unverifiedEmail, setUnverifiedEmail] = useState('');
  const [resending, setResending] = useState(false);
  const [resendStatus, setResendStatus] = useState('');
  const router = useRouter();
  const totpRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (step === '2fa') totpRef.current?.focus();
  }, [step]);

  // Public status probe — drives the "Register" link. Silent failure: if the
  // status endpoint is unreachable we simply hide the link.
  useEffect(() => {
    getRegistrationStatus().then(res => {
      if (res.success) setRegistrationEnabled(!!res.registrationEnabled);
    }).catch(() => {});
  }, []);

  // The single exit for every way of signing in - credentials and 2FA both
  // land here - which is why the Beam-vs-browser navigation rule only has to
  // be applied once. See navigateAfterLogin for why it is not always a push.
  const finish = (token: string) => {
    void token;
    navigateAfterLogin(popLoginRedirect(), router.push);
  };

  const handleCredentialsSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      const result = await login(username, password);
      if (result.success) {
        finish(result.token);
      } else if (result.requires2FA) {
        setStep('2fa');
      } else if (result.requires2FASetup) {
        // Policy demands 2FA but the user hasn't enrolled. Stash the short-lived
        // setup token in sessionStorage (not localStorage — it expires in 15
        // minutes and shouldn't outlive the tab) and route to the forced setup.
        sessionStorage.setItem('setupToken', result.setupToken || '');
        if (result.setupTokenExpires) {
          sessionStorage.setItem('setupTokenExpires', String(result.setupTokenExpires));
        }
        router.push('/setup-2fa');
      } else if (result.requiresVerification) {
        setUnverifiedEmail(result.email || '');
        setStep('verify-needed');
      } else {
        setError(result.message || 'Login failed.');
      }
    } catch {
      setError('Could not connect to the server.');
    } finally {
      setLoading(false);
    }
  };

  const handle2FASubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      const result = await login(username, password, totpCode.replace(/\s/g, ''));
      if (result.success) {
        finish(result.token);
      } else {
        setError(result.message || 'Invalid 2FA code.');
      }
    } catch {
      setError('Could not connect to the server.');
    } finally {
      setLoading(false);
    }
  };

  const handleResend = async () => {
    if (!unverifiedEmail) return;
    setResending(true);
    setResendStatus('');
    try {
      const res = await resendVerification(unverifiedEmail);
      // Backend silently succeeds in all no-op cases for enumeration safety,
      // so we always show the same confirmation message.
      setResendStatus(res.success ? 'If the address is registered, a new verification email is on its way.' : (res.message || 'Failed to send.'));
    } catch {
      setResendStatus('Could not contact the server.');
    } finally {
      setResending(false);
    }
  };

  const back = () => {
    setStep('credentials');
    setTotpCode('');
    setError('');
    setUnverifiedEmail('');
    setResendStatus('');
  };

  return (
    <div className="card w-full max-w-md p-8">
      <h1 className="text-4xl text-center mb-8 font-logo tracking-widest select-none">
        <span className="text-(--accent-light)">D</span>
        <span className="text-(--base-09)">ylaris</span>
      </h1>

      {error && (
        <div className="alert alert-error mb-4 justify-center text-center font-medium">
          {error}
        </div>
      )}

      {step === 'credentials' && (
        <>
          <form onSubmit={handleCredentialsSubmit} className="space-y-5">
            <div className="flex flex-col gap-[5px]">
              <label htmlFor="username" className="input-label">Username</label>
              <input
                type="text"
                id="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                disabled={loading}
                autoComplete="username"
                className="input-field w-full disabled:opacity-40 disabled:cursor-not-allowed"
              />
            </div>
            <div className="flex flex-col gap-[5px]">
              <label htmlFor="password" className="input-label">Password</label>
              <input
                type="password"
                id="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={loading}
                autoComplete="current-password"
                className="input-field w-full disabled:opacity-40 disabled:cursor-not-allowed"
              />
            </div>
            <button
              type="submit"
              disabled={loading}
              className="btn btn-primary btn-lg w-full mt-2"
            >
              {loading ? 'Authenticating...' : 'Login'}
            </button>
          </form>

          <div className="mt-4 flex flex-col items-center gap-2 text-xs text-(--base-06)">
            <Link href="/forgot-password" className="text-(--base-07) hover:text-(--accent-light) transition-colors">
              Forgot password?
            </Link>
            {registrationEnabled && (
              <span>
                No account yet?{' '}
                <Link href="/register" className="text-(--accent-light) hover:underline">
                  Register
                </Link>
              </span>
            )}
          </div>
        </>
      )}

      {step === '2fa' && (
        <form onSubmit={handle2FASubmit} className="space-y-5">
          <div className="flex items-center gap-2 text-(--accent-light) mb-1">
            <ShieldCheck size={16} />
            <span className="text-sm font-medium">Two-factor required</span>
          </div>
          <p className="text-xs text-(--base-06) -mt-2">
            Enter the 6-digit code from your authenticator app, or one of your single-use backup codes.
          </p>
          <div className="flex flex-col gap-[5px]">
            <label htmlFor="totp" className="input-label">Code</label>
            <input
              ref={totpRef}
              type="text"
              id="totp"
              value={totpCode}
              onChange={(e) => setTotpCode(e.target.value)}
              disabled={loading}
              autoComplete="one-time-code"
              inputMode="text"
              placeholder="123 456"
              className="input-field input-mono w-full text-center tracking-widest text-lg disabled:opacity-40"
            />
          </div>
          <button
            type="submit"
            disabled={loading || totpCode.trim().length < 6}
            className="btn btn-primary btn-lg w-full"
          >
            {loading ? 'Verifying...' : 'Verify & Login'}
          </button>
          <button
            type="button"
            onClick={back}
            disabled={loading}
            className="flex items-center justify-center gap-1.5 w-full text-xs text-(--base-06) hover:text-(--base-08) transition-colors"
          >
            <ArrowLeft size={12} />
            Back to login
          </button>
        </form>
      )}

      {step === 'verify-needed' && (
        <div className="space-y-4">
          <div className="flex items-center gap-2 text-(--accent-light)">
            <MailCheck size={18} />
            <span className="font-medium">Verify your email</span>
          </div>
          <p className="text-sm text-(--base-07)">
            Your account is set up but the email address {unverifiedEmail && (
              <span className="font-mono text-(--base-09)">{unverifiedEmail}</span>
            )} has not been confirmed yet. Click the link in the verification email to finish, or request a new one below.
          </p>
          <button
            type="button"
            onClick={handleResend}
            disabled={resending || !unverifiedEmail}
            className="btn btn-secondary w-full disabled:opacity-40"
          >
            {resending ? 'Sending...' : 'Resend verification email'}
          </button>
          {resendStatus && (
            <p className="text-xs text-(--base-06) text-center">{resendStatus}</p>
          )}
          <button
            type="button"
            onClick={back}
            className="flex items-center justify-center gap-1.5 w-full text-xs text-(--base-06) hover:text-(--base-08) transition-colors"
          >
            <ArrowLeft size={12} />
            Back to login
          </button>
        </div>
      )}
    </div>
  );
}
