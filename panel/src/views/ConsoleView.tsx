"use client";

import React, { useState, useEffect, useLayoutEffect, useRef, memo } from 'react';
import { Server, ServerStats, sendConsoleCommand } from '@/lib/api';
import { API_URL } from '@/lib/api/core';
import { subscribeEventSource, type StreamStatus, type Subscription } from '@/lib/sse';
import { levelClass, type Level } from '@/lib/consoleLog';
import { appendLive, heldLines, levelled, mergeOrdered, oldestStreamId, type ConsoleLine } from '@/lib/consoleLines';
import { Power, Send, Cpu, MemoryStick, ArrowDown, RefreshCw } from 'lucide-react';
import { parseStatsSample } from '@/lib/statsSample';
import { jvmHeapMB } from '@/lib/jvmHeap';
import MemoryHelp from '@/components/MemoryHelp';

// Standard ANSI color codes (SGR 30-37, 40-47, 90-97). These are fixed by the
// ANSI spec for terminal color rendering and are intentionally NOT mapped to
// design tokens — server console output expects spec-faithful colors.
const ANSI_COLORS: Record<number, string> = {
  30: '#4d4d4d', 31: '#e74c3c', 32: '#2ecc71', 33: '#f39c12',
  34: '#3498db', 35: '#9b59b6', 36: '#1abc9c', 37: '#ecf0f1',
  90: '#7f8c8d', 91: '#ff6b6b', 92: '#55efc4', 93: '#f1c40f',
  94: '#74b9ff', 95: '#a29bfe', 96: '#81ecec', 97: '#ffffff',
};
const ANSI_BG: Record<number, string> = {
  40: '#4d4d4d', 41: '#e74c3c', 42: '#2ecc71', 43: '#f39c12',
  44: '#3498db', 45: '#9b59b6', 46: '#1abc9c', 47: '#ecf0f1',
};

