// FPV tab (docs/12): live video from hx-esp32-cam-fpv air units, decoded in
// the worker (js/hxfpv.js). Receive-only: the page never talks to the air unit.
import { app } from '../app.js';
import * as settings from '../settings.js';
import { encode as encodeFilter } from '../filter.js';
import { crc32 } from '../codec.js';
import { captureFilter, RESOLUTIONS, RATES } from '../hxfpv.js';
import { h, inline, toast, download, fmtNum, fmtBytes, messageEl } from './dom.js';
import { initOpenhd } from './openhd.js';

const hex4 = (n) => `0x${n.toString(16).toUpperCase().padStart(4, '0')}`;
const pct = (a, b) => (b ? `${((100 * a) / b).toFixed(1)} %` : '–');

/** FPV tab: system switch (hx-esp32-cam-fpv / OpenHD), one viewer each. */
export function initFpvTab(panel) {
  let current = settings.get().fpvSystem === 'openhd' ? 'openhd' : 'hx';
  const hxRoot = h('div'), ohdRoot = h('div');
  const seg = (v, label) => h('button.small.seg', { 'data-v': v, onclick: () => select(v) }, label);
  const bar = h('div.row.fpv-system', { role: 'group', 'aria-label': 'FPV system' }, h('span.small.muted', 'System'), seg('hx', 'hx-esp32-cam-fpv'), seg('openhd', 'OpenHD'));
  panel.append(bar, hxRoot, ohdRoot);
  const vis = (which) => () => !panel.classList.contains('hidden') && current === which;
  const views = { hx: initFpv(hxRoot, { isVisible: vis('hx') }), openhd: initOpenhd(ohdRoot, { isVisible: vis('openhd') }) };
  function select(v) {
    current = v;
    settings.update((x) => { x.fpvSystem = v; });
    hxRoot.classList.toggle('hidden', v !== 'hx');
    ohdRoot.classList.toggle('hidden', v !== 'openhd');
    for (const b of bar.querySelectorAll('button')) b.setAttribute('aria-pressed', String(b.dataset.v === v));
    for (const [k, view] of Object.entries(views)) { if (k !== v) view.setLarge(false); view.checkVisible(); }
  }
  new MutationObserver(() => { for (const view of Object.values(views)) view.checkVisible(); }).observe(panel, { attributes: true, attributeFilter: ['class'] });
  select(current);
}

