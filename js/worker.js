// Dedicated Worker (docs/04 §2): COBS/CRC decode, header seq accounting,
// packet store (typed-array ring, no per-packet objects), markers, display
// filter, view windows for the virtualized list, pcapng recording/export.
import { Decoder } from './codec.js';
import * as W from './wire.js';
import * as D from './dot11.js';
import * as P from './pcapng.js';
import { compileDisplayFilter } from './display-filter.js';
import { Receiver as HxReceiver } from './hxfpv.js';
import * as OHD from './openhd.js';
import { Depacketizer } from './rtp.js';
import * as MAV from './mavlink.js';
import { mp4Files } from './mp4.js';
import { Merger, HOLD_MS } from './diversity.js';

const host = { frames: 0, bytesIn: 0, linkLost: 0, deviceGaps: 0, unknownTypes: 0, badPkt: 0, overruns: 0, parseMs: 0 };

// ---- dongles (docs/14): one parse state per member; member 0 = the single-dongle case ------------
const mems = new Map(); // id → { dec, lastSeq, lastGen, lastRxSeq, label, mac, st }
function mem(id) {
  let x = mems.get(id);
  if (!x) {
    x = { id, dec: new Decoder(), lastSeq: -1, lastGen: -1, lastRxSeq: -1, label: `Dongle ${id + 1}`, mac: '', st: { frames: 0, bytes: 0, pkts: 0, linkLost: 0, deviceGaps: 0, badPkt: 0, overruns: 0 } };
    mems.set(id, x);
  }
  return x;
}
mem(0);
// OpenHD FEC with several dongles: another dongle's copy of a lost packet can arrive after newer
// blocks (USB delivery skew between dongles), so incomplete blocks are held up to HOLD_MS then.
let ohdOpts = {};
function applyOhdOpts() { ohd.setOptions({ ...ohdOpts, holdMs: merger ? HOLD_MS : 0 }); }
// With 2+ dongles the merger dedups their streams (first copy wins, at once); with one it is not used,
// so a single dongle behaves exactly as before.
let merger = null, dueTimer = 0;
function onFinal(e) { if (rec && e.abs !== null && e.abs >= oldest) recordPacket(e.abs); }
function setMembers(list) {
  for (const x of list) Object.assign(mem(x.id), { label: x.label, mac: x.mac });
  const before = merger ? merger.size : 1;
  if (list.length >= 2) {
    merger ??= new Merger({ onFinal });
    merger.setMembers(list.map((x) => ({ id: x.id, mac: x.mac })));
  } else if (merger) { drainMerger(true); merger = null; }
  applyOhdOpts();
  if (list.length !== before) addMark(list.length >= 2 ? `Diversity: ${list.length} dongles (${list.map((x) => x.label).join(', ')})` : 'Diversity off: one dongle', 'cfg');
}
function drainMerger(all) {
  if (!merger) return;
  if (all) merger.flush();
  for (const b of merger.due(all ? Infinity : performance.now())) storeBad(b);
  clearTimeout(dueTimer); dueTimer = 0;
  if (merger.held.length) dueTimer = setTimeout(() => { dueTimer = 0; drainMerger(false); }, HOLD_MS + 1);
}
// a bad copy no other dongle delivered cleanly: emitted as it is, after the hold
function storeBad(b) {
  const abs = store({ cfgGen: mem(b.id).lastGen }, { ...b.meta, tsUs: Math.round(b.g) }, b.data, b.id);
  if (rec) recordPacket(abs);
}

