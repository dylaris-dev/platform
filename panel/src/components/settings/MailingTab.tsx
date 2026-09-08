'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { AlertTriangle, Eye, Mail, RotateCcw, Send } from 'lucide-react';
import {
    listMailTemplates, saveMailTemplate, resetMailTemplate,
    previewMailTemplate, testSendMailTemplate,
    type MailTemplate, type MailPreview,
} from '@/lib/api/mailTemplates';
import SettingsCard from '@/components/settings/SettingsCard';
import { Skeleton } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { useBusy } from '@/lib/useBusy';

/**
 * Editing the mail the platform sends.
 *
 * The editor opens on what WILL be sent, not on an empty box: a blank field
 * that silently means "the default" is how people end up retyping wording that
 * is already there.
 *
 * The preview renders in an iframe rather than into the page. Email HTML is a
 * document with its own body background and font stack, and dropping it into
 * the panel would both inherit the panel's styles and let it fight with them -
 * the point of a preview is to show what the mail client will show.
 */
export default function MailingTab() {
    const [templates, setTemplates] = useState<MailTemplate[]>([]);
    const [loading, setLoading] = useState(true);
    const [activeKey, setActiveKey] = useState<string>('');
    const [subject, setSubject] = useState('');
    const [body, setBody] = useState('');
    const [preview, setPreview] = useState<MailPreview | null>(null);
    const [previewTab, setPreviewTab] = useState<'html' | 'text'>('html');
    const [saving, runSave] = useBusy();
    const [previewing, runPreview] = useBusy();
    const [testing, runTest] = useBusy();
    const [resetting, runReset] = useBusy();

    const subjectRef = useRef<HTMLInputElement>(null);
    const bodyRef = useRef<HTMLTextAreaElement>(null);
    // Which field a chip should insert into. Tracked rather than guessed,
    // because inserting a variable into the wrong field is worse than useless.
    const lastFocused = useRef<'subject' | 'body'>('body');

    const active = templates.find(t => t.key === activeKey) || null;
    const dirty = !!active && (subject !== active.currentSubject || body !== active.currentBody);

    const load = useCallback(async (keepKey?: string) => {
        setLoading(true);
        const res = await listMailTemplates();
        setLoading(false);
        if (!res.success || !Array.isArray(res.templates)) {
            toast(res.message || 'Could not load the mail templates.', false);
            return;
        }
        setTemplates(res.templates);
        const next = res.templates.find((t: MailTemplate) => t.key === (keepKey || activeKey)) || res.templates[0];
        if (next) {
            setActiveKey(next.key);
            setSubject(next.currentSubject);
            setBody(next.currentBody);
        }
        setPreview(null);
    }, [activeKey]);

    useEffect(() => { void load(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, []);

    const select = (t: MailTemplate) => {
        setActiveKey(t.key);
        setSubject(t.currentSubject);
        setBody(t.currentBody);
        setPreview(null);
    };

    /** Insert {{name}} where the cursor is, in whichever field had focus. */
    const insertVariable = (name: string) => {
        const token = `{{${name}}}`;
        if (lastFocused.current === 'subject' && subjectRef.current) {
            const el = subjectRef.current;
            const at = el.selectionStart ?? subject.length;
            const next = subject.slice(0, at) + token + subject.slice(el.selectionEnd ?? at);
            setSubject(next);
            requestAnimationFrame(() => { el.focus(); el.setSelectionRange(at + token.length, at + token.length); });
            return;
        }
        const el = bodyRef.current;
        const at = el?.selectionStart ?? body.length;
        const end = el?.selectionEnd ?? at;
        setBody(body.slice(0, at) + token + body.slice(end));
        requestAnimationFrame(() => { el?.focus(); el?.setSelectionRange(at + token.length, at + token.length); });
    };

    const doPreview = async () => {
        if (!active) return;
        await runPreview(async () => {
            const res = await previewMailTemplate(active.key, subject, body);
            if (res.success && res.preview) setPreview(res.preview);
            else toast(res.message || 'Could not render the preview.', false);
        });
    };

    const doSave = async () => {
        if (!active) return;
        await runSave(async () => {
            const res = await saveMailTemplate(active.key, subject, body);
            if (res.success) { toast('Saved.'); await load(active.key); }
            else toast(res.message || 'Could not save.', false);
        });
    };

    const doReset = async () => {
        if (!active) return;
        if (!(await confirmDialog({
            title: 'Reset to default',
            message: `Discard your wording for "${active.name}" and go back to the one this release ships with?`,
        }))) return;
        await runReset(async () => {
            const res = await resetMailTemplate(active.key);
            if (res.success) { toast('Back to the default wording.'); await load(active.key); }
            else toast(res.message || 'Could not reset.', false);
        });
    };

    const doTest = async () => {
        if (!active) return;
        await runTest(async () => {
            const res = await testSendMailTemplate(active.key);
            toast(res.message || (res.success ? 'Sent.' : 'Could not send.'), !!res.success);
        });
    };

    if (loading) return <div className="space-y-3"><Skeleton className="h-8 w-56" /><Skeleton className="h-64 w-full" /></div>;

    return (
        <SettingsCard
            title="Outgoing mail"
            description="The wording of every mail the platform sends. Edits take effect on the next send; no restart."
            icon={Mail}
        >
            <div className="grid grid-cols-1 lg:grid-cols-[220px_1fr] gap-4">
                <nav className="flex flex-col gap-1" aria-label="Mail templates">
                    {templates.map(t => (
                        <button key={t.key} type="button" onClick={() => select(t)}
                            className={`text-left px-3 py-2 rounded-md transition-colors ${
                                t.key === activeKey ? 'bg-(--accent-ghost) text-(--base-09)' : 'text-(--base-07) hover:bg-(--base-02)'
                            }`}>
                            <span className="block text-sm">{t.name}</span>
                            <span className="block text-xs text-(--base-06)">
                                {t.edited ? 'Edited' : 'Default wording'}
                            </span>
                        </button>
                    ))}
                </nav>

                {active && (
                    <div className="space-y-3 min-w-0">
                        <p className="text-xs text-(--base-06)">{active.description}</p>

                        <div>
                            <label className="input-label mb-1 block" htmlFor="mt-subject">Subject</label>
                            <input id="mt-subject" ref={subjectRef} className="input-field w-full" value={subject}
                                onFocus={() => { lastFocused.current = 'subject'; }}
                                onChange={e => setSubject(e.target.value)} />
                        </div>

                        <div>
                            <p className="input-label mb-1">
                                Variables <span className="text-(--base-06)">- click to insert at the cursor</span>
                            </p>
                            <div className="flex flex-wrap gap-1.5">
                                {active.variables.map(v => (
                                    <button key={v.name} type="button" title={`${v.description} (e.g. ${v.example})`}
                                        onClick={() => insertVariable(v.name)}
                                        className="px-2 py-1 rounded font-mono text-xs bg-(--base-02) text-(--base-08) border border-(--base-04) hover:border-(--accent) hover:text-(--accent-light) transition-colors">
                                        {`{{${v.name}}}`}
                                    </button>
                                ))}
                            </div>
                        </div>

                        <div>
                            <label className="input-label mb-1 block" htmlFor="mt-body">Body</label>
                            <textarea id="mt-body" ref={bodyRef} rows={12} className="input-field w-full font-mono text-sm"
                                value={body}
                                onFocus={() => { lastFocused.current = 'body'; }}
                                onChange={e => setBody(e.target.value)} />
                            <p className="text-xs text-(--base-06) mt-1">
                                A blank line starts a new paragraph. <code>{'# Heading'}</code> makes a heading.{' '}
                                <code>{'[Label](url)'}</code> becomes a button on its own line, or a link inside a
                                sentence. Everything else is sent as written, and both an HTML and a plain-text copy
                                go out.
                            </p>
                        </div>

                        <div className="flex flex-wrap gap-2">
                            <button type="button" className="btn btn-primary btn-sm" disabled={!dirty || saving} onClick={doSave}>
                                {saving ? 'Saving...' : 'Save'}
                            </button>
                            <button type="button" className="btn btn-sm" disabled={previewing} onClick={doPreview}>
                                <Eye size={14} /> {previewing ? 'Rendering...' : 'Preview'}
                            </button>
                            <button type="button" className="btn btn-sm" disabled={testing} onClick={doTest}>
                                <Send size={14} /> {testing ? 'Sending...' : 'Send a test to myself'}
                            </button>
                            {active.edited && (
                                <button type="button" className="btn btn-sm btn-danger"
                                    disabled={resetting} onClick={doReset}>
                                    <RotateCcw size={14} /> {resetting ? 'Resetting...' : 'Reset to default'}
                                </button>
                            )}
                        </div>

                        {dirty && (
                            <div className="alert alert-warning text-xs">
                                <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                                <p>Unsaved changes. Preview and the test send use what is in the boxes; the platform keeps sending the saved version until you save.</p>
                            </div>
                        )}

                        {preview && (
                            <div className="border border-(--base-04) rounded-md overflow-hidden">
                                <div className="flex items-center gap-1 px-2 py-1.5 border-b border-(--base-04) bg-(--base-02)">
                                    <span className="text-xs text-(--base-06) mr-2 truncate">{preview.subject}</span>
                                    {(['html', 'text'] as const).map(tab => (
                                        <button key={tab} type="button" onClick={() => setPreviewTab(tab)}
                                            className={`px-2 py-0.5 rounded text-xs transition-colors ${
                                                previewTab === tab ? 'bg-(--accent-ghost) text-(--accent-light)' : 'text-(--base-06) hover:text-(--base-08)'
                                            }`}>
                                            {tab === 'html' ? 'HTML' : 'Plain text'}
                                        </button>
                                    ))}
                                </div>
                                {previewTab === 'html' ? (
                                    <iframe title="Mail preview" sandbox="" srcDoc={preview.html}
                                        className="w-full h-[420px] bg-white border-0" />
                                ) : (
                                    <pre className="p-3 text-xs whitespace-pre-wrap break-words text-(--base-08) max-h-[420px] overflow-auto">
                                        {preview.text}
                                    </pre>
                                )}
                            </div>
                        )}
                    </div>
                )}
            </div>
        </SettingsCard>
    );
}
