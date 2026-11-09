// Passive OpenHD receiver (docs/13): 802.11 fingerprint → session key →
// auth/decrypt → Gen A/B framing → FEC (video) / dedupe (telemetry) /
// management. Receive-only. Air units are told apart by their session key
// (random per boot); several OpenHD systems on one channel work as long as
// their bind phrase is known.
//
// Also: Sender (demo mode and tests), capture filter preset, channel helpers.
import * as C from './ohdcrypto.js';
import * as FEC from './ohdfec.js';

export const UID = Object.freeze({ AIR: 1, GND: 2 });
export const PORT = Object.freeze({ TELE_GND: 3, TELE_AIR: 4, VIDEO0: 10, VIDEO1: 11, MGMT_AIR: 20, MGMT_GND: 21, AUDIO: 30, SESSION: 127 });
const VIDEO_PORTS = new Set([PORT.VIDEO0, PORT.VIDEO1]);
const EXPECTED_TYPE = new Map([[PORT.VIDEO0, 0], [PORT.VIDEO1, 0], [PORT.TELE_AIR, 1], [PORT.TELE_GND, 1]]);
const NO_FRAMING = new Set([PORT.MGMT_AIR, PORT.MGMT_GND, PORT.SESSION]);

export const hexToBytes = (s) => Uint8Array.from(s.replace(/\s/g, '').match(/../g) ?? [], (x) => parseInt(x, 16));
export const bytesToHex = (b) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

/** beforenm keys of the default bind phrase "openhd" (docs/13 §4.6). Public: the phrase is in OpenHD's source. */
export const DEFAULT_KEYS = Object.freeze({
  air: '978a890d4b79f33f700fcdb7e4b91be6d6eb903a6638d2381d9b8e3ea9b20125',
  gnd: '5be985599ab9271ee02aef52532bf4f5bd83eba8915934373f0ef1468a33f003',
});

/** OpenHD 5.8 GHz "race band" and the 2.4 GHz default (docs/13 §3), as Wi-Fi channels. */
export const RACE_BAND_MHZ = [5700, 5745, 5785, 5825, 5865, 5260, 5280];
export const DEFAULT_FREQ = 5745;
export function freqToChannel(mhz) {
  if (mhz === 2484) return 14;
  if (mhz >= 2412 && mhz <= 2472 && (mhz - 2407) % 5 === 0) return (mhz - 2407) / 5;
  if (mhz >= 5160 && mhz <= 5885 && (mhz - 5000) % 5 === 0) return (mhz - 5000) / 5;
  return 0;
}

const PORT_NAMES = { 3: 'telemetry', 4: 'telemetry', 10: 'video', 11: 'video 2', 20: 'management', 21: 'management', 30: 'audio', 127: 'session key' };
export const portName = (p) => PORT_NAMES[p] ?? `port ${p}`;

/** HT40 side OpenHD uses on a channel (ohd_interface wifi_channel.h in_40Mhz_ht40_plus): true = HT40+. */
export function ht40Plus(ch) {
  if (ch <= 14) return ch === 1 || ch === 9;
  if (ch >= 149) return ((ch - 149) / 4) % 2 === 0;
  return ((ch - 36) / 4) % 2 === 0;
}

/** 802.11 fingerprint (§4.1). f = frame without FCS. */
export function parseFrame(f) {
  if (f.length < 25 || f[0] !== 0x08 || f[1] !== 0x01) return null;
  for (let i = 4; i < 10; i++) if (f[i] !== 0xff) return null;
  if (f[10] !== f[16] || f[15] !== f[21]) return null;
  const n = new DataView(f.buffer, f.byteOffset);
  const nonce = BigInt(n.getUint32(11, true)) | (BigInt(n.getUint32(17, true)) << 32n);
  return { uid: f[10], enc: f[15] & 1, port: f[15] >> 1, nonce, payload: f.subarray(24) };
}

/** Gen B WBPacketHeader heuristic (§2). */
export function looksGenB(port, body) {
  const t = EXPECTED_TYPE.get(port);
  return t !== undefined && body.length >= 8 && (body[0] === 0 || body[0] === 1) && body[1] === t && (body[6] | (body[7] << 8)) === body.length;
}

const u16 = (b, o) => b[o] | (b[o + 1] << 8);
const u32 = (b, o) => (b[o] | (b[o + 1] << 8) | (b[o + 2] << 16) | (b[o + 3] << 24)) >>> 0;