// ---- packet store ----------------------------------------------------------------------
let N = 200000, BYTES = 64 << 20;
let col, bytes, wp = 0, oldest = 0, next = 0;
function alloc(n) {
  N = n;
  BYTES = Math.min(256 << 20, Math.max(16 << 20, n * 320));
  col = {
    ts: new Float64Array(N), hostMs: new Float64Array(N), off: new Uint32Array(N), origLen: new Uint16Array(N), capLen: new Uint16Array(N),
    rxSeq: new Uint16Array(N), ch: new Uint8Array(N), flags: new Uint8Array(N), rssi: new Int8Array(N), noise: new Int8Array(N),
    rate: new Uint8Array(N), bb: new Uint8Array(N), gen: new Uint8Array(N),
    m0: new Uint8Array(N), rx: new Uint8Array(N), rssiM: new Int8Array(N * 8), // diversity: first dongle, dongles that heard it, RSSI each
  };
  bytes = new Uint8Array(BYTES);
  wp = 0; oldest = 0; next = 0;
  markers.clear();
  markerKeys = [];
  order = []; orderHead = 0;
}
const markers = new Map(); // abs packet index (marker shown before it) → [text]
let pendingMarks = [];     // marks waiting for the next packet
let order = [], orderHead = 0; // visible items: abs index (≥0) or ~markerKey (<0)
let markerKeys = [];       // ~k → {before, text}
let filterFn = null;
let firstTs = null;
let captureEpoch = 0;
const cfgNotes = new Map();

alloc(N);

// ---- hx-esp32-cam-fpv receiver (docs/12) -----------------------------------------------------
// Decodes every unit in range; JPEGs of the shown unit go to the main thread
// (at most 2 in flight, so a slow display drops frames instead of queueing).
const fpvView = { on: false, unit: 'auto', inFlight: 0, skipped: 0 };
const hx = new HxReceiver({ now: () => performance.now(), onFrame: (u, jpeg, info) => {
  if (!fpvView.on || shownUnit() !== u.id) return;
  if (fpvView.inFlight >= 2) { fpvView.skipped++; return; }
  fpvView.inFlight++;
  self.postMessage({ t: 'fpv-frame', unit: u.id, info, buf: jpeg }, [jpeg.buffer]);
} });
function shownUnit() {
  if (fpvView.unit !== 'auto') return fpvView.unit;
  let best = null, now = performance.now();
  for (const u of hx.units.values()) if (now - u.last < 3000 && (!best || u.st.framesOk + u.st.framesFec > best.st.framesOk + best.st.framesFec)) best = u;
  return best?.id ?? null;
}
function hxPush(meta, data) {
  try { hx.push(W.frameBytes(meta, data), { rssi: meta.rssi, channel: meta.channel }); } catch (e) { host.hxErrors = (host.hxErrors ?? 0) + 1; host.hxLastError = String(e.message ?? e); }
}

