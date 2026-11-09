// Bootstrap: feature detection, start screen, top bar, tabs, banners,
// dialogs, keyboard shortcuts and the global error net.
import { app } from './app.js';
import { h, $, banner, clearBanner, dialog, toast, messageEl } from './ui/dom.js';
import { unsupportedReason, describe } from './errors.js';
import { SerialTransport, UsbTransport, pickTransport } from './transport.js';
import * as settings from './settings.js';
import * as W from './wire.js';
import { log, subscribe } from './log.js';
import { initRadio } from './ui/radio.js';
import { initFilter } from './ui/filterpanel.js';
import { initStats } from './ui/stats.js';
import { initDevice } from './ui/device.js';
import { initFpvTab } from './ui/fpv.js';
import { initLog } from './ui/logpanel.js';
import { initSettings } from './ui/settingspanel.js';
import { initPackets } from './ui/packets.js';
import * as flasher from './flasher.js';
import { initWakeLock } from './wakelock.js';
import { versionInfo, versionLines } from './version.js';

// ---- global error net: nothing fails silently -----------------------------------------
window.addEventListener('error', (e) => internalError(e.error ?? e.message));
window.addEventListener('unhandledrejection', (e) => internalError(e.reason));
function internalError(err) {
  const d = describe(err, 'Internal error');
  log('error', `${d.title}: ${d.text}${err?.stack ? '\n' + err.stack : ''}`, 'page');
  banner('internal', { ...d, title: 'Unexpected error in the page', actions: ['The capture usually keeps running. If something looks wrong, reload the page.', 'Please copy the log (Log tab → Copy all) and report it.'] },
    [h('button', { onclick: () => selectTab('log') }, 'Open log')]);
}

const s = settings.load();
applyTheme(s.theme);
export function applyTheme(t) {
  if (t === 'auto') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t;
}

// ---- tabs ------------------------------------------------------------------------------
const main = $('#app');
export function selectTab(name) {
  for (const b of document.querySelectorAll('#tabs [role=tab]')) b.setAttribute('aria-selected', String(b.dataset.tab === name));
  for (const p of document.querySelectorAll('.panel')) p.classList.toggle('hidden', p.id !== `panel-${name}`);
  main.classList.toggle('show-packets', name === 'packets');
  if (name === 'log') $('#log-badge').classList.add('hidden');
}
$('#tabs').addEventListener('click', (e) => { const t = e.target.closest('[data-tab]'); if (t) selectTab(t.dataset.tab); });
if (matchMedia('(max-width: 899px)').matches) selectTab('packets');

// ---- start screen ------------------------------------------------------------------------
const support = unsupportedReason();
if (support) {
  $('#support').append(messageEl(support));
  $('#start-connect').disabled = true;
  $('#start-flash').disabled = true;
  $('#btn-connect').disabled = true;
}
main.classList.add('has-start');
function hideStart() { $('#start').classList.add('hidden'); main.classList.remove('has-start'); }
async function offerKnown() {
  if (support) return;
  try {
    const T = pickTransport(s.transport);
    const known = T === SerialTransport ? await SerialTransport.known() : T === UsbTransport ? await UsbTransport.known() : [];
    if (known.length === 1) {
      const b = $('#start-reconnect');
      b.textContent = 'Reconnect to your dongle';
      b.classList.remove('hidden');
      b.onclick = () => { hideStart(); app.connect(known[0]); };
    }
  } catch (e) { log('debug', `getPorts failed: ${e.message}`, 'page'); }
}

$('#start-connect').onclick = () => { hideStart(); doConnect(); };
$('#start-demo').onclick = () => { hideStart(); app.connect('demo'); };
$('#start-flash').onclick = () => { hideStart(); flasher.flashBare(); };

async function doConnect() {
  if (!(await legalOk())) return;
  const d = await app.connect('pick');
  if (d && d.level === 'info') toast(d.title);
}

