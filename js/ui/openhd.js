// FPV tab, OpenHD part (docs/13): passive receiver for OpenHD drones. The
// worker decrypts/authenticates, FEC-decodes and depacketizes (js/openhd.js,
// js/rtp.js, js/mavlink.js); this module decodes video with WebCodecs, shows
// telemetry and drives the dongle (channel, capture filter). Receive-only.
import { app } from '../app.js';
import * as settings from '../settings.js';
import { encode as encodeFilter } from '../filter.js';
import { crc32 } from '../codec.js';
import { captureFilter, isOhdFilter, DEFAULT_KEYS, RACE_BAND_MHZ, DEFAULT_FREQ, freqToChannel, ht40Plus } from '../openhd.js';
import { keysFromKeyFile } from '../ohdkeys.js';
import { BW, channelFreq } from '../wire.js';
import { COUNTRIES } from '../regdom.js';
import { flipControl, flippedCanvas } from './flip.js';
import { h, inline, toast, download, fmtNum, fmtBytes, messageEl } from './dom.js';

const deg = (r) => (r * 180) / Math.PI;
const pct = (a, b) => (b ? `${((100 * a) / b).toFixed(1)} %` : '–');
const GPS_FIX = ['no GPS', 'no fix', '2D', '3D', 'DGPS', 'RTK float', 'RTK fixed', 'static', 'PPP'];
const SEVERITY = ['EMERG', 'ALERT', 'CRIT', 'ERROR', 'WARN', 'NOTICE', 'INFO', 'DEBUG'];
const CAM_ENC = ['H.264', 'H.265', 'MJPEG'];
const PRESETS = [...RACE_BAND_MHZ, 2452];
// Channels the ESP32-C5 driver tunes to in some country (docs/02 §1): 2.4 GHz 1–14, 5 GHz union of the tables.
const TUNABLE = [...Array.from({ length: 14 }, (_, i) => i + 1), ...new Set(Object.values(COUNTRIES).flatMap((c) => c.ch5))].sort((a, b) => a - b);
/** MHz → { ch, exact } with ch = the nearest tunable channel (0 = outside 2.4/5 GHz). */
function channelFor(mhz) {
  if (!(mhz >= 2400 && mhz <= 2500) && !(mhz >= 5150 && mhz <= 5925)) return { ch: 0, exact: false };
  const band = TUNABLE.filter((c) => (mhz < 3000 ? c <= 14 : c > 14));
  const ch = band.reduce((a, c) => (Math.abs(channelFreq(c) - mhz) < Math.abs(channelFreq(a) - mhz) ? c : a));
  return { ch, exact: channelFreq(ch) === mhz };
}