// ---- OpenHD receiver (docs/13) ---------------------------------------------------------------
// Decodes every OpenHD unit; access units of the shown unit/camera go to the
// main thread (WebCodecs). If the display falls behind, sending stops until
// the next key frame. Telemetry is parsed here (MAVLink), recordings are
// collected here so they continue while the tab is hidden.
const ohdView = { on: false, unit: 'auto', cam: 0, codec: 'auto', inFlight: 0, needKey: true, waitSince: 0, sent: 0, skipped: 0, forced: 0 };
const ohdFlight = new Map(); // unit id → MAV.FlightState
const ohdDepack = new Map(); // `${unit}:${cam}` → Depacketizer
let ohdRec = null;           // { video: {unit, cam, codec, format, parts: [{data,key,ts}], size, aus} | null, tlog: {parts, size, msgs} | null, started }
let ohdShown = null;
const ohd = new OHD.Receiver({
  now: () => performance.now(),
  onRtp(u, cam, pkt, info) {
    const want = (ohdView.on && u.id === ohdShown && cam === ohdView.cam) || (ohdRec?.video && ohdRec.video.unit === u.id && ohdRec.video.cam === cam);
    if (!want) return;
    const key = `${u.id}:${cam}`;
    let d = ohdDepack.get(key);
    if (!d) { d = new Depacketizer({ codec: ohdView.codec, onAU: (au) => ohdAU(u, cam, au) }); ohdDepack.set(key, d); }
    d.push(pkt, info);
  },
  onTelemetry(u, data, dir) {
    let fs = ohdFlight.get(u.id);
    if (!fs) { fs = new MAV.FlightState(); ohdFlight.set(u.id, fs); }
    const msgs = MAV.parse(data);
    fs.add(msgs);
    if (ohdRec?.tlog) for (const m of msgs) if (m.crcOk !== false) { const r = MAV.tlogRecord(m.raw); ohdRec.tlog.parts.push(r); ohdRec.tlog.size += r.length; ohdRec.tlog.msgs++; }
    void dir;
  },
  onSession(u, kind) { if (kind === 'restart') { ohdFlight.delete(u.id); for (const k of ohdDepack.keys()) if (k.startsWith(`${u.id}:`)) ohdDepack.delete(k); } },
});
function ohdAU(u, cam, au) {
  const r = ohdRec?.video;
  let recorded = false;
  if (r && r.unit === u.id && r.cam === cam && (r.aus || au.key)) { r.parts.push({ data: au.data, key: au.key, ts: au.ts }); r.size += au.data.length; r.aus++; r.codec = au.codec; recorded = true; }
  if (!ohdView.on || u.id !== ohdShown || cam !== ohdView.cam) return;
  const now = performance.now();
  if (ohdView.inFlight > 20) { ohdView.needKey = true; ohdView.waitSince = now; }
  let forced = false;
  if (ohdView.needKey) {
    // streams with intra refresh never send IDR frames: after 3 s, start at any frame that has parameter sets cached
    if (!au.key && !(au.hasParams && now - ohdView.waitSince > 3000 && ohdView.inFlight < 5)) { ohdView.skipped++; return; }
    forced = !au.key;
    if (forced) ohdView.forced++;
    ohdView.needKey = false;
  }
  ohdView.inFlight++; ohdView.sent++;
  const data = recorded ? au.data.slice() : au.data;
  self.postMessage({ t: 'ohd-au', unit: u.id, cam, codec: au.codec, codecString: au.codecString, key: au.key || forced, forced, damaged: au.damaged, data }, [data.buffer]);
}
function ohdShownUnit() {
  const units = [...ohd.units.values()].filter((u) => u.dir === OHD.UID.AIR);
  if (ohdView.unit !== 'auto') return units.some((u) => u.id === ohdView.unit) ? ohdView.unit : null;
  let best = null;
  const now = performance.now();
  for (const u of units) if (now - u.last < 3000 && (!best || u.st.video[0] + u.st.video[1] > best.st.video[0] + best.st.video[1])) best = u;
  return best?.id ?? null;
}
function ohdPush(meta, data) {
  try { ohd.push(W.frameBytes(meta, data), { rssi: meta.rssi, channel: meta.channel }); } catch (e) { host.ohdErrors = (host.ohdErrors ?? 0) + 1; host.ohdLastError = String(e.message ?? e); }
}
function ohdSummary() {
  const s = ohd.summary(performance.now());
  if (!s.units.length && !s.g.ohdFrames[1] && !s.g.ohdFrames[2]) return null;
  const prev = ohdShown;
  ohdShown = ohdShownUnit();
  if (prev !== ohdShown) ohdView.needKey = true, ohdView.waitSince = performance.now();
  for (const id of ohdFlight.keys()) if (!s.units.some((u) => u.id === id)) ohdFlight.delete(id);
  for (const u of s.units) {
    u.flight = ohdFlight.get(u.id)?.summary() ?? null;
    const d0 = ohdDepack.get(`${u.id}:0`), d1 = ohdDepack.get(`${u.id}:1`);
    u.rtp = [d0 ? { ...d0.st, codec: d0.codec } : null, d1 ? { ...d1.st, codec: d1.codec } : null];
  }
  const rec = ohdRec ? { video: ohdRec.video ? { size: ohdRec.video.size, aus: ohdRec.video.aus } : null, tlog: ohdRec.tlog ? { size: ohdRec.tlog.size, msgs: ohdRec.tlog.msgs } : null, ms: performance.now() - ohdRec.started } : null;
  return { ...s, shown: ohdShown, view: { sent: ohdView.sent, skipped: ohdView.skipped, forced: ohdView.forced, inFlight: ohdView.inFlight }, rec };
}
function ohdRecStop() {
  if (!ohdRec) return;
  const out = [];
  const v = ohdRec.video;
  if (v?.aus) {
    let done = false;
    if (v.format === 'mp4') {
      try {
        for (const f of mp4Files(v.codec, v.parts)) out.push({ kind: 'video', ext: 'mp4', codec: v.codec, unit: v.unit, parts: f.parts, frames: f.frames, ms: f.ms });
        done = out.length > 0;
      } catch (e) { out.push({ kind: 'note', text: `MP4 could not be written (${e.message}); saved the raw stream instead.` }); }
    }
    if (!done) out.push({ kind: 'video', ext: v.codec === 'h265' ? 'h265' : 'h264', codec: v.codec, unit: v.unit, parts: v.parts.map((a) => a.data) });
  }
  if (ohdRec.tlog?.msgs) out.push({ kind: 'tlog', parts: ohdRec.tlog.parts });
  ohdRec = null;
  self.postMessage({ t: 'ohd-rec-data', files: out });
}

