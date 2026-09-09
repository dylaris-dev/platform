"use client";

import React, { Suspense, useEffect, useState } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import LoginForm from "@/components/LoginForm";
import { getSetupStatus } from '@/lib/api/setup';
import { demoLogin } from '@/lib/api/auth';
import { hasSession } from '@/lib/api/sessionState';
import { navigateAfterLogin, popLoginRedirect } from '@/lib/postLogin';

function LoginPageInner() {
  const router = useRouter();
  const params = useSearchParams();
  // ?demo=1 starts the read-only demo session here rather than anywhere else.
  // The demo endpoint hands back a session TOKEN, and a token can only be stored
  // by the panel's own origin - so the storefront cannot establish this session
  // itself, and passing the token across in a URL would put a live credential in
  // browser history and every referrer header. It links here instead.
  const wantsDemo = params.get('demo') === '1';
  const [demoError, setDemoError] = useState('');

  useEffect(() => {
    (async () => {
      // Fresh-Install means no users exist at all, so login
      // would 503. Redirect to /setup. Lost-Admin mode still lets
      // non-admin users log in normally — no redirect for that case.
      const status = await getSetupStatus();
      if (status.mode === 'fresh_install') {
        router.replace('/setup');
        return;
      }
      // Already signed in. A push is enough here and inside Beam too: this
      // page IS a document, so the proxy's cookie replay has already run.
      if (hasSession()) {
        router.push(popLoginRedirect());
        return;
      }
      if (wantsDemo) {
        const res = await demoLogin();
        if (res.success) {
          // This one DID establish a session, so it follows the same rule as
          // the sign-in form.
          navigateAfterLogin('/servers', router.replace);
          return;
        }
        // Fall through to the normal form rather than stranding the visitor on
        // an error: they came here to look at the panel, and the sign-in box is
        // still a reasonable thing to land on.
        setDemoError(res.message || 'The demo is not available right now.');
      }
    })();
  }, [router, wantsDemo]);

  return (
    <div className="min-h-screen flex items-center justify-center p-4 bg-(--background)">
      <div className="w-full max-w-md space-y-4">
        {demoError && (
          <div className="alert alert-warning text-sm">{demoError}</div>
        )}
        <LoginForm />
      </div>
    </div>
  );
}

export default function LoginPage() {
  // useSearchParams needs a Suspense boundary in the app router.
  return (
    <Suspense fallback={<div className="min-h-screen bg-(--background)" />}>
      <LoginPageInner />
    </Suspense>
  );
}
