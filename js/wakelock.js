// Keep the screen on (Screen Wake Lock API, docs/04 §18): a phone that dims and locks
// mid-capture suspends the tab, and the dongle's watchdog then stops the capture; FPV
// viewing needs the screen anyway. Setting keepAwake: 'connected' (default: while a
// dongle or the demo is connected, or a recording runs) | 'always' | 'off'.
// The browser drops the lock whenever the page is hidden; it is taken again on return.
import { app } from './app.js';
import * as settings from './settings.js';
import { log } from './log.js';

let lock = null, pending = false, lastError = '';
const listeners = new Set();
export const supported = () => 'wakeLock' in navigator;
export function state() { return { supported: supported(), active: !!lock, wanted: wanted(), error: lastError }; }
export function onChange(fn) { listeners.add(fn); }
const changed = () => { for (const fn of listeners) fn(state()); };

function wanted() {
  const m = settings.get().keepAwake ?? 'connected';
  return m === 'always' || (m === 'connected' && (app.conn === 'connected' || app.conn === 'reconnecting' || !!app.recording));
}

export async function refresh() {
  if (!supported()) return;
  const want = wanted() && document.visibilityState === 'visible';
  if (want && !lock && !pending) {
    pending = true;
    try {
      lock = await navigator.wakeLock.request('screen');
      lastError = '';
      lock.addEventListener('release', () => { lock = null; changed(); });
      log('debug', 'Screen wake lock on', 'page');
    } catch (e) { // e.g. battery saver, or the page went hidden meanwhile: not worth a banner
      lastError = e.message || String(e);
      log('debug', `Screen wake lock refused: ${lastError}`, 'page');
    } finally { pending = false; }
    if (lock && !wanted()) await refresh(); // the state changed while requesting
  } else if (!wanted() && lock) {
    const l = lock;
    lock = null;
    await l.release().catch(() => {});
    log('debug', 'Screen wake lock off', 'page');
  }
  changed();
}

export function initWakeLock() {
  for (const ev of ['conn', 'rec']) app.addEventListener(ev, refresh);
  document.addEventListener('visibilitychange', refresh);
  refresh();
}