export function initOpenhd(root, { isVisible, onLargeChange }) {
  const s = settings.get();
  // ---------------- setup ----------------
  const freqSel = h('select', { 'aria-label': 'OpenHD frequency' },
    PRESETS.map((f) => h('option', { value: f }, `${f} MHz (ch ${freqToChannel(f)})${f === DEFAULT_FREQ ? ' – OpenHD default' : f === 2452 ? ' – 2.4 GHz default' : ''}`)),
    h('option', { value: 0 }, 'Custom frequency…'));
  const freqIn = h('input', { type: 'number', min: 2412, max: 5885, step: 5, style: { width: '6em' }, 'aria-label': 'Frequency in MHz', oninput: drawFreqNote });
  const freqUnit = h('span.small', 'MHz'), freqNote = h('span.small');
  const bwSel = h('select', { 'aria-label': 'Channel width' }, h('option', { value: 20 }, '20 MHz'), h('option', { value: 40 }, '40 MHz'));
  const followCb = h('input', { type: 'checkbox', checked: s.ohdFollow !== false, onchange: () => settings.update((x) => { x.ohdFollow = followCb.checked; }) });
  const groundCb = h('input', { type: 'checkbox', checked: !!s.ohdGround, onchange: () => settings.update((x) => { x.ohdGround = groundCb.checked; }) });
  const phraseIn = h('input', { type: 'password', placeholder: 'bind phrase', autocomplete: 'off', style: { width: '12em' }, 'aria-label': 'Bind phrase' });
  const keyFileIn = h('input', { type: 'file', class: 'hidden', onchange: () => loadKeyFile(keyFileIn.files[0]) });
  const keyState = h('span.small'), keyMsg = h('div');
  const unverCb = h('input', { type: 'checkbox', checked: !!s.ohdUnverified, onchange: () => { settings.update((x) => { x.ohdUnverified = unverCb.checked; }); sendOptions(); } });
  const queueSel = h('select', { 'aria-label': 'FEC queue', onchange: () => { settings.update((x) => { x.ohdQueue = Number(queueSel.value); }); sendOptions(); } },
    h('option', { value: 1 }, '1 block (lowest latency, like OpenHD)'), h('option', { value: 2 }, '2 blocks'), h('option', { value: 4 }, '4 blocks (smoother on a jittery USB link)'));
  queueSel.value = String(s.ohdQueue ?? 1);
  const gappedCb = h('input', { type: 'checkbox', checked: !!s.ohdGapped, onchange: () => { settings.update((x) => { x.ohdGapped = gappedCb.checked; }); sendOptions(); } });
  const codecSel = h('select', { 'aria-label': 'Codec', onchange: () => { settings.update((x) => { x.ohdCodec = codecSel.value; }); sendView(); } },
    h('option', { value: 'auto' }, 'auto'), h('option', { value: 'h264' }, 'H.264'), h('option', { value: 'h265' }, 'H.265'));
  codecSel.value = s.ohdCodec ?? 'auto';
  const filterState = h('div.small'), setupMsg = h('div');

  function showFreq(mhz, custom = false) {
    const preset = !custom && PRESETS.includes(mhz);
    freqSel.value = String(preset ? mhz : 0);
    freqIn.value = mhz;
    for (const el of [freqIn, freqUnit]) el.classList.toggle('hidden', preset);
    drawFreqNote();
  }
  bwSel.value = String(s.ohdBw ?? 20);
  freqSel.addEventListener('change', () => { const f = Number(freqSel.value); if (f) showFreq(f); else { showFreq(Number(freqIn.value) || DEFAULT_FREQ, true); freqIn.focus(); } });
  function wantedMHz() { return Number(freqSel.value) || Number(freqIn.value); }
  function region() {
    const c = app.config?.country ?? settings.get().desired.country;
    const base = COUNTRIES[c?.cc] ?? COUNTRIES['01'];
    return c?.mode === 1 ? { cc: c.cc, schan: c.schan, nchan: c.nchan, ch5: base.ch5 } : { cc: c?.cc ?? '01', ...base };
  }
  function allowedHere(r, ch) { return ch <= 14 ? ch >= r.schan && ch < r.schan + r.nchan : r.ch5.includes(ch); }
  function drawFreqNote() {
    const mhz = wantedMHz(), { ch, exact } = channelFor(mhz), r = region();
    if (!ch) { freqNote.className = 'small warn-text'; freqNote.textContent = 'Enter 2412–2484 or 5180–5885 MHz'; return; }
    if (!exact) { freqNote.className = 'small warn-text'; freqNote.textContent = `not a Wi-Fi channel centre – nearest: ${channelFreq(ch)} MHz (ch ${ch})`; return; }
    const ok = allowedHere(r, ch);
    freqNote.className = ok ? 'small muted' : 'small warn-text';
    freqNote.textContent = `= ch ${ch}${ok ? '' : ` – not enabled for country ${r.cc}; pick a country that allows it in the Radio tab`}`;
  }

  showFreq(channelFreq(s.ohdChannel ?? freqToChannel(DEFAULT_FREQ)) || DEFAULT_FREQ);

  // ---------------- video ----------------
  const canvas = h('canvas', { width: 640, height: 360, 'aria-label': 'OpenHD video' });
  const osdTop = h('div.fpv-osd', 'No video'), osdBottom = h('div.fpv-osd.fpv-osd-bottom');
  const video = h('div.fpv-video.wide', canvas, osdTop, osdBottom);
  const slot = h('div', video);
  const stage = h('section.fpv-stage', { 'aria-label': 'OpenHD video (large)' });
  document.querySelector('#app').append(stage);
  const largeBtn = h('button.small.fpv-large-btn', { onclick: () => setLarge(!large) }, 'Large view');
  const pauseBtn = h('button.small', { onclick: togglePause }, 'Pause');
  const camSel = h('select.small', { 'aria-label': 'Camera', onchange: () => { cam = Number(camSel.value); resetDecoder(); sendView(); } }, h('option', { value: 0 }, 'primary camera'), h('option', { value: 1 }, 'secondary camera'));
  const flip = flipControl(canvas, 'ohdFlip');
  const recVideoBtn = h('button.small', { onclick: () => toggleRec('video') }, 'Record video');
  const recFmtSel = h('select.small', { 'aria-label': 'Video recording format', title: 'MP4 has timestamps and plays in any player. Raw is the bare H.264/H.265 stream (players must be told the frame rate).', onchange: () => settings.update((x) => { x.ohdRecFormat = recFmtSel.value; }) },
    h('option', { value: 'mp4' }, 'MP4'), h('option', { value: 'raw' }, 'raw .h264/.h265'));
  recFmtSel.value = s.ohdRecFormat === 'raw' ? 'raw' : 'mp4';
  const recTlogBtn = h('button.small', { onclick: () => toggleRec('tlog') }, 'Record telemetry (.tlog)');
  const recState = h('span.small.muted');
  const unitsBox = h('div'), linkT = h('table.stats-table'), flightT = h('table.stats-table'), ohdT = h('table.stats-table'), textsBox = h('div.small.mono'), msgBox = h('div.small'), hints = h('div');

  root.append(
    h('p.small.muted', 'Video and telemetry from OpenHD drones (2.5, 2.6 and 2.7). The dongle only listens: keep using your OpenHD ground station. For monitoring your own OpenHD system: telemetry is encrypted with your bind phrase.'),
    h('h3', 'Setup'),
    h('div.row', 'Frequency ', freqSel, freqIn, freqUnit, bwSel, h('button', { onclick: tune }, 'Tune dongle')),
    h('div.row', freqNote),
    h('div.row.small', h('label', followCb, ' Follow the air unit when it changes channel (needs the bind phrase)')),
    h('div.row', h('button', { onclick: applyFilter }, 'Use OpenHD capture filter'), h('button', { onclick: restoreFilter }, 'Restore previous filter')),
    h('div.row.small', h('label', groundCb, ' Also capture the ground station (session, uplink telemetry)')),
    filterState, setupMsg,
    h('div.row', 'Bind phrase ', phraseIn, h('button', { onclick: usePhrase }, 'Use'), h('button', { onclick: () => keyFileIn.click() }, 'Load txrx.key…'), keyFileIn,
      h('button.small', { onclick: () => setKeys(null, 'default') }, 'Default')),
    h('div.small', keyState), keyMsg,
    h('div.row.small', h('label', unverCb, ' Show video even when the bind phrase is unknown (unverified, video only)')),
    h('details', h('summary.small', 'Advanced'),
      h('div.row.small', 'FEC queue ', queueSel), h('div.row.small', h('label', gappedCb, ' Forward packets after a gap of a lost block (more picture, more artifacts)')),
      h('div.row.small', 'Codec ', codecSel)),
    h('h3', 'Video'),
    slot,
    h('div.row', largeBtn, h('button.small', { onclick: fullscreen }, 'Fullscreen'), h('button.small', { onclick: snapshot }, 'Snapshot'), pauseBtn, camSel, flip.el,
      h('button.small', { onclick: () => { app.post({ t: 'ohd-reset' }); resetDecoder(); toast('OpenHD receiver reset'); } }, 'Reset')),
    h('div.row', recVideoBtn, recFmtSel, recTlogBtn, recState),
    hints,
    h('h3', 'Air units'), unitsBox,
    h('h3', 'Link (shown unit)'), linkT,
    h('h3', 'Flight (MAVLink)'), flightT,
    h('h3', 'OpenHD air unit'), ohdT,
    h('h3', 'Messages'), textsBox, msgBox,
  );

  // ---------------- keys ----------------
  function keyLabel() {
    const k = settings.get().ohdKeys;
    return !k || k.label === 'default' ? 'Keys: default bind phrase "openhd"' : `Keys: ${k.label === 'file' ? 'from txrx.key' : 'your bind phrase'} (saved in this browser; "Default" forgets them)`;
  }
  function currentKeys() { const k = settings.get().ohdKeys; return k && k.label !== 'default' ? { air: k.air, gnd: k.gnd } : { ...DEFAULT_KEYS }; }
  function setKeys(keys, label) {
    settings.update((x) => { x.ohdKeys = keys ? { ...keys, label } : null; });
    app.post({ t: 'ohd-keys', keys: currentKeys() });
    keyState.textContent = keyLabel();
    inline(keyMsg, null);
    if (label === 'default') toast('Default OpenHD keys');
  }
  let keyWorker = null;
  function usePhrase() {
    const p = phraseIn.value;
    if (!p) { inline(keyMsg, { level: 'info', title: 'Enter the bind phrase', text: 'The phrase set in OpenHD (QOpenHD → bind phrase). Empty = default "openhd".', actions: [] }); return; }
    if (p === 'openhd') { setKeys(null, 'default'); return; }
    keyWorker?.terminate();
    keyWorker = new Worker(new URL('../ohdkeys-worker.js', import.meta.url), { type: 'module' });
    keyState.textContent = 'Deriving keys (Argon2id, takes a few seconds)… 0 %';
    keyWorker.onmessage = (e) => {
      const m = e.data;
      if (m.t === 'progress') keyState.textContent = `Deriving keys (Argon2id, takes a few seconds)… ${Math.round(m.p * 100)} %`;
      else if (m.t === 'done') { setKeys(m.keys, 'phrase'); phraseIn.value = ''; toast('Bind phrase keys in use'); keyWorker.terminate(); keyWorker = null; }
      else { inline(keyMsg, { level: 'error', title: 'Could not derive the keys', text: m.message, actions: ['Try again; if the browser is low on memory (needs 64 MiB), load txrx.key instead.'] }); keyState.textContent = keyLabel(); keyWorker.terminate(); keyWorker = null; }
    };
    keyWorker.onerror = (e) => { inline(keyMsg, e, 'Key derivation'); keyState.textContent = keyLabel(); };
    keyWorker.postMessage({ phrase: p });
  }
  async function loadKeyFile(file) {
    keyFileIn.value = '';
    if (!file) return;
    try { setKeys(keysFromKeyFile(new Uint8Array(await file.arrayBuffer())), 'file'); toast('Keys loaded from txrx.key'); } catch (e) {
      inline(keyMsg, { level: 'error', title: 'Not an OpenHD key file', text: e.message, actions: ['Copy /usr/local/share/openhd/txrx.key from the air or ground unit (128 bytes).'] });
    }
  }
  keyState.textContent = keyLabel();
  app.post({ t: 'ohd-keys', keys: currentKeys() });
  function sendOptions() { app.post({ t: 'ohd-options', opts: { queue: Number(queueSel.value), forwardGapped: gappedCb.checked, unverified: unverCb.checked } }); }
  sendOptions();

  // ---------------- dongle setup ----------------
  function wantedChannel() { return channelFor(wantedMHz()).ch; }
  async function tune(chArg, bwArg, quiet) {
    let ch = chArg;
    if (typeof chArg !== 'number') {
      const mhz = wantedMHz(), c = channelFor(mhz);
      if (!c.exact) {
        inline(setupMsg, c.ch
          ? { level: 'warn', title: `${mhz} MHz is not a Wi-Fi channel centre`, text: `The ESP32-C5 radio tunes only to Wi-Fi channel centres (2.4 GHz: 5 MHz steps, 2412–2472 and 2484; 5 GHz: 20 MHz steps, 5180–5885). OpenHD also uses only these. Nearest: ${channelFreq(c.ch)} MHz (ch ${c.ch}).`, actions: [`Enter ${channelFreq(c.ch)} MHz.`] }
          : { level: 'warn', title: 'Frequency out of range', text: 'The dongle receives 2412–2484 MHz and 5180–5885 MHz.', actions: [] });
        return false;
      }
      ch = c.ch;
    }
    const mhz = typeof bwArg === 'number' ? bwArg : Number(bwSel.value);
    const bw = mhz === 40 ? (ht40Plus(ch) ? BW.HT40_PLUS : BW.HT40_MINUS) : BW.HT20;
    settings.update((x) => { x.ohdChannel = ch; x.ohdBw = mhz; x.desired.channel = ch; x.desired.bw = bw; });
    const err = await app.act(`Channel ${ch}`, (c) => c.setChannel(ch, bw));
    if (!inline(setupMsg, err) && !quiet) toast(`Dongle on channel ${ch}${mhz === 40 ? ` HT40${bw === BW.HT40_PLUS ? '+' : '−'}` : ''}`);
    return !err;
  }
  function wantedFilter() { return captureFilter({ ground: groundCb.checked }); }
  async function applyFilter() {
    const set = wantedFilter(), cur = settings.get().desired.filter;
    if (!isOhdFilter(cur)) settings.update((x) => { x.ohdPrevFilter = structuredClone(cur); });
    const err = await app.act('OpenHD capture filter', (c) => c.setFilter(set));
    if (inline(setupMsg, err)) return;
    settings.update((x) => { x.desired.filter = structuredClone(set); });
    let note = 'OpenHD capture filter active';
    if (app.config?.snaplen) {
      const e2 = await app.act('Snaplen', (c) => c.setSnaplen(0));
      if (!e2) { settings.update((x) => { x.desired.snaplen = 0; }); note += ', snaplen set to full frames'; } else inline(setupMsg, e2);
    }
    toast(note);
    drawFilterState();
  }
  async function restoreFilter() {
    const prev = settings.get().ohdPrevFilter;
    if (!prev) { inline(setupMsg, { level: 'info', title: 'No previous filter saved', text: 'The previous filter is saved when you switch to the OpenHD capture filter.', actions: [] }); return; }
    const err = await app.act('Restore filter', (c) => c.setFilter(prev));
    if (inline(setupMsg, err)) return;
    settings.update((x) => { x.desired.filter = structuredClone(prev); });
    toast('Previous filter restored');
    drawFilterState();
  }
  function drawFilterState() {
    const cfg = app.config;
    if (!cfg) { filterState.textContent = 'Connect the dongle (or try the demo: the demo OpenHD drone is on channel 149; set a country that allows it, e.g. FI, in the Radio tab).'; return; }
    const on = [false, true].some((g) => { try { return (crc32(encodeFilter(captureFilter({ ground: g }))) >>> 0) === (cfg.rulesCrc32 >>> 0); } catch { return false; } });
    filterState.replaceChildren(
      h('span', `Dongle: channel ${cfg.channel}${cfg.bw ? ' (40 MHz)' : ''}${cfg.channel !== wantedChannel() ? ` (OpenHD channel is ${wantedChannel()})` : ''} · `),
      h(on ? 'span.ok-text' : 'span.muted', on ? 'OpenHD filter active ✓' : 'other filter (OpenHD frames are still decoded, but the USB link may overflow)'),
      ...(cfg.snaplen ? [h('span.warn-text', ` · snaplen ${cfg.snaplen}: frames are cut, video cannot be decoded`)] : []),
      h('span', ` · capture ${cfg.captureOn ? 'on' : 'off'}`));
  }
  app.addEventListener('config', () => { drawFilterState(); drawFreqNote(); });
  app.addEventListener('conn', drawFilterState);
  drawFilterState();

  // follow the air unit's announced channel (management frames, §10.4)
  let lastFollow = 0;
  function follow(u) {
    if (!followCb.checked || !u?.mgmt || !app.config) return;
    const ch = freqToChannel(u.mgmt.freq), mhz = u.mgmt.bw === 40 ? 40 : 20;
    if (!ch) return;
    const bwWanted = mhz === 40 ? (ht40Plus(ch) ? BW.HT40_PLUS : BW.HT40_MINUS) : BW.HT20;
    if (app.config.channel === ch && (app.config.bw ?? 0) === bwWanted) return;
    if (performance.now() - lastFollow < 5000) return;
    lastFollow = performance.now();
    showFreq(channelFreq(ch)); bwSel.value = String(mhz);
    tune(ch, mhz, true).then((ok) => ok && toast(`Following the air unit to ${u.mgmt.freq} MHz (ch ${ch}, ${mhz} MHz)`, 4000));
  }

  // ---------------- view control ----------------
  let shown = 'auto', cam = 0, paused = false, visible = false, large = false;
  function sendView() { app.post({ t: 'ohd-view', on: visible && !paused, unit: shown, cam, codec: codecSel.value }); }
  function checkVisible() {
    const v = large || isVisible() || document.fullscreenElement === video;
    if (v !== visible) { visible = v; sendView(); if (!v) resetDecoder(); }
  }
  document.addEventListener('fullscreenchange', checkVisible);
  function setLarge(on) {
    if (on === large) return;
    large = on;
    (large ? stage : slot).append(video);
    stage.classList.toggle('on', large);
    document.querySelector('#app').classList.toggle('fpv-large', large);
    largeBtn.textContent = large ? 'Back to packet list' : 'Large view';
    onLargeChange?.(large);
    checkVisible();
  }
  function togglePause() { paused = !paused; pauseBtn.textContent = paused ? 'Resume' : 'Pause'; sendView(); }
  function fullscreen() {
    const req = video.requestFullscreen ?? video.webkitRequestFullscreen;
    if (!req) { toast('Fullscreen is not available in this browser'); return; }
    req.call(video).catch((e) => inline(setupMsg, e, 'Fullscreen'));
  }
  function snapshot() {
    if (!frames) { toast('No frame yet'); return; }
    flippedCanvas(canvas, flip.value()).toBlob((b) => b && download(`openhd_${new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19)}.png`, b), 'image/png');
  }

  // ---------------- WebCodecs decoding ----------------
  const ctx = canvas.getContext('2d');
  let dec = null, decCodec = null, frames = 0, decodeErrors = 0, lastErr = '', unsupported = null, ts = 0, lastAu = null;
  const ftimes = [];
  function resetDecoder() { try { dec?.close(); } catch { /* closed */ } dec = null; decCodec = null; }
  function needKey(err) { if (err) { decodeErrors++; lastErr = String(err.message ?? err); } resetDecoder(); app.post({ t: 'ohd-need-key' }); }
  app.addEventListener('ohd-au', (e) => {
    const m = e.detail;
    app.post({ t: 'ohd-ack' });
    lastAu = m;
    if (paused || !visible) return;
    if (typeof VideoDecoder !== 'function') { unsupported = 'This browser has no WebCodecs VideoDecoder (use a current Chrome, Edge or Firefox).'; return; }
    if (!m.codecString) return;
    if (m.key && (!dec || decCodec !== m.codecString)) {
      resetDecoder();
      try {
        dec = new VideoDecoder({ output: drawFrame, error: (err) => { needKey(err); } });
        dec.configure({ codec: m.codecString, optimizeForLatency: true });
        decCodec = m.codecString;
        VideoDecoder.isConfigSupported({ codec: m.codecString }).then((r) => {
          unsupported = r.supported ? null : `This browser cannot decode ${m.codec === 'h265' ? 'H.265' : 'this H.264 profile'} (${m.codecString}). ${m.codec === 'h265' ? 'Set the OpenHD camera to H.264, or use a browser/OS with an H.265 decoder.' : ''}`;
        }).catch(() => {});
      } catch (err) { needKey(err); return; }
    }
    if (!dec || dec.state !== 'configured') return;
    if (dec.decodeQueueSize > 8) { needKey(); return; } // display behind: skip to the next key frame
    try { dec.decode(new EncodedVideoChunk({ type: m.key ? 'key' : 'delta', timestamp: (ts += 33333), data: m.data })); } catch (err) { needKey(err); }
  });
  function drawFrame(vf) {
    try {
      if (canvas.width !== vf.displayWidth || canvas.height !== vf.displayHeight) { canvas.width = vf.displayWidth; canvas.height = vf.displayHeight; }
      ctx.drawImage(vf, 0, 0);
    } finally { vf.close(); }
    frames++;
    const now = performance.now();
    ftimes.push(now);
    while (ftimes.length && now - ftimes[0] > 2000) ftimes.shift();
  }

  // ---------------- recording ----------------
  let recOn = { video: false, tlog: false };
  function toggleRec(kind) {
    const next = { ...recOn, [kind]: !recOn[kind] };
    app.post({ t: 'ohd-rec-stop' }); // files of the previous selection are delivered first
    recOn = next;
    if (next.video || next.tlog) app.post({ t: 'ohd-rec-start', video: next.video, tlog: next.tlog, format: recFmtSel.value });
    recFmtSel.disabled = !!next.video;
    recVideoBtn.textContent = recOn.video ? 'Stop video recording' : 'Record video';
    recTlogBtn.textContent = recOn.tlog ? 'Stop telemetry recording' : 'Record telemetry (.tlog)';
  }
  app.addEventListener('ohd-rec-data', (e) => {
    const d = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    const videos = e.detail.files.filter((f) => f.kind === 'video');
    e.detail.files.forEach((f) => {
      if (f.kind === 'note') toast(f.text, 6000);
      else if (f.kind === 'video') download(`openhd_${d}${videos.length > 1 ? `_${videos.indexOf(f) + 1}` : ''}.${f.ext}`, new Blob(f.parts, { type: f.ext === 'mp4' ? 'video/mp4' : 'application/octet-stream' }));
      else download(`openhd_${d}.tlog`, new Blob(f.parts));
    });
  });

  // ---------------- stats / telemetry ----------------
  let prevT = 0, prev = new Map(), prevDrop = null;
  app.addEventListener('host', (e) => {
    checkVisible();
    const o = e.detail.ohd;
    const now = performance.now(), dt = prevT ? (now - prevT) / 1000 : 0;
    prevT = now;
    if (!o) {
      unitsBox.replaceChildren(h('p.small.muted', app.config?.captureOn ? `No OpenHD frames yet. Tune to the OpenHD frequency (default 5745 MHz = ch 149; dongle on ${app.config.channel}).` : 'Start capture to look for OpenHD drones.'));
      drawUnit(null); hintsFor(null, e.detail); osd(null);
      return;
    }
    const air = o.units.filter((u) => u.dir === 1), gnd = o.units.filter((u) => u.dir === 2);
    const rate = (u) => { const p = prev.get(u.id); return dt && p ? { pps: (u.st.packets - p.packets) / dt, kbps: (8 * (u.st.videoBytes[0] + u.st.videoBytes[1] - p.vb)) / dt / 1000 } : null; };
    const rates = new Map(o.units.map((u) => [u.id, rate(u)]));
    prev = new Map(o.units.map((u) => [u.id, { packets: u.st.packets, vb: u.st.videoBytes[0] + u.st.videoBytes[1] }]));
    const radio = (val, label) => h('label.small', h('input', { type: 'radio', name: 'ohd-unit', checked: shown === val, onchange: () => { shown = val; resetDecoder(); sendView(); } }), ` ${label}`);
    const many = air.length > 1;
    unitsBox.replaceChildren(...[
      air.length ? h('table.stats-table',
        h('tr', [...(many ? ['show'] : []), 'unit', 'OpenHD', 'pkt/s', 'video', 'loss', 'RSSI', 'seen'].map((t) => h('td.muted', t))),
        air.map((u) => {
          const r = rates.get(u.id);
          return h('tr', many ? h('td', radio(u.id, '')) : null, h('td', `#${u.id}${u.verified ? '' : ' ?'}`), h('td', u.gen === 'B' ? '2.7' : u.gen === 'A' ? '2.5/2.6' : '?'),
            h('td', r ? fmtNum(Math.round(r.pps)) : ''), h('td', r ? `${(r.kbps / 1000).toFixed(1)} Mb/s` : ''), h('td', u.loss === null ? '' : `${(100 * u.loss).toFixed(1)} %`),
            h('td', u.rssi === null ? '' : `${u.rssi}`), h('td', u.ageMs < 1500 ? 'now' : `${(u.ageMs / 1000).toFixed(0)} s`));
        })) : h('p.small.muted', 'No air unit yet.'),
      many ? h('div.row', radio('auto', `Auto (most video${o.shown !== null ? `: #${o.shown}` : ''})`)) : null,
      gnd.length ? h('p.small', `Ground station seen (${gnd.map((g) => `session ${g.sessionId}, ${g.rssi ?? '?'} dBm, ${g.ageMs < 1500 ? 'now' : `${(g.ageMs / 1000).toFixed(0)} s ago`}`).join('; ')})`) : null,
    ].filter(Boolean));
    const u = air.find((x) => x.id === (shown === 'auto' ? o.shown : shown)) ?? null;
    drawUnit(u, o);
    hintsFor(o, e.detail, u);
    osd(u, o);
    follow(u);
    if (o.rec) recState.textContent = `recording ${((o.rec.ms ?? 0) / 1000).toFixed(0)} s${o.rec.video ? ` · video ${fmtBytes(o.rec.video.size)}` : ''}${o.rec.tlog ? ` · ${fmtNum(o.rec.tlog.msgs)} MAVLink messages` : ''}`;
    else recState.textContent = '';
  });

  function osd(u, o) {
    if (!u) { osdTop.textContent = 'No video'; osdBottom.textContent = ''; return; }
    const fps = (ftimes.length / 2).toFixed(0), rtp = u.rtp?.[cam];
    const codec = rtp?.codec ? (rtp.codec === 'h265' ? 'H.265' : 'H.264') : '';
    osdTop.textContent = `#${u.id} · ${codec} ${canvas.width}×${canvas.height} · ${fps} fps · loss ${u.loss === null ? '–' : `${(100 * u.loss).toFixed(1)} %`} · ${u.rssi ?? '–'} dBm${u.verified ? '' : ' · UNVERIFIED'}`;
    const f = u.flight?.flight;
    if (!f) { osdBottom.textContent = o?.keys?.air && !u.flight ? 'no telemetry' : ''; return; }
    const p = [];
    if (f.mode || f.armed !== null) p.push(`${f.armed ? 'ARMED' : 'disarmed'} ${f.mode ?? ''}`);
    if (f.pos) p.push(`alt ${f.pos.relAlt.toFixed(1)} m`);
    else if (f.hud) p.push(`alt ${f.hud.alt.toFixed(1)} m`);
    if (f.hud) p.push(`${f.hud.groundspeed.toFixed(1)} m/s`, `hdg ${f.hud.heading}°`);
    if (f.battery) p.push(`${f.battery.voltage.toFixed(2)} V${f.battery.remaining >= 0 ? ` ${f.battery.remaining} %` : ''}`);
    if (f.gps) p.push(`${GPS_FIX[f.gps.fixType] ?? f.gps.fixType} ${f.gps.satellites} sats`);
    osdBottom.textContent = p.join(' · ');
  }

  function drawUnit(u, o) {
    const kv = (t, list) => t.replaceChildren(...list.filter(Boolean).map(([k, v]) => h('tr', h('td', k), h('td', v))));
    if (!u) { linkT.replaceChildren(); flightT.replaceChildren(); ohdT.replaceChildren(); textsBox.replaceChildren(); msgBox.replaceChildren(); return; }
    const st = u.st, f0 = u.fec[cam], rtp = u.rtp?.[cam];
    const blocks = f0 ? f0.blocksOk + f0.blocksFec + f0.blocksLost : 0;
    kv(linkT, [
      ['Unit', `#${u.id} · session ${u.sessionId ?? '–'} · OpenHD ${u.gen === 'B' ? '2.7 (WBPacketHeader)' : u.gen === 'A' ? '2.5/2.6' : 'unknown yet'}${u.verified ? ' · authenticated' : ' · UNVERIFIED'}`],
      ['Frames from the air unit', `${fmtNum(st.packets)} · missed by the dongle ${u.loss === null ? '–' : `${(100 * u.loss).toFixed(1)} %`} now (${fmtNum(st.nonceGaps)} total)`],
      f0 ? ['FEC blocks', `${fmtNum(f0.blocksOk)} complete · ${fmtNum(f0.blocksFec)} repaired (${fmtNum(f0.recovered)} packets) · ${fmtNum(f0.blocksLost)} lost (${pct(f0.blocksLost, blocks)})`] : ['FEC blocks', 'no video on this camera'],
      f0 ? ['FEC packets', `primary ${fmtNum(f0.primaries)} · secondary ${fmtNum(f0.secondaries)} (${fmtNum(f0.secUnneeded)} not needed) · duplicates ${fmtNum(f0.duplicates)} · late ${fmtNum(f0.late)}${f0.bad ? ` · bad ${f0.bad}` : ''}`] : null,
      rtp ? ['Video', `${rtp.codec ?? 'codec ?'} · ${fmtNum(rtp.aus)} frames (${fmtNum(rtp.keys)} key) · damaged ${fmtNum(rtp.damaged)} · RTP gaps ${fmtNum(rtp.gaps)}${rtp.notRtp ? ` · not RTP ${fmtNum(rtp.notRtp)}` : ''}`] : null,
      ['Display', `${fmtNum(frames)} frames shown · decoder errors ${decodeErrors}${o?.view?.skipped ? ` · ${fmtNum(o.view.skipped)} skipped waiting for a key frame` : ''}${o?.view?.forced ? ` · ${o.view.forced} starts without key frame` : ''}`],
      ['Telemetry', `${fmtNum(st.telemetry)} packets (${fmtNum(st.teleDup)} duplicates dropped)${st.audio ? ` · audio ${fmtNum(st.audio)}` : ''}${st.retransmitted ? ` · retransmitted ${fmtNum(st.retransmitted)}` : ''}`],
      u.mgmt ? ['Air unit announces', `${u.mgmt.freq} MHz (ch ${freqToChannel(u.mgmt.freq) || '?'}) · ${u.mgmt.bw} MHz`] : null,
      ['RSSI at dongle', u.rssi === null ? '–' : `${u.rssi} dBm (ch ${u.channel})`],
      st.restarts ? ['Air unit restarts', fmtNum(st.restarts)] : null,
    ]);
    const fs = u.flight, f = fs?.flight;
    kv(flightT, f ? [
      ['Vehicle', `${f.autopilot ?? '?'} ${f.vehicle ?? ''} (system ${f.sys})`],
      ['State', `${f.armed ? 'ARMED' : 'disarmed'}${f.mode ? ` · ${f.mode}` : ''}`],
      f.gps ? ['GPS', `${GPS_FIX[f.gps.fixType] ?? f.gps.fixType} · ${f.gps.satellites} satellites · HDOP ${(f.gps.eph / 100).toFixed(1)}`] : null,
      f.pos ? ['Position', `${f.pos.lat.toFixed(6)}, ${f.pos.lon.toFixed(6)} · ${f.pos.relAlt.toFixed(1)} m above home (${f.pos.alt.toFixed(1)} m MSL)`] : f.gps ? ['Position', `${f.gps.lat.toFixed(6)}, ${f.gps.lon.toFixed(6)} · ${f.gps.alt.toFixed(1)} m MSL`] : null,
      f.hud ? ['Speed', `ground ${f.hud.groundspeed.toFixed(1)} m/s · air ${f.hud.airspeed.toFixed(1)} m/s · climb ${f.hud.climb.toFixed(1)} m/s · heading ${f.hud.heading}° · throttle ${f.hud.throttle} %`] : null,
      f.att ? ['Attitude', `roll ${deg(f.att.roll).toFixed(1)}° · pitch ${deg(f.att.pitch).toFixed(1)}° · yaw ${deg(f.att.yaw).toFixed(1)}°`] : null,
      f.battery ? ['Battery', `${f.battery.voltage.toFixed(2)} V · ${f.battery.current.toFixed(1)} A${f.battery.remaining >= 0 ? ` · ${f.battery.remaining} %` : ''}`] : null,
      f.rcRssi !== null ? ['RC RSSI', `${f.rcRssi}`] : null,
    ] : [['', u.verified ? 'No flight controller telemetry yet.' : 'Telemetry needs the bind phrase.']]);
    const d = fs?.openhd;
    kv(ohdT, d ? [
      d.OPENHD_VERSION_MESSAGE ? ['Version', `${d.OPENHD_VERSION_MESSAGE.major}.${d.OPENHD_VERSION_MESSAGE.minor}.${d.OPENHD_VERSION_MESSAGE.patch}${['', ' beta', ' alpha', ' dev'][d.OPENHD_VERSION_MESSAGE.releaseType] ?? ''}`] : null,
      d.OPENHD_CAMERA_STATUS_AIR ? ['Camera', `${d.OPENHD_CAMERA_STATUS_AIR.width}×${d.OPENHD_CAMERA_STATUS_AIR.height} @ ${d.OPENHD_CAMERA_STATUS_AIR.fps} fps · ${CAM_ENC[d.OPENHD_CAMERA_STATUS_AIR.encoding] ?? '?'} · ${(d.OPENHD_CAMERA_STATUS_AIR.bitrateKbit / 1000).toFixed(1)} Mbit/s · key frame every ${d.OPENHD_CAMERA_STATUS_AIR.keyframeInterval}${d.OPENHD_CAMERA_STATUS_AIR.recording ? ' · recording' : ''}`] : null,
      d.OPENHD_CORE_STATUS ? ['Computer', `CPU ${d.OPENHD_CORE_STATUS.cpuTemp} °C · ${d.OPENHD_CORE_STATUS.cpuClock} MHz · Wi-Fi card ${d.OPENHD_CORE_STATUS.wifi0Temp} °C · ${fmtNum(d.OPENHD_CORE_STATUS.storageLeftMb)} MB free${d.OPENHD_CORE_STATUS.undervolt ? ' · UNDERVOLTAGE' : ''}`] : null,
      d.OPENHD_STATS_MONITOR_MODE_WIFI_LINK ? ['Air link', `${d.OPENHD_STATS_MONITOR_MODE_WIFI_LINK.freq} MHz · ${d.OPENHD_STATS_MONITOR_MODE_WIFI_LINK.width} MHz · MCS ${d.OPENHD_STATS_MONITOR_MODE_WIFI_LINK.mcs} · TX ${(d.OPENHD_STATS_MONITOR_MODE_WIFI_LINK.txBps / 1e6).toFixed(1)} Mbit/s · pollution ${d.OPENHD_STATS_MONITOR_MODE_WIFI_LINK.pollution} %`] : null,
      d.OPENHD_STATS_WB_VIDEO_AIR ? ['Video encoder', `${(d.OPENHD_STATS_WB_VIDEO_AIR.encoderBitrate / 1e6).toFixed(1)} Mbit/s · FEC ${d.OPENHD_STATS_WB_VIDEO_AIR.fecPerc} % · dropped ${d.OPENHD_STATS_WB_VIDEO_AIR.droppedFrames}`] : null,
    ].filter(Boolean) : [['', 'No OpenHD status messages yet (they are part of the telemetry).']]);
    textsBox.replaceChildren(...(fs?.texts ?? []).slice().reverse().map((t) => h('div', `${SEVERITY[t.severity] ?? t.severity} [${t.sys}] ${t.text}`)));
    if (fs) {
      const rows = fs.systems.map((sy) => `system ${sy.sys}: ${Object.entries(sy.counts).sort((a, b) => b[1] - a[1]).slice(0, 12).map(([id, n]) => `${id}×${n}`).join(' ')}`);
      msgBox.replaceChildren(h('details', h('summary', `${fmtNum(fs.total)} MAVLink messages${fs.badCrc ? ` · ${fs.badCrc} bad CRC` : ''}${fs.unknown ? ` · ${fs.unknown} not in the dialect` : ''}`), ...rows.map((r) => h('div.mono', r))));
    } else msgBox.replaceChildren();
  }

  function hintsFor(o, host, u) {
    const list = [];
    const g = o?.g;
    if (unsupported) list.push({ level: 'error', title: 'Video cannot be decoded here', text: unsupported, actions: [] });
    if (g && !o.units.some((x) => x.dir === 1) && g.badSession[1]) {
      list.push({ level: 'warn', title: 'Wrong bind phrase', text: `${fmtNum(g.badSession[1])} session packets from the air unit do not open with the current keys.`, actions: ['Enter your bind phrase or load txrx.key.', 'Or tick "Show video even when the bind phrase is unknown" (video only, unverified).'] });
    } else if (g && g.ohdFrames[1] && !o.units.some((x) => x.dir === 1)) {
      list.push({ level: 'info', title: 'OpenHD drone heard, waiting for its session packet', text: 'The air unit announces its session key about once per second.', actions: [] });
    }
    if (g?.authFail > 50 && u) list.push({ level: 'info', title: `${fmtNum(g.authFail)} frames failed authentication`, text: 'Some OpenHD frames did not verify (radio errors, or another OpenHD system with a different bind phrase).', actions: [] });
    const drop = app.devStats?.dropRing ?? null;
    if (drop !== null && prevDrop !== null && drop > prevDrop && u) list.push({ level: 'warn', title: 'Dongle drops packets: USB link full', text: `${fmtNum(drop - prevDrop)} frames dropped. The dongle's USB link carries about 1 MB/s; OpenHD's default video bitrate is higher.`, actions: ['Use the OpenHD capture filter.', 'Lower the OpenHD video bitrate to about 6 Mbit/s (QOpenHD → camera bitrate), or use a lower MCS.'] });
    prevDrop = drop;
    if (u && app.config?.snaplen) list.push({ level: 'error', title: 'Snaplen cuts the frames', text: `Snaplen is ${app.config.snaplen} bytes; OpenHD frames are up to ~1500 bytes. Use the OpenHD capture filter (it sets full frames).`, actions: [] });
    if (u && u.ageMs > 3000) list.push({ level: 'info', title: 'Air unit silent', text: `Nothing from #${u.id} for ${(u.ageMs / 1000).toFixed(0)} s.`, actions: [] });
    if (u && o?.view?.forced) list.push({ level: 'info', title: 'Stream without key frames', text: 'The air unit seems to use intra refresh (no IDR frames); the picture builds up over a second after starting.', actions: [] });
    if (host?.ohdErrors) list.push({ level: 'error', title: 'OpenHD decoder error', text: host.ohdLastError, actions: ['Reset; if it repeats, export the capture (pcapng) and report it.'] });
    if (decodeErrors && lastErr) list.push({ level: 'warn', title: `Video decoder errors (${decodeErrors})`, text: lastErr, actions: ['Usually after lost frames; the decoder restarts at the next key frame.'] });
    hints.replaceChildren(...list.map((d) => messageEl(d)));
  }

  return { setLarge, checkVisible, lastAu: () => lastAu };
}