function addMark(text, kind = 'info') { pendingMarks.push({ text, kind }); }

function store(h, meta, data, m = 0) {
  const n = data.length;
  if (wp + n > BYTES) wp = 0;
  // evict packets whose bytes we overwrite, and slots we reuse
  while (oldest < next && (next - oldest >= N || overlaps(oldest, wp, n))) evict();
  const i = next % N;
  col.ts[i] = meta.tsUs; col.hostMs[i] = Date.now(); col.off[i] = wp; col.origLen[i] = meta.origLen; col.capLen[i] = n;
  col.rxSeq[i] = meta.rxSeq; col.ch[i] = meta.channel; col.flags[i] = meta.flags; col.rssi[i] = meta.rssi; col.noise[i] = meta.noise;
  col.rate[i] = meta.rate; col.bb[i] = meta.bbFormat; col.gen[i] = h.cfgGen;
  col.m0[i] = m; col.rx[i] = 1 << m; col.rssiM[i * 8 + m] = meta.rssi;
  bytes.set(data, wp);
  wp += n;
  const abs = next++;
  if (firstTs === null) firstTs = meta.tsUs;
  if (pendingMarks.length) {
    markers.set(abs, pendingMarks);
    for (const m of pendingMarks) { const k = markerKeys.length; markerKeys.push({ before: abs, ...m }); order.push(~k); }
    pendingMarks = [];
  }
  if (!filterFn || safeMatch(abs)) order.push(abs);
  if (rec && !merger) recordPacket(abs); // with a merger: when the frame's copy window closes (all RSSIs known)
  return abs;
}

function overlaps(abs, start, len) {
  const i = abs % N, o = col.off[i], l = col.capLen[i];
  return o < start + len && start < o + l;
}

function evict() {
  oldest++;
  // drop leading order entries that point at evicted packets (and their markers)
  while (orderHead < order.length) {
    const v = order[orderHead];
    if (v >= 0 ? v < oldest : markerKeys[~v].before < oldest) orderHead++; else break;
  }
  if (orderHead > 65536) { order = order.slice(orderHead); orderHead = 0; }
}

function packetAt(abs) {
  const i = abs % N;
  return {
    meta: { tsUs: col.ts[i], channel: col.ch[i], flags: col.flags[i], rssi: col.rssi[i], noise: col.noise[i], rate: col.rate[i],
      bbFormat: col.bb[i], origLen: col.origLen[i], capLen: col.capLen[i], rxSeq: col.rxSeq[i] },
    data: bytes.subarray(col.off[i], col.off[i] + col.capLen[i]), cfgGen: col.gen[i], hostMs: col.hostMs[i],
  };
}

