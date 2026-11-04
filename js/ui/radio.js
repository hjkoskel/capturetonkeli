// Radio panel (docs/04 §5): country, band, channel grid, bandwidth, readback.
import { app } from '../app.js';
import { h, inline } from './dom.js';
import * as W from '../wire.js';
import { COUNTRIES } from '../regdom.js';
import * as settings from '../settings.js';

const CH24 = Array.from({ length: 14 }, (_, i) => i + 1);
const UNII = [['UNII-1', [36, 40, 44, 48]], ['UNII-2', [52, 56, 60, 64]], ['UNII-2C', [100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144]],
  ['UNII-3 / SRD', [149, 153, 157, 161, 165, 169, 173, 177]]];

export function initRadio(root) {
  const s = settings.get();
  let band = s.desired.channel > 14 ? 5 : 2;
  let pendingCh = null;

  // country
  const mode = h('select', { 'aria-label': 'Country mode' }, h('option', { value: '0' }, 'Validated country code'), h('option', { value: '1' }, 'Manual range (advanced)'));
  const cc = h('select', { 'aria-label': 'Country' }, Object.keys(COUNTRIES).map((c) => h('option', { value: c }, c === '01' ? '01 – world safe' : c)));
  const schan = h('input', { type: 'number', min: 1, max: 14, value: s.desired.country.schan ?? 1, style: { width: '4em' }, 'aria-label': 'First channel' });
  const nchan = h('input', { type: 'number', min: 1, max: 14, value: s.desired.country.nchan ?? 13, style: { width: '4em' }, 'aria-label': 'Channel count' });
  const manualRow = h('div.row.hidden', '2.4 GHz from ', schan, ' count ', nchan, h('span', { title: 'Unvalidated table. Receive-only use.' }, '⚠'));
  const applyCountry = h('button', { onclick: doCountry }, 'Apply');
  const countryMsg = h('div');
  mode.value = String(s.desired.country.mode ?? 0);
  cc.value = COUNTRIES[s.desired.country.cc] ? s.desired.country.cc : '01';

  // channel
  const b24 = h('button', { onclick: () => setBand(2) }, '2.4 GHz');
  const b5 = h('button', { onclick: () => setBand(5) }, '5 GHz');
  const grid = h('div');
  const bw = h('select', { 'aria-label': 'Bandwidth', onchange: () => setChannel(app.config?.channel ?? s.desired.channel) },
    h('option', { value: '0' }, 'HT20'), h('option', { value: '1' }, 'HT40+'), h('option', { value: '2' }, 'HT40−'));
  const bwNote = h('span.muted.small');
  const chMsg = h('div');
  const readback = h('dl.kv');

  root.append(
    h('h3', 'Country / regulatory'),
    h('details', h('summary', 'Why does this matter?'), h('p.small.muted',
      'The dongle only receives, so no channels are restricted for transmission. The ESP-IDF radio driver still uses the country table to decide which channels it will tune to; pick the table that unlocks the channels you need (e.g. JP for channel 14, FI/US for 5 GHz 149+).')),
    h('div.row', mode), h('div.row', cc, applyCountry), manualRow,
    h('p.small.muted', 'Kept in dongle RAM only and re-sent on every connect (no flash writes).'),
    countryMsg,
    h('h3', 'Channel'),
    h('div.row', h('div.seg', b24, b5), bw, bwNote),
    grid, chMsg,
    h('p.small.muted', 'Keys ← / → step through the channels of the band.'),
    h('h3', 'Current radio (dongle readback)'), readback,
  );

  mode.onchange = () => { manualRow.classList.toggle('hidden', mode.value !== '1'); updateApply(); };
  for (const el of [cc, schan, nchan]) el.oninput = updateApply;
  manualRow.classList.toggle('hidden', mode.value !== '1');

  function region() {
    const c = app.config?.country;
    if (!c) return COUNTRIES[cc.value] ?? COUNTRIES['01'];
    const base = COUNTRIES[c.cc] ?? COUNTRIES['01'];
    return c.mode === 1 ? { schan: c.schan, nchan: c.nchan, ch5: base.ch5 } : base;
  }
  const allowed = (r, ch) => (ch <= 14 ? ch >= r.schan && ch < r.schan + r.nchan : r.ch5.includes(ch));

  function drawGrid() {
    const caps = app.info?.caps ?? (W.CAP.GHZ5 | W.CAP.HT40);
    b5.disabled = !(caps & W.CAP.GHZ5);
    b5.title = b5.disabled ? 'This dongle has no 5 GHz capability' : '';
    if (b5.disabled && band === 5) band = 2;
    b24.classList.toggle('on', band === 2); b5.classList.toggle('on', band === 5);
    const ht40 = !!(caps & W.CAP.HT40);
    bw.disabled = !ht40;
    bwNote.textContent = ht40 ? '' : 'HT40 not supported by this dongle yet';
    if (!ht40) bw.value = '0';
    const r = region();
    const cur = app.config?.channel ?? s.desired.channel;
    const btn = (ch) => {
      const ok = allowed(r, ch);
      const mhz = W.channelFreq(ch);
      return h(`button${ch === cur ? '.cur' : ''}${ok ? '' : '.dim'}${W.DFS.has(ch) ? '.dfs' : ''}${ch === pendingCh ? '.pending' : ''}`,
        { onclick: () => setChannel(ch), title: `${mhz} MHz${W.DFS.has(ch) ? ' · DFS (receive OK)' : ''}${ok ? '' : ' · not in the current country table'}`,
          'aria-pressed': String(ch === cur) }, String(ch));
    };
    grid.replaceChildren(...(band === 2 ? [h('div.chgrid', CH24.map(btn))]
      : UNII.flatMap(([name, chs]) => [h('div.unii', name), h('div.chgrid', chs.map(btn))])));
  }

  function setBand(b) { band = b; drawGrid(); }

  async function setChannel(ch) {
    const b = Number(bw.value);
    pendingCh = ch;
    drawGrid();
    inline(chMsg, null);
    const err = await app.act(`Channel ${ch}`, (c) => c.setChannel(ch, b));
    pendingCh = null;
    if (err) {
      const msg = inline(chMsg, err);
      if (err.title.includes('country table')) {
        chMsg.querySelector('.b-main').append(h('div', h('button.small', { onclick: () => { mode.focus(); root.scrollTop = 0; } }, 'Change country table')));
      }
      void msg;
    } else {
      settings.update((x) => { x.desired.channel = ch; x.desired.bw = b; });
      const got = app.config;
      if (got && got.bw !== b) inline(chMsg, { level: 'info', title: `Requested ${W.bwName(b)}, the dongle applied ${W.bwName(got.bw)}`, text: '', actions: [] });
    }
    drawGrid();
  }

  function updateApply() {
    const c = app.config?.country;
    const want = desiredCountry();
    applyCountry.disabled = !!c && c.cc === want.cc && c.mode === want.mode && (want.mode === 0 || (c.schan === want.schan && c.nchan === want.nchan));
  }
  const desiredCountry = () => ({ cc: cc.value, mode: Number(mode.value), schan: Number(schan.value), nchan: Number(nchan.value) });

  async function doCountry() {
    const d = desiredCountry();
    if (d.mode === 1 && (d.schan < 1 || d.nchan < 1 || d.schan + d.nchan - 1 > 14)) {
      inline(countryMsg, { level: 'error', title: 'Manual range must stay within channels 1–14', text: `From ${d.schan}, ${d.nchan} channels ends at ${d.schan + d.nchan - 1}.`, actions: [] });
      return;
    }
    applyCountry.classList.add('pending');
    const err = await app.act(`Country ${d.cc}`, (c) => c.setCountry(d.cc, d.mode, d.schan, d.nchan));
    applyCountry.classList.remove('pending');
    inline(countryMsg, err);
    if (!err) {
      settings.update((x) => { x.desired.country = d; });
      if (app.config && app.config.channel !== settings.get().desired.channel) {
        inline(countryMsg, { level: 'info', title: `Channel ${settings.get().desired.channel} is not in the new table; the dongle moved to channel ${app.config.channel}`, text: '', actions: [] });
      }
    }
    drawGrid();
  }

  function drawReadback() {
    const c = app.config;
    if (!c) { readback.replaceChildren(h('dt', 'State'), h('dd', 'not connected')); return; }
    const kv = [['Band', c.band ? '5 GHz' : '2.4 GHz'], ['Channel', `${c.channel} (${W.channelFreq(c.channel)} MHz)`], ['Bandwidth', W.bwName(c.bw)],
      ['Country', `${c.country.cc} ${c.country.mode ? `manual ${c.country.schan}–${c.country.schan + c.country.nchan - 1}` : 'validated'} (2.4 GHz ${c.country.schan}–${c.country.schan + c.country.nchan - 1})`],
      ['Noise floor', app.devStats ? `${app.devStats.noiseFloor} dBm` : '–'], ['cfg_gen', c.cfgGen]];
    readback.replaceChildren(...kv.flatMap(([k, v]) => [h('dt', k), h('dd', String(v))]));
  }

  app.addEventListener('config', (e) => {
    const c = e.detail;
    band = c.channel > 14 ? 5 : 2;
    if (document.activeElement !== cc && document.activeElement !== mode) {
      mode.value = String(c.country.mode);
      if (COUNTRIES[c.country.cc]) cc.value = c.country.cc;
      manualRow.classList.toggle('hidden', c.country.mode !== 1);
    }
    bw.value = String(c.bw);
    drawGrid(); drawReadback(); updateApply();
  });
  app.addEventListener('stats', drawReadback);
  app.addEventListener('info', drawGrid);
  app.addEventListener('config-error', (e) => {
    if (e.detail.what.startsWith('Channel')) inline(chMsg, e.detail.d);
    if (e.detail.what === 'Country') inline(countryMsg, e.detail.d);
  });

  // ←/→ sweep (docs/04 §5)
  document.addEventListener('keydown', (e) => {
    if (e.target.closest('input, textarea, select, dialog') || e.ctrlKey || e.altKey || e.metaKey) return;
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight' || app.conn !== 'connected') return;
    const list = (band === 2 ? CH24 : UNII.flatMap(([, c]) => c)).filter((ch) => allowed(region(), ch));
    const i = list.indexOf(app.config.channel);
    const next = list[(i + (e.key === 'ArrowRight' ? 1 : -1) + list.length) % list.length];
    if (next) { e.preventDefault(); setChannel(next); }
  });

  drawGrid(); drawReadback(); updateApply();
}