export function initFpv(root, { isVisible }) {
  const s = settings.get();
  const chIn = h('input', { type: 'number', min: 1, max: 177, value: s.fpvChannel ?? 7, style: { width: '5em' }, 'aria-label': 'FPV channel' });
  const keepSel = h('select', { 'aria-label': 'FEC packets kept', onchange: () => settings.update((x) => { x.fpvKeep = Number(keepSel.value); }) },
    h('option', { value: 8 }, 'packets 0…7 of each block (k + 2 FEC at k = 6)'),
    h('option', { value: 16 }, 'packets 0…15 (all for n ≤ 16)'),
    h('option', { value: 0 }, 'all packets'));
  keepSel.value = String(s.fpvKeep ?? 8);
  const filterState = h('div.small'), setupMsg = h('div');
  const canvas = h('canvas', { width: 640, height: 480, 'aria-label': 'FPV video' });
  const osdTxt = h('div.fpv-osd', 'No video');
  const video = h('div.fpv-video', canvas, osdTxt);
  const slot = h('div', video);
  const stage = h('section.fpv-stage', { 'aria-label': 'FPV video (large)' });
  const largeBtn = h('button.small.fpv-large-btn', { onclick: () => setLarge(!large) }, 'Large view');
  const pauseBtn = h('button.small', { onclick: togglePause }, 'Pause');
  const unitsBox = h('div'), statsT = h('table.stats-table'), airT = h('table.stats-table'), hints = h('div');

  root.append(
    h('p.small.muted', 'Live video from hx-esp32-cam-fpv air units. The dongle only listens: run your normal ground station to link with the air unit; this page shows the same stream. Units are told apart by their air device ID.'),
    h('h3', 'Setup'),
    h('div.row', 'Channel ', chIn, h('button', { onclick: tune }, 'Tune dongle')),
    h('div.row', h('button', { onclick: applyFilter }, 'Use FPV capture filter'), h('button', { onclick: restoreFilter }, 'Restore previous filter')),
    h('div.row.small', 'Keep ', keepSel),
    filterState, setupMsg,
    h('h3', 'Video'),
    slot,
    h('div.row', largeBtn, h('button.small', { onclick: fullscreen }, 'Fullscreen'), h('button.small', { onclick: snapshot }, 'Save JPEG'), pauseBtn,
      h('button.small', { onclick: () => { app.post({ t: 'fpv-reset' }); last = null; draw(null); toast('FPV decoder reset'); } }, 'Reset decoder')),
    hints,
    h('h3', 'Air units'), unitsBox,
    h('h3', 'Link (shown unit)'), statsT,
    h('h3', 'Air unit status (OSD packets)'), airT,
  );

  let shown = 'auto', paused = false, lastJpeg = null, last = null, visible = false, large = false;
  let decoding = false, decodeErrors = 0;
  const ftimes = [];
  const ctx = canvas.getContext('2d');

  // ---- worker view control: frames are only sent while this panel (or fullscreen) is visible ----
  function sendView() { app.post({ t: 'fpv-view', on: visible && !paused, unit: shown }); }
  function checkVisible() {
    const v = large || isVisible() || document.fullscreenElement === video;
    if (v !== visible) { visible = v; sendView(); }
  }
  document.addEventListener('fullscreenchange', checkVisible);
  document.querySelector('#app').append(stage);
  checkVisible();
  sendView();

  // Large view: the video takes the place of the packet list (desktop layout).
  function setLarge(on) {
    if (on === large) return;
    large = on;
    (large ? stage : slot).append(video);
    stage.classList.toggle('on', large);
    document.querySelector('#app').classList.toggle('fpv-large', large);
    largeBtn.textContent = large ? 'Back to packet list' : 'Large view';
    checkVisible();
  }

  app.addEventListener('fpv-frame', async (e) => {
    const m = e.detail;
    const blob = new Blob([m.buf], { type: 'image/jpeg' });
    lastJpeg = { blob, info: m.info, unit: m.unit };
    if (decoding) { app.post({ t: 'fpv-ack' }); return; }
    decoding = true;
    try {
      const bmp = await createImageBitmap(blob);
      if (canvas.width !== bmp.width || canvas.height !== bmp.height) { canvas.width = bmp.width; canvas.height = bmp.height; }
      ctx.drawImage(bmp, 0, 0);
      bmp.close();
      const now = performance.now();
      ftimes.push(now);
      while (ftimes.length && now - ftimes[0] > 2000) ftimes.shift();
      osdTxt.textContent = `${hex4(m.unit)} · frame ${m.info.index} · ${canvas.width}×${canvas.height} · ${(m.info.size / 1024).toFixed(1)} kB · ${(ftimes.length / 2).toFixed(0)} fps${m.info.fec ? ' · FEC' : ''}`;
      osdTxt.classList.toggle('fec', m.info.fec);
    } catch {
      decodeErrors++;
    } finally {
      decoding = false;
      app.post({ t: 'fpv-ack' });
    }
  });

  // ---- setup actions ----
  function wantedFilter() { return captureFilter(Number(keepSel.value)); }
  async function applyFilter() {
    const set = wantedFilter();
    const cur = settings.get().desired.filter;
    if (!isFpvFilter(cur)) settings.update((x) => { x.fpvPrevFilter = structuredClone(cur); });
    const err = await app.act('FPV capture filter', (c) => c.setFilter(set));
    if (inline(setupMsg, err)) return;
    settings.update((x) => { x.desired.filter = structuredClone(set); });
    let note = 'FPV capture filter active';
    if (app.config && app.config.snaplen) {
      const e2 = await app.act('Snaplen', (c) => c.setSnaplen(0));
      if (!e2) { settings.update((x) => { x.desired.snaplen = 0; }); note += ', snaplen set to full frames'; } else inline(setupMsg, e2);
    }
    toast(note);
    drawFilterState();
  }
  async function restoreFilter() {
    const prev = settings.get().fpvPrevFilter;
    if (!prev) { inline(setupMsg, { level: 'info', title: 'No previous filter saved', text: 'The previous filter is saved when you switch to the FPV capture filter.', actions: [] }); return; }
    const err = await app.act('Restore filter', (c) => c.setFilter(prev));
    if (inline(setupMsg, err)) return;
    settings.update((x) => { x.desired.filter = structuredClone(prev); });
    toast('Previous filter restored');
    drawFilterState();
  }
  async function tune() {
    const ch = Number(chIn.value);
    settings.update((x) => { x.fpvChannel = ch; x.desired.channel = ch; });
    const err = await app.act(`Channel ${ch}`, (c) => c.setChannel(ch, 0));
    if (!inline(setupMsg, err)) toast(`Dongle on channel ${ch}`);
  }
  function isFpvFilter(f) { return !!f?.rules?.some((r) => r.name?.startsWith('hx-esp32-cam-fpv')); }
  function drawFilterState() {
    const cfg = app.config;
    if (!cfg) { filterState.textContent = 'Connect the dongle (or try the demo) to set it up.'; return; }
    let body = null;
    try { body = encodeFilter(wantedFilter()); } catch { /* cannot happen: fixed filter */ }
    const on = body && (crc32(body) >>> 0) === (cfg.rulesCrc32 >>> 0);
    filterState.replaceChildren(
      h('span', `Dongle: channel ${cfg.channel}${cfg.channel !== Number(chIn.value) ? ` (FPV channel is ${chIn.value})` : ''} · `),
      h(on ? 'span.ok-text' : 'span.muted', on ? 'FPV filter active ✓' : 'other filter (all FPV packets still decoded, but the USB link may overflow)'),
      ...(cfg.snaplen ? [h('span.warn-text', ` · snaplen ${cfg.snaplen}: frames are cut, video cannot be decoded`)] : []),
      h('span', ` · capture ${cfg.captureOn ? 'on' : 'off'}`));
  }
  app.addEventListener('config', drawFilterState);
  app.addEventListener('conn', drawFilterState);
  drawFilterState();

  function togglePause() { paused = !paused; pauseBtn.textContent = paused ? 'Resume' : 'Pause'; sendView(); }
  function fullscreen() {
    const req = video.requestFullscreen ?? video.webkitRequestFullscreen;
    if (!req) { toast('Fullscreen is not available in this browser'); return; }
    req.call(video).catch((e) => inline(setupMsg, e, 'Fullscreen'));
  }
  function snapshot() {
    if (!lastJpeg) { toast('No frame yet'); return; }
    const d = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    download(`fpv_${hex4(lastJpeg.unit)}_${d}_f${lastJpeg.info.index}.jpg`, lastJpeg.blob);
  }

  // ---- stats ----
  let prevDrop = null, prevPk = new Map(), prevT = 0;
  app.addEventListener('host', (e) => {
    const f = e.detail.fpv;
    const now = performance.now(), dt = prevT ? (now - prevT) / 1000 : 0;
    prevT = now;
    if (!f) { draw(null); unitsBox.replaceChildren(h('p.small.muted', app.config?.captureOn ? `No hx-esp32-cam-fpv packets yet. Check the channel (dongle on ${app.config.channel}) and that the air unit is powered.` : 'Start capture to look for air units.')); hintsFor(null, e.detail); return; }
    const rates = new Map(f.units.map((u) => [u.id, dt && prevPk.has(u.id) ? (u.st.packets - prevPk.get(u.id)) / dt : null]));
    prevPk = new Map(f.units.map((u) => [u.id, u.st.packets]));
    const radio = (val, label) => h('label.small', h('input', { type: 'radio', name: 'fpv-unit', checked: shown === val, onchange: () => { shown = val; sendView(); } }), ` ${label}`);
    unitsBox.replaceChildren(
      h('table.stats-table',
        h('tr', h('td.muted', 'show'), h('td.muted', 'air ID'), h('td.muted', 'GS ID'), h('td.muted', 'pkt/s'), h('td.muted', 'RSSI'), h('td.muted', 'frames'), h('td.muted', 'last seen')),
        f.units.sort((a, b) => a.id - b.id).map((u) => h('tr',
          h('td', radio(u.id, '')), h('td.mono', hex4(u.id)), h('td.mono', u.gs ? hex4(u.gs) : 'none'),
          h('td', rates.get(u.id) === null ? '' : fmtNum(Math.round(rates.get(u.id)))), h('td', u.rssi === null ? '' : `${u.rssi} dBm`),
          h('td', fmtNum(u.st.framesOk + u.st.framesFec)), h('td', u.ageMs < 1500 ? 'now' : `${(u.ageMs / 1000).toFixed(0)} s ago`)))),
      h('div.row', radio('auto', `Auto (most active${f.shown !== null ? `: ${hex4(f.shown)}` : ''})`)));
    const u = f.units.find((x) => x.id === (shown === 'auto' ? f.shown : shown)) ?? null;
    draw(u, f);
    hintsFor(f, e.detail, u);
    if (u?.config?.wifiChannel && !chIn.matches(':focus') && Number(chIn.value) !== u.config.wifiChannel) chIn.value = u.config.wifiChannel;
  });

  function draw(u, f) {
    last = u;
    if (!u) { statsT.replaceChildren(); airT.replaceChildren(); return; }
    const st = u.st, blocks = st.blocksOk + st.blocksFec + st.blocksLost, frames = st.framesOk + st.framesFec + st.framesLost;
    const kv = (t, list) => t.replaceChildren(...list.filter(Boolean).map(([k, v]) => h('tr', h('td', k), h('td', v))));
    kv(statsT, [
      ['Air / GS ID', `${hex4(u.id)} / ${u.gs ? hex4(u.gs) : 'not linked'}`],
      ['FEC', `k = ${u.k} (${u.kSource}) · slot ${u.mtu} B${u.config ? ` · n = ${u.config.fecN}` : ''}`],
      ['Packets', `${fmtNum(st.packets)} (data ${fmtNum(st.dataPackets)}, FEC ${fmtNum(st.fecPackets)} of which ${fmtNum(st.fecUnneeded)} not needed, duplicates ${fmtNum(st.duplicates)}, too late ${fmtNum(st.late)})`],
      ['Blocks', `${fmtNum(st.blocksOk)} complete · ${fmtNum(st.blocksFec)} repaired by FEC · ${fmtNum(st.blocksLost)} lost (${pct(st.blocksLost, blocks)})`],
      ['Frames', `${fmtNum(st.framesOk)} clean · ${fmtNum(st.framesFec)} via FEC · ${fmtNum(st.framesLost)} lost (${pct(st.framesLost, frames)})`],
      ['Video data', fmtBytes(st.videoBytes)],
      st.crcErrors || st.badSize || st.badJpeg ? ['Errors', `CRC ${st.crcErrors} · size ${st.badSize} · not JPEG ${st.badJpeg}${decodeErrors ? ` · display ${decodeErrors}` : ''}`] : null,
      st.restarts ? ['Air unit restarts', fmtNum(st.restarts)] : null,
      f?.skipped ? ['Frames not shown (display busy)', fmtNum(f.skipped)] : null,
      ['RSSI at dongle', u.rssi === null ? '–' : `${u.rssi} dBm (ch ${u.channel})`],
    ]);
    const o = u.osd, c = u.config;
    kv(airT, o || c ? [
      o && ['Air RSSI / noise', `${o.rssiDbm} / ${o.noiseFloorDbm} dBm (what the air unit hears from the GS)`],
      o && ['Camera', `${RESOLUTIONS[o.resolution] ?? o.resolution} · ${o.captureFps} fps · quality ${o.quality}${o.ov5640 ? ' · OV5640' : ''}${o.suspended ? ' · SUSPENDED' : ''}`],
      o && ['Radio', `ch ${o.wifiChannel} · ${RATES[o.wifiRate] ?? o.wifiRate} · queue ${o.wifiQueueMin}…${o.wifiQueueMax}${o.wifiOverflow ? ' · OVERFLOW' : ''}`],
      o && o.frameSizeMax ? ['Frame size', `${fmtBytes(o.frameSizeMin)} … ${fmtBytes(o.frameSizeMax)}`] : null,
      o && o.temperature ? ['Temperature', `${o.temperature} °C${o.overheat ? ' · THROTTLING' : ''}`] : null,
      o && ['SD card', o.sdDetected ? `${o.sdFreeGB.toFixed(1)} / ${o.sdTotalGB.toFixed(1)} GB free${o.airRecording ? ' · recording' : ''}${o.sdError ? ' · ERROR' : ''}` : 'none'],
      c && ['Config packet', `${RESOLUTIONS[c.resolution] ?? c.resolution} · ch ${c.wifiChannel} · ${RATES[c.wifiRate] ?? c.wifiRate} · ${c.wifiPower} dBm · FEC ${c.fecK}/${c.fecN} · MTU ${c.mtu}`],
    ] : [['', 'No OSD packet received yet.']]);
  }

  function hintsFor(f, host, u) {
    const list = [];
    if (f?.wrongVersion) list.push({ level: 'warn', title: `Air unit uses packet version ${f.lastWrongVersion}`, text: 'This viewer understands hx-esp32-cam-fpv packet version 2 (firmware 0.3). Update the air unit or tell us the version.', actions: [] });
    const drop = app.devStats?.dropRing ?? null;
    if (drop !== null && prevDrop !== null && drop > prevDrop && u) list.push({ level: 'warn', title: 'Dongle drops packets: USB link full', text: `${fmtNum(drop - prevDrop)} frames dropped since the last update. Use the FPV capture filter and keep fewer FEC packets (0…7).`, actions: [] });
    prevDrop = drop;
    if (u && app.config?.snaplen) list.push({ level: 'error', title: 'Snaplen cuts the frames', text: `Snaplen is ${app.config.snaplen} bytes; FPV packets are ~1500 bytes. Use the FPV capture filter (it sets full frames).`, actions: [] });
    if (u && u.ageMs > 3000) list.push({ level: 'info', title: 'Air unit silent', text: `Nothing from ${hex4(u.id)} for ${(u.ageMs / 1000).toFixed(0)} s.`, actions: [] });
    if (host?.hxErrors) list.push({ level: 'error', title: 'FPV decoder error', text: host.hxLastError, actions: ['Reset decoder; if it repeats, export the capture (pcapng) and report it.'] });
    hints.replaceChildren(...list.map((d) => messageEl(d)));
  }
  return { setLarge, checkVisible };
}
