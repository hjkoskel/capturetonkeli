// Multi-dongle receive diversity, host side (docs/14 §4): merges the PKT streams of
// several dongles on the same channel into one. First copy wins (emitted at once,
// no added latency); later copies of the same air frame only add their RSSI.
//
// Dedup key: CRC-32 over the captured bytes + orig_len + channel (docs/14 §4.3 fallback
// key; the ESP32-C5 delivers no FCS, docs/03 rev 3, so the FCS itself is not available).
// Two copies with the same key are the same air frame only when their times, mapped to
// the reference dongle's clock, agree within a tolerance:
//   - rough (always available): each dongle's offset to the host clock from the lower
//     envelope of (host arrival − device time); tolerance 10 ms (USB delivery jitter);
//   - fine (after lock): t_ref = a·t + b fitted over anchors (frames heard by both, not
//     control frames); tolerance from the fit residual (≥ 20 µs).
// The reference is the dongle with the lowest MAC (docs/14 §4.1).
//
// Copies with a bad FCS (fcs_err) never match by key. They are attached to a good copy
// of the same length and time that differs in ≤ 16 bits (they then only add RSSI), else
// held up to HOLD_MS for a good copy to arrive, then emitted as they are. Repairing
// frames from two bad copies (§4.5) needs the received FCS, which the C5 does not deliver.
import { crc32 } from './codec.js';

export const MAX_MEMBERS = 8;
const WINDOW_MS = 100;               // a frame's copies must arrive within this (host time)
export const HOLD_MS = 15;           // bad copies wait this long for a good one (USB batching skew)
const MAX_BIT_DIFF = 16;
const UNLOCKED_TOL_US = 10000;      // ≥ USB delivery jitter between dongles (rough offsets)
const ENVELOPE_SLACK = 200e-6;       // rough offset may rise 200 µs/s (drift, and recovery)
const ANCHOR_SPAN_US = 2e6, MAX_ANCHORS = 400, LOCK_MIN = 8, LOCK_RMS_US = 50;

/** Maps one dongle's device time (µs) to the reference dongle's. */
export class Clock {
  constructor() { this.reset(); this.env = null; this.envT = 0; }
  reset() { this.anchors = []; this.a = 1; this.b = 0; this.rms = Infinity; this.locked = false; this.used = 0; this.sinceFit = 0; }
  /** Rough: lower envelope of host − device time. */
  observe(tsUs, hostUs) {
    const s = hostUs - tsUs;
    this.env = this.env === null ? s : Math.min(this.env + ENVELOPE_SLACK * Math.max(0, hostUs - this.envT), s);
    this.envT = hostUs;
  }
  /** Fine: one frame heard by this dongle at x and by the reference at y. */
  anchor(x, y) {
    const A = this.anchors;
    A.push([x, y]);
    while (A.length > MAX_ANCHORS || (A.length && x - A[0][0] > ANCHOR_SPAN_US)) A.shift();
    if (++this.sinceFit >= (this.locked ? 16 : 2)) this.fit();
  }
  fit() {
    this.sinceFit = 0;
    let pts = this.anchors;
    if (pts.length < 2) return;
    // robust start: drop pairs far from the median offset (a stale or wrong pair must not tilt the fit)
    const off = pts.map(([x, y]) => y - x).sort((p, q) => p - q), med = off[off.length >> 1];
    pts = pts.filter(([x, y]) => Math.abs(y - x - med) < 2000);
    if (pts.length < 2) return;
    for (let pass = 0; pass < 2; pass++) {
      const n = pts.length;
      let mx = 0, my = 0;
      for (const [x, y] of pts) { mx += x; my += y; }
      mx /= n; my /= n;
      let sxx = 0, sxy = 0;
      for (const [x, y] of pts) { sxx += (x - mx) ** 2; sxy += (x - mx) * (y - my); }
      // with less than 0.2 s of anchors the slope is noise: offset only
      const a = sxx > n * 1e10 ? sxy / sxx : 1, b = my - a * mx;
      let ss = 0;
      const res = pts.map(([x, y]) => y - (a * x + b));
      for (const r of res) ss += r * r;
      const rms = Math.sqrt(ss / n);
      this.a = a; this.b = b; this.rms = rms; this.used = n;
      const lim = Math.max(50, 3 * rms);
      const keep = pts.filter((_, i) => Math.abs(res[i]) <= lim);
      if (keep.length === n || keep.length < 2) break;
      pts = keep;
    }
    this.locked = this.used >= LOCK_MIN && this.rms < LOCK_RMS_US;
  }
  fine(ts) { return this.a * ts + this.b; }
  /** This dongle's clock rate error vs the reference (+ = runs fast). */
  get ppm() { return (1 / this.a - 1) * 1e6; }
  tolUs() { return this.locked ? Math.min(500, Math.max(20, 4 * this.rms + 10)) : UNLOCKED_TOL_US; }
}