// ---- top bar ------------------------------------------------------------------------------
const btnConnect = $('#btn-connect');
btnConnect.onclick = () => {
  hideStart();
  if (app.conn === 'disconnected') doConnect(); else app.disconnect();
};
const CONN_TEXT = { disconnected: 'Disconnected', choosing: 'Choose the dongle…', opening: 'Opening port…', handshaking: 'Handshake…', connected: 'Connected', reconnecting: 'Reconnecting…' };
app.addEventListener('conn', (e) => {
  const st = e.detail;
  btnConnect.textContent = st === 'disconnected' ? 'Connect' : st === 'connected' ? 'Disconnect' : 'Cancel';
  btnConnect.classList.toggle('primary', st === 'disconnected');
  $('#link-text').textContent = CONN_TEXT[st] + (st === 'connected' && app.transport?.kind === 'demo' ? ' (demo)' : '') + groupText();
  $('#link-dot').className = `dot ${st === 'connected' ? 'green' : st === 'disconnected' ? 'grey' : 'yellow'}`;
  $('#btn-capture').disabled = st !== 'connected';
  $('#btn-record').disabled = st !== 'connected' && !app.recording;
  if (st === 'disconnected') $('#link-rtt').textContent = '';
  if (st !== 'disconnected') hideStart();
});
function groupText() { const n = app.members.length; return app.conn === 'connected' && n > 1 ? ` · ${n} dongles` : ''; }
app.addEventListener('members', () => {
  if (app.conn !== 'connected') return;
  $('#link-text').textContent = CONN_TEXT.connected + (app.transport?.kind === 'demo' ? ' (demo)' : '') + groupText();
  document.querySelector('.group-note')?.classList.toggle('hidden', app.members.length < 2);
});
app.addEventListener('link', (e) => {
  const map = { up: 'green', degraded: 'yellow', dead: 'red', down: 'grey' };
  if (app.conn === 'connected') {
    $('#link-dot').className = `dot ${map[e.detail]}`;
    $('#link-text').textContent = (e.detail === 'degraded' ? 'Connected – dongle slow to answer' : e.detail === 'dead' ? 'Link dead' : 'Connected') + groupText();
  }
});
let lastHostLost = 0, lastLossAt = -Infinity;
app.addEventListener('host', (e) => {
  const st = e.detail;
  const r = app.client?.rttStats();
  if (r && app.conn === 'connected') {
    $('#link-rtt').textContent = `RTT ${r.median.toFixed(1)} ms`;
    $('#link-rtt').title = `median ${r.median.toFixed(2)} ms, p95 ${r.p95.toFixed(2)} ms (last 20 PINGs)`;
  }
  if (st.linkLost + st.overruns > lastHostLost) { lastLossAt = performance.now(); lastHostLost = st.linkLost + st.overruns; }
  if (app.conn === 'connected' && app.client?.link === 'up') {
    $('#link-dot').className = `dot ${performance.now() - lastLossAt < 5000 ? 'yellow' : 'green'}`;
  }
});
app.addEventListener('info', (e) => {
  const fb = $('#fw-badge');
  const i = e.detail;
  if (!i) { fb.classList.add('hidden'); return; }
  const flags = [i.fwFlags & W.FW_FLAG.DIRTY && 'dirty', i.fwFlags & W.FW_FLAG.DEV && 'dev', i.fwFlags & W.FW_FLAG.IDF_MISMATCH && 'idf≠'].filter(Boolean);
  fb.textContent = `fw ${W.fwString(i.fw)}${flags.length ? ' ' + flags.join(' ') : ''} · proto ${i.protoVer} · ${W.macString(i.mac).slice(9)}`;
  fb.className = `badge${flags.length || app.unsupportedFw ? ' warn' : ''}`;
  fb.title = app.unsupportedFw ? 'Unsupported firmware version for this page' : flags.length ? 'Development firmware build' : 'Firmware on the dongle';
  if (app.unsupportedFw) {
    banner('unsupported', { level: 'warn', title: `Firmware ${W.fwString(i.fw)} is not the version this page was tested with`,
      text: `Page expects ${app.hostFw ? `${app.hostFw.major}.${app.hostFw.minor}.x` : '?'}. Running in unsupported mode; recordings are marked.`, actions: [] });
  }
});

