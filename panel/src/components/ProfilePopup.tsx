"use client";

import { useState, useEffect, useCallback } from 'react';
import { X, ShieldCheck, ShieldOff, Copy, Check, AlertTriangle, Bug, Trash2, RefreshCw, KeyRound, HelpCircle, Pencil, History as HistoryIcon, ChevronDown } from 'lucide-react';
import { QRCodeSVG } from 'qrcode.react';
import { setupTOTP, verifyTOTP, disableTOTP, get2FAStatus, regenerateBackupCodes } from '@/lib/api/auth';
import { getSecurityQuestionPool, getMySecurityQuestions, setMySecurityQuestions, SecurityQAItem } from '@/lib/api/securityQuestions';
import { getMyUsernameHistory, type UsernameHistoryEntry } from '@/lib/api/accountPolicy';
import { isUsername } from '@/lib/validation';
import { useDevMode, setDevModeEnabled, clearDevLog } from '@/lib/devLog';
import { ReauthFields, reauthReady } from '@/components/ReauthFields';

interface UserProfile {
    username: string;
    minecraftUsername?: string;
    email?: string;
    is2FAEnabled?: boolean;
    isAdmin?: boolean;
}

interface ProfilePopupProps {
  currentUser: UserProfile;
  onClose: () => void;
  onUpdate: (data: {
      newUsername?: string;
      oldPassword: string;
      newPassword?: string;
      minecraftUsername?: string;
      email?: string;
  }) => Promise<void>;
  onTwoFactorChange?: () => void;
  error: string;
  success: string;
}

