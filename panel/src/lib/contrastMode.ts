// High-contrast mode: a per-viewer display preference, so it lives in this
// browser's localStorage and nowhere else. The CSS is the
// :root[data-contrast="high"] block in app/globals.css.
//
// 'high' and 'normal' are explicit choices; no stored value follows the OS
// (prefers-contrast: more). Storing 'normal' rather than removing the key is
// what lets someone whose OS asks for more contrast still turn it off here.

export const CONTRAST_KEY = 'dylaris:contrast';

export function wantsHighContrast(stored: string | null, prefersMore: boolean): boolean {
    if (stored === 'high') return true;
    if (stored === 'normal') return false;
    return prefersMore;
}

// Runs inline in <head> before first paint (app/layout.tsx), so it cannot import
// anything and must mirror wantsHighContrast. contrastMode.test.ts executes this
// exact string against the same cases. Storage can throw (private mode, blocked
// site data); the OS preference still applies then.
export const CONTRAST_BOOTSTRAP =
    `(function(){var s=null;try{s=localStorage.getItem(${JSON.stringify(CONTRAST_KEY)})}catch(e){}` +
    `var m=false;try{m=window.matchMedia('(prefers-contrast: more)').matches}catch(e){}` +
    `if(s==='high'||(s!=='normal'&&m))document.documentElement.setAttribute('data-contrast','high')})()`;

export function isHighContrast(): boolean {
    if (typeof document === 'undefined') return false;
    return document.documentElement.getAttribute('data-contrast') === 'high';
}

export function setHighContrast(on: boolean): void {
    if (on) document.documentElement.setAttribute('data-contrast', 'high');
    else document.documentElement.removeAttribute('data-contrast');
    try {
        localStorage.setItem(CONTRAST_KEY, on ? 'high' : 'normal');
    } catch {
        // Applies for this page view only; nothing else to do.
    }
}