const isAnchor = (meta, data) => !(meta.flags & 1) && data.length >= 24 && ((data[0] >> 2) & 3) !== 1;
const popcount = (v) => { let c = 0; for (; v; v &= v - 1) c++; return c; };
const BITS = Uint8Array.from({ length: 256 }, (_, i) => popcount(i));
function bitDiff(a, b, max) {
  let d = 0;
  for (let i = 0; i < a.length; i++) { const x = a[i] ^ b[i]; if (x && (d += BITS[x]) > max) return d; }
  return d;
}

export class Merger {
  /** onFinal(entry): called when a frame's copy window closes (deferred recording, stats). */
  constructor({ windowMs = WINDOW_MS, onFinal = null } = {}) {
    this.windowMs = windowMs; this.onFinal = onFinal;
    this.members = new Map(); // id → { mac, clock, st }
    this.ref = null;
    this.table = new Map();   // key → [entry]
    this.byLen = new Map();   // channel/len → [entry] (good entries, for bad copies)
    this.held = [];           // bad copies waiting for a good one
    this.queue = []; this.qHead = 0;
    this.st = { out: 0, dups: 0, unique: 0, rescued: 0, badOut: 0 };
  }

  /** Active members: [{id, mac: 'aa:bb:..'}]. The lowest MAC is the clock reference. */
  setMembers(list) {
    const ids = new Set(list.map((x) => x.id));
    for (const id of this.members.keys()) if (!ids.has(id)) this.members.delete(id);
    for (const { id, mac } of list) {
      if (id < 0 || id >= MAX_MEMBERS) throw new Error(`member id ${id} out of range`);
      if (!this.members.has(id)) this.members.set(id, { mac, clock: new Clock(), st: { rx: 0, first: 0, unique: 0, copies: 0, bad: 0 } });
      else this.members.get(id).mac = mac;
    }
    const ref = [...this.members.entries()].sort((a, b) => (a[1].mac < b[1].mac ? -1 : 1))[0]?.[0] ?? null;
    if (ref !== this.ref) { this.ref = ref; for (const m of this.members.values()) m.clock.reset(); }
  }
  get size() { return this.members.size; }

  /** Times of a copy: rough = estimated host time (µs, always), fine = reference clock (null until locked). */
  times(id, tsUs) {
    const m = this.members.get(id);
    const fine = id === this.ref ? tsUs : m.clock.locked ? m.clock.fine(tsUs) : null;
    return { rough: tsUs + m.clock.env, fine };
  }
  /** Group time stored for a frame: the reference clock, estimated via the host clock until locked. */
  groupTs(t) {
    if (t.fine !== null) return t.fine;
    const env = this.members.get(this.ref)?.clock.env;
    return env === null || env === undefined ? t.rough : t.rough - env;
  }
  tol(id) { return id === this.ref ? 20 : this.members.get(id)?.clock.tolUs() ?? UNLOCKED_TOL_US; } // a member may have left
  /** Same air time? Fine axis when both copies have it, else the rough one. */
  near(id, t, e) {
    if (t.fine !== null && e.t.fine !== null) return Math.abs(t.fine - e.t.fine) <= Math.max(this.tol(id), this.tol(e.m0));
    return Math.abs(t.rough - e.t.rough) <= UNLOCKED_TOL_US;
  }