function copiesAt(abs) {
  const i = abs % N, out = [];
  for (let m = 0; m < 8; m++) if (col.rx[i] & (1 << m)) out.push({ id: m, rssi: col.rssiM[i * 8 + m], first: m === col.m0[i] });
  return out;
}

function safeMatch(abs) {
  try { const p = packetAt(abs); return filterFn({ meta: p.meta, data: W.frameBytes(p.meta, p.data) }); } catch { return false; }
}

// ---- ingest ---------------------------------------------------------------------------
function onFrame(x, h, body) {
  host.frames++; x.st.frames++;
  // in a group a gap on one dongle is usually covered by the others: counted per dongle, not marked
  const who = merger ? `${x.label}: ` : '';
  if (h.type === W.T.EVT_RESET) x.lastSeq = -1;
  if (x.lastSeq >= 0) {
    const gap = (h.seq - x.lastSeq - 1) & 0xffff;
    if (gap) { host.linkLost += gap; x.st.linkLost += gap; if (!merger) addMark(`${gap} frame${gap > 1 ? 's' : ''} lost on the USB link (host side)`, 'gap'); }
  }
  x.lastSeq = h.seq;
  if (h.type === W.T.PKT) {
    let p;
    try { p = W.decPkt(body); } catch { host.badPkt++; x.st.badPkt++; return; }
    x.st.pkts++;
    if (h.cfgGen !== x.lastGen) {
      if (x.lastGen >= 0 && (!merger || x.id === merger.ref)) addMark(cfgNotes.get(`${x.id}:${h.cfgGen}`) ?? `Config changed (cfg_gen ${h.cfgGen})`, 'cfg');
      x.lastGen = h.cfgGen;
      x.lastRxSeq = -1;
    }
    if (x.lastRxSeq >= 0) {
      const gap = (p.meta.rxSeq - x.lastRxSeq - 1) & 0xffff;
      if (gap && gap < 0x8000) { host.deviceGaps += gap; x.st.deviceGaps += gap; if (!merger) addMark(`Gap: ${gap} frame${gap > 1 ? 's' : ''} dropped on the dongle (USB too slow → lower snaplen or filter more)`, 'gap'); }
    }
    x.lastRxSeq = p.meta.rxSeq;
    if (!merger) {
      store(h, p.meta, p.data, x.id);
      hxPush(p.meta, p.data);
      ohdPush(p.meta, p.data);
      return;
    }
    if (!merger.members.has(x.id)) return; // not (yet / any more) in the group
    const r = merger.offer(x.id, p.meta, p.data, performance.now());
    if (r.dup === false) {
      const abs = store(h, { ...p.meta, tsUs: Math.round(r.groupTs) }, p.data, x.id);
      merger.bind(r.entry, abs);
      hxPush(p.meta, p.data); // live consumers get the first copy at once
      ohdPush(p.meta, p.data);
    } else if (r.dup && r.entry.abs !== null && r.entry.abs >= oldest) {
      const i = r.entry.abs % N;
      col.rx[i] |= 1 << x.id;
      col.rssiM[i * 8 + x.id] = p.meta.rssi;
    }
    drainMerger(false);
    return;
  }
  if (h.type === W.T.EVT_RESET) { addMark(`${who}Dongle restarted`, 'reset'); x.lastRxSeq = -1; }
  // everything else goes to protocol.js on the main thread
  self.postMessage({ t: 'frame', m: x.id, h, body }, [body.buffer]);
}

