"use client";

import React, { useState, useEffect, useMemo, useRef } from 'react';
import { Server, ServerStats, sendConsoleCommand } from '@/lib/api';
import { API_URL } from '@/lib/api/core';
import { subscribeEventSource } from '@/lib/sse';
import { levelClass, computeLineLevels } from '@/lib/consoleLog';
import { Power, Send, Cpu, MemoryStick } from 'lucide-react';

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

interface ConsoleViewProps {
  server: Server;
}

export default function ConsoleView({ server }: ConsoleViewProps) {
  const [lines, setLines] = useState<string[]>([]);
  const [liveStats, setLiveStats] = useState<ServerStats | null>(null);
  const [command, setCommand] = useState('');
  const [sendError, setSendError] = useState('');
  const bottomRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const sendingRef = useRef(false);

  const [suggestions, setSuggestions] = useState<string[]>([]);
  const [selectedSuggestion, setSelectedSuggestion] = useState(0);

  const activeSubServer = server.activeSubServer || '';

  // Continuation lines (exception header + `at ...` stack frames) carry no
  // level token of their own, so they inherit the level of the record they
  // follow - computed in one pass so the render loop below stays a simple map.
  const lineLevels = useMemo(() => computeLineLevels(lines), [lines]);

  // Stats stream (was passed as prop from Dashboard previously)
  useEffect(() => {
    setLiveStats(null);
    return subscribeEventSource(`/servers/${server.id}/stats/stream`, (e) => {
      try { setLiveStats(JSON.parse(e.data) as ServerStats); } catch { /* ignore */ }
    });
  }, [server.id]);

  useEffect(() => {
    setLines([]);
    const pendingLines: string[] = [];
    let historyLoaded = false;

    const subParam = activeSubServer ? `?sub_server=${encodeURIComponent(activeSubServer)}` : '';
    const stopStream = subscribeEventSource(`/servers/${server.id}/console/stream${subParam}`, (e) => {
      if (!historyLoaded) {
        pendingLines.push(e.data);
      } else {
        setLines(prev => [...prev.slice(-999), e.data]);
      }
    });

    const historyUrl = `${API_URL}/servers/${server.id}/console/history${activeSubServer ? `?sub_server=${encodeURIComponent(activeSubServer)}` : ''}`;
    fetch(historyUrl)
      .then(r => r.json())
      .then((data: { lines?: string[] }) => {
        historyLoaded = true;
        const history = data.lines ?? [];
        setLines([...history, ...pendingLines].slice(-1000));
      })
      .catch(() => {
        historyLoaded = true;
        setLines(pendingLines.slice(-1000));
      });

    return stopStream;
  }, [server.id, activeSubServer]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [lines]);

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
      {/* Stats bar */}
      {liveStats && (
        <div className="shrink-0 flex items-center gap-6 px-5 h-12 bg-(--base-01) border-b-2 border-(--base-03)">
          <div className="flex items-center gap-2">
            <Cpu size={16} className="text-(--base-06)" />
            <span className="mono-label">CPU</span>
            <span className="text-sm font-semibold font-mono text-(--base-09)">{liveStats.cpu.toFixed(1)}%</span>
            <div className="w-20 h-2 bg-(--base-03) rounded-full overflow-hidden">
              <div className="h-full bg-(--primary) rounded-full transition-all duration-500" style={{ width: `${Math.min(100, liveStats.cpuLimit > 0 ? (liveStats.cpu / (liveStats.cpuLimit * 100)) * 100 : liveStats.cpu)}%` }} />
            </div>
          </div>
          <div className="flex items-center gap-2">
            <MemoryStick size={16} className="text-(--base-06)" />
            <span className="mono-label">RAM</span>
            <span className="text-sm font-semibold font-mono text-(--base-09)">
              {liveStats.memUsed >= 1024 ? `${(liveStats.memUsed / 1024).toFixed(1)}G` : `${liveStats.memUsed}M`}/{liveStats.memLimit >= 1024 ? `${(liveStats.memLimit / 1024).toFixed(1)}G` : `${liveStats.memLimit}M`}
            </span>
            <div className="w-20 h-2 bg-(--base-03) rounded-full overflow-hidden">
              <div className="h-full bg-(--success) rounded-full transition-all duration-500" style={{ width: `${liveStats.memLimit > 0 ? (liveStats.memUsed / liveStats.memLimit) * 100 : 0}%` }} />
            </div>
          </div>
        </div>
      )}
      {/* Log output */}
      <div className="flex-1 overflow-y-auto p-4 font-mono text-sm bg-(--base-00)">
        {isOffline && lines.length === 0 ? (
          <div className="flex flex-col items-center justify-center h-full text-(--base-06)">
            <Power size={48} className="mb-3 opacity-30" />
            <p className="h-section text-(--base-07)">Server is offline</p>
            <p className="text-sm mt-1 text-(--base-06)">Start the server to see console output.</p>
          </div>
        ) : lines.length === 0 ? (
          <p className="text-(--base-06) italic">Waiting for server output...</p>
        ) : (
          lines.map((line, i) => (
            <div key={i} className={`whitespace-pre-wrap break-all leading-5 ${levelClass(lineLevels[i])}`}>
              {parseAnsiLine(line)}
            </div>
          ))
        )}
        <div ref={bottomRef} />
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