  /**
   * One received copy. Returns { dup: false, entry, groupTs } for the first copy (store and
   * emit it now, then bind(entry, abs)) or { dup: true, entry } for a later copy.
   */
  offer(id, meta, data, hostMs) {
    const mem = this.members.get(id);
    if (!mem) throw new Error(`unknown member ${id}`);
    this.expire(hostMs);
    mem.st.rx++;
    mem.clock.observe(meta.tsUs, hostMs * 1000);
    const t = this.times(id, meta.tsUs);
    if (meta.flags & 1) { // fcs_err: only ever an extra copy
      mem.st.bad++;
      const e = this.similar(id, meta, data, t);
      if (e) { this.attach(e, id, meta, true); this.st.rescued++; return { dup: true, entry: e }; }
      this.held.push({ id, meta, data: data.slice(), t, g: this.groupTs(t), host: hostMs });
      return { held: true };
    }
    const key = crc32(data) * 4096 + (meta.origLen & 0xfff);
    const bit = 1 << id, list = this.table.get(key);
    if (list) {
      for (const e of list) {
        if (e.mask & bit || e.ch !== meta.channel || e.capLen !== data.length || !this.near(id, t, e)) continue;
        this.attach(e, id, meta, false);
        if (e.anchor) this.anchors(e, id, meta.tsUs);
        return { dup: true, entry: e };
      }
    }
    const g = this.groupTs(t);
    const e = { key, g, t, ch: meta.channel, capLen: data.length, origLen: meta.origLen, mask: bit, m0: id, host: hostMs, abs: null, anchor: isAnchor(meta, data),
      data: data.slice(), copies: [{ id, rssi: meta.rssi, tsUs: meta.tsUs }] };
    if (list) list.push(e); else this.table.set(key, [e]);
    const lk = meta.channel * 65536 + meta.origLen;
    const ll = this.byLen.get(lk);
    if (ll) ll.push(e); else this.byLen.set(lk, [e]);
    this.queue.push(e);
    mem.st.first++;
    // bad copies of this frame that arrived first now only add their RSSI
    for (let i = 0; i < this.held.length; i++) {
      const h = this.held[i];
      if (!(e.mask & (1 << h.id)) && this.sameFrame(e, h.id, h.meta, h.data, h.t)) {
        this.attach(e, h.id, h.meta, true); this.st.rescued++;
        this.held.splice(i--, 1);
      }
    }
    return { dup: false, entry: e, groupTs: g };
  }

  attach(e, id, meta, bad) {
    e.mask |= 1 << id;
    e.copies.push({ id, rssi: meta.rssi, tsUs: meta.tsUs, bad });
    this.members.get(id).st.copies++;
  }
  sameFrame(e, id, meta, data, t) {
    if (e.ch !== meta.channel || e.origLen !== meta.origLen || e.capLen !== data.length || !this.near(id, t, e)) return false;
    return bitDiff(e.data, data, MAX_BIT_DIFF) <= MAX_BIT_DIFF;
  }
  similar(id, meta, data, t) {
    for (const e of this.byLen.get(meta.channel * 65536 + meta.origLen) ?? []) if (!(e.mask & (1 << id)) && this.sameFrame(e, id, meta, data, t)) return e;
    return null;
  }
  /** Bad copies whose wait is over and that no good copy claimed: emit them as they are. */
  due(hostMs) {
    const out = [];
    while (this.held.length && hostMs - this.held[0].host >= HOLD_MS) { out.push(this.held.shift()); this.st.badOut++; }
    return out;
  }
  bind(e, abs) { e.abs = abs; }

  anchors(e, id, ts) {
    if (id === this.ref) {
      for (const c of e.copies) if (c.id !== id) this.members.get(c.id)?.clock.anchor(c.tsUs, ts);
    } else {
      const r = e.copies.find((c) => c.id === this.ref);
      if (r) this.members.get(id).clock.anchor(ts, r.tsUs);
    }
  }

  /** Closes the copy window of frames older than windowMs (host time). */
  expire(hostMs) {
    const q = this.queue;
    while (this.qHead < q.length && hostMs - q[this.qHead].host > this.windowMs) {
      const e = q[this.qHead++];
      const list = this.table.get(e.key);
      const i = list.indexOf(e);
      if (i >= 0) list.splice(i, 1);
      if (!list.length) this.table.delete(e.key);
      const lk = e.ch * 65536 + e.origLen, ll = this.byLen.get(lk), j = ll.indexOf(e);
      if (j >= 0) ll.splice(j, 1);
      if (!ll.length) this.byLen.delete(lk);
      e.data = null;
      const n = popcount(e.mask);
      this.st.out++;
      this.st.dups += n - 1;
      if (n === 1 && this.members.size > 1) { this.st.unique++; const m = this.members.get(e.m0); if (m) m.st.unique++; }
      this.onFinal?.(e);
    }
    if (this.qHead > 4096 && this.qHead * 2 > q.length) { this.queue = q.slice(this.qHead); this.qHead = 0; }
  }
  /** Closes every open window (stop, member change). */
  flush() { this.expire(Infinity); }

  summary() {
    return {
      ref: this.ref, ...this.st, open: this.queue.length - this.qHead, held: this.held.length,
      members: [...this.members.entries()].map(([id, m]) => ({ id, mac: m.mac, ...m.st, ref: id === this.ref,
        clock: id === this.ref ? { ref: true } : { locked: m.clock.locked, ppm: m.clock.used >= LOCK_MIN ? m.clock.ppm : null, rmsUs: Number.isFinite(m.clock.rms) ? m.clock.rms : null,
          anchors: m.clock.used, offsetUs: m.clock.locked ? m.clock.b + (m.clock.a - 1) * (m.clock.anchors.at(-1)?.[0] ?? 0) : null, tolUs: m.clock.tolUs() } })),
    };
  }
}