// ---------------------------------------------------------------------------------------------
/**
 * One FEC video stream (§5.4). Primaries are forwarded in order, only from the
 * oldest open block, as soon as they are available (no wait on a clean link).
 * Q = max open blocks (OpenHD ground default 1).
 */
export class FecStream {
  constructor({ out, queue = 1, forwardGapped = false }) {
    this.out = out; this.Q = queue; this.forwardGapped = forwardGapped;
    this.reset();
    this.st = { fragments: 0, primaries: 0, secondaries: 0, secUnneeded: 0, duplicates: 0, late: 0, bad: 0, recovered: 0, blocksOk: 0, blocksFec: 0, blocksLost: 0, primariesLost: 0, restarts: 0 };
  }
  reset() { this.blocks = new Map(); this.lastClosed = -1; this.gap = false; }
  oldest() { let o = null; for (const b of this.blocks.values()) if (!o || b.idx < o.idx) o = b; return o; }

  push(body) {
    const st = this.st;
    if (body.length < 8) { st.bad++; return; }
    const blk = u32(body, 0), fi = body[4], k = body[5];
    if (k === 0 || k > 128 || fi === 255) { st.bad++; return; }
    st.fragments++;
    if (fi < k) st.primaries++; else st.secondaries++;
    if (blk <= this.lastClosed) {
      if (this.lastClosed - blk > 1000) { st.restarts++; this.reset(); } // sender restarted without a new session
      else { if (fi >= k) st.secUnneeded++; else st.late++; return; }
    }
    let b = this.blocks.get(blk);
    if (!b) { b = { idx: blk, k, prim: new Map(), sec: new Map(), fwd: 0, fec: false }; this.blocks.set(blk, b); }
    if (b.k !== k) { st.bad++; return; }
    const sym = body.subarray(6);
    if (fi < k) {
      if (b.prim.has(fi)) { st.duplicates++; return; }
      if (u16(body, 6) + 2 > sym.length) { st.bad++; return; }
      b.prim.set(fi, sym);
    } else {
      if (b.sec.has(fi - k)) { st.duplicates++; return; }
      if (b.prim.size === k) { st.secUnneeded++; return; }
      b.sec.set(fi - k, sym);
    }
    if (b.prim.size < k && b.prim.size + b.sec.size >= k) this.recover(b);
    if (b.prim.size === k) for (const o of [...this.blocks.values()].filter((x) => x.idx < b.idx).sort((x, y) => x.idx - y.idx)) this.giveUp(o); // a newer block is complete
    this.drain();
    while (this.blocks.size > this.Q) { this.giveUp(this.oldest()); this.drain(); }
  }
  emit(b, j) {
    const s = b.prim.get(j);
    this.out(s.subarray(2, 2 + u16(s, 0)), { block: b.idx, recovered: !!s.recovered, gap: this.gap });
    this.gap = false;
  }
  /** Forward what the oldest blocks have in order; finish complete ones. */
  drain() {
    for (let o = this.oldest(); o; o = this.oldest()) {
      while (o.fwd < o.k && o.prim.has(o.fwd)) this.emit(o, o.fwd++);
      if (o.fwd < o.k) return;
      if (o.fec) this.st.blocksFec++; else this.st.blocksOk++;
      this.blocks.delete(o.idx);
      this.lastClosed = Math.max(this.lastClosed, o.idx);
    }
  }
  recover(b) {
    let L = 0;
    for (const s of b.sec.values()) L = Math.max(L, s.length);
    let rec;
    try { rec = FEC.decode(b.k, b.prim, b.sec, L); } catch { this.st.bad++; return; }
    for (const s of rec.values()) if (u16(s, 0) + 2 > L) { this.st.bad++; return; }
    for (const [j, s] of rec) { s.recovered = true; b.prim.set(j, s); this.st.recovered++; }
    b.fec = true;
  }
  giveUp(b) {
    // what is in order was forwarded already (b was not the oldest, or drain stopped at a gap)
    if (b === this.oldest()) while (b.fwd < b.k && b.prim.has(b.fwd)) this.emit(b, b.fwd++);
    this.blocks.delete(b.idx);
    this.lastClosed = Math.max(this.lastClosed, b.idx);
    if (b.prim.size === b.k && b.fwd === b.k) { if (b.fec) this.st.blocksFec++; else this.st.blocksOk++; return; }
    this.st.blocksLost++;
    this.st.primariesLost += b.k - b.prim.size;
    this.gap = true;
    if (this.forwardGapped) for (let j = b.fwd; j < b.k; j++) if (b.prim.has(j)) this.emit(b, j); else this.gap = true;
  }
}

