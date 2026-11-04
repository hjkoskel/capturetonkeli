// Unified log (docs/04 §12): device EVT_LOG, RSP_ERR, protocol and UI events.
const MAX = 5000;
const lines = [];
const listeners = new Set();
export const LEVELS = ['error', 'warn', 'info', 'debug'];

export function log(level, text, src = 'page') {
  const e = { t: new Date(), level, text: String(text), src };
  lines.push(e);
  if (lines.length > MAX) lines.shift();
  for (const l of listeners) l(e);
  if (level === 'error') console.error(`[ctk] ${text}`); else if (level === 'debug') console.debug(`[ctk] ${text}`);
}
export const all = () => lines;
export const subscribe = (fn) => { listeners.add(fn); return () => listeners.delete(fn); };
export const fmt = (e) => `${e.t.toISOString().slice(11, 23)} ${e.level.toUpperCase().padEnd(5)} [${e.src}] ${e.text}`;
export const text = () => lines.map(fmt).join('\n');
