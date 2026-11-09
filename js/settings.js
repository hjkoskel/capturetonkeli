// Settings persisted in localStorage under a versioned key (docs/04 §14).
import { emptySet } from './filter.js';
import { DEFAULT_TYPE_MASK, DEFAULT_CTRL_MASK } from './wire.js';

const KEY = 'ctk.settings.v1';

function localeCountry() {
  try { return (new Intl.Locale(navigator.language).maximize().region ?? '').toUpperCase(); } catch { return ''; }
}

export function defaults() {
  return {
    desired: { country: { cc: localeCountry() || '01', mode: 0, schan: 1, nchan: 13 }, channel: 6, bw: 0, snaplen: 0,
      typeMask: DEFAULT_TYPE_MASK, ctrlMask: DEFAULT_CTRL_MASK, filter: emptySet() },
    autoCapture: true, autoReconnect: true, transport: 'auto', storeSize: 200000, timeFormat: 'rel', theme: 'auto', colors: true, keepAwake: 'connected',
    recName: 'capture_{date}_ch{ch}.pcapng', recFiltered: false, pingInterval: 500, logLevel: 'info', showRaw: false,
    savedFilters: {}, acceptedLegal: false, dontAskFw: {}, fpvChannel: 7, fpvKeep: 8, fpvPrevFilter: null, fpvSystem: 'hx', fpvFlip: 'none', dongleLabels: {},
    ohdChannel: 149, ohdBw: 20, ohdFollow: true, ohdGround: false, ohdKeys: null, ohdUnverified: false, ohdQueue: 1, ohdGapped: false, ohdCodec: 'auto', ohdFlip: 'none', ohdRecFormat: 'mp4', ohdPrevFilter: null,
  };
}

let cur = null;
export function load() {
  const d = defaults();
  try {
    const s = JSON.parse(localStorage.getItem(KEY) ?? 'null');
    cur = s ? { ...d, ...s, desired: { ...d.desired, ...s.desired, country: { ...d.desired.country, ...s.desired?.country } } } : d;
  } catch { cur = d; }
  return cur;
}
export const get = () => cur ?? load();
export function save() {
  try { localStorage.setItem(KEY, JSON.stringify(cur)); } catch { /* private mode / quota: settings stay in memory */ }
}
export function update(fn) { fn(get()); save(); }
export function reset() { cur = defaults(); save(); }
export function exportJSON() { return JSON.stringify(get(), null, 2); }
export function importJSON(text) {
  const s = JSON.parse(text);
  if (typeof s !== 'object' || !s.desired) throw new Error('This is not a capturetonkeli settings file');
  cur = { ...defaults(), ...s };
  save();
}