/** Drops repeated FECDisabled sequence numbers (§5.1), remembers the last 100. */
class Dedupe {
  constructor() { this.set = new Set(); this.ring = []; }
  seen(seq) {
    if (this.set.has(seq)) return true;
    this.set.add(seq); this.ring.push(seq);
    if (this.ring.length > 100) this.set.delete(this.ring.shift());
    return false;
  }
}

// ---------------------------------------------------------------------------------------------
let nextUnitId = 1;
/**
 * Receiver. Callbacks get the unit object (see summary() for its public fields).
 *   onRtp(unit, cam, rtpPacket, info)      cam 0/1, info {block, recovered, gap}
 *   onTelemetry(unit, data, dir)            data = aggregated MAVLink frames (dir 1 air→gnd, 2 gnd→air)
 *   onMgmt(unit, {freq, bw})
 *   onSession(unit, kind)                   kind 'new' | 'restart'
 */
export class Receiver {
  constructor({ keys = DEFAULT_KEYS, onRtp, onTelemetry, onMgmt, onSession, now = () => Date.now(), queue = 1, forwardGapped = false, unverified = false } = {}) {
    this.cb = { onRtp, onTelemetry, onMgmt, onSession };
    this.now = now;
    this.opts = { queue, forwardGapped, unverified };
    this.setKeys(keys);
    this.reset();
  }
  setKeys(keys) {
    const k = (v) => (v ? (typeof v === 'string' ? hexToBytes(v) : v) : null);
    this.K = { 1: k(keys?.air), 2: k(keys?.gnd) };
    this.reset();
  }
  setOptions(o) {
    Object.assign(this.opts, o);
    for (const u of this.units.values()) for (const s of u.fec.values()) { s.Q = this.opts.queue; s.forwardGapped = this.opts.forwardGapped; }
  }
  reset() {
    this.units = new Map();
    this.g = { frames: 0, foreign: 0, ohdFrames: { 1: 0, 2: 0 }, sessionPackets: { 1: 0, 2: 0 }, badSession: { 1: 0, 2: 0 }, noSession: 0, authFail: 0, ports: {}, lastOhd: 0, unverified: 0 };
  }

  newUnit(dir, key, sessPkt) {
    const now = this.now();
    const u = {
      id: nextUnitId++, dir, key, sessPkt, created: now, last: now, nonceLast: null, gen: null, genMiss: 0,
      fec: new Map(), dedupe: new Map(), mgmt: null, rssi: null, channel: null, verified: !!key,
      st: { packets: 0, bytes: 0, nonceGaps: 0, nonceSpan: 0, video: [0, 0], videoBytes: [0, 0], telemetry: 0, teleDup: 0, mgmt: 0, audio: 0, retransmitted: 0, arqRequests: 0, restarts: 0, authFail: 0, ports: {} },
      prevSum: null,
    };
    this.units.set(u.id, u);
    return u;
  }
  resetStreams(u) { u.fec.clear(); u.dedupe.clear(); u.gen = null; u.genMiss = 0; u.nonceLast = null; }

  /** One captured 802.11 frame (no FCS). meta: {rssi, channel}. Returns true if it was an OpenHD frame. */
  push(f, meta = {}) {
    this.g.frames++;
    const h = parseFrame(f);
    if (!h || (h.uid !== UID.AIR && h.uid !== UID.GND)) { this.g.foreign++; return false; }
    const dir = h.uid, now = this.now();
    this.g.ohdFrames[dir]++;
    this.g.lastOhd = now;
    this.g.ports[`${dir}:${h.port}`] = (this.g.ports[`${dir}:${h.port}`] ?? 0) + 1;
    if (h.port === PORT.SESSION) { this.session(dir, h, meta, now); return true; }

    // find the unit whose session key verifies this packet (closest nonce first)
    const cands = [...this.units.values()].filter((u) => u.dir === dir && u.key);
    if (cands.length > 1) cands.sort((a, b) => dist(a, h.nonce) - dist(b, h.nonce));
    let unit = null, body = null;
    for (const u of cands) {
      body = h.enc ? C.aeadOpen(u.key, h.nonce, h.payload) : C.authOpen(u.key, h.nonce, h.payload);
      if (body) { unit = u; break; }
    }
    if (!unit) {
      if (cands.length) { this.g.authFail++; return true; }
      if (this.opts.unverified && !h.enc && VIDEO_PORTS.has(h.port) && h.payload.length > 16) {
        unit = [...this.units.values()].find((u) => u.dir === dir && !u.key) ?? this.newUnit(dir, null, null);
        body = h.payload.subarray(0, h.payload.length - 16);
        this.g.unverified++;
      } else { this.g.noSession++; return true; }
    }
    this.track(unit, h, f.length, meta, now);
    this.payload(unit, h, body);
    return true;
  }