// ---- view rows (only what the list needs) -----------------------------------------------
function row(abs) {
  const p = packetAt(abs);
  const h = D.parse(W.frameBytes(p.meta, p.data));
  const m = p.meta;
  const legacy = !(m.flags & W.PKT.RATE_UNKNOWN) && m.bbFormat <= 1;
  const rate = m.flags & W.PKT.RATE_UNKNOWN ? W.bbName(m.bbFormat) : legacy ? `${m.rate / 2} Mb/s` : `MCS${m.rate} ${W.bbName(m.bbFormat)}`;
  let info = '';
  if (h.ssid !== undefined) info = h.ssid ? `SSID "${h.ssid}"` : 'SSID (hidden)';
  if (h.seq !== undefined) info += `${info ? ' · ' : ''}seq ${h.seq}`;
  const fl = D.flagsText(h);
  if (fl) info += ` · ${fl}`;
  if (h.truncated && m.capLen < m.origLen) info += ' · [truncated]';
  const oh = OHD.parseFrame(W.frameBytes(p.meta, p.data));
  if (oh) info = `OpenHD ${oh.uid === 1 ? 'air' : oh.uid === 2 ? 'ground' : `uid ${oh.uid}`} ${OHD.portName(oh.port)}${oh.enc ? ' (encrypted)' : ''} · nonce ${oh.nonce}`;
  return {
    n: abs + 1, t: (m.tsUs - (firstTs ?? m.tsUs)) / 1e6, tsUs: m.tsUs, hostMs: p.hostMs, ch: m.channel, ht40: !!(m.flags & W.PKT.HT40),
    rssi: m.rssi, rate, len: m.capLen < m.origLen ? `${m.capLen}/${m.origLen}` : `${m.origLen}`, trunc: m.capLen < m.origLen,
    type: h.name, cls: D.TYPE_CLASS[h.type], src: h.addr2 ?? '', dst: h.addr1 ?? '', bssid: h.bssid ?? '', info,
    fcsErr: !!(m.flags & W.PKT.FCS_ERR),
  };
}

function view(start, count) {
  const total = order.length - orderHead;
  if (start < 0) start = Math.max(0, total - count); // follow: last rows
  const rows = [];
  for (let k = start; k < Math.min(total, start + count); k++) {
    const v = order[orderHead + k];
    rows.push(v >= 0 ? row(v) : { marker: markerKeys[~v].text, kind: markerKeys[~v].kind });
  }
  return { total, start, rows, stored: next - oldest, received: next };
}

function detail(n) {
  const abs = n - 1;
  if (abs < oldest || abs >= next) return null;
  const p = packetAt(abs);
  return { n, meta: p.meta, data: p.data.slice(), cfgGen: p.cfgGen, hostMs: p.hostMs, rel: (p.meta.tsUs - (firstTs ?? 0)) / 1e6,
    copies: merger || col.rx[abs % N] !== 1 ? copiesAt(abs).map((c) => ({ ...c, label: mems.get(c.id)?.label ?? `Dongle ${c.id + 1}` })) : null };
}

function setFilter(expr) {
  try {
    filterFn = expr.trim() ? compileDisplayFilter(expr) : null;
  } catch (e) {
    return { ok: false, error: e.message };
  }
  order = []; orderHead = 0;
  const markAt = new Map();
  markerKeys.forEach((m, k) => { if (m.before >= oldest) (markAt.get(m.before) ?? markAt.set(m.before, []).get(m.before)).push(~k); });
  for (let a = oldest; a < next; a++) {
    const ms = markAt.get(a);
    if (ms) order.push(...ms);
    if (!filterFn || safeMatch(a)) order.push(a);
  }
  return { ok: true, shown: order.length - orderHead };
}

// ---- recording / export (pcapng) ----------------------------------------------------------
let rec = null; // {chunks:[], size, lastFlush}
function recordPacket(abs) {
  const p = packetAt(abs);
  if (rec.filtered && filterFn && !safeMatch(abs)) return;
  const ms = markers.get(abs);
  const comment = ms ? ms.map((m) => m.text).join('; ') : '';
  const ants = merger ? copiesAt(abs).map((c) => ({ ant: c.id, rssi: c.rssi })) : null;
  const b = P.epb(p.meta, p.data, comment, ants);
  rec.chunks.push(b); rec.size += b.length; rec.packets++;
  if (rec.size >= 1 << 20) flushRec();
}
function flushRec() {
  if (!rec || !rec.size) return;
  const out = new Uint8Array(rec.size);
  let o = 0;
  for (const c of rec.chunks) { out.set(c, o); o += c.length; }
  rec.chunks = []; rec.size = 0;
  self.postMessage({ t: 'recdata', buf: out }, [out.buffer]);
}