const ProfilePopup: React.FC<ProfilePopupProps> = ({ currentUser, onClose, onUpdate, onTwoFactorChange, error, success }) => {
  const [currentView, setCurrentView] = useState("general");

  const [newUsername, setNewUsername] = useState(currentUser.username || "");
  const [minecraftUsername, setMinecraftUsername] = useState(currentUser.minecraftUsername || "");
  const [email, setEmail] = useState(currentUser.email || "");

  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");

  const [loading, setLoading] = useState(false);
  const [localError, setLocalError] = useState("");

  // 2FA wizard state
  const [twoFactorOpen, setTwoFactorOpen] = useState(false);
  const [twoFactorMode, setTwoFactorMode] = useState<'enable' | 'disable'>('enable');
  const [twoFactorEnabled, setTwoFactorEnabled] = useState(currentUser.is2FAEnabled || false);
  const [backupCodesRemaining, setBackupCodesRemaining] = useState<number | null>(null);
  const [regenerateOpen, setRegenerateOpen] = useState(false);

  // Reload backup-code count whenever 2FA state changes (enable/disable/regen)
  // OR the user opens the Security tab. Cheap fetch — single GET.
  useEffect(() => {
    if (!twoFactorEnabled) {
      setBackupCodesRemaining(null);
      return;
    }
    get2FAStatus().then(res => {
      if (res.success && typeof res.remainingBackupCodes === 'number') {
        setBackupCodesRemaining(res.remainingBackupCodes);
      }
    });
  }, [twoFactorEnabled, currentView]);

  const handleSubmit = async (e: React.FormEvent) => {
      e.preventDefault();
      setLocalError("");
      if (newUsername !== currentUser.username && !isUsername(newUsername)) {
          setLocalError("Username: 3-32 characters, must start with a letter or digit, then letters, digits, . _ or -");
          return;
      }
      if (newPassword !== confirmPassword) {
          // Surface the mismatch instead of silently dropping the password
          // change (sending "" left the user thinking it had changed).
          setLocalError("New password and confirmation do not match");
          return;
      }
      setLoading(true);
      await onUpdate({
          newUsername,
          oldPassword,
          newPassword,
          minecraftUsername,
          email,
      });
      setLoading(false);
  };

  return (
    <>
    <div className="modal-overlay animate-fade-in">
      <div className="modal-panel w-full max-w-md">
        <div className="modal-header flex justify-between items-center">
          <h2 className="modal-title">Profile Settings</h2>
          <button onClick={onClose} className="text-(--base-06) hover:text-(--error-light) transition-colors">
            <X size={20} />
          </button>
        </div>

        <div className="flex gap-1 px-6 pt-4 border-b border-(--base-03)">
          <button onClick={() => setCurrentView("general")} className={`pb-2.5 px-3 font-medium text-sm transition-colors ${currentView === "general" ? "border-b-2 border-(--accent) text-(--accent-light)" : "text-(--base-07) hover:text-(--base-09)"}`}>General</button>
          <button onClick={() => setCurrentView("security")} className={`pb-2.5 px-3 font-medium text-sm transition-colors ${currentView === "security" ? "border-b-2 border-(--accent) text-(--accent-light)" : "text-(--base-07) hover:text-(--base-09)"}`}>Security</button>
          {currentUser.isAdmin && (
            <button onClick={() => setCurrentView("developer")} className={`pb-2.5 px-3 font-medium text-sm transition-colors ${currentView === "developer" ? "border-b-2 border-(--accent) text-(--accent-light)" : "text-(--base-07) hover:text-(--base-09)"}`}>Developer</button>
          )}
        </div>

        <div className="modal-body">
          {(localError || error) && <div className="alert alert-error mb-4 font-medium">{localError || error}</div>}
          {success && <div className="alert alert-success mb-4 font-medium">{success}</div>}

          <form onSubmit={handleSubmit} className="space-y-4">
            {currentView === "general" && (
              <div className="space-y-4 animate-fade-in">
                <div className="flex flex-col gap-[5px]">
                  <label className="input-label">Username</label>
                  <input type="text" value={newUsername} onChange={e => setNewUsername(e.target.value)} disabled={loading} className="input-field w-full disabled:opacity-40 disabled:cursor-not-allowed" />
                  <p className="text-xs text-(--base-06) mt-1">
                    Username changes follow the platform&apos;s cooldown policy. The admin
                    can disable changes or set a delay between renames.
                  </p>
                </div>
                <div className="flex flex-col gap-[5px]">
                  <label className="input-label">Email</label>
                  <input type="email" value={email} onChange={e => setEmail(e.target.value)} disabled={loading} className="input-field w-full disabled:opacity-40 disabled:cursor-not-allowed" />
                </div>
                <div className="flex flex-col gap-[5px]">
                  <label className="input-label">Minecraft Username (For Avatar)</label>
                  <input type="text" value={minecraftUsername} onChange={e => setMinecraftUsername(e.target.value)} disabled={loading} className="input-field w-full disabled:opacity-40 disabled:cursor-not-allowed" />
                </div>
                <UsernameHistorySection />
              </div>
            )}

            {currentView === "developer" && currentUser.isAdmin && (
              <DeveloperPanel />
            )}

            {currentView === "security" && (
              <div className="space-y-4 animate-fade-in">
                <div className="flex flex-col gap-[5px]">
                  <label className="input-label">New Password</label>
                  <input type="password" autoComplete="new-password" value={newPassword} onChange={e => setNewPassword(e.target.value)} disabled={loading} className="input-field w-full disabled:opacity-40 disabled:cursor-not-allowed" placeholder="Leave blank to keep current" />
                </div>
                {newPassword && (
                  <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Confirm Password</label>
                    <input type="password" autoComplete="new-password" value={confirmPassword} onChange={e => setConfirmPassword(e.target.value)} disabled={loading} className="input-field w-full disabled:opacity-40 disabled:cursor-not-allowed" />
                  </div>
                )}

                {/* 2FA section */}
                <div className="pt-2">
                  <div className="flex items-center justify-between gap-3 p-3 rounded-md bg-(--base-02) border border-(--base-03)">
                    <div className="flex items-start gap-2.5 min-w-0">
                      {twoFactorEnabled
                        ? <ShieldCheck size={16} className="text-(--success-light) shrink-0 mt-0.5" />
                        : <ShieldOff size={16} className="text-(--base-06) shrink-0 mt-0.5" />}
                      <div className="min-w-0">
                        <div className="font-medium text-sm text-(--base-09)">Two-Factor Authentication</div>
                        <div className="text-xs text-(--base-06)">
                          {twoFactorEnabled
                            ? 'Enabled — login requires an authenticator code.'
                            : 'Add an extra layer of security via TOTP authenticator app.'}
                        </div>
                      </div>
                    </div>
                    <button
                      type="button"
                      onClick={() => {
                        setTwoFactorMode(twoFactorEnabled ? 'disable' : 'enable');
                        setTwoFactorOpen(true);
                      }}
                      className={`shrink-0 px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                        twoFactorEnabled
                          ? 'bg-(--error-ghost) text-(--error-light) hover:bg-(--error)/15 border border-(--error)/15'
                          : 'bg-(--accent-ghost) text-(--accent-light) hover:bg-(--accent)/15 border border-(--accent-border)'
                      }`}
                    >
                      {twoFactorEnabled ? 'Disable' : 'Enable'}
                    </button>
                  </div>

                  <SecurityQuestionsSection twoFactorEnabled={twoFactorEnabled} />

                  {/* Backup-code health row — only shown when 2FA is on. The
                      remaining count comes from /auth/2fa/status; we never
                      surface the codes themselves, just how many are left. */}
                  {twoFactorEnabled && backupCodesRemaining !== null && (
                    <div className="mt-2 flex items-center justify-between gap-3 p-3 rounded-md bg-(--base-02) border border-(--base-03)">
                      <div className="flex items-start gap-2.5 min-w-0">
                        <KeyRound size={16} className={`shrink-0 mt-0.5 ${
                          backupCodesRemaining <= 2 ? 'text-(--error-light)'
                          : backupCodesRemaining <= 4 ? 'text-(--warning-light)'
                          : 'text-(--base-07)'
                        }`} />
                        <div className="min-w-0">
                          <div className="font-medium text-sm text-(--base-09)">Recovery codes</div>
                          <div className="text-xs text-(--base-06)">
                            {backupCodesRemaining > 0
                              ? <>You have <span className="font-mono">{backupCodesRemaining}</span> unused code{backupCodesRemaining === 1 ? '' : 's'} left.{backupCodesRemaining <= 3 && ' Generate a new set.'}</>
                              : 'No codes left — regenerate before you lose access to your authenticator.'}
                          </div>
                        </div>
                      </div>
                      <button
                        type="button"
                        onClick={() => setRegenerateOpen(true)}
                        className="shrink-0 px-3 py-1.5 rounded-md text-xs font-medium bg-(--accent-ghost) text-(--accent-light) hover:bg-(--accent)/15 border border-(--accent-border) inline-flex items-center gap-1.5"
                      >
                        <RefreshCw size={12} />
                        Regenerate
                      </button>
                    </div>
                  )}
                </div>
              </div>
            )}

            {/* The current-password gate and submit button only apply to
                profile data (general/security). The developer tab toggles
                live state in localStorage on its own — no save needed. */}
            {currentView !== "developer" && (
              <>
                <div className="pt-4 border-t border-(--base-03)">
                  <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Current Password <span className="opacity-70">(required to save profile changes)</span></label>
                    <input type="password" autoComplete="current-password" value={oldPassword} onChange={e => setOldPassword(e.target.value)} required disabled={loading} className="input-field w-full disabled:opacity-40 disabled:cursor-not-allowed" />
                  </div>
                </div>

                <button type="submit" disabled={loading} className="btn btn-primary btn-lg w-full mt-4">
                  {loading ? 'Saving...' : 'Save Changes'}
                </button>
              </>
            )}
          </form>
        </div>
      </div>
    </div>

    {twoFactorOpen && (
      <TwoFactorWizard
        mode={twoFactorMode}
        onClose={() => setTwoFactorOpen(false)}
        onComplete={() => {
          setTwoFactorOpen(false);
          setTwoFactorEnabled(twoFactorMode === 'enable');
          onTwoFactorChange?.();
        }}
      />
    )}

    {regenerateOpen && (
      <RegenerateBackupCodesWizard
        onClose={() => setRegenerateOpen(false)}
        onComplete={(newRemaining) => {
          setRegenerateOpen(false);
          setBackupCodesRemaining(newRemaining);
        }}
      />
    )}
    </>
  );
};