const btnCap = $('#btn-capture');
btnCap.onclick = async () => {
  const on = !app.config?.captureOn;
  const d = await app.setCapture(on);
  if (d) banner('capture', d);
};
app.addEventListener('config', (e) => {
  const on = e.detail.captureOn;
  btnCap.textContent = on ? '■ Stop' : '▶ Capture';
  btnCap.classList.toggle('on', on);
});
const btnRec = $('#btn-record');
btnRec.onclick = async () => {
  if (app.recording) { app.stopRecording(); return; }
  const d = await app.startRecording();
  if (d) banner('rec', d);
};
app.addEventListener('rec', (e) => {
  const r = e.detail;
  btnRec.classList.toggle('on', !!r);
  btnRec.textContent = r ? `■ ${(r.bytes / 1e6).toFixed(1)} MB` : '● Record';
  btnRec.disabled = !r && app.conn !== 'connected';
});
app.addEventListener('rec-done', (e) => toast(`Saved ${e.detail.name}: ${e.detail.packets} packets`, 4000));
window.addEventListener('beforeunload', (e) => {
  if (app.recording || app.flashing) { e.preventDefault(); e.returnValue = ''; }
});

// menu
const menu = $('#menu'), btnMenu = $('#btn-menu');
btnMenu.onclick = () => { const open = menu.classList.toggle('hidden'); btnMenu.setAttribute('aria-expanded', String(!open)); };
document.addEventListener('click', (e) => { if (!e.target.closest('#menu, #btn-menu')) menu.classList.add('hidden'); });
menu.addEventListener('click', async (e) => {
  const act = e.target.dataset.act;
  menu.classList.add('hidden');
  if (act === 'disconnect') app.disconnect();
  if (act === 'export') {
    const filtered = !!$('#dfilter').value.trim();
    const n = await app.exportBuffer(filtered);
    toast(`Exported ${n} packets${filtered ? ' (display filter applied)' : ''}`);
  }
  if (act === 'install') promptInstall();
  if (act === 'help') showHelp();
  if (act === 'about') showAbout();
});

// ---- banners from the app ------------------------------------------------------------------
app.addEventListener('banner', (e) => {
  const { key, d, reconnect } = e.detail;
  const buttons = [];
  if (reconnect) buttons.push(h('button.primary', { onclick: () => { clearBanner(key); app.reconnect(); } }, 'Reconnect'));
  if (d.level === 'error' || d.level === 'warn') { $('#log-badge').textContent = '!'; $('#log-badge').classList.remove('hidden'); }
  banner(key, d, buttons);
});
app.addEventListener('banner-clear', (e) => clearBanner(e.detail));
subscribe((entry) => { if (entry.level === 'error') { $('#log-badge').textContent = '!'; $('#log-badge').classList.remove('hidden'); } });

// ---- firmware mismatch dialog (docs/04 §4.1) ------------------------------------------------
app.addEventListener('version-mismatch', async (e) => {
  const { err, resolve } = e.detail;
  const dev = W.fwString(err.info.fw), page = app.hostFw ? `${app.hostFw.major}.${app.hostFw.minor}.${app.hostFw.patch}` : '?';
  const cb = h('input', { type: 'checkbox', id: 'dontask' });
  const body = [
    h('p', h('strong', 'Dongle: '), `${dev} (build ${Array.from(err.info.buildId, (b) => b.toString(16).padStart(2, '0')).join('').slice(0, 8)}…)`),
    h('p', h('strong', 'Page: '), `${page}${app.manifest ? ` (bundled firmware build ${String(app.manifest.build_id).slice(0, 8)}…)` : ''}`),
    h('p', err.protocol ? 'This page cannot talk to the firmware on the dongle. The firmware must be updated.' : `This page is tested only with firmware ${app.hostFw?.major}.${app.hostFw?.minor}.x.`),
    h('p.muted.small', 'Updating writes only the flash sectors that changed (MD5-compared) and takes about 5–20 s. Keep the dongle plugged in.'),
  ];
  if (!err.protocol) body.push(h('label', cb, ' Don\'t ask again for this dongle and firmware version'));
  const buttons = [{ label: 'Cancel', value: 'cancel' }];
  if (!err.protocol) buttons.push({ label: `Continue with ${dev} (unsupported)`, value: 'continue' });
  if (app.manifest) {
    const newer = app.hostFw && W.fwToWire(err.info.fw) > W.fwToWire(app.hostFw);
    buttons.push({ label: err.protocol ? `Flash bundled firmware ${app.manifest.version}` : newer ? `Install release firmware ${app.manifest.version} (downgrade)` : `Update dongle to ${app.manifest.version}`,
      value: err.protocol ? 'flash' : 'update', primary: true });
  }
  const choice = await dialog(err.protocol ? 'Protocol version mismatch' : 'Firmware version mismatch', body, buttons);
  resolve({ choice: choice || 'cancel', dontAsk: cb.checked });
});

