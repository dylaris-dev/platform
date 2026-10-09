"use client";

import { useEffect, useRef, useState, useMemo } from 'react';
import CodeMirror from '@uiw/react-codemirror';
import type { Extension } from '@codemirror/state';
import { json } from '@codemirror/lang-json';
import { yaml } from '@codemirror/lang-yaml';
import { javascript } from '@codemirror/lang-javascript';
import { xml } from '@codemirror/lang-xml';
import { EditorView, type ViewUpdate } from '@codemirror/view';
import { dylarisTheme, dylarisHighlight } from './codemirror-theme';
import { propertiesLanguage } from './codemirror-properties';
import { stripGz } from './utils';

export type FileLanguage = 'json' | 'yaml' | 'properties' | 'javascript' | 'xml' | 'plain';

export function detectLanguage(filename: string): FileLanguage {
  const lower = stripGz(filename).toLowerCase();
  const ext = lower.includes('.') ? lower.substring(lower.lastIndexOf('.')) : '';
  switch (ext) {
    case '.json':
    case '.json5':
      return 'json';
    case '.yml':
    case '.yaml':
      return 'yaml';
    case '.properties':
    case '.cfg':
    case '.conf':
    case '.config':
    case '.ini':
      return 'properties';
    case '.js':
    case '.mjs':
    case '.cjs':
    case '.ts':
      return 'javascript';
    case '.xml':
    case '.html':
    case '.htm':
      return 'xml';
    default:
      return 'plain';
  }
}

function getLanguageExtension(lang: FileLanguage): Extension[] {
  switch (lang) {
    case 'json':
      return [json()];
    case 'yaml':
      return [yaml()];
    case 'properties':
      return [propertiesLanguage()];
    case 'javascript':
      return [javascript({ typescript: false })];
    case 'xml':
      return [xml()];
    case 'plain':
    default:
      return [];
  }
}

interface CodeMirrorEditorProps {
  value: string;
  onChange: (next: string) => void;
  filename: string;
  readOnly?: boolean;
  // Selection to move to and scroll into view. A new object is a new jump, so
  // the caller sets it only when the user navigates, never while they type.
  jumpTo?: { from: number; to: number } | null;
  className?: string;
  onCursorChange?: (line: number, col: number) => void;
}

export default function CodeMirrorEditor({
  value,
  onChange,
  filename,
  readOnly,
  jumpTo,
  className,
  onCursorChange,
}: CodeMirrorEditorProps) {
  const language = useMemo(() => detectLanguage(filename), [filename]);
  const [extensions, setExtensions] = useState<Extension[]>([]);
  const viewRef = useRef<EditorView | null>(null);

  useEffect(() => {
    setExtensions([
      ...getLanguageExtension(language),
      dylarisTheme,
      dylarisHighlight,
      EditorView.lineWrapping,
      EditorView.updateListener.of((update: ViewUpdate) => {
        if (onCursorChange && update.selectionSet) {
          const head = update.state.selection.main.head;
          const line = update.state.doc.lineAt(head);
          onCursorChange(line.number, head - line.from + 1);
        }
      }),
    ]);
  }, [language, onCursorChange]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view || !jumpTo) return;
    // Offsets come from a debounced scan, so the doc may have moved under them.
    const len = view.state.doc.length;
    const from = Math.min(jumpTo.from, len);
    view.dispatch({
      selection: { anchor: from, head: Math.min(jumpTo.to, len) },
      scrollIntoView: true,
    });
  }, [jumpTo]);

  return (
    <div className={className} style={{ height: '100%' }}>
      {/* @uiw wraps .cm-editor in its own div with auto height, so height="100%"
          alone resolved against nothing: the editor grew to the whole document
          and the clipped parent hid everything below the fold, unscrollable.
          The wrapper needs the height too for .cm-scroller to scroll. */}
      <CodeMirror
        style={{ height: '100%' }}
        value={value}
        onChange={onChange}
        extensions={extensions}
        readOnly={readOnly}
        theme="dark"
        basicSetup={{
          lineNumbers: true,
          foldGutter: true,
          highlightActiveLine: !readOnly,
          highlightActiveLineGutter: !readOnly,
          bracketMatching: true,
          closeBrackets: !readOnly,
          autocompletion: language === 'json' || language === 'yaml',
          searchKeymap: true,
        }}
        height="100%"
        onCreateEditor={(view: EditorView) => {
          viewRef.current = view;
        }}
      />
    </div>
  );
}
