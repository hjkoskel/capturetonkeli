// Filter panel (docs/04 §6) + capture options (§7).
import { app } from '../app.js';
import { h, inline, download, toast, dialog } from './dom.js';
import * as F from '../filter.js';
import * as W from '../wire.js';
import { crc32 } from '../codec.js';
import * as settings from '../settings.js';

const RULER = [[0, 2, 'Frame Control'], [2, 4, 'Duration'], [4, 10, 'Addr1 (receiver)'], [10, 16, 'Addr2 (transmitter)'], [16, 22, 'Addr3 (BSSID/…)'],
  [22, 24, 'Sequence Control'], [24, 30, 'Addr4 / QoS / body'], [30, 1e9, 'frame body']];
const rulerText = (off, len) => RULER.filter(([a, b]) => off < b && off + len > a).map(([, , n]) => n).join(' + ');

export function initFilter(root) {
  let set = structuredClone(settings.get().desired.filter ?? F.emptySet());
  let applied = null; // body hex last applied
  const rulesEl = h('div');
  const msg = h('div');
  const badge = h('span.syncbadge');
  const presetSel = h('select', { 'aria-label': 'Insert preset' }, h('option', { value: '' }, 'Insert preset…'),
    Object.entries(F.PRESETS).map(([k, p]) => h('option', { value: k }, p.label)));
  const rssiEn = h('input', { type: 'checkbox' }), rssi = h('input', { type: 'range', min: -100, max: 0, step: 1 }), rssiVal = h('span.mono');
  const lminEn = h('input', { type: 'checkbox' }), lmin = h('input', { type: 'number', min: 0, max: 65535, style: { width: '6em' } });
  const lmaxEn = h('input', { type: 'checkbox' }), lmax = h('input', { type: 'number', min: 0, max: 65535, style: { width: '6em' } });
  const dropFcs = h('input', { type: 'checkbox' });
  const applyBtn = h('button.primary', { onclick: apply }, 'Apply filter');
  const savedSel = h('select', { 'aria-label': 'Saved filter sets' });

  root.append(
    h('h3', 'Filter on the dongle'),
    h('p.small.muted', 'Rules are OR-ed, terms inside a rule are AND-ed. Offsets count from the 802.11 Frame Control byte. Frames that fail are never sent over USB.'),
    h('div.row', presetSel, h('button', { onclick: () => { set.rules.push({ name: '', enabled: true, terms: [{ offset: 0, value: '', mask: '', negate: false }] }); edited(); } }, '+ Rule')),
    rulesEl,
    h('h3', 'Global conditions'),
    h('div.row', h('label', dropFcs, ' Drop frames with bad FCS')),
    h('div.row', h('label', rssiEn, ' Minimum RSSI '), rssi, rssiVal),
    h('div.row', h('label', lminEn, ' Length ≥ '), lmin, h('label', lmaxEn, ' ≤ '), lmax),
    h('div.row', applyBtn, badge),
    msg,
    h('div.row', savedSel, h('button.small', { onclick: saveNamed }, 'Save as…'), h('button.small', { onclick: exportSet }, 'Export'),
      h('label.small', h('button.small', { onclick: (e) => e.currentTarget.nextElementSibling.click() }, 'Import'),
        h('input.hidden', { type: 'file', accept: '.json,application/json', onchange: importSet }))),
  );

  // ---- capture options (§7) ----
  const snapSel = h('select', { 'aria-label': 'Snaplen preset' }, [['0', 'Full frames'], ['32', '32 (MAC header)'], ['64', '64'], ['128', '128'], ['256', '256'], ['custom', 'Custom…']]
    .map(([v, t]) => h('option', { value: v }, t)));
  const snapIn = h('input', { type: 'number', min: 0, max: 65535, style: { width: '6em' }, 'aria-label': 'Snaplen bytes' });
  const snapMsg = h('div'), snapHint = h('p.small.muted');
  const typeBoxes = W.PROMIS_TYPE.map(([n, b]) => [h('input', { type: 'checkbox', value: String(b) }), n]);
  const ctrlBoxes = W.PROMIS_CTRL.map(([n, b]) => [h('input', { type: 'checkbox', value: String(b >>> 0) }), n]);
  const maskMsg = h('div');
  root.append(
    h('h3', 'Capture options'),
    h('div.row', 'Snaplen ', snapSel, snapIn, h('span.small.muted', 'bytes per frame (0 = unlimited)')),
    snapHint, snapMsg,
    h('details', h('summary', 'Driver pre-filter (advanced)'),
      h('p.small.muted', 'ESP-IDF promiscuous type masks. Prefer the rules above: the driver may mis-classify frames on some versions (doc 02 §1).'),
      h('div.row', typeBoxes.map(([c, n]) => h('label.small', c, ` ${n}`))),
      h('div.row', ctrlBoxes.map(([c, n]) => h('label.small', c, ` ${n}`))),
      h('div.row', h('button.small', { onclick: applyMask }, 'Apply masks'), h('button.small', { onclick: () => setMaskBoxes(W.DEFAULT_TYPE_MASK, W.DEFAULT_CTRL_MASK) }, 'All')),
      maskMsg),
  );

  // ---- editor rendering ----
  function render() {
    queueMicrotask(() => markMatches());
    rulesEl.replaceChildren(...(set.rules.length ? set.rules.map(ruleEl) : [h('p.muted.small', 'No rules: all frames pass (subject to the global conditions).')]));
    const g = set.global ?? (set.global = {});
    dropFcs.checked = !!g.drop_fcs_err;
    rssiEn.checked = g.rssi_min !== null && g.rssi_min !== undefined; rssi.value = g.rssi_min ?? -70; rssiVal.textContent = `${rssi.value} dBm`;
    lminEn.checked = g.len_min !== null && g.len_min !== undefined; lmin.value = g.len_min ?? 0;
    lmaxEn.checked = g.len_max !== null && g.len_max !== undefined; lmax.value = g.len_max ?? 1500;
    validate();
  }

  function ruleEl(r, ri) {
    const en = h('input', { type: 'checkbox', checked: r.enabled !== false, title: 'Enabled (disabled rules are not sent)', onchange: () => { r.enabled = en.checked; edited(); } });
    const name = h('input', { type: 'text', value: r.name ?? '', placeholder: `Rule ${ri + 1}`, oninput: () => { r.name = name.value; edited(false); } });
    const mv = (d) => () => { const j = ri + d; if (j < 0 || j >= set.rules.length) return; [set.rules[ri], set.rules[j]] = [set.rules[j], set.rules[ri]]; edited(); };
    return h(`div.rule${r.enabled === false ? '.disabled' : ''}`, { 'data-ri': ri },
      h('div.rule-head', en, name,
        h('button.icon', { title: 'Move up', onclick: mv(-1) }, '↑'), h('button.icon', { title: 'Move down', onclick: mv(1) }, '↓'),
        h('button.icon', { title: 'Duplicate', onclick: () => { set.rules.splice(ri + 1, 0, structuredClone(r)); edited(); } }, '⧉'),
        h('button.icon.danger', { title: 'Delete rule', onclick: () => { set.rules.splice(ri, 1); edited(); } }, '✕')),
      r.terms.map((t, ti) => termEl(r, t, ti)),
      r.terms.length < F.MAX_TERMS ? h('button.small', { onclick: () => { r.terms.push({ offset: 0, value: '', mask: '', negate: false }); edited(); } }, '+ term (AND)') : null,
      h('div.rule-err.small', { style: { color: 'var(--bad)' } }));
  }

  function termEl(r, t, ti) {
    const off = h('input', { type: 'text', value: String(t.offset), title: 'Offset (decimal or 0x hex)', 'aria-label': 'Offset' });
    const val = h('input', { type: 'text', value: t.value ?? '', placeholder: 'value e.g. 80 or aa:bb:…', 'aria-label': 'Value (hex, ? = any nibble)' });
    const mask = h('input', { type: 'text', value: t.mask ?? '', placeholder: 'mask (default ff…)', 'aria-label': 'Mask' });
    const neg = h('input', { type: 'checkbox', checked: !!t.negate, title: 'Negate (NOT)' });
    const ruler = h('div.ruler');
    const upd = () => {
      t.offset = /^0x/i.test(off.value) ? parseInt(off.value, 16) : Number(off.value);
      t.value = val.value; t.mask = mask.value; t.negate = neg.checked;
      edited(false);
    };
    off.oninput = val.oninput = mask.oninput = upd; neg.onchange = upd;
    const el = h('div.term', off, val, mask, h('label.small', { title: 'NOT' }, neg, '¬'),
      h('button.icon.danger', { title: 'Delete term', onclick: () => { r.terms.splice(ti, 1); if (!r.terms.length) set.rules.splice(set.rules.indexOf(r), 1); edited(); } }, '✕'), ruler);
    el.dataset.ti = ti;
    return el;
  }

  /** Validates everything, marks fields red, returns encoded body or null. */
  function validate() {
    let ok = true;
    set.rules.forEach((r, ri) => {
      const re = rulesEl.querySelector(`[data-ri="${ri}"]`);
      if (!re) return;
      re.querySelector('.rule-err').textContent = '';
      r.terms.forEach((t, ti) => {
        const te = re.querySelectorAll('.term')[ti];
        const [off, val, mask] = te.querySelectorAll('input[type=text]');
        for (const x of [off, val, mask]) x.classList.remove('invalid');
        try {
          const c = F.compileTerm(t);
          te.querySelector('.ruler').textContent = `bytes ${c.offset}–${c.offset + c.value.length - 1}: ${rulerText(c.offset, c.value.length)}`;
        } catch (e) {
          ok = false;
          (/offset/.test(e.message) ? off : /mask/.test(e.message) ? mask : val).classList.add('invalid');
          te.querySelector('.ruler').textContent = e.message;
        }
      });
    });
    let body = null;
    try { body = F.encode(set, app.info?.maxRules ?? F.MAX_RULES); } catch (e) { ok = false; inline(msg, e, 'Filter'); }
    if (ok) inline(msg, null);
    applyBtn.disabled = !ok;
    syncBadge(body);
    return ok ? body : null;
  }

  function syncBadge(body) {
    const cfg = app.config;
    if (!cfg) { badge.textContent = ''; badge.className = 'syncbadge'; return; }
    const local = body ? crc32(body) : null;
    const devCrc = cfg.rulesCrc32 >>> 0;
    const hex8 = (n) => n.toString(16).toUpperCase().padStart(8, '0');
    if (local === devCrc) { badge.textContent = `✓ In sync (crc ${hex8(devCrc)})`; badge.className = 'syncbadge ok'; }
    else if (applied && body && applied === devCrc) { badge.textContent = 'Edited – not applied'; badge.className = 'syncbadge edit'; }
    else { badge.textContent = `⚠ Dongle filter differs (crc ${hex8(devCrc)})`; badge.className = 'syncbadge bad'; }
  }

  function edited(rerender = true) {
    if (rerender) render(); else validate();
    settings.update((x) => { x.desired.filter = structuredClone(set); });
  }

  async function apply() {
    const body = validate();
    if (!body) return;
    applyBtn.classList.add('pending');
    const err = await app.act('Apply filter', (c) => c.setFilter(set));
    applyBtn.classList.remove('pending');
    if (err) inline(msg, err); else { applied = app.config.rulesCrc32 >>> 0; toast(`Filter applied: ${app.config.nRules} rule${app.config.nRules === 1 ? '' : 's'}`); }
    syncBadge(body);
  }

  presetSel.onchange = async () => {
    const p = F.PRESETS[presetSel.value];
    presetSel.value = '';
    if (!p) return;
    let arg;
    if (p.arg === 'mac') {
      const inp = h('input', { type: 'text', placeholder: 'aa:bb:cc:dd:ee:ff', style: { width: '100%' } });
      const sel = app.selectedPacket;
      if (sel) inp.value = p.label.startsWith('From') ? sel.src : p.label.startsWith('To') ? sel.dst : sel.bssid;
      if ((await dialog(p.label, [h('label', 'MAC address', inp)], [{ label: 'Cancel', value: '' }, { label: 'Insert', value: 'ok', primary: true }])) !== 'ok') return;
      arg = inp.value.trim();
      if (!/^([0-9a-f]{2}[:-]){5}[0-9a-f]{2}$/i.test(arg)) { inline(msg, { level: 'error', title: 'Not a MAC address', text: `"${arg}" should look like aa:bb:cc:dd:ee:ff`, actions: [] }); return; }
    }
    set.rules.push(...p.rules(arg));
    edited();
  };

  for (const [el, fn] of [[dropFcs, () => { set.global.drop_fcs_err = dropFcs.checked; }], [rssiEn, () => { set.global.rssi_min = rssiEn.checked ? Number(rssi.value) : null; }],
    [rssi, () => { rssiVal.textContent = `${rssi.value} dBm`; if (rssiEn.checked) set.global.rssi_min = Number(rssi.value); }],
    [lminEn, () => { set.global.len_min = lminEn.checked ? Number(lmin.value) : null; }], [lmin, () => { if (lminEn.checked) set.global.len_min = Number(lmin.value); }],
    [lmaxEn, () => { set.global.len_max = lmaxEn.checked ? Number(lmax.value) : null; }], [lmax, () => { if (lmaxEn.checked) set.global.len_max = Number(lmax.value); }]]) {
    el.addEventListener('input', () => { fn(); edited(false); });
  }

  // named sets / import / export
  function fillSaved() {
    const names = Object.keys(settings.get().savedFilters);
    savedSel.replaceChildren(h('option', { value: '' }, names.length ? 'Load saved set…' : 'No saved sets'), names.map((n) => h('option', { value: n }, n)));
  }
  savedSel.onchange = () => { const v = settings.get().savedFilters[savedSel.value]; if (v) { set = structuredClone(v); edited(); } savedSel.value = ''; };
  async function saveNamed() {
    const inp = h('input', { type: 'text', style: { width: '100%' } });
    if ((await dialog('Save filter set', [h('label', 'Name', inp)], [{ label: 'Cancel', value: '' }, { label: 'Save', value: 'ok', primary: true }])) !== 'ok' || !inp.value.trim()) return;
    settings.update((x) => { x.savedFilters[inp.value.trim()] = structuredClone(set); });
    fillSaved();
  }
  function exportSet() { download('filter.json', JSON.stringify(set, null, 2), 'application/json'); }
  async function importSet(e) {
    const f = e.target.files[0];
    e.target.value = '';
    if (!f) return;
    try {
      const j = JSON.parse(await f.text());
      if (!Array.isArray(j.rules)) throw new F.FilterError('expected {version:1, rules:[…], global:{…}}');
      F.encode(j);
      set = { version: 1, rules: j.rules, global: { ...F.emptySet().global, ...j.global } };
      edited();
      toast(`Imported ${set.rules.length} rules`);
    } catch (err) { inline(msg, err, `Import ${f.name}`); }
  }

  // ---- capture options logic ----
  let snapT;
  function showSnap(n) {
    const pre = ['0', '32', '64', '128', '256'].includes(String(n)) ? String(n) : 'custom';
    snapSel.value = pre; snapIn.value = n;
    snapIn.classList.toggle('hidden', pre !== 'custom');
  }
  snapSel.onchange = () => { if (snapSel.value === 'custom') { snapIn.classList.remove('hidden'); snapIn.focus(); return; } setSnap(Number(snapSel.value)); };
  snapIn.oninput = () => { clearTimeout(snapT); snapT = setTimeout(() => setSnap(Number(snapIn.value)), 300); };
  async function setSnap(n) {
    if (!Number.isInteger(n) || n < 0 || n > 65535) { inline(snapMsg, { level: 'error', title: 'Snaplen must be 0–65535', text: '', actions: [] }); return; }
    settings.update((x) => { x.desired.snaplen = n; });
    inline(snapMsg, await app.act(`Snaplen ${n || 'unlimited'}`, (c) => c.setSnaplen(n)));
  }
  function setMaskBoxes(t, c) {
    for (const [b] of typeBoxes) b.checked = !!((t >>> 0) & Number(b.value));
    for (const [b] of ctrlBoxes) b.checked = !!((c >>> 0) & Number(b.value));
  }
  async function applyMask() {
    let t = 0, c = 0;
    for (const [b] of typeBoxes) if (b.checked) t |= Number(b.value);
    for (const [b] of ctrlBoxes) if (b.checked) c = (c | Number(b.value)) >>> 0;
    if (typeBoxes.every(([b]) => b.checked)) t = W.DEFAULT_TYPE_MASK;
    settings.update((x) => { x.desired.typeMask = t >>> 0; x.desired.ctrlMask = c >>> 0; });
    inline(maskMsg, await app.act('Driver masks', (cl) => cl.setDriverMask(t >>> 0, c >>> 0)));
  }

  // snaplen hint from current rates (docs/04 §7)
  app.addEventListener('stats', () => {
    const st = app.devStats, prev = app._prevStatsForHint;
    app._prevStatsForHint = st;
    if (!prev || !st || st.deviceTimeUs <= prev.deviceTimeUs) return;
    const dt = (st.deviceTimeUs - prev.deviceTimeUs) / 1e6;
    const fps = (st.rxPassed - prev.rxPassed) / dt;
    const bps = (st.txBytes - prev.txBytes) / dt;
    const drop = (st.dropRing - prev.dropRing) / dt;
    snapHint.textContent = fps > 0 ? `At ${Math.round(fps)} frames/s the link carries ${(bps / 1e3).toFixed(0)} kB/s (USB limit ≈ 960 kB/s)${drop > 0 ? ` – the dongle drops ${Math.round(drop)} frames/s: lower the snaplen or filter more.` : '.'}` : '';
  });

  app.addEventListener('config', (e) => {
    const c = e.detail;
    if (document.activeElement !== snapIn) showSnap(c.snaplen);
    setMaskBoxes(c.typeMask, c.ctrlMask);
    syncBadge((() => { try { return F.encode(set); } catch { return null; } })());
  });
  app.addEventListener('filter-add-rule', (e) => { set.rules.push(e.detail); edited(); });
  // "Would match?" per rule for the selected packet (docs/04 §6), same evaluator as the dongle
  app.addEventListener('packet-selected', () => markMatches());
  function markMatches() {
    const p = app.selectedPacket;
    set.rules.forEach((r, ri) => {
      const head = rulesEl.querySelector(`[data-ri="${ri}"] .rule-head`);
      if (!head) return;
      head.querySelector('.wm')?.remove();
      if (!p) return;
      let ok;
      try { ok = r.terms.map(F.compileTerm).every((t) => F.termMatch(t, p.data)); } catch { return; }
      head.append(h('span.wm.small', { title: `Selected packet #${p.n} ${ok ? 'matches' : 'does not match'} this rule (on its captured bytes)` }, ok ? '✓' : '✗'));
    });
  }
  app.addEventListener('config-error', (e) => { if (e.detail.what === 'Filter') inline(msg, e.detail.d); if (e.detail.what === 'Snaplen') inline(snapMsg, e.detail.d); });

  showSnap(settings.get().desired.snaplen);
  setMaskBoxes(settings.get().desired.typeMask, settings.get().desired.ctrlMask);
  fillSaved();
  render();
}