// ─────────────────────────────────────────────
// Username history
// ─────────────────────────────────────────────
// Used to be its own dropdown entry and its own page, which put a rarely-read
// audit list at the same level as "Edit Profile". It belongs next to the field
// it is the history OF, so it lives here, collapsed. The fetch is deferred to
// the first expand: most people opening this popup came to change something,
// not to read their rename log.
function UsernameHistorySection() {
  const [open, setOpen] = useState(false);
  const [rows, setRows] = useState<UsernameHistoryEntry[] | null>(null);

  useEffect(() => {
    if (!open || rows !== null) return;
    getMyUsernameHistory().then(r => setRows(r)).catch(() => setRows([]));
  }, [open, rows]);

  return (
    <div className="rounded-md border border-(--base-03) bg-(--base-02)">
      <button
        type="button"
        onClick={() => setOpen(o => !o)}
        aria-expanded={open}
        className="w-full flex items-center justify-between gap-2 px-3 py-2.5 text-left transition-colors hover:bg-(--base-03)/60 rounded-md"
      >
        <span className="flex items-center gap-2 text-sm text-(--base-08)">
          <HistoryIcon size={15} className="text-(--base-06)" />
          Username history
        </span>
        <ChevronDown
          size={15}
          className="text-(--base-06) transition-transform duration-200"
          style={{ transform: open ? 'rotate(180deg)' : 'rotate(0deg)' }}
        />
      </button>
      {open && (
        <div className="px-3 pb-3 pt-1 border-t border-(--base-03)">
          {rows === null ? (
            <p className="text-xs text-(--base-06)">Loading…</p>
          ) : rows.length === 0 ? (
            <p className="text-xs text-(--base-06)">You haven&apos;t renamed your account.</p>
          ) : (
            <ul className="space-y-2">
              {rows.map(r => (
                <li key={r.id}>
                  <div className="font-mono text-xs text-(--base-09)">{r.oldUsername} → {r.newUsername}</div>
                  <div className="text-[11px] text-(--base-06) mt-0.5">
                    {new Date(r.changedAt).toLocaleString()} · {r.byAdmin ? 'by admin' : 'self'}
                  </div>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}

// ─────────────────────────────────────────────
// Two-Factor Wizard
// ─────────────────────────────────────────────

function TwoFactorWizard({ mode, onClose, onComplete }: {
  mode: 'enable' | 'disable';
  onClose: () => void;
  onComplete: () => void;
}) {
  if (mode === 'enable') return <EnableWizard onClose={onClose} onComplete={onComplete} />;
  return <DisableWizard onClose={onClose} onComplete={onComplete} />;
}

type EnableStep = 'loading' | 'scan' | 'backup';

function EnableWizard({ onClose, onComplete }: { onClose: () => void; onComplete: () => void }) {
  const [step, setStep] = useState<EnableStep>('loading');
  const [secret, setSecret] = useState('');
  // Enabling 2FA re-authenticates: a session token alone must not be enough to
  // bind a new authenticator to the account and take the backup codes.
  const [password, setPassword] = useState('');
  const [otpURL, setOtpURL] = useState('');
  const [code, setCode] = useState('');
  const [backupCodes, setBackupCodes] = useState<string[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);
  const [copied, setCopied] = useState(false);

  // Every call mints a FRESH secret, so the QR below and the entry in the
  // authenticator only match as long as this is the last one fetched. That is
  // the usual cause of "the code is invalid": an entry scanned from an earlier
  // setup attempt keeps producing codes that can never verify. `startOver` makes
  // the fix reachable, and the hint on the error names it.
  const loadSecret = useCallback(() => {
    setStep('loading');
    setError('');
    setCode('');
    setupTOTP().then(res => {
      if (res?.success) {
        setSecret(res.secret);
        setOtpURL(res.otpAuthURL);
        setStep('scan');
      } else {
        setError(res?.message || 'Failed to start 2FA setup');
      }
    });
  }, []);

  useEffect(() => {
    loadSecret();
  }, [loadSecret]);

  const handleVerify = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setBusy(true);
    try {
      const res = await verifyTOTP(secret, code.replace(/\s/g, ''), password);
      if (res?.success && Array.isArray(res.backupCodes)) {
        setBackupCodes(res.backupCodes);
        setStep('backup');
      } else {
        setError(res?.message || 'Invalid code');
      }
    } finally {
      setBusy(false);
    }
  };

  const copyAll = async () => {
    try {
      await navigator.clipboard.writeText(backupCodes.join('\n'));
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch { /* ignore */ }
  };

  return (
    <div className="modal-overlay animate-fade-in z-50">
      <div className="modal-panel w-full max-w-md">
        <div className="modal-header flex items-center justify-between">
          <h2 className="modal-title flex items-center gap-2">
            <ShieldCheck size={18} />
            Enable Two-Factor
          </h2>
          <button onClick={onClose} className="text-(--base-06) hover:text-(--error-light)"><X size={18} /></button>
        </div>

        <div className="modal-body">
          {error && (
            <div className="alert alert-error mb-3 flex-col items-start gap-2">
              <span>{error}</span>
              {/* Naming the usual cause here rather than in a tooltip: a
                  rejected code almost always means the authenticator still holds
                  an entry from an earlier open of this wizard, and no amount of
                  retyping will ever fix that one. */}
              {step === 'scan' && (
                <>
                  <span className="text-xs text-(--base-07)">
                    The code has to come from the QR shown right now. If your app still has an older
                    &ldquo;Dylaris&rdquo; entry from a previous attempt, delete it and scan again. A code from
                    that entry can never be accepted.
                  </span>
                  <button type="button" onClick={loadSecret} disabled={busy} className="btn btn-secondary btn-sm">
                    Start over with a new QR
                  </button>
                </>
              )}
            </div>
          )}

          {step === 'loading' && (
            <p className="text-sm text-(--base-06) py-8 text-center">Generating secret…</p>
          )}

          {step === 'scan' && (
            <form onSubmit={handleVerify} className="space-y-4">
              <p className="text-sm text-(--base-07)">
                Scan this QR with your authenticator app (Google Authenticator, Authy, 1Password etc.) and enter the 6-digit code shown to confirm.
              </p>
              <div className="flex justify-center bg-white p-4 rounded-md">
                <QRCodeSVG value={otpURL} size={180} />
              </div>
              <div className="flex flex-col gap-[5px]">
                <label className="input-label">Or enter this secret manually</label>
                <code className="input-mono w-full bg-(--base-02) border border-(--base-03) rounded-md px-3 py-2 text-xs text-(--base-08) break-all select-all">{secret}</code>
              </div>
              <div className="flex flex-col gap-[5px]">
                <label className="input-label">Verification code</label>
                <input
                  type="text"
                  value={code}
                  onChange={e => setCode(e.target.value)}
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  placeholder="123 456"
                  className="input-field input-mono w-full text-center tracking-widest"
                />
              </div>
              <div className="flex flex-col gap-[5px]">
                <label className="input-label">Your password</label>
                <input
                  type="password"
                  value={password}
                  onChange={e => setPassword(e.target.value)}
                  autoComplete="current-password"
                  placeholder="Confirm it is you"
                  className="input-field w-full"
                />
                <p className="text-xs text-(--base-06)">
                  Confirms it is you before this authenticator becomes the second factor on your account.
                </p>
              </div>
              <button type="submit" disabled={busy || code.replace(/\s/g, '').length < 6 || !password} className="btn btn-primary w-full">
                {busy ? 'Verifying…' : 'Verify & Enable'}
              </button>
            </form>
          )}

          {step === 'backup' && (
            <div className="space-y-4">
              <div className="alert alert-warning text-(--warning) text-xs">
                <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                <span>
                  Store these backup codes somewhere safe. Each code works exactly once and lets you log in if you lose your authenticator. They will never be shown again.
                </span>
              </div>
              <div className="grid grid-cols-2 gap-2">
                {backupCodes.map(c => (
                  <code key={c} className="bg-(--base-02) border border-(--base-03) rounded-md px-2.5 py-1.5 text-xs font-mono text-(--base-09) text-center select-all">
                    {c}
                  </code>
                ))}
              </div>
              <button type="button" onClick={copyAll} className="btn btn-secondary btn-sm w-full">
                {copied ? <><Check size={12} /> Copied</> : <><Copy size={12} /> Copy all codes</>}
              </button>
              <label className="flex items-start gap-2 text-xs text-(--base-07) cursor-pointer pt-1">
                <input type="checkbox"
                            className="checkbox mt-0.5" checked={acknowledged} onChange={e => setAcknowledged(e.target.checked)} />
                <span>I have saved these codes in a secure location.</span>
              </label>
              <button
                type="button"
                onClick={onComplete}
                disabled={!acknowledged}
                className="btn btn-primary w-full disabled:opacity-40 disabled:cursor-not-allowed"
              >
                Done
              </button>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function DisableWizard({ onClose, onComplete }: { onClose: () => void; onComplete: () => void }) {
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setBusy(true);
    try {
      const res = await disableTOTP(password, code.replace(/\s/g, ''));
      if (res?.success) {
        onComplete();
      } else {
        setError(res?.message || 'Failed to disable 2FA');
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="modal-overlay animate-fade-in z-50">
      <div className="modal-panel w-full max-w-md">
        <div className="modal-header flex items-center justify-between">
          <h2 className="modal-title flex items-center gap-2">
            <ShieldOff size={18} className="text-(--error-light)" />
            Disable Two-Factor
          </h2>
          <button onClick={onClose} className="text-(--base-06) hover:text-(--error-light)"><X size={18} /></button>
        </div>

        <div className="modal-body space-y-4">
          {error && (
            <div className="alert alert-error">
              {error}
            </div>
          )}
          <p className="text-sm text-(--base-07)">
            Verify your password and provide a current authenticator code (or backup code) to disable 2FA.
          </p>
          <form onSubmit={handleSubmit} className="space-y-3">
            <div className="flex flex-col gap-[5px]">
              <label className="input-label">Password</label>
              <input type="password" value={password} onChange={e => setPassword(e.target.value)} className="input-field w-full" required />
            </div>
            <div className="flex flex-col gap-[5px]">
              <label className="input-label">Authenticator code or backup code</label>
              <input
                type="text"
                value={code}
                onChange={e => setCode(e.target.value)}
                inputMode="text"
                placeholder="123 456"
                className="input-field input-mono w-full text-center tracking-widest"
                required
              />
            </div>
            <button type="submit" disabled={busy} className="btn btn-danger w-full">
              {busy ? 'Disabling…' : 'Disable 2FA'}
            </button>
          </form>
        </div>
      </div>
    </div>
  );
}

// ─────────────────────────────────────────────
// Developer / Debug panel — admin only
// ─────────────────────────────────────────────

function DeveloperPanel() {
  const devMode = useDevMode();

  return (
    <div className="space-y-4 animate-fade-in">
      <div className="alert alert-warning text-xs">
        <AlertTriangle size={14} className="shrink-0 mt-0.5" />
        <span>
          Developer mode exposes diagnostic windows and verbose logging across
          the panel. Useful when chasing bugs; noisy in regular use. Persisted
          per-browser only — does not affect other users.
        </span>
      </div>

      <div className="flex items-center justify-between gap-3 p-3 rounded-md bg-(--base-02) border border-(--base-03)">
        <div className="flex items-start gap-2.5 min-w-0">
          <Bug size={16} className={`shrink-0 mt-0.5 ${devMode ? 'text-(--accent-light)' : 'text-(--base-06)'}`} />
          <div className="min-w-0">
            <div className="font-medium text-sm text-(--base-09)">Developer Mode</div>
            <div className="text-xs text-(--base-06)">
              {devMode
                ? 'Active — debug log windows are visible.'
                : 'Off — the panel behaves normally for end-users.'}
            </div>
          </div>
        </div>
        <button
          type="button"
          role="switch"
          aria-checked={devMode}
          onClick={() => setDevModeEnabled(!devMode)}
          className={`toggle-track shrink-0 ${devMode ? 'toggle-track-on' : 'toggle-track-off'}`}
        >
          <span className={`toggle-knob ${devMode ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
        </button>
      </div>

      <div>
        <h3 className="mono-label mb-2">What this enables</h3>
        <ul className="text-xs text-(--base-07) space-y-1.5 list-disc pl-5">
          <li>
            <strong className="text-(--base-09)">File browser debug log</strong> — a
            timestamped panel below the file browser showing every Beam-tunnel
            step (SetSession, ConnectToServer, BeamUploadStart/Chunk/Finish)
            with the raw error message on failure. Useful for pinpointing where
            an upload or browse breaks.
          </li>
          <li>
            <strong className="text-(--base-09)">Future hooks</strong> — additional
            diagnostic windows will gate on this same toggle, so flipping it
            here is the single switch.
          </li>
        </ul>
      </div>

      <button
        type="button"
        onClick={clearDevLog}
        className="btn btn-secondary btn-sm w-full"
      >
        <Trash2 size={12} /> Clear debug log buffer
      </button>
    </div>
  );
}

// ─────────────────────────────────────────────
// Regenerate Backup Codes Wizard
// ─────────────────────────────────────────────
// Two-stage: password+TOTP gate → show fresh codes once → done.
// The gate is identical to DisableWizard; we reuse the same defense-in-depth
// pattern but the destructive action is "rotate codes" not "kill 2FA".
function RegenerateBackupCodesWizard({ onClose, onComplete }: {
  onClose: () => void;
  onComplete: (newRemaining: number) => void;
}) {
  const [stage, setStage] = useState<'gate' | 'codes'>('gate');
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [newCodes, setNewCodes] = useState<string[]>([]);
  const [acknowledged, setAcknowledged] = useState(false);
  const [copied, setCopied] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setBusy(true);
    try {
      const res = await regenerateBackupCodes(password, code.replace(/\s/g, ''));
      if (res?.success && Array.isArray(res.backupCodes)) {
        setNewCodes(res.backupCodes);
        setStage('codes');
      } else {
        setError(res?.message || 'Failed to regenerate codes');
      }
    } finally {
      setBusy(false);
    }
  };

  const copyAll = async () => {
    try {
      await navigator.clipboard.writeText(newCodes.join('\n'));
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch { /* clipboard may be blocked */ }
  };

  return (
    <div className="modal-overlay animate-fade-in z-50">
      <div className="modal-panel w-full max-w-md">
        <div className="modal-header flex items-center justify-between">
          <h2 className="modal-title flex items-center gap-2">
            <RefreshCw size={18} className="text-(--accent-light)" />
            Regenerate Backup Codes
          </h2>
          <button onClick={onClose} className="text-(--base-06) hover:text-(--error-light)"><X size={18} /></button>
        </div>

        <div className="modal-body space-y-4">
          {error && <div className="alert alert-error">{error}</div>}

          {stage === 'gate' && (
            <>
              <div className="alert alert-warning text-xs">
                <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                <span>
                  Any previously generated backup codes will be <strong>invalidated</strong>. You will receive a fresh set of 10 single-use codes — save them immediately.
                </span>
              </div>
              <p className="text-sm text-(--base-07)">
                Confirm your password and a current authenticator code (or one of your existing backup codes) to continue.
              </p>
              <form onSubmit={handleSubmit} className="space-y-3">
                <div className="flex flex-col gap-[5px]">
                  <label className="input-label">Password</label>
                  <input type="password" value={password} onChange={e => setPassword(e.target.value)} className="input-field w-full" required />
                </div>
                <div className="flex flex-col gap-[5px]">
                  <label className="input-label">Authenticator code or backup code</label>
                  <input
                    type="text"
                    value={code}
                    onChange={e => setCode(e.target.value)}
                    inputMode="text"
                    placeholder="123 456"
                    className="input-field input-mono w-full text-center tracking-widest"
                    required
                  />
                </div>
                <button type="submit" disabled={busy} className="btn btn-primary w-full">
                  {busy ? 'Regenerating…' : 'Regenerate codes'}
                </button>
              </form>
            </>
          )}

          {stage === 'codes' && (
            <>
              <div className="alert alert-warning text-(--warning) text-xs">
                <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                <span>
                  Save these new codes somewhere safe. Each works exactly once and lets you log in if you lose your authenticator. They will <strong>never be shown again</strong>.
                </span>
              </div>
              <div className="grid grid-cols-2 gap-2">
                {newCodes.map(c => (
                  <code key={c} className="bg-(--base-02) border border-(--base-03) rounded-md px-2.5 py-1.5 text-xs font-mono text-(--base-09) text-center select-all">
                    {c}
                  </code>
                ))}
              </div>
              <button type="button" onClick={copyAll} className="btn btn-secondary btn-sm w-full">
                {copied ? <><Check size={12} /> Copied</> : <><Copy size={12} /> Copy all codes</>}
              </button>
              <label className="flex items-start gap-2 text-xs text-(--base-07) cursor-pointer pt-1">
                <input type="checkbox"
                            className="checkbox mt-0.5" checked={acknowledged} onChange={e => setAcknowledged(e.target.checked)} />
                <span>I have saved these codes in a secure location.</span>
              </label>
              <button
                type="button"
                onClick={() => onComplete(newCodes.length)}
                disabled={!acknowledged}
                className="btn btn-primary w-full disabled:opacity-40 disabled:cursor-not-allowed"
              >
                Done
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

// ─────────────────────────────────────────────
// Security Questions Section
// ─────────────────────────────────────────────
// Self-contained: probes the pool endpoint on mount; if security questions
// are disabled by policy, the section renders nothing at all (zero footprint
// in the security tab). Otherwise the user sees their current status and
// can pick/refresh questions inline — no separate modal.
// twoFactorEnabled is passed in rather than probed: this section already
// renders inside the security tab, which owns that state and keeps it live
// across enabling and disabling 2FA. A second source would disagree with the
// toggle sitting right above it the moment somebody flips it.
function SecurityQuestionsSection({ twoFactorEnabled }: { twoFactorEnabled: boolean }) {
  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [required, setRequired] = useState(3);
  const [pool, setPool] = useState<string[]>([]);
  const [currentCount, setCurrentCount] = useState(0);
  const [editing, setEditing] = useState(false);
  const [items, setItems] = useState<SecurityQAItem[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [savedToast, setSavedToast] = useState(false);
  // These answers ARE the password-reset path, so Core asks who is writing
  // them: overwriting them survives a password change and even revoking every
  // API key.
  const [reauthPassword, setReauthPassword] = useState('');
  const [reauthCode, setReauthCode] = useState('');

  useEffect(() => {
    getSecurityQuestionPool().then(res => {
      if (!res.success || !res.enabled) {
        setEnabled(false);
        return;
      }
      setEnabled(true);
      setRequired(res.required || 3);
      setPool(res.pool || []);
    });
    getMySecurityQuestions().then(res => {
      if (res.success && Array.isArray(res.questions)) {
        setCurrentCount(res.questions.length);
      }
    });
  }, []);

  if (enabled !== true) return null;

  const startEdit = () => {
    setItems(Array.from({ length: required }, () => ({ question: '', answer: '' })));
    setError('');
    setEditing(true);
  };

  const handleSave = async () => {
    setError('');
    if (items.some(qa => !qa.question || !qa.answer.trim())) {
      setError('Pick a question and provide an answer for every row.');
      return;
    }
    const picked = items.map(qa => qa.question);
    if (new Set(picked).size !== picked.length) {
      setError('Each question may be chosen at most once.');
      return;
    }
    setSaving(true);
    const res = await setMySecurityQuestions(items, reauthPassword, reauthCode.replace(/\s/g, '') || undefined);
    setSaving(false);
    if (res.success) {
      setCurrentCount(items.length);
      setEditing(false);
      setReauthPassword('');
      setReauthCode('');
      setSavedToast(true);
      setTimeout(() => setSavedToast(false), 2500);
    } else {
      setError(res.message || 'Failed to save.');
    }
  };

  return (
    <div className="mt-2 p-3 rounded-md bg-(--base-02) border border-(--base-03)">
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-start gap-2.5 min-w-0">
          <HelpCircle size={16} className={`shrink-0 mt-0.5 ${currentCount === 0 ? 'text-(--warning-light)' : 'text-(--base-07)'}`} />
          <div className="min-w-0">
            <div className="font-medium text-sm text-(--base-09)">Security questions</div>
            <div className="text-xs text-(--base-06)">
              {currentCount === 0
                ? 'Not set up — these help recover your account if you lose your password.'
                : <>You have <span className="font-mono">{currentCount}</span> question{currentCount === 1 ? '' : 's'} set up. The policy requires <span className="font-mono">{required}</span>.</>}
            </div>
          </div>
        </div>
        {!editing && (
          <button
            type="button"
            onClick={startEdit}
            className="shrink-0 px-3 py-1.5 rounded-md text-xs font-medium bg-(--accent-ghost) text-(--accent-light) hover:bg-(--accent)/15 border border-(--accent-border) inline-flex items-center gap-1.5"
          >
            <Pencil size={12} />
            {currentCount === 0 ? 'Set up' : 'Update'}
          </button>
        )}
      </div>

      {editing && (
        <div className="mt-3 space-y-3 pt-3 border-t border-(--base-03)">
          <p className="text-xs text-(--base-06)">
            Pick {required} question{required === 1 ? '' : 's'} and give a memorable answer for each. Answers are case-insensitive.
          </p>
          {items.map((qa, idx) => {
            const otherPicks = items.filter((_, i) => i !== idx).map(o => o.question);
            const options = pool.filter(p => !otherPicks.includes(p));
            return (
              <div key={idx} className="space-y-1.5">
                <select
                  value={qa.question}
                  onChange={e => {
                    const next = [...items];
                    next[idx] = { ...next[idx], question: e.target.value };
                    setItems(next);
                  }}
                  className="input-field w-full text-sm"
                  disabled={saving}
                >
                  <option value="">— Pick question #{idx + 1} —</option>
                  {options.map(q => <option key={q} value={q}>{q}</option>)}
                </select>
                <input
                  type="text"
                  value={qa.answer}
                  onChange={e => {
                    const next = [...items];
                    next[idx] = { ...next[idx], answer: e.target.value };
                    setItems(next);
                  }}
                  placeholder="Your answer"
                  maxLength={200}
                  className="input-field w-full text-sm"
                  disabled={saving || !qa.question}
                />
              </div>
            );
          })}
          <ReauthFields
            idPrefix="secq"
            twoFactorEnabled={twoFactorEnabled}
            password={reauthPassword}
            code={reauthCode}
            onPassword={setReauthPassword}
            onCode={setReauthCode}
            disabled={saving}
          />
          {error && <p className="text-xs text-(--error-light)">{error}</p>}
          <div className="flex gap-2 justify-end">
            <button
              type="button"
              onClick={() => { setEditing(false); setReauthPassword(''); setReauthCode(''); }}
              disabled={saving}
              className="btn btn-secondary btn-sm"
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={handleSave}
              disabled={saving || !reauthReady(twoFactorEnabled, reauthPassword, reauthCode)}
              className="btn btn-primary btn-sm inline-flex items-center gap-1.5"
            >
              {saving && <RefreshCw size={12} className="animate-spin" />}
              Save
            </button>
          </div>
        </div>
      )}
      {savedToast && (
        <p className="mt-2 text-xs text-(--success-light) flex items-center gap-1">
          <Check size={12} /> Security questions saved.
        </p>
      )}
    </div>
  );
}

export default ProfilePopup;
