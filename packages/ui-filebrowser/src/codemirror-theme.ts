import { EditorView } from '@codemirror/view';
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
import { tags as t } from '@lezer/highlight';

// Theme keyed to the Dylaris design tokens (Carbon Steel x Plasma Violet).
// Uses CSS variables so it stays consistent if tokens are tweaked globally.
export const dylarisTheme = EditorView.theme(
  {
    '&': {
      color: 'var(--base-09)',
      backgroundColor: 'var(--base-01)',
      fontSize: '13px',
      fontFamily: 'var(--font-mono)',
    },
    '.cm-content': {
      caretColor: 'var(--accent-light)',
      padding: '12px 0',
    },
    '.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--accent-light)' },
    '&.cm-focused .cm-selectionBackgroundØ, .cm-selectionBackground, .cm-content ::selection': {
      backgroundColor: 'var(--accent-ghost)',
    },
    '.cm-activeLine': { backgroundColor: 'rgba(255,255,255,0.02)' },
    '.cm-activeLineGutter': { backgroundColor: 'var(--base-02)', color: 'var(--base-08)' },
    '.cm-gutters': {
      backgroundColor: 'var(--base-01)',
      color: 'var(--base-05)',
      border: 'none',
      borderRight: '1px solid var(--base-03)',
    },
    '.cm-lineNumbers .cm-gutterElement': { padding: '0 12px 0 8px' },
    '.cm-foldPlaceholder': {
      backgroundColor: 'var(--base-03)',
      border: 'none',
      color: 'var(--base-07)',
    },
    '.cm-tooltip': {
      backgroundColor: 'var(--base-02)',
      border: '1px solid var(--base-03)',
      color: 'var(--base-09)',
    },
    '.cm-tooltip-autocomplete > ul > li[aria-selected]': {
      backgroundColor: 'var(--accent-ghost)',
      color: 'var(--base-09)',
    },
    '.cm-matchingBracket, .cm-nonmatchingBracket': {
      backgroundColor: 'var(--accent-ghost)',
      outline: 'none',
    },
    '.cm-searchMatch': {
      backgroundColor: 'rgba(184,123,50,0.25)',
      outline: '1px solid var(--warning)',
    },
    '.cm-searchMatch.cm-searchMatch-selected': {
      backgroundColor: 'var(--warning-ghost)',
    },
    // Ctrl+F search / replace panel. CodeMirror ships no theme for the panel
    // chrome, so without these it renders with stock light-gray defaults
    // against the dark editor. Keyed to the Dylaris tokens instead.
    '.cm-panels': {
      backgroundColor: 'var(--base-02)',
      color: 'var(--base-08)',
    },
    '.cm-panels.cm-panels-bottom': { borderTop: '1px solid var(--base-03)' },
    '.cm-panels.cm-panels-top': { borderBottom: '1px solid var(--base-03)' },
    '.cm-panel.cm-search': {
      padding: '8px 10px',
      fontFamily: 'var(--font-mono)',
      fontSize: '12px',
      display: 'flex',
      flexWrap: 'wrap',
      alignItems: 'center',
      gap: '6px',
    },
    '.cm-panel.cm-search label': {
      fontSize: '11px',
      color: 'var(--base-07)',
      display: 'inline-flex',
      alignItems: 'center',
      gap: '3px',
    },
    '.cm-panel.cm-search input[type=checkbox]': {
      accentColor: 'var(--accent)',
    },
    '.cm-textfield': {
      backgroundColor: 'var(--base-01)',
      color: 'var(--base-09)',
      border: '1px solid var(--base-04)',
      borderRadius: '6px',
      padding: '3px 8px',
      fontFamily: 'var(--font-mono)',
      fontSize: '12px',
      outline: 'none',
    },
    '.cm-textfield:focus': {
      borderColor: 'var(--accent)',
      boxShadow: '0 0 0 3px rgba(112,72,200,0.15)',
    },
    '.cm-button': {
      backgroundColor: 'var(--base-03)',
      color: 'var(--base-08)',
      backgroundImage: 'none',
      border: '1px solid var(--base-04)',
      borderRadius: '6px',
      padding: '3px 10px',
      fontSize: '12px',
      cursor: 'pointer',
    },
    '.cm-button:hover': {
      backgroundColor: 'var(--base-04)',
      color: 'var(--base-09)',
    },
    '.cm-panel.cm-search [name=close]': {
      color: 'var(--base-06)',
      cursor: 'pointer',
      fontSize: '16px',
      padding: '0 6px',
    },
    '.cm-panel.cm-search [name=close]:hover': {
      color: 'var(--base-09)',
    },
    '.cm-scroller': {
      fontFamily: 'var(--font-mono)',
      lineHeight: '1.55',
    },
  },
  { dark: true },
);

export const dylarisHighlightStyle = HighlightStyle.define([
  { tag: t.keyword, color: 'var(--accent-light)', fontWeight: '600' },
  { tag: [t.name, t.deleted, t.character, t.propertyName, t.macroName], color: 'var(--primary-light)' },
  { tag: [t.function(t.variableName), t.labelName], color: 'var(--primary-light)' },
  { tag: [t.color, t.constant(t.name), t.standard(t.name)], color: 'var(--warning-light)' },
  { tag: [t.definition(t.name), t.separator], color: 'var(--base-08)' },
  { tag: [t.typeName, t.className, t.number, t.changed, t.annotation, t.modifier, t.self, t.namespace], color: 'var(--warning-light)' },
  { tag: [t.operator, t.operatorKeyword, t.url, t.escape, t.regexp, t.link, t.special(t.string)], color: 'var(--accent-light)' },
  // base-06 measured ~3:1 on base-01; config files are half comments.
  { tag: [t.meta, t.comment], color: 'var(--base-07)', fontStyle: 'italic' },
  { tag: t.strong, fontWeight: '700' },
  { tag: t.emphasis, fontStyle: 'italic' },
  { tag: t.strikethrough, textDecoration: 'line-through' },
  { tag: t.link, color: 'var(--accent-light)', textDecoration: 'underline' },
  { tag: t.heading, fontWeight: '700', color: 'var(--accent-light)' },
  { tag: [t.atom, t.bool, t.special(t.variableName)], color: 'var(--warning-light)' },
  // t.content is every YAML value (plain scalars are not t.string), so
  // without it most of a plugin config rendered in the base grey.
  { tag: [t.processingInstruction, t.string, t.inserted, t.content], color: 'var(--success-light)' },
  { tag: t.invalid, color: 'var(--error-light)' },
]);

export const dylarisHighlight = syntaxHighlighting(dylarisHighlightStyle);