function parseAnsiLine(line: string): React.ReactNode {
  const regex = /\x1b\[([0-9;]*)m/g;
  const parts: React.ReactNode[] = [];
  let lastIdx = 0;
  let style: React.CSSProperties = {};
  let match: RegExpExecArray | null;

  while ((match = regex.exec(line)) !== null) {
    if (match.index > lastIdx) {
      parts.push(<span key={parts.length} style={{ ...style }}>{line.slice(lastIdx, match.index)}</span>);
    }
    for (const c of match[1].split(';').map(Number)) {
      if (c === 0) style = {};
      else if (c === 1) style = { ...style, fontWeight: 'bold' };
      else if (c === 3) style = { ...style, fontStyle: 'italic' };
      else if (c === 4) style = { ...style, textDecoration: 'underline' };
      else if (ANSI_COLORS[c]) style = { ...style, color: ANSI_COLORS[c] };
      else if (ANSI_BG[c]) style = { ...style, backgroundColor: ANSI_BG[c] };
    }
    lastIdx = match.index + match[0].length;
  }

  if (lastIdx === 0) return line;
  if (lastIdx < line.length) {
    parts.push(<span key={parts.length} style={{ ...style }}>{line.slice(lastIdx)}</span>);
  }
  return parts;
}

// Memoised so a new line renders one row, not every row in a 5000-line buffer.
const ConsoleRow = memo(function ConsoleRow({ text, level, count }: { text: string; level: Level; count?: number }) {
  return (
    <div className={`whitespace-pre-wrap break-all leading-5 ${levelClass(level)}`}>
      {parseAnsiLine(text)}
      {count !== undefined && count > 1 && (
        <span className="ml-2 px-1.5 rounded bg-(--base-03) text-xs text-(--base-08)" title={`Repeated ${count} times`}>
          &times;{count.toLocaleString()}
        </span>
      )}
    </div>
  );
});

// Live lines are applied in batches: one render per line kept the browser
// busy for as long as a server spammed its console.
const LIVE_BATCH_MS = 100;

interface HistoryPage { lines?: string[]; ids?: string[]; more?: boolean; trimmed?: boolean }

const GAP_TEXT = 'Some lines printed while the console was disconnected were not retained.';

function historyLines(data: HistoryPage, nextKey: () => string): { key: string; text: string }[] {
  const ids = data.ids ?? [];
  return (data.lines ?? []).map((text, i) => ({ key: ids[i] || nextKey(), text }));
}

const COMMANDS = [
  'advancement', 'attribute', 'ban', 'ban-ip', 'banlist', 'bossbar', 'clear', 'clone',
  'damage', 'data', 'datapack', 'debug', 'defaultgamemode', 'deop', 'difficulty',
  'effect', 'enchant', 'execute', 'experience', 'fill', 'forceload', 'function',
  'gamemode', 'gamerule', 'give', 'help', 'item', 'kick', 'kill', 'list', 'locate',
  'loot', 'me', 'msg', 'op', 'pardon', 'pardon-ip', 'particle', 'place', 'playsound',
  'publish', 'recipe', 'reload', 'ride', 'return', 'say', 'schedule', 'scoreboard',
  'seed', 'setblock', 'setworldspawn', 'spawnpoint', 'spectate', 'spreadplayers',
  'stop', 'stopsound', 'summon', 'tag', 'team', 'teammsg', 'teleport', 'tell',
  'tellraw', 'tick', 'time', 'title', 'tp', 'trigger', 'weather', 'whitelist',
  'worldborder', 'xp',
];

const shortMB = (mb: number) => (mb >= 1024 ? `${(mb / 1024).toFixed(1)}G` : `${mb}M`);
const share = (used: number, limit: number) => (limit > 0 ? Math.min(100, (used / limit) * 100) : 0);

interface ConsoleViewProps {
  server: Server;
}

export default function ConsoleView({ server }: ConsoleViewProps) {
  const [lines, setLines] = useState<ConsoleLine[]>([]);
  // 'more': older lines can be fetched; 'start': Core said there are none
  // before startKeyRef; 'unknown': a Core that predates paging, so no top row.
  const [olderState, setOlder] = useState<'more' | 'loading' | 'start' | 'unknown'>('unknown');
  // Whether the start reached is where the log began, or only where retention
  // cut it. A Core that does not say is taken as the honest "retained" case.
  const [startTrimmed, setStartTrimmed] = useState(true);
  const startKeyRef = useRef('');
  const [streamStatus, setStreamStatus] = useState<StreamStatus>('connecting');
  const streamRef = useRef<Subscription | null>(null);
  // Following trims the top, so the start reached earlier may no longer be
  // the oldest line shown; then there is more to fetch again.
  const oldestId = oldestStreamId(lines);
  const older = olderState === 'start' && oldestId !== startKeyRef.current ? 'more' : olderState;
  const olderRef = useRef(older);
  olderRef.current = older;
  const linesRef = useRef(lines);
  linesRef.current = lines;
  // Bumped per server/sub-server so a page fetched for the previous one is dropped.
  const genRef = useRef(0);
  // scrollHeight before older lines were prepended, to keep the view anchored.
  const anchorRef = useRef<number | null>(null);
  const seqRef = useRef(0);
  const [liveStats, setLiveStats] = useState<ServerStats | null>(null);
  const [command, setCommand] = useState('');
  const [sendError, setSendError] = useState('');
  const logRef = useRef<HTMLDivElement>(null);
  // Follow new output only while the reader is at the bottom: jumping back
  // down on every line made scrolling up to read anything impossible.
  const followRef = useRef(true);
  const [following, setFollowing] = useState(true);
  // Lines that arrived while the reader was scrolled up, folded and capped
  // like the shown buffer. Held instead of appended, so the text under the
  // reader stays still; released when they return to the bottom.
  const heldRef = useRef<ConsoleLine[]>([]);
  const [heldCount, setHeldCount] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const sendingRef = useRef(false);

  const [suggestions, setSuggestions] = useState<string[]>([]);
  const [selectedSuggestion, setSelectedSuggestion] = useState(0);

  const activeSubServer = server.activeSubServer || '';

  // Stats stream (was passed as prop from Dashboard previously)
  useEffect(() => {
    setLiveStats(null);
    // Only the newest sample is shown, so a replayed buffer is one render,
    // not one per sample over the whole console.
    let latest: ServerStats | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const stop = subscribeEventSource(`/servers/${server.id}/stats/stream`, (e) => {
      const s = parseStatsSample(e.data);
      if (!s) return;
      latest = s;
      timer ??= setTimeout(() => { timer = null; setLiveStats(latest); }, LIVE_BATCH_MS);
    }, undefined, { pauseWhenHidden: true });
    return () => {
      if (timer) clearTimeout(timer);
      stop();
    };
  }, [server.id]);

  const nextKey = () => `local-${++seqRef.current}`;

  // Heap against -Xmx is what the server actually uses; the process figure
  // sits near its limit regardless, because the heap is committed up front.
  const heapMaxMb = server.memory > 0 ? jvmHeapMB(server.memory) : 0;
  const heapMb = liveStats?.javaHeapUsed && heapMaxMb > 0 ? liveStats.javaHeapUsed : 0;
  const historyUrl = (extra: string) => {
    const params = new URLSearchParams(extra);
    if (activeSubServer) params.set('sub_server', activeSubServer);
    const q = params.toString();
    return `${API_URL}/servers/${server.id}/console/history${q ? `?${q}` : ''}`;
  };

  useEffect(() => {
    const gen = ++genRef.current;
    setLines([]);
    setOlder('unknown');
    setStartTrimmed(true);
    startKeyRef.current = '';
    anchorRef.current = null;
    followRef.current = true;
    setFollowing(true);
    heldRef.current = [];
    setHeldCount(0);
    const pendingLines: ConsoleLine[] = [];
    let historyLoaded = false;
    let batch: { key: string; text: string }[] = [];
    let batchTimer: ReturnType<typeof setTimeout> | null = null;

    const flushBatch = () => {
      batchTimer = null;
      const entries = batch;
      batch = [];
      if (followRef.current) {
        setLines(prev => appendLive(prev, entries));
      } else {
        heldRef.current = appendLive(heldRef.current, entries);
        setHeldCount(heldLines(heldRef.current));
      }
    };

    const subParam = activeSubServer ? `?sub_server=${encodeURIComponent(activeSubServer)}` : '';
    const push = (entry: { key: string; text: string }) => {
      if (!historyLoaded) {
        pendingLines.push({ ...entry, level: 'info' }); // re-levelled on merge
        return;
      }
      batch.push(entry);
      batchTimer ??= setTimeout(flushBatch, LIVE_BATCH_MS);
    };
    // A reopened stream resumes after the last line received (?after=), and
    // appendLive drops anything replayed that is already shown.
    const stopStream = subscribeEventSource(
      `/servers/${server.id}/console/stream${subParam}`,
      (e) => push({ key: e.lastEventId || nextKey(), text: e.data }),
      { gap: () => push({ key: nextKey(), text: GAP_TEXT }) },
      { resumeParam: 'after', onStatus: setStreamStatus },
    );
    streamRef.current = stopStream;

    const settle = (history: ConsoleLine[]) => {
      if (gen !== genRef.current) return;
      historyLoaded = true;
      setLines(mergeOrdered(history, pendingLines));
    };
    fetch(historyUrl(''))
      .then(r => r.json())
      .then((data: HistoryPage) => {
        if (gen !== genRef.current) return;
        const oldest = data.ids?.[0] ?? '';
        startKeyRef.current = oldest;
        if (typeof data.more === 'boolean') setOlder(data.more && oldest ? 'more' : 'start');
        setStartTrimmed(data.trimmed !== false);
        settle(levelled(historyLines(data, nextKey)));
      })
      .catch(() => settle([]));

    return () => {
      if (batchTimer) clearTimeout(batchTimer);
      stopStream();
      if (streamRef.current === stopStream) streamRef.current = null;
    };
    // historyUrl and nextKey read only server.id, activeSubServer and refs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [server.id, activeSubServer]);

  const loadOlder = () => {
    const before = oldestStreamId(linesRef.current);
    if (olderRef.current !== 'more' || !before) return;
    const gen = genRef.current;
    olderRef.current = 'loading';
    setOlder('loading');
    fetch(historyUrl(`before=${encodeURIComponent(before)}&count=1000`))
      .then(r => {
        if (!r.ok) throw new Error(`history ${r.status}`);
        return r.json();
      })
      .then((data: HistoryPage) => {
        if (gen !== genRef.current) return;
        const page = levelled(historyLines(data, nextKey));
        startKeyRef.current = data.ids?.[0] || before;
        setOlder(data.more && page.length > 0 ? 'more' : 'start');
        setStartTrimmed(data.trimmed !== false);
        if (page.length === 0) return;
        anchorRef.current = logRef.current?.scrollHeight ?? null;
        setLines(prev => mergeOrdered(page, prev));
      })
      .catch(() => {
        // Left retryable: the next scroll to the top asks again.
        if (gen === genRef.current) setOlder('more');
      });
  };

  // A layout effect, so the position is corrected before the browser paints
  // the prepended lines.
  useLayoutEffect(() => {
    const el = logRef.current;
    if (el && anchorRef.current !== null) {
      el.scrollTop += el.scrollHeight - anchorRef.current;
      anchorRef.current = null;
      return;
    }
    if (el && followRef.current) el.scrollTop = el.scrollHeight;
  }, [lines]);

  const onLogScroll = () => {
    const el = logRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
    followRef.current = atBottom;
    setFollowing(atBottom);
    if (atBottom) releaseHeld();
    if (el.scrollTop < 50 && !atBottom) loadOlder();
  };

  const releaseHeld = () => {
    const held = heldRef.current;
    if (held.length === 0) return;
    heldRef.current = [];
    setHeldCount(0);
    setLines(prev => appendLive(prev, held));
  };

  const jumpToBottom = () => {
    const el = logRef.current;
    if (!el) return;
    followRef.current = true;
    setFollowing(true);
    releaseHeld();
    // At once: a smooth scroll fires scroll events short of the bottom, which
    // switched following off again while new lines kept arriving.
    el.scrollTop = el.scrollHeight;
  };

  useEffect(() => {
    if (!command.trim()) { setSuggestions([]); return; }

    const parts = command.split(/\s+/);
    const lastWord = parts[parts.length - 1].toLowerCase();

    if (parts.length === 1) {
      const matches = COMMANDS.filter(c => c.startsWith(lastWord)).slice(0, 6);
      setSuggestions(matches);
    } else {
      setSuggestions([]);
    }
    setSelectedSuggestion(0);
  }, [command]);

  const handleSend = async () => {
    const trimmed = command.trim();
    if (!trimmed || sendingRef.current) return;
    sendingRef.current = true;
    setCommand('');
    setSuggestions([]);
    setSendError('');
    jumpToBottom();
    try {
      // The input is cleared optimistically, which is right for a console, but
      // the answer was then discarded: a refused command (server not running, no
      // console.write) left an empty prompt and no echo in the stream, which is
      // exactly what a command that WAS accepted looks like. Put it back so it
      // can be re-sent, and say why.
      const res = await sendConsoleCommand(server.id, trimmed);
      if (res?.success === false) {
        setSendError(res.message || res.error || 'The command was not accepted.');
        setCommand(trimmed);
      }
    } catch {
      setSendError('The command could not be sent.');
      setCommand(trimmed);
    } finally {
      sendingRef.current = false;
      inputRef.current?.focus();
    }
  };

  const applySuggestion = (suggestion: string) => {
    const parts = command.split(/\s+/);
    parts[parts.length - 1] = suggestion;
    setCommand(parts.join(' ') + ' ');
    setSuggestions([]);
    inputRef.current?.focus();
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      handleSend();
    } else if (e.key === 'Tab') {
      e.preventDefault();
      if (suggestions.length > 0) {
        applySuggestion(suggestions[selectedSuggestion]);
      }
    } else if (e.key === 'ArrowUp' && suggestions.length > 0) {
      e.preventDefault();
      setSelectedSuggestion(prev => (prev > 0 ? prev - 1 : suggestions.length - 1));
    } else if (e.key === 'ArrowDown' && suggestions.length > 0) {
      e.preventDefault();
      setSelectedSuggestion(prev => (prev < suggestions.length - 1 ? prev + 1 : 0));
    } else if (e.key === 'Escape') {
      setSuggestions([]);
    }
  };

  const isOffline = server.status === 'stopped' || server.status === 'offline' || server.status === 'pending_setup';

  return (
    <div className="h-full flex flex-col card overflow-hidden">
      {/* Stats bar, and whether the console stream is live */}
      <div className="shrink-0 flex items-center gap-4 sm:gap-6 px-5 h-12 bg-(--base-01) border-b-2 border-(--base-03)">
        {liveStats && (<>
          <div className="flex items-center gap-2">
            <Cpu size={16} className="text-(--base-06)" />
            <span className="mono-label">CPU</span>
            <span className="text-sm font-semibold font-mono text-(--base-09)">{liveStats.cpu.toFixed(1)}%</span>
            <div className="hidden sm:block w-20 h-2 bg-(--base-03) rounded-full overflow-hidden">
              <div className="h-full bg-(--primary) rounded-full transition-all duration-500" style={{ width: `${Math.min(100, Math.max(0, liveStats.cpu))}%` }} />
            </div>
          </div>
          <div className="flex items-center gap-2">
            <MemoryStick size={16} className="text-(--base-06)" />
            <span className="mono-label">{heapMb > 0 ? 'Heap' : 'RAM'}</span>
            <span className="text-sm font-semibold font-mono text-(--base-09)">
              {heapMb > 0 ? `${shortMB(heapMb)}/${shortMB(heapMaxMb)}` : `${shortMB(liveStats.memUsed)}/${shortMB(liveStats.memLimit)}`}
            </span>
            <div className="hidden sm:block w-20 h-2 bg-(--base-03) rounded-full overflow-hidden">
              <div className="h-full bg-(--success) rounded-full transition-all duration-500" style={{ width: `${heapMb > 0 ? share(heapMb, heapMaxMb) : share(liveStats.memUsed, liveStats.memLimit)}%` }} />
            </div>
            {heapMb > 0 && (
              <span className="hidden md:inline text-xs font-mono text-(--base-06)">Process {shortMB(liveStats.memUsed)}/{shortMB(liveStats.memLimit)}</span>
            )}
            <MemoryHelp bookedMb={server.memory} limitMb={Math.max(liveStats.memLimit, server.memory || 0)} />
          </div>
        </>)}
        <div className="ml-auto flex items-center gap-2" role="status" aria-live="polite">
          {streamStatus === 'live' ? (
            <span className="badge badge-success"><span className="badge-dot bg-(--success)" />Live</span>
          ) : streamStatus === 'disconnected' ? (
            <>
              <span className="badge badge-error"><span className="badge-dot bg-(--error)" />Disconnected</span>
              <button type="button" onClick={() => streamRef.current?.reconnect()} className="btn btn-secondary btn-sm">
                <RefreshCw size={13} />
                Reconnect
              </button>
            </>
          ) : (
            <span className="badge badge-warning"><span className="badge-dot bg-(--warning)" />{streamStatus === 'connecting' ? 'Connecting...' : 'Reconnecting...'}</span>
          )}
        </div>
      </div>
      {/* Log output */}
      <div className="flex-1 min-h-0 relative">
      {/* overflow-anchor off: a prepend is anchored by hand above, and the
          browser's own scroll anchoring would shift the view a second time. */}
      <div ref={logRef} onScroll={onLogScroll} style={{ overflowAnchor: 'none' }} className="h-full overflow-y-auto p-4 font-mono text-sm bg-(--base-00)">
        {isOffline && lines.length === 0 ? (
          <div className="flex flex-col items-center justify-center h-full text-(--base-06)">
            <Power size={48} className="mb-3 opacity-30" />
            <p className="h-section text-(--base-07)">Server is offline</p>
            <p className="text-sm mt-1 text-(--base-06)">Start the server to see console output.</p>
          </div>
        ) : lines.length === 0 ? (
          <p className="text-(--base-06) italic">Waiting for server output...</p>
        ) : (
          <>
            {/* One fixed-height row for every state, so its text changing never moves the lines below. */}
            {older !== 'unknown' && (
              <p className="h-6 text-xs leading-5 text-(--base-06) italic" aria-live="polite">
                {older === 'loading' ? 'Loading older lines...' : older === 'start' ? (startTrimmed ? 'Start of retained history (last 5000 lines)' : 'Start of log') : ''}
              </p>
            )}
            {lines.map(line => <ConsoleRow key={line.key} text={line.text} level={line.level} count={line.count} />)}
          </>
        )}
      </div>
      {!following && lines.length > 0 && (
        <button
          type="button"
          onClick={jumpToBottom}
          className="absolute bottom-3 left-1/2 -translate-x-1/2 flex items-center gap-1.5 px-3 py-1.5 rounded-full bg-(--base-02) border border-(--base-04) text-xs font-medium text-(--base-08) shadow-md hover:text-(--base-09) hover:border-(--accent-border) transition-colors"
          title="Jump to the newest output"
        >
          <ArrowDown size={13} />
          {heldCount > 0 ? `${heldCount.toLocaleString()} new ${heldCount === 1 ? 'line' : 'lines'}` : 'Latest output'}
        </button>
      )}
      </div>

      {/* Command input with autocomplete */}
      <div className="shrink-0 relative">
        {suggestions.length > 0 && (
          <div className="dropdown-menu bottom-full left-0 right-0 rounded-b-none rounded-t-[--radius-xl] max-h-48 overflow-y-auto">
            {suggestions.map((s, i) => (
              <button
                key={s}
                onClick={() => applySuggestion(s)}
                className={`dropdown-item w-full font-mono text-sm ${
                  i === selectedSuggestion
                    ? 'bg-(--accent-ghost) text-(--accent-light)'
                    : ''
                }`}
              >
                {s}
              </button>
            ))}
          </div>
        )}
        {server.role === 'demo' ? (
          <div className="border-t border-(--base-03) px-3 py-2.5 bg-(--base-02) text-center text-xs text-(--base-06) font-mono">
            Read-only demo &mdash; console input disabled
          </div>
        ) : (
          <div className="border-t border-(--base-03) flex items-center bg-(--base-02)">
            {sendError && (
              <span className="px-3 py-2.5 text-xs text-(--error-light) font-mono shrink-0" role="alert">{sendError}</span>
            )}
            <span className="px-3 py-2.5 text-(--accent-light) font-mono font-medium select-none">&gt;</span>
            <input
              ref={inputRef}
              value={command}
              onChange={e => setCommand(e.target.value)}
              onKeyDown={handleKeyDown}
              placeholder="Enter command... (Tab for autocomplete)"
              className="flex-1 py-2.5 bg-transparent outline-none text-sm text-(--base-09) placeholder:text-(--base-06) font-mono"
              autoComplete="off"
              spellCheck={false}
            />
            <button
              onClick={handleSend}
              disabled={!command.trim()}
              className="px-4 py-2.5 text-(--accent-light) hover:bg-(--base-03) transition-colors disabled:opacity-30 disabled:cursor-not-allowed"
              title="Send command"
            >
              <Send size={20} />
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