  session(dir, h, meta, now) {
    this.g.sessionPackets[dir]++;
    const p = h.payload;
    if (h.enc || p.length !== 72) { this.g.badSession[dir]++; return; }
    for (const u of this.units.values()) {
      if (u.dir === dir && u.sessPkt && C.equal(u.sessPkt, p)) { u.last = now; this.trackRf(u, meta); return; }
    }
    if (!this.K[dir]) return;
    const key = C.secretboxOpen(p.subarray(24), p.subarray(0, 24), this.K[dir]);
    if (!key) { this.g.badSession[dir]++; return; }
    let u = [...this.units.values()].find((x) => x.dir === dir && x.key && C.equal(x.key, key));
    if (u) { u.sessPkt = Uint8Array.from(p); u.last = now; return; }
    // a new session: the same unit restarted (old one silent), or another OpenHD system
    u = [...this.units.values()].filter((x) => x.dir === dir && x.key && now - x.last > 500).sort((a, b) => b.last - a.last)[0];
    if (u) {
      u.key = key; u.sessPkt = Uint8Array.from(p); u.last = now; u.st.restarts++;
      this.resetStreams(u);
      this.cb.onSession?.(u, 'restart');
    } else {
      u = this.newUnit(dir, key, Uint8Array.from(p));
      this.cb.onSession?.(u, 'new');
    }
    // a verified session replaces the "unverified" pseudo unit of that direction
    for (const x of this.units.values()) if (x.dir === dir && !x.key) this.units.delete(x.id);
    this.trackRf(u, meta);
  }

  track(u, h, len, meta, now) {
    const st = u.st;
    st.packets++; st.bytes += len;
    st.ports[h.port] = (st.ports[h.port] ?? 0) + 1;
    if (u.nonceLast !== null) {
      const d = h.nonce - u.nonceLast;
      if (d > 0n && d < 100000n) { st.nonceSpan += Number(d); if (d > 1n) st.nonceGaps += Number(d - 1n); }
    } else st.nonceSpan++;
    if (u.nonceLast === null || h.nonce > u.nonceLast) u.nonceLast = h.nonce;
    u.last = now;
    this.trackRf(u, meta);
  }
  trackRf(u, meta) {
    if (meta.rssi !== undefined && meta.rssi !== null) u.rssi = u.rssi === null ? meta.rssi : Math.round(0.8 * u.rssi + 0.2 * meta.rssi);
    if (meta.channel) u.channel = meta.channel;
  }

