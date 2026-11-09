// Statistics panel (docs/04 §9).
import { app } from '../app.js';
import { h, inline, confirmDlg, fmtNum, fmtBytes } from './dom.js';

const DEV = [['rxTotal', 'Frames seen'], ['rxPassed', 'Passed filter'], ['dropRing', 'Dropped on dongle (USB too slow)'], ['dropFcs', 'Dropped: bad FCS'],
  ['txFrames', 'Sent over USB'], ['txBytes', 'USB bytes'], ['hostRxCrcErr', 'Bad frames from host'], ['hostRxOversize', 'Oversize frames from host']];

export function initStats(root) {
  const devT = h('table.stats-table'), hostT = h('table.stats-table'), usbT = h('table.stats-table'), usbHint = h('div');
  const gauge = h('div.gauge', h('div', { style: { width: '0%' } })), gaugeTxt = h('span.small.mono');
  const diag = h('div.diag', 'Not connected.');
  const canvas = h('canvas.spark', { width: 600, height: 90, 'aria-label': 'Rates over the last 120 s' });
  const msg = h('div');
  root.append(
    h('div.diag-wrap', diag),
    h('h3', 'Dongle counters'), devT,
    h('div.row', 'Ring high-water ', h('div', { style: { flex: 1 } }, gauge), gaugeTxt),
    h('h3', 'This page'), hostT,
    h('h3', 'USB link (this browser)'), usbT, usbHint,
    h('h3', 'Last 120 s'), canvas,
    h('div.small.muted', '— passed/s  — dropped/s  — host lost/s'),
    h('div.row', h('button', { onclick: refresh }, 'Refresh now'), h('button.danger', { onclick: resetCounters }, 'Reset counters')),
    msg,
  );

  let prev = null;
  const hist = []; // {pass, drop, lost}
  let lastHost = null;

  function rows(t, list) { t.replaceChildren(h('tr', h('td', ''), h('td.muted', 'total'), h('td.muted', '/s')), ...list.map(([k, a, b]) => h('tr', h('td', k), h('td', a), h('td', b ?? '')))); }

  function draw(st) {
    const dt = prev ? (st.deviceTimeUs - prev.deviceTimeUs) / 1e6 : 0;
    const rate = (k) => (prev && dt > 0 ? Math.max(0, (st[k] - prev[k]) / dt) : null);
    rows(devT, DEV.map(([k, label]) => {
      const r = rate(k);
      return [label, k === 'txBytes' ? fmtBytes(st[k]) : fmtNum(st[k]), r === null ? '' : k === 'txBytes' ? `${fmtBytes(r)}/s (${Math.round(r / 9600)}% of USB)` : fmtNum(Math.round(r))];
    }));
    const hw = st.ringHighWater;
    gauge.className = `gauge${hw >= 90 ? ' r' : hw >= 50 ? ' y' : ''}`;
    gauge.firstChild.style.width = `${hw}%`;
    gaugeTxt.textContent = `${hw}%`;
    const pass = rate('rxPassed') ?? 0, drop = rate('dropRing') ?? 0;
    const lost = lastHost ? lastHost.lostRate : 0;
    hist.push({ pass, drop, lost });
    if (hist.length > 120) hist.shift();
    spark();
    // loss diagnosis (docs/04 §9)
    const seen = rate('rxTotal') ?? 0;
    let text;
    if (!seen && st.rxTotal === 0) text = app.config?.captureOn ? 'Capture is on but the dongle hears nothing: quiet channel, wrong channel, or antenna problem.' : 'Capture is off. Press ▶ Capture.';
    else if (drop > 0) text = `Dongle dropping ${Math.round(100 * drop / Math.max(1, pass))}% (USB saturated) → lower snaplen or tighten filters.`;
    else if (lost > 0) text = 'Host losing frames → close other tabs, use a desktop browser, or lower the packet store size.';
    else text = `OK: ${fmtNum(Math.round(pass))} frames/s delivered, no losses.`;
    diag.textContent = text;
    prev = st;
  }

  function spark() {
    const ctx = canvas.getContext('2d');
    const w = canvas.width = canvas.clientWidth * devicePixelRatio || 600, hgt = canvas.height = 90 * devicePixelRatio;
    ctx.clearRect(0, 0, w, hgt);
    const max = Math.max(1, ...hist.map((x) => Math.max(x.pass, x.drop, x.lost)));
    const css = getComputedStyle(document.documentElement);
    for (const [k, col] of [['pass', '--ok'], ['drop', '--bad'], ['lost', '--warn']]) {
      ctx.strokeStyle = css.getPropertyValue(col).trim() || '#888';
      ctx.lineWidth = 1.5 * devicePixelRatio;
      ctx.beginPath();
      hist.forEach((x, i) => { const px = (i / 119) * w, py = hgt - (x[k] / max) * (hgt - 4) - 2; if (i) ctx.lineTo(px, py); else ctx.moveTo(px, py); });
      ctx.stroke();
    }
    ctx.fillStyle = css.getPropertyValue('--muted').trim();
    ctx.font = `${10 * devicePixelRatio}px sans-serif`;
    ctx.fillText(`max ${fmtNum(Math.round(max))}/s`, 4, 12 * devicePixelRatio);
  }

  app.addEventListener('stats', (e) => draw(e.detail));
  app.addEventListener('host', (e) => {
    const s = e.detail, d = s.decoder;
    const lostRate = lastHost ? Math.max(0, s.linkLost - lastHost.linkLost) * 2 : 0;
    lastHost = { ...s, lostRate };
    rows(hostT, [
      ['Frames decoded', fmtNum(d.frames)], ['Packets received', fmtNum(s.received), fmtNum(Math.round(s.pps))], ['Bytes in', fmtBytes(s.bytesIn), `${fmtBytes(s.bps)}/s`],
      ['Lost on USB link (seq gaps)', fmtNum(s.linkLost)], ['Dropped on dongle (rx_seq gaps)', fmtNum(s.deviceGaps)], ['CRC errors', fmtNum(d.crcErrors)],
      ['COBS errors', fmtNum(d.cobsErrors)], ['Oversize frames', fmtNum(d.oversize)], ['Resyncs / overruns', `${d.resyncs} / ${s.overruns}`],
      ['Packet store', `${fmtNum(s.stored)} (${Math.round(s.fill * 100)}%)`], ['Parse time', `${s.parseMs.toFixed(1)} ms / 0.5 s`],
      ['PING RTT', (() => { const r = app.client?.rttStats(); return r ? `${r.median.toFixed(1)} ms (p95 ${r.p95.toFixed(1)})` : '–'; })()],
    ]);
  });
  // WebUSB read loop (docs/01 §6): is the browser keeping reads queued, and how full are they?
  let prevLs = null;
  app.addEventListener('host', () => {
    const t = app.transport;
    if (!t?.linkStats) { usbT.replaceChildren(h('tr', h('td', t ? `${t.label}: the operating system driver buffers the data; see "Bytes in" above.` : 'Not connected.'))); usbHint.replaceChildren(); prevLs = null; return; }
    const l = t.linkStats(), now = performance.now();
    const p = prevLs && prevLs.t0 === l.t0 ? prevLs : null, dt = p ? (now - p.now) / 1000 : 0;
    const per = (k) => (p && dt ? (l[k] - p[k]) / dt : null);
    const reads = per('reads'), bytes = per('bytes');
    // how long the queued reads last at the current read rate: a longer main-thread stall stops the dongle sending
    const coverMs = reads > 0 ? (1000 * l.inflight) / reads : null;
    rows(usbT, [
      ['API', `WebUSB, ${l.inflight} reads of ${fmtBytes(l.size)} queued`],
      ['Received', fmtBytes(l.bytes), bytes === null ? '' : `${fmtBytes(bytes)}/s`],
      ['Reads', fmtNum(l.reads), reads === null ? '' : fmtNum(Math.round(reads))],
      ['Bytes per read', l.reads ? fmtBytes(Math.round(l.bytes / l.reads)) : '–', reads && bytes !== null ? fmtBytes(Math.round(bytes / Math.max(1, reads))) : ''],
      ['Reads filled completely', `${l.reads ? Math.round((100 * l.full) / l.reads) : 0} %`],
      ['Queued reads last', coverMs === null ? '–' : `≈ ${fmtNum(Math.round(coverMs))} ms at this rate`],
      ['Page busy (longest stall)', `${l.lagMs.toFixed(0)} ms`],
      ['Longest gap between reads', `${l.maxGapMs.toFixed(0)} ms`],
    ]);
    const starved = coverMs !== null && l.lagMs > coverMs;
    const drop = (app.devStats?.dropRing ?? 0) > (prevLs?.drop ?? Infinity);
    usbHint.replaceChildren(...(drop && starved ? [h('div.inline-msg.warn', 'The dongle drops frames and the page was busy longer than the queued USB reads last: the browser cannot keep reads queued. Queue more reads (Settings → Developer), show another tab than the packet list, filter more or lower the snaplen.')]
      : drop ? [h('div.inline-msg.warn', 'The dongle drops frames although reads are always queued: the USB link itself is full. Filter more or lower the snaplen.')] : []));
    prevLs = { ...l, now, drop: app.devStats?.dropRing ?? 0 };
  });
  app.addEventListener('conn', (e) => { if (e.detail === 'disconnected') { diag.textContent = 'Not connected.'; prev = null; } });

  async function refresh() { inline(msg, await app.actOne('Refresh stats', async (c) => { const st = await c.stats(false); app.primary.devStats = st; draw(st); })); }
  async function resetCounters() {
    if (!(await confirmDlg('Reset counters?', 'Zeroes the dongle counters and this page\'s rate history.', 'Reset'))) return;
    inline(msg, await app.act('Reset stats', async (c) => { await c.stats(true); prev = null; hist.length = 0; }));
  }
  rows(devT, []); rows(hostT, []);
}
