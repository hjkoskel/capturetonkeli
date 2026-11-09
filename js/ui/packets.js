// Packet list (virtualized) and detail pane (docs/04 §8).
import { app } from '../app.js';
import { h, $, toast } from './dom.js';
import * as D from '../dot11.js';
import * as W from '../wire.js';
import * as settings from '../settings.js';

export function initPackets() {
  const grid = $('#grid'), spacer = $('#grid-spacer'), rowsEl = $('#grid-rows'), empty = $('#empty');
  const auto = $('#autoscroll'), jump = $('#jump-live'), freezeBtn = $('#btn-freeze'), dfilter = $('#dfilter'), dmsg = $('#dfilter-msg');
  const detailEl = $('#detail'), detailBody = $('#detail-body');
  let ROW = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--row')) || 24;
  let total = 0, frozen = false, inflight = false, selected = null, lastStart = -2, newSince = 0, lastTotal = 0;
  let programmaticScroll = false;

  function frame() {
    requestAnimationFrame(frame);
    if (frozen || inflight || document.hidden) return;
    const now = performance.now();
    if (now - (frame.t ?? 0) < 33) return; // ≤ 30 fps
    frame.t = now;
    const count = Math.ceil(grid.clientHeight / ROW) + 2;
    const start = auto.checked ? -1 : Math.max(0, Math.floor(grid.scrollTop / ROW));
    inflight = true;
    app.ask({ t: 'view', start, count }).then((v) => { inflight = false; render(v, count); });
  }

  function render(v, count) {
    total = v.total;
    spacer.style.height = `${total * ROW}px`;
    empty.classList.toggle('hidden', total > 0);
    empty.textContent = app.conn === 'connected' ? (app.config?.captureOn ? 'Waiting for packets…' : 'Capture is stopped. Press ▶ Capture.') : 'No packets yet.';
    if (!auto.checked && total > lastTotal) newSince += total - lastTotal;
    lastTotal = total;
    jump.classList.toggle('hidden', auto.checked || newSince === 0);
    jump.textContent = `Jump to live (${newSince} new)`;
    if (auto.checked) {
      programmaticScroll = true;
      grid.scrollTop = Math.max(0, total * ROW - grid.clientHeight);
    }
    rowsEl.style.transform = `translateY(${v.start * ROW}px)`;
    const fmtT = timeFmt();
    rowsEl.replaceChildren(...v.rows.map((r) => (r.marker ? h(`div.prow.marker.${r.kind}`, { role: 'row' }, h('span', `▸ ${r.marker}`)) : rowEl(r, fmtT))));
    lastStart = v.start;
    void count;
  }

  function timeFmt() {
    const tf = settings.get().timeFormat;
    if (tf === 'abs') return (r) => new Date(r.hostMs).toLocaleTimeString(undefined, { hour12: false }) + `.${String(r.hostMs % 1000).padStart(3, '0')}`;
    if (tf === 'dev') return (r) => String(r.tsUs);
    return (r) => r.t.toFixed(6);
  }

  function rowEl(r, fmtT) {
    const el = h(`div.prow.${r.cls}${r.fcsErr ? '.fcs' : ''}${selected === r.n ? '.sel' : ''}`, { role: 'row', 'data-n': r.n },
      h('span', r.n), h('span', fmtT(r)), h('span', r.ch, r.ht40 ? '⁴⁰' : ''),
      h('span', r.rssi, h('span.rssi-bar', { style: { width: `${Math.max(1, Math.min(14, (r.rssi + 100) / 5))}px` } })),
      h('span', r.rate), h(`span${r.trunc ? '.trunc' : ''}`, r.len), h('span.t-type', r.type), h('span', r.src), h('span', r.dst), h('span', r.bssid),
      h('span', { title: r.info }, r.info));
    return el;
  }

  grid.addEventListener('scroll', () => {
    if (programmaticScroll) { programmaticScroll = false; return; }
    const atBottom = grid.scrollTop + grid.clientHeight >= grid.scrollHeight - ROW * 2;
    if (!atBottom && auto.checked) { auto.checked = false; newSince = 0; }
  });
  auto.onchange = () => { newSince = 0; };
  jump.onclick = () => { auto.checked = true; newSince = 0; };
  freezeBtn.onclick = () => toggleFreeze();
  function toggleFreeze() { frozen = !frozen; freezeBtn.classList.toggle('on', frozen); freezeBtn.textContent = frozen ? 'Frozen' : 'Freeze'; }
  $('#btn-clear').onclick = () => { app.post({ t: 'clear' }); selected = null; detailBody.replaceChildren('Select a packet.'); };
  document.addEventListener('keydown', (e) => {
    if (e.key === ' ' && !e.target.closest('input, textarea, select, button, dialog')) { e.preventDefault(); toggleFreeze(); }
  });

  // row selection (click + ↑/↓ in the grid)
  rowsEl.addEventListener('click', (e) => { const r = e.target.closest('.prow[data-n]'); if (r) select(Number(r.dataset.n)); });
  grid.addEventListener('keydown', (e) => {
    if (selected === null || (e.key !== 'ArrowDown' && e.key !== 'ArrowUp')) return;
    e.preventDefault(); e.stopPropagation();
    select(selected + (e.key === 'ArrowDown' ? 1 : -1));
  });

  async function select(n) {
    const r = await app.ask({ t: 'detail', n });
    if (!r.d) { detailBody.replaceChildren(h('p.muted', 'This packet is no longer in the store.')); return; }
    selected = n;
    for (const el of rowsEl.querySelectorAll('.prow.sel')) el.classList.remove('sel');
    rowsEl.querySelector(`.prow[data-n="${n}"]`)?.classList.add('sel');
    renderDetail(r.d);
    detailEl.classList.add('open');
  }
  $('#detail-close').onclick = () => detailEl.classList.remove('open');

  // display filter
  let dT;
  dfilter.addEventListener('input', () => {
    clearTimeout(dT);
    dT = setTimeout(async () => {
      const r = await app.ask({ t: 'filter', expr: dfilter.value });
      dfilter.classList.toggle('invalid', !r.ok);
      dmsg.textContent = r.ok ? (dfilter.value.trim() ? `showing ${r.shown} items` : '') : r.error;
      dmsg.style.color = r.ok ? '' : 'var(--bad)';
      dfilter.setAttribute('aria-invalid', String(!r.ok));
    }, 300);
  });

  // ---- detail ----
  function renderDetail(d) {
    const m = d.meta, frame = W.frameBytes(m, d.data), p = D.parse(frame);
    const hexAll = Array.from(d.data, (b) => b.toString(16).padStart(2, '0'));
    app.selectedPacket = { n: d.n, data: frame, meta: m, src: p.addr2 ?? '', dst: p.addr1 ?? '', bssid: p.bssid ?? '', fc0: d.data[0] };
    app.emit('packet-selected', app.selectedPacket);
    const flags = Object.entries(W.PKT).filter(([, b]) => m.flags & b).map(([k]) => k.toLowerCase()).join(', ') || 'none';
    const rate = m.flags & W.PKT.RATE_UNKNOWN ? 'unknown' : m.bbFormat <= 1 ? `${m.rate / 2} Mb/s` : `MCS ${m.rate}`;
    const meta = h('dl.kv', ...[['Packet', `#${d.n}`], [d.copies ? 'Group time' : 'Device time', `${m.tsUs} µs (+${d.rel.toFixed(6)} s)`], ['Host time', new Date(d.hostMs).toISOString()],
      ['Channel', `${m.channel} (${W.channelFreq(m.channel)} MHz)`], ['RSSI / noise', `${m.rssi} / ${m.noise} dBm (SNR ${m.rssi - m.noise} dB)`],
      ['PHY', `${W.bbName(m.bbFormat)}, ${rate}`], ['Length', `${m.capLen} of ${m.origLen} bytes${m.flags & W.PKT.NO_FCS ? ' (no FCS)' : ''}`],
      ['Flags', flags], ['rx_seq / cfg_gen', `${m.rxSeq} / ${d.cfgGen}`],
      // diversity (docs/14): which dongles heard this frame, RSSI at each; the first one is shown above
      ...(d.copies ? [['Received by', d.copies.map((c) => `${c.label} ${c.rssi} dBm${c.first ? ' (first)' : ''}`).join(' · ') || '–']] : []),
    ].flatMap(([k, v]) => [h('dt', k), h('dd', String(v))]));

    const tree = h('div.tree');
    const add = (k, v, range) => tree.append(h('div', range ? { 'data-s': range[0], 'data-l': range[1] } : {}, h('span.k', `${k}: `), v));
    add('Type', `${p.name} (type ${p.type}, subtype ${p.subtype})`, [0, 2]);
    if (d.data.length >= 2) add('Flags', [p.toDS && 'ToDS', p.fromDS && 'FromDS', p.retry && 'Retry', p.pwrMgmt && 'PwrMgt', p.moreData && 'MoreData', p.protected && 'Protected', p.order && 'Order'].filter(Boolean).join(' ') || '—', [1, 1]);
    for (const [name, s, l] of p.fields.slice(1)) {
      let v = '';
      if (name.startsWith('Duration')) v = String(p.duration);
      else if (name.startsWith('Addr1')) v = p.addr1; else if (name.startsWith('Addr2')) v = p.addr2; else if (name === 'Addr3') v = p.addr3; else if (name === 'Addr4') v = p.addr4;
      else if (name.startsWith('Sequence')) v = `seq ${p.seq}, frag ${p.frag}`; else if (name.startsWith('QoS')) v = `TID ${p.tid}`; else v = `${l} bytes`;
      add(name, v, [s, l]);
    }
    for (const ie of p.ies ?? []) {
      const v = ie.id === 0 ? `"${ie.text}"` : ie.id === 3 ? `channel ${ie.data[0]}` : ie.oui ? `OUI ${ie.oui}` : `${ie.data.length} bytes`;
      add(`IE ${ie.name}`, v, [ie.start, ie.len]);
    }
    if (frame.length < d.data.length) {
      const fcs = Array.from(d.data.subarray(frame.length), (b) => b.toString(16).padStart(2, '0')).join('');
      add('FCS', `0x${fcs.match(/../g).reverse().join('')}${m.flags & W.PKT.FCS_ERR ? ' (BAD)' : ' (ok)'}`, [frame.length, 4]);
    }
    if (p.truncated && m.capLen < m.origLen) tree.append(h('div.muted', '[truncated: increase snaplen to decode more]'));

    const dump = h('div.hexdump');
    for (let o = 0; o < d.data.length; o += 16) {
      const line = h('div', h('span.off', o.toString(16).padStart(4, '0') + '  '));
      for (let i = o; i < o + 16; i++) line.append(i < d.data.length ? h('span', { 'data-i': i }, hexAll[i] + ' ') : '   ');
      line.append(' ', Array.from(d.data.subarray(o, o + 16), (b) => (b >= 32 && b < 127 ? String.fromCharCode(b) : '.')).join(''));
      dump.append(line);
    }
    // field ↔ bytes highlighting
    const hl = (s, l, on) => { for (let i = s; i < s + l; i++) dump.querySelector(`[data-i="${i}"]`)?.classList.toggle('hl', on); };
    tree.addEventListener('mouseover', (e) => { const t = e.target.closest('[data-s]'); if (t) { t.classList.add('hl'); hl(+t.dataset.s, +t.dataset.l, true); } });
    tree.addEventListener('mouseout', (e) => { const t = e.target.closest('[data-s]'); if (t) { t.classList.remove('hl'); hl(+t.dataset.s, +t.dataset.l, false); } });
    dump.addEventListener('mouseover', (e) => {
      const i = Number(e.target.dataset?.i);
      if (Number.isNaN(i)) return;
      for (const t of tree.querySelectorAll('[data-s]')) t.classList.toggle('hl', i >= +t.dataset.s && i < +t.dataset.s + +t.dataset.l);
    });

    const copyHex = () => copy(hexAll.join(' '), 'Hex copied');
    const copyWs = () => copy(Array.from({ length: Math.ceil(d.data.length / 16) }, (_, k) => `${(k * 16).toString(16).padStart(4, '0')}  ${hexAll.slice(k * 16, k * 16 + 16).join(' ')}`).join('\n') + '\n',
      'Copied (Wireshark: File → Import from Hex Dump, encapsulation 802.11)');
    const mkRule = () => {
      const rule = { name: `${p.name}${p.addr2 ? ' from ' + p.addr2 : ''}`, enabled: true,
        terms: [{ offset: 0, value: d.data[0].toString(16).padStart(2, '0'), mask: 'fc', negate: false }, ...(p.addr2 ? [{ offset: 10, value: p.addr2, mask: '', negate: false }] : [])] };
      app.emit('filter-add-rule', rule);
      toast('Rule added to the Filter tab (not applied yet)');
    };
    detailBody.replaceChildren(
      h('div.row', h('button.small', { onclick: mkRule, disabled: d.data.length < 1 }, 'Make rule from this packet'), h('button.small', { onclick: copyHex }, 'Copy hex'),
        h('button.small', { onclick: copyWs }, 'Copy as Wireshark hex dump')),
      h('div.detail-cols', h('div', meta, h('h4', '802.11'), tree), dump));
  }

  async function copy(text, okMsg) {
    try { await navigator.clipboard.writeText(text); toast(okMsg); } catch { toast('Clipboard not available in this browser'); }
  }

  app.addEventListener('conn', () => { ROW = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--row')) || 24; });
  requestAnimationFrame(frame);
  void lastStart;
}