  payload(u, h, body) {
    const port = h.port;
    if (port === PORT.MGMT_AIR || port === PORT.MGMT_GND) {
      u.st.mgmt++;
      if (port === PORT.MGMT_AIR && body.length >= 6 && body[0] === 0) {
        u.mgmt = { freq: u32(body, 1), bw: body[5] };
        this.cb.onMgmt?.(u, u.mgmt);
      }
      return;
    }
    if (!NO_FRAMING.has(port)) {
      if (u.gen === null && EXPECTED_TYPE.has(port)) u.gen = looksGenB(port, body) ? 'B' : 'A';
      if (u.gen === 'B') {
        if (body.length < 8 || (body[6] | (body[7] << 8)) !== body.length) {
          if (++u.genMiss >= 5) { u.gen = null; u.genMiss = 0; } // re-detect
          return;
        }
        u.genMiss = 0;
        if (body[0] & 1) u.st.retransmitted++;
        if (body[1] === 3) { u.st.arqRequests++; return; }
        body = body.subarray(8);
      } else if (u.gen === null) {
        if (port !== PORT.AUDIO) return; // framing unknown yet (only audio before any video/telemetry)
      }
    }
    if (VIDEO_PORTS.has(port)) {
      const cam = port - PORT.VIDEO0;
      let s = u.fec.get(port);
      if (!s) {
        s = new FecStream({ queue: this.opts.queue, forwardGapped: this.opts.forwardGapped, out: (pkt, info) => {
          u.st.video[cam]++; u.st.videoBytes[cam] += pkt.length;
          this.cb.onRtp?.(u, cam, pkt, info);
        } });
        u.fec.set(port, s);
      }
      s.push(body);
      return;
    }
    if (port === PORT.TELE_AIR || port === PORT.TELE_GND || port === PORT.AUDIO) {
      if (body.length < 9) return;
      const seq = u32(body, 0) + u32(body, 4) * 4294967296;
      let d = u.dedupe.get(port);
      if (!d) { d = new Dedupe(); u.dedupe.set(port, d); }
      if (d.seen(seq)) { u.st.teleDup++; return; }
      if (port === PORT.AUDIO) { u.st.audio++; return; }
      u.st.telemetry++;
      this.cb.onTelemetry?.(u, body.subarray(8), u.dir);
    }
  }

  /** Plain-object snapshot for the UI. */
  summary(now = this.now()) {
    const units = [];
    for (const u of this.units.values()) {
      if (now - u.last > 15000) { this.units.delete(u.id); continue; }
      const p = u.prevSum, st = u.st;
      let loss = null;
      if (p && st.nonceSpan > p.span) loss = Math.max(0, Math.min(1, (st.nonceGaps - p.gaps) / (st.nonceSpan - p.span)));
      u.prevSum = { span: st.nonceSpan, gaps: st.nonceGaps };
      const fec = {};
      for (const [port, s] of u.fec) fec[port - PORT.VIDEO0] = { ...s.st, queue: s.Q };
      units.push({ id: u.id, dir: u.dir, verified: u.verified, gen: u.gen, ageMs: now - u.last, upMs: now - u.created, rssi: u.rssi, channel: u.channel,
        mgmt: u.mgmt, st: { ...st, video: [...st.video], videoBytes: [...st.videoBytes], ports: { ...st.ports } }, loss, fec,
        sessionId: u.sessPkt ? bytesToHex(u.sessPkt.subarray(0, 4)) : null });
    }
    return { units, g: { ...this.g, ohdFrames: { ...this.g.ohdFrames }, sessionPackets: { ...this.g.sessionPackets }, badSession: { ...this.g.badSession }, ports: { ...this.g.ports } }, keys: { air: !!this.K[1], gnd: !!this.K[2] } };
  }
}
const dist = (u, n) => (u.nonceLast === null ? Number.MAX_SAFE_INTEGER : Math.abs(Number(n - u.nonceLast)));

