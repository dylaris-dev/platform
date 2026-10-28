'use client';

import { useEffect, useState } from 'react';
import { usePathname } from 'next/navigation';
import { HelpCircle, ArrowLeft, ArrowRight, Check, X } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import GuardedLink from '@/components/GuardedLink';
import { contextualTour, tours, type TourTopic } from './tours';
import styles from './help.module.css';

export default function HelpTourButton({ topic, compact = false }: { topic?: TourTopic; compact?: boolean }) {
    const pathname = usePathname();
    const [open, setOpen] = useState(false);
    const [selected, setSelected] = useState<TourTopic>('servers');
    const [step, setStep] = useState(0);
    useEffect(() => setOpen(false), [pathname]);
    const tour = tours.find(item => item.id === selected)!;
    const current = tour.steps[step];
    return <>
        <button type="button" className="btn btn-ghost btn-sm" title="Help and guided examples" aria-label="Help and guided examples" onClick={() => { setSelected(topic || contextualTour(pathname)); setStep(0); setOpen(true); }}><HelpCircle size={15} />{!compact && 'Help'}</button>
        {open && <div className="modal-overlay" onClick={() => setOpen(false)}>
            <ModalPanel className={`modal-panel ${styles.dialog}`} onClose={() => setOpen(false)} onClick={event => event.stopPropagation()}>
                <div className="modal-header flex items-start justify-between gap-4"><div><p className="mono-label text-(--accent-light)">Guided examples</p><h2 className="modal-title mt-1">{tour.title}</h2></div><button className="btn btn-ghost btn-sm" aria-label="Close walkthrough" onClick={() => setOpen(false)}><X size={16} /></button></div>
                <div className={styles.safety} role="note">Example preview only. No servers, addresses, files, publications or billing changes are created. Your actual permissions stay unchanged.</div>
                <div className={styles.body}>
                    <nav aria-label="Help topics" className={styles.topics}>{tours.map(item => <button key={item.id} aria-current={item.id === selected ? 'step' : undefined} onClick={() => { setSelected(item.id); setStep(0); }}>{item.title}</button>)}<GuardedLink href="/help" className="btn btn-ghost btn-sm mt-4">Open help center</GuardedLink></nav>
                    <div className={styles.main}>
                        <div className="text-xs text-(--base-06)">Step {step + 1} of {tour.steps.length} · {current.area} / {current.tab}</div>
                        <div className={styles.preview} aria-label="Example screen">
                            <div className="flex justify-between items-center gap-3 border-b border-(--base-03) p-3"><strong className="text-sm">{current.area}</strong><span className="badge badge-neutral">Example data</span></div>
                            <nav className={styles.previewTabs} aria-label="Example walkthrough steps">{tour.steps.map((item, index) => <button key={index} aria-current={index === step ? 'step' : undefined} onClick={() => setStep(index)}>{item.tab}</button>)}</nav>
                            <dl className="p-4 space-y-3">{current.fields.map(([label, value]) => <div key={label} className="flex justify-between items-start gap-4 text-sm"><dt className="text-(--base-06)">{label}</dt><dd className="text-right font-medium">{value}</dd></div>)}</dl>
                        </div>
                        <h3 className="font-display text-xl">{current.title}</h3><p className="text-sm text-(--base-07) leading-relaxed">{current.body}</p>
                        <p className="text-xs text-(--base-06) leading-relaxed border-l-2 border-(--accent-border) pl-3">{current.note}</p>
                        <p className="text-xs text-(--base-06)">{tour.access}</p>
                        <div className="flex gap-1.5" aria-label="Walkthrough progress">{tour.steps.map((_, index) => <span key={index} className={`h-1 flex-1 rounded ${index <= step ? 'bg-(--accent)' : 'bg-(--base-03)'}`} />)}</div>
                    </div>
                </div>
                <div className="modal-footer justify-between"><button className="btn btn-secondary" onClick={() => setStep(value => value - 1)} disabled={step === 0}><ArrowLeft size={14} />Back</button><button className="btn btn-primary" onClick={() => { if (step === tour.steps.length - 1) setOpen(false); else setStep(value => value + 1); }}>{step === tour.steps.length - 1 ? <><Check size={14} />Finish walkthrough</> : <>Next<ArrowRight size={14} /></>}</button></div>
            </ModalPanel>
        </div>}
    </>;
}
