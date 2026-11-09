// Settings panel (docs/04 §14).
import { app } from '../app.js';
import { h, confirmDlg, download, toast, inline } from './dom.js';
import * as settings from '../settings.js';
import * as wake from '../wakelock.js';
import { USB_TUNING } from '../transport.js';

export function initSettings(root, { applyTheme }) {
  const s = settings.get();
  const msg = h('div');
  const wakeNote = h('p.small.muted');
  const drawWake = (w) => {
    wakeNote.textContent = !w.supported ? 'This browser cannot keep the screen on (no Screen Wake Lock API); change the screen timeout in the system settings instead.'
      : w.active ? 'Screen is kept on now.' : w.wanted && w.error ? `The browser refused to keep the screen on (${w.error}); battery saver can cause this.`
        : 'A phone that locks its screen suspends this page, and the dongle then stops the capture after 2 s.';
  };
  wake.onChange(drawWake);
  drawWake(wake.state());
  const chk = (key, label) => h('label.row', h('input', { type: 'checkbox', checked: !!s[key], onchange: (e) => settings.update((x) => { x[key] = e.target.checked; }) }), label);
  const sel = (key, label, opts, after) => h('label.field', label,
    h('select', { onchange: (e) => { settings.update((x) => { x[key] = e.target.value; }); after?.(e.target.value); } },
      opts.map(([v, t]) => h('option', { value: v, selected: s[key] === v }, t))));
  const num = (key, label, min, max, after) => h('label.field', label,
    h('input', { type: 'number', min, max, value: s[key], onchange: (e) => {
      const v = Math.max(min, Math.min(max, Number(e.target.value) || min));
      e.target.value = v;
      settings.update((x) => { x[key] = v; });
      after?.(v);
    } }));

  root.append(
    h('h3', 'Session'),
    chk('autoCapture', ' Start capture automatically on connect'),
    chk('autoReconnect', ' Reconnect automatically when the link is lost'),
    sel('keepAwake', 'Keep the screen on', [['connected', 'While connected or recording'], ['always', 'Always while this page is open'], ['off', 'Never (the system decides)']], () => wake.refresh()),
    wakeNote,
    sel('transport', 'Transport', [['auto', 'Auto (Web Serial; on Android WebUSB until Web Serial shows a USB port)'], ['serial', 'Web Serial'], ['webusb', 'WebUSB (CDC-ACM shim)']]),
    h('h3', 'Packet list'),
    num('storeSize', 'Packet store size (packets kept; older ones are discarded)', 1000, 2000000, (v) => { app.post({ t: 'store-size', n: v }); toast('Packet store resized (list cleared)'); }),
    sel('timeFormat', 'Time column', [['rel', 'Relative to first packet'], ['abs', 'Host clock'], ['dev', 'Device µs']]),
    chk('colors', ' Colour rows by frame class'),
    sel('theme', 'Theme', [['auto', 'Follow system'], ['light', 'Light'], ['dark', 'Dark']], applyTheme),
    h('h3', 'Recording'),
    h('label.field', 'File name template ({date}, {ch})', h('input', { type: 'text', value: s.recName, onchange: (e) => settings.update((x) => { x.recName = e.target.value || 'capture_{date}.pcapng'; }) })),
    chk('recFiltered', ' Record only packets matching the display filter'),
    h('h3', 'Developer'),
    num('pingInterval', 'PING interval (ms)', 250, 2000),
    num('usbInflight', 'WebUSB reads queued (Android; next connect)', 2, 64, (v) => { USB_TUNING.inflight = v; }),
    sel('usbSize', 'WebUSB read size (next connect)', [['16384', '16 KB'], ['65536', '64 KB (newer Android only)']], (v) => { USB_TUNING.size = Number(v); }),
    chk('showRaw', ' Log every received control frame'),
    h('h3', 'Settings file'),
    h('div.row',
      h('button.small', { onclick: () => download('capturetonkeli-settings.json', settings.exportJSON(), 'application/json') }, 'Export'),
      h('label.small', h('button.small', { onclick: (e) => e.currentTarget.nextElementSibling.click() }, 'Import'),
        h('input.hidden', { type: 'file', accept: '.json', onchange: importS })),
      h('button.small.danger', { onclick: resetAll }, 'Reset all settings')),
    msg,
  );
  document.body.classList.toggle('colors', !!s.colors);
  root.addEventListener('change', () => document.body.classList.toggle('colors', !!settings.get().colors));

  async function importS(e) {
    const f = e.target.files[0];
    e.target.value = '';
    if (!f) return;
    try { settings.importJSON(await f.text()); toast('Settings imported – reloading'); setTimeout(() => location.reload(), 800); } catch (err) { inline(msg, err, 'Import settings'); }
  }
  async function resetAll() {
    if (await confirmDlg('Reset all settings?', 'Desired configuration, saved filters and preferences go back to defaults.', 'Reset', true)) { settings.reset(); location.reload(); }
  }
}