function exportBuffer(opts, filtered) {
  const parts = [P.header(opts)];
  let size = parts[0].length, count = 0;
  for (let a = oldest; a < next; a++) {
    if (filtered && filterFn && !safeMatch(a)) continue;
    const p = packetAt(a);
    const ms = markers.get(a);
    const i = a % N, multi = col.rx[i] & (col.rx[i] - 1); // heard by 2+ dongles
    const b = P.epb(p.meta, p.data, ms ? ms.map((m) => m.text).join('; ') : '', merger || multi ? copiesAt(a).map((c) => ({ ant: c.id, rssi: c.rssi })) : null);
    parts.push(b); size += b.length; count++;
  }
  const out = new Uint8Array(size);
  let o = 0;
  for (const c of parts) { out.set(c, o); o += c.length; }
  return { buf: out, count };
}

// ---- diversity summary (docs/14 §5) ----------------------------------------------------------
let prevMem = new Map();
function groupSummary() {
  const now = performance.now(), sum = merger?.summary() ?? null;
  const list = [...mems.values()].filter((x) => x.mac || x.st.frames).map((x) => {
    const p = prevMem.get(x.id) ?? { t: now, pkts: x.st.pkts, bytes: x.st.bytes }, dt = Math.max(0.001, (now - p.t) / 1000);
    prevMem.set(x.id, { t: now, pkts: x.st.pkts, bytes: x.st.bytes });
    const g = sum?.members.find((y) => y.id === x.id) ?? null;
    return { id: x.id, label: x.label, mac: x.mac, ...x.st, pps: (x.st.pkts - p.pkts) / dt, bps: (x.st.bytes - p.bytes) / dt, merge: g };
  });
  return sum ? { active: true, ...sum, members: list } : { active: false, members: list };
}

// ---- message loop -------------------------------------------------------------------------
let rateWin = { t: performance.now(), pkts: 0, bytes: 0 };
setInterval(() => {
  const now = performance.now(), dt = (now - rateWin.t) / 1000;
  const decoder = {};
  for (const x of mems.values()) for (const [k, v] of Object.entries(x.dec.stats)) if (typeof v === 'number') decoder[k] = (decoder[k] ?? 0) + v;
  const s = { ...host, decoder, stored: next - oldest, received: next, pps: (next - rateWin.pkts) / dt, bps: (host.bytesIn - rateWin.bytes) / dt,
    fill: (next - oldest) / N, recording: rec ? { packets: rec.packets } : null,
    fpv: hx.units.size || hx.wrongVersion ? { ...hx.summary(), shown: shownUnit(), skipped: fpvView.skipped } : null,
    ohd: ohdSummary(), group: groupSummary() };
  rateWin = { t: now, pkts: next, bytes: host.bytesIn };
  host.parseMs = 0;
  self.postMessage({ t: 'tick', s });
  if (rec && now - (rec.lastFlush ?? 0) > 1000) { rec.lastFlush = now; flushRec(); }
}, 500);