app.addEventListener('request-flash', (e) => (e.detail === 'flash' ? flasher.flashBare() : flasher.updateToBundled()));

// ---- legal notice (docs/04 §15) -----------------------------------------------------------------
async function legalOk() {
  if (settings.get().acceptedLegal) return true;
  const ok = (await dialog('Before you capture', [
    'capturetonkeli puts the dongle in receive-only monitor mode: it listens to Wi-Fi frames and never transmits.',
    h('strong', 'Capture only networks you own or are authorised to test.'),
    ' Intercepting other people\'s communications may be illegal (e.g. in Finland under the communications secrecy provisions). This is not legal advice.',
  ], [{ label: 'Cancel', value: 'no' }, { label: 'I understand', value: 'ok', primary: true }])) === 'ok';
  if (ok) settings.update((x) => { x.acceptedLegal = true; });
  return ok;
}
// ---- version (docs/04 §19): start page, About, log ------------------------------------------------
let versionText = ['Version: …', ''];
async function showVersion() {
  versionText = versionLines(await versionInfo(app.manifest));
  $('#version-head').textContent = versionText[0];
  $('#version-detail').textContent = versionText[1];
  log('info', `${versionText[0]} · ${versionText[1]}`, 'page');
}
$('#version-copy').onclick = async () => {
  try { await navigator.clipboard.writeText(`capturetonkeli ${versionText.join('\n')}\n${navigator.userAgent}`); toast('Version information copied'); } catch (e) { toast(`Copy failed: ${e.message}`); }
};

function showAbout() {
  dialog('About capturetonkeli', [
    h('p.small', versionText[0], h('br'), versionText[1]),
    `Web UI for the XIAO ESP32-C5 capture dongle. Protocol CTP v${W.PROTO_VER}. Page firmware bundle: ${app.manifest ? app.manifest.version : 'none (development page)'}.`,
    'Receive-only: the dongle never transmits. Capture only networks you own or are authorised to test.',
    h('p', h('a', { href: 'docs/04-web-ui-spec.md' }, 'Specification'), ' · ', h('a', { href: 'docs/03-usb-link-protocol.md' }, 'Protocol')),
    h('p.small', 'Copyright 2026 Henri Koskela. Licensed under the ', h('a', { href: 'LICENSE' }, 'Apache License 2.0'),
      '. The firmware images also contain ESP-IDF and other ', h('a', { href: 'THIRD_PARTY_NOTICES.md' }, 'third-party software'), '.'),
  ]);
}
function showHelp() {
  dialog('Keyboard shortcuts', [h('div.kv', ...[['C', 'connect / disconnect'], ['S', 'start / stop capture'], ['R', 'record'], ['Space', 'freeze the packet list'],
    ['← / →', 'previous / next channel'], ['/', 'focus the display filter'], ['Ctrl+E', 'export buffer'], ['?', 'this help']].flatMap(([k, v]) => [h('dt', k), h('dd', v)]))]);
}

// ---- keyboard shortcuts (docs/04 §16) -------------------------------------------------------------
document.addEventListener('keydown', (e) => {
  if (e.target.closest('input, textarea, select, dialog') || e.altKey || e.metaKey) return;
  if (e.ctrlKey && e.key.toLowerCase() === 'e') { e.preventDefault(); menu.querySelector('[data-act=export]').click(); return; }
  if (e.ctrlKey) return;
  const k = e.key;
  if (k === 'c' || k === 'C') btnConnect.click();
  else if ((k === 's' || k === 'S') && !btnCap.disabled) btnCap.click();
  else if ((k === 'r' || k === 'R') && !btnRec.disabled) btnRec.click();
  else if (k === '/') { e.preventDefault(); $('#dfilter').focus(); }
  else if (k === '?') showHelp();
  else return;
  e.preventDefault();
});