// ---------------------------------------------------------------------------------------------
/** One transmitting OpenHD instance (demo mode, tests). rng() → [0, 1). */
export class Sender {
  constructor({ K = DEFAULT_KEYS.air, uid = UID.AIR, nonce = 0x10000000n, rng = Math.random, genB = false } = {}) {
    this.K = typeof K === 'string' ? hexToBytes(K) : K;
    this.uid = uid; this.nonce = BigInt(nonce); this.rng = rng; this.genB = genB;
    const rb = (n) => Uint8Array.from({ length: n }, () => Math.floor(rng() * 256));
    this.sessionKey = rb(32);
    const n24 = rb(24);
    this.sessionPacket = new Uint8Array(72);
    this.sessionPacket.set(n24); this.sessionPacket.set(C.secretboxSeal(this.sessionKey, n24, this.K), 24);
    this.block = new Map(); this.seq = new Map(); this.bidx = new Map();
  }
  header(port, enc) {
    const f = new Uint8Array(24), n = this.nonce++;
    f[0] = 0x08; f[1] = 0x01; f.fill(0xff, 4, 10);
    const dv = new DataView(f.buffer);
    f[10] = f[16] = this.uid;
    dv.setUint32(11, Number(n & 0xffffffffn), true); dv.setUint32(17, Number(n >> 32n), true);
    f[15] = f[21] = (port << 1) | (enc ? 1 : 0);
    return { f, n };
  }
  frame(port, enc, body) {
    const { f, n } = this.header(port, enc);
    const p = enc ? C.aeadSeal(this.sessionKey, n, body) : C.authSeal(this.sessionKey, n, body);
    const out = new Uint8Array(24 + p.length);
    out.set(f); out.set(p, 24);
    return out;
  }
  session() { const { f } = this.header(PORT.SESSION, false); const out = new Uint8Array(96); out.set(f); out.set(this.sessionPacket, 24); return out; }
  genBHdr(port, type, d) {
    if (!this.genB) return d;
    const key = port * 256 + type, i = this.bidx.get(key) ?? 0;
    this.bidx.set(key, i + 1);
    const o = new Uint8Array(8 + d.length), dv = new DataView(o.buffer);
    o[1] = type; dv.setUint32(2, i, true); dv.setUint16(6, o.length, true); o.set(d, 8);
    return o;
  }
  /** One FEC block from RTP packets (each ≤ 1441 B). m secondaries. Returns frames in send order. */
  video(packets, { cam = 0, m = Math.max(1, Math.round(packets.length * 0.2)), enc = false } = {}) {
    const port = PORT.VIDEO0 + cam, blk = this.block.get(port) ?? 0, k = packets.length;
    this.block.set(port, blk + 1);
    const hdr = (fi, size) => { const h = new Uint8Array(8), dv = new DataView(h.buffer); dv.setUint32(0, blk, true); h[4] = fi; h[5] = k; dv.setUint16(6, size, true); return h; };
    const out = [], syms = [];
    let L = 0;
    packets.forEach((p, j) => {
      const b = new Uint8Array(8 + p.length);
      b.set(hdr(j, p.length)); b.set(p, 8);
      syms.push(b.subarray(6));
      L = Math.max(L, p.length + 2);
      out.push(this.frame(port, enc, this.genBHdr(port, 0, b)));
    });
    FEC.encode(syms, m, L).forEach((s, r) => {
      const b = new Uint8Array(6 + L);
      b.set(hdr(k + r, 0).subarray(0, 6)); b.set(s, 6);
      out.push(this.frame(port, enc, this.genBHdr(port, 0, b)));
    });
    return out;
  }
  /** Telemetry (FECDisabled), encrypted, injected `n` times. */
  telemetry(data, { n = 2, port = this.uid === UID.AIR ? PORT.TELE_AIR : PORT.TELE_GND } = {}) {
    const s = this.seq.get(port) ?? 0;
    this.seq.set(port, s + 1);
    const b = new Uint8Array(8 + data.length);
    new DataView(b.buffer).setUint32(0, s, true);
    b.set(data, 8);
    return Array.from({ length: n }, () => this.frame(port, true, this.genBHdr(port, 1, b)));
  }
  mgmt(freq, bw) {
    const b = new Uint8Array(6);
    new DataView(b.buffer).setUint32(1, freq, true); b[5] = bw;
    return this.frame(PORT.MGMT_AIR, true, b);
  }
}

// ---------------------------------------------------------------------------------------------
/**
 * Capture filter preset (docs/13 §10.1): OpenHD frames from the air unit
 * (session, video 10/11 plain or encrypted, telemetry, management, audio),
 * optionally the ground unit's session/telemetry/management.
 */
export function captureFilter({ ground = false } = {}) {
  const rule = (name, uid, port, pmask) => ({
    name: `OpenHD ${name}`, enabled: true,
    terms: [
      { offset: 0, value: '08 01', negate: false },
      { offset: 4, value: `ff ff ff ff ff ff 0${uid}`, negate: false },
      { offset: 15, value: `${port} 0${uid}`, mask: `${pmask} ff`, negate: false },
    ],
  });
  const rules = [
    rule('session (air)', 1, 'fe', 'ff'), rule('video (air)', 1, '14', 'fc'), rule('telemetry (air)', 1, '08', 'fe'),
    rule('management (air)', 1, '28', 'fe'), rule('audio (air)', 1, '3c', 'fe'),
  ];
  if (ground) rules.push(rule('session (ground)', 2, 'fe', 'ff'), rule('telemetry (ground)', 2, '06', 'fe'), rule('management (ground)', 2, '2a', 'fe'));
  return { version: 1, rules, global: { drop_fcs_err: true, rssi_min: null, len_min: null, len_max: null } };
}
export const isOhdFilter = (f) => !!f?.rules?.some((r) => r.name?.startsWith('OpenHD '));