self.onmessage = (e) => {
  const m = e.data;
  switch (m.t) {
    case 'chunk': {
      const t0 = performance.now(), x = mem(m.m ?? 0);
      host.bytesIn += m.buf.length; x.st.bytes += m.buf.length;
      x.dec.feed(m.buf, (h, body) => onFrame(x, h, body));
      host.parseMs += performance.now() - t0;
      break;
    }
    case 'resync': {
      const x = mem(m.m ?? 0);
      x.dec.reset(); x.lastSeq = -1;
      if (m.overrun) { host.overruns++; x.st.overruns++; }
      if (m.mark) addMark(merger ? `${x.label}: ${m.mark}` : m.mark, 'reset');
      break;
    }
    case 'members': setMembers(m.list); break;
    case 'caps': self.postMessage({ t: 'caps', id: m.id, usb: 'usb' in navigator, serial: 'serial' in navigator }); break;
    case 'mark': addMark(m.text, m.kind ?? 'info'); break;
    case 'cfgnote': cfgNotes.set(`${m.m ?? 0}:${m.gen}`, m.text); if (cfgNotes.size > 300) cfgNotes.delete(cfgNotes.keys().next().value); break;
    case 'capture-start': captureEpoch++; for (const x of mems.values()) x.lastRxSeq = -1; break;
    case 'view': self.postMessage({ t: 'view', id: m.id, ...view(m.start, m.count) }); break;
    case 'detail': self.postMessage({ t: 'detail', id: m.id, d: detail(m.n) }); break;
    case 'filter': self.postMessage({ t: 'filter', id: m.id, ...setFilter(m.expr) }); break;
    case 'clear': merger?.flush(); alloc(N); firstTs = null; break;
    case 'fpv-view': fpvView.on = !!m.on; fpvView.unit = m.unit ?? 'auto'; break;
    case 'fpv-ack': fpvView.inFlight = Math.max(0, fpvView.inFlight - 1); break;
    case 'fpv-reset': hx.reset(); fpvView.inFlight = 0; fpvView.skipped = 0; break;
    case 'ohd-view':
      Object.assign(ohdView, { on: !!m.on, unit: m.unit ?? 'auto', cam: m.cam ?? 0 });
      if (m.codec && m.codec !== ohdView.codec) { ohdView.codec = m.codec; for (const d of ohdDepack.values()) d.setCodec(m.codec); }
      ohdShown = ohdShownUnit(); ohdView.needKey = true; ohdView.waitSince = performance.now(); ohdView.inFlight = 0;
      break;
    case 'ohd-ack': ohdView.inFlight = Math.max(0, ohdView.inFlight - 1); break;
    case 'ohd-need-key': ohdView.needKey = true; ohdView.waitSince = performance.now(); break;
    case 'ohd-keys': ohd.setKeys(m.keys); ohdFlight.clear(); ohdDepack.clear(); break;
    case 'ohd-options': ohdOpts = m.opts; applyOhdOpts(); break;
    case 'ohd-reset': ohd.reset(); ohdFlight.clear(); ohdDepack.clear(); ohdView.needKey = true; ohdView.inFlight = 0; ohdView.sent = ohdView.skipped = ohdView.forced = 0; break;
    case 'ohd-rec-start': {
      const unit = ohdShownUnit();
      ohdRec = { started: performance.now(), video: m.video && unit !== null ? { unit, cam: ohdView.cam, codec: null, format: m.format === 'raw' ? 'raw' : 'mp4', parts: [], size: 0, aus: 0 } : null, tlog: m.tlog ? { parts: [], size: 0, msgs: 0 } : null };
      break;
    }
    case 'ohd-rec-stop': ohdRecStop(); break;
    case 'store-size': merger?.flush(); alloc(Math.max(10000, Math.min(2000000, m.n | 0))); firstTs = null; break;
    case 'rec-start': rec = { chunks: [P.header(m.opts)], size: 0, packets: 0, filtered: !!m.filtered }; rec.size = rec.chunks[0].length; break;
    case 'rec-stop':
      if (rec && merger) for (let k = merger.qHead; k < merger.queue.length; k++) { const e = merger.queue[k]; if (e.abs !== null && e.abs >= oldest) recordPacket(e.abs); } // still open: record now
      flushRec(); self.postMessage({ t: 'rec-stopped', packets: rec?.packets ?? 0 }); rec = null; break;
    case 'export': { const r = exportBuffer(m.opts, m.filtered); self.postMessage({ t: 'export', id: m.id, buf: r.buf, count: r.count }, [r.buf.buffer]); break; }
    default: break;
  }
};

self.onerror = (e) => { self.postMessage({ t: 'error', message: String(e.message ?? e) }); };