// ---- panels ----------------------------------------------------------------------------------------
initRadio($('#panel-radio'));
initFilter($('#panel-filter'));
initStats($('#panel-stats'));
initFpvTab($('#panel-fpv'));
initDevice($('#panel-device'));
initLog($('#panel-log'));
initSettings($('#panel-settings'), { applyTheme });
initPackets();
initWakeLock();
app.loadManifest().then(showVersion);
offerKnown();
log('info', `Page loaded (${navigator.userAgent})`, 'page');

// ---- installable app (docs/04 §18) -----------------------------------------------------------
// Service worker: offline + update banner (docs/11 §3). Failures only log. Not on plain
// http (make serve), so development never runs stale cached files.
if ('serviceWorker' in navigator && location.protocol === 'https:') {
  let reloading = false;
  navigator.serviceWorker.addEventListener('controllerchange', () => { if (reloading) location.reload(); });
  const offerUpdate = (reg) => {
    if (!reg.waiting || !navigator.serviceWorker.controller) return; // first install: nothing to replace
    banner('update', { level: 'info', title: 'A new version of the app is ready', text: 'Reload to use it. Nothing changes until you do.', actions: [] },
      [h('button.primary', { onclick: () => {
        if (app.recording) { toast('Stop the recording first, then reload'); return; }
        reloading = true; reg.waiting.postMessage({ t: 'skip-waiting' });
      } }, 'Reload')]);
  };
  navigator.serviceWorker.register('./sw.js', { scope: './' }).then((reg) => {
    offerUpdate(reg);
    reg.addEventListener('updatefound', () => {
      const w = reg.installing;
      w?.addEventListener('statechange', () => { if (w.state === 'installed') offerUpdate(reg); });
    });
    // an installed app stays open for days: look for a new version every 30 min and when it comes back to the front
    const check = () => reg.update().catch(() => { /* offline */ });
    setInterval(check, 30 * 60 * 1000);
    document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') check(); });
  }).catch((e) => log('debug', `service worker: ${e.message}`, 'page'));
}

// "Install as app": Chrome/Edge (desktop, Android) fire beforeinstallprompt when the page is installable.
let installEvt = null;
const standalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
function showInstall(on) { menu.querySelector('[data-act=install]').classList.toggle('hidden', !on); $('#start-install').classList.toggle('hidden', !on); }
window.addEventListener('beforeinstallprompt', (e) => { e.preventDefault(); installEvt = e; showInstall(!standalone()); });
window.addEventListener('appinstalled', () => { installEvt = null; showInstall(false); toast('Installed: start capturetonkeli from your home screen or app menu', 5000); log('info', 'Installed as app', 'page'); });
async function promptInstall() {
  if (!installEvt) { toast('Use the browser menu → Install app / Add to Home screen'); return; }
  const e = installEvt;
  installEvt = null;
  showInstall(false);
  const { outcome } = await e.prompt().then(() => e.userChoice).catch(() => ({ outcome: 'error' }));
  log('info', `Install prompt: ${outcome}`, 'page');
}
$('#start-install-btn').onclick = promptInstall;
if (standalone()) log('info', 'Running as installed app', 'page');

// App shortcut "Demo" (manifest) and links to ./#demo start demo mode.
function demoFromUrl(url) {
  if (new URL(url).hash !== '#demo') return;
  history.replaceState(null, '', location.pathname + location.search);
  if (app.conn === 'disconnected') { hideStart(); app.connect('demo'); }
}
demoFromUrl(location.href);
window.addEventListener('hashchange', () => demoFromUrl(location.href));
// launch_handler focus-existing: a shortcut tapped while the app is open arrives here
window.launchQueue?.setConsumer((p) => { if (p.targetURL) demoFromUrl(p.targetURL); });
