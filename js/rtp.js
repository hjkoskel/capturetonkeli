// RTP → H.264 (RFC 6184) / H.265 (RFC 7798) access units in Annex B format,
// for WebCodecs VideoDecoder (docs/13 §6). One Depacketizer per video stream.
// Access units end at the RTP marker bit or a timestamp change. A sequence gap
// marks the access unit as damaged (the decoder may still conceal it).

const SC = Uint8Array.of(0, 0, 0, 1);
const H264_VALID = new Set([1, 5, 6, 7, 8, 9, 10, 11, 12, 24, 28]);
const H265_VALID = new Set([0, 1, 2, 3, 8, 9, 16, 17, 18, 19, 20, 21, 32, 33, 34, 35, 39, 40, 48, 49]);

/** Codec guess for one RTP payload: 'h264', 'h265' or null (ambiguous). */
export function guessCodec(p) {
  if (p.length < 3 || p[0] & 0x80) return null;
  const t5 = (p[0] >> 1) & 0x3f, h265 = !(p[0] & 0x01) && p[1] >= 1 && p[1] <= 7 && H265_VALID.has(t5);
  const h264 = H264_VALID.has(p[0] & 0x1f) && (p[0] & 0x1f) !== 0;
  if (h265 && !h264) return 'h265';
  if (h264 && !h265) return 'h264';
  return null;
}

export class Depacketizer {
  /** onAU({codec, data (Annex B), key, damaged, ts, codecString, params}) */
  constructor({ codec = 'auto', onAU }) {
    this.want = codec; this.onAU = onAU;
    this.reset();
    this.st = { packets: 0, notRtp: 0, gaps: 0, aus: 0, keys: 0, damaged: 0, bytes: 0 };
  }
  reset() {
    this.codec = this.want === 'auto' ? null : this.want;
    this.votes = { h264: 0, h265: 0 };
    this.nals = []; this.fu = null; this.ts = null; this.lastSeq = null; this.damaged = false; this.pending = [];
    this.params = {}; // cached VPS/SPS/PPS (Annex B bytes without start code), by name
  }
  setCodec(c) { if (c !== this.want) { this.want = c; this.reset(); } }

  push(p, { gap = false } = {}) {
    this.st.packets++;
    if (p.length < 12 || p[0] >> 6 !== 2) { this.st.notRtp++; return; }
    const cc = p[0] & 0x0f, marker = p[1] & 0x80, seq = (p[2] << 8) | p[3];
    const ts = ((p[4] << 24) | (p[5] << 16) | (p[6] << 8) | p[7]) >>> 0;
    let off = 12 + 4 * cc, end = p.length;
    if (p[0] & 0x10) { if (off + 4 > end) return; off += 4 + 4 * ((p[off + 2] << 8) | p[off + 3]); }
    if (p[0] & 0x20) end -= p[end - 1];
    if (off >= end) return;
    const pl = p.subarray(off, end);
    if ((this.lastSeq !== null && seq !== ((this.lastSeq + 1) & 0xffff)) || gap) { this.st.gaps++; this.damaged = true; this.fu = null; }
    this.lastSeq = seq;
    if (this.ts !== null && ts !== this.ts && (this.nals.length || this.fu)) this.flush();
    this.ts = ts;
    if (!this.codec) {
      // undecided: keep the packets and replay them once the codec is known
      this.pending.push([pl, marker, ts]);
      const g = guessCodec(pl);
      if (g && ++this.votes[g] >= 3 && this.votes[g] > 2 * this.votes[g === 'h264' ? 'h265' : 'h264']) this.codec = g;
      if (!this.codec) { if (this.pending.length > 200) this.pending.shift(); return; }
      const pend = this.pending;
      this.pending = [];
      this.ts = pend[0][2];
      for (const [q, m, t] of pend) {
        if (t !== this.ts && this.nals.length) this.flush();
        this.ts = t;
        this.payload(q);
        if (m) this.flush();
      }
      return;
    }
    this.payload(pl);
    if (marker) this.flush();
  }
  payload(pl) { if (this.codec === 'h264') this.h264(pl); else this.h265(pl); }

  h264(p) {
    const t = p[0] & 0x1f;
    if (t >= 1 && t <= 23) this.nals.push(p);
    else if (t === 24) this.aggregate(p, 1);
    else if (t === 28 && p.length > 2) {
      const h = p[1];
      if (h & 0x80) this.fu = [Uint8Array.of((p[0] & 0xe0) | (h & 0x1f)), p.subarray(2)];
      else if (this.fu) this.fu.push(p.subarray(2));
      if (h & 0x40 && this.fu) { this.nals.push(concat(this.fu)); this.fu = null; }
    }
  }
  h265(p) {
    const t = (p[0] >> 1) & 0x3f;
    if (t < 48) this.nals.push(p);
    else if (t === 48) this.aggregate(p, 2);
    else if (t === 49 && p.length > 3) {
      const h = p[2];
      if (h & 0x80) this.fu = [Uint8Array.of((p[0] & 0x81) | ((h & 0x3f) << 1), p[1]), p.subarray(3)];
      else if (this.fu) this.fu.push(p.subarray(3));
      if (h & 0x40 && this.fu) { this.nals.push(concat(this.fu)); this.fu = null; }
    }
  }
  aggregate(p, skip) {
    for (let i = skip; i + 2 <= p.length;) {
      const n = (p[i] << 8) | p[i + 1];
      if (!n || i + 2 + n > p.length) break;
      this.nals.push(p.subarray(i + 2, i + 2 + n));
      i += 2 + n;
    }
  }

  flush() {
    const nals = this.nals;
    this.nals = []; this.fu = null;
    const damaged = this.damaged;
    this.damaged = false;
    if (!nals.length) return;
    const h265 = this.codec === 'h265';
    const type = (n) => (h265 ? (n[0] >> 1) & 0x3f : n[0] & 0x1f);
    const PARAM = h265 ? { 32: 'vps', 33: 'sps', 34: 'pps' } : { 7: 'sps', 8: 'pps' };
    let key = false;
    const have = {};
    for (const n of nals) {
      const t = type(n);
      if (PARAM[t]) { this.params[PARAM[t]] = Uint8Array.from(n); have[PARAM[t]] = true; }
      if (h265 ? t >= 16 && t <= 21 : t === 5) key = true;
    }
    const need = h265 ? ['vps', 'sps', 'pps'] : ['sps', 'pps'];
    const parts = [];
    if (key) for (const k of need) if (!have[k] && this.params[k]) parts.push(SC, this.params[k]);
    for (const n of nals) parts.push(SC, n);
    const data = concat(parts);
    this.st.aus++; this.st.bytes += data.length;
    if (key) this.st.keys++;
    if (damaged) this.st.damaged++;
    this.onAU({ codec: this.codec, data, key, damaged, ts: this.ts, codecString: codecString(this.codec, this.params.sps), hasParams: need.every((k) => this.params[k]) });
  }
}

function concat(parts) {
  let n = 0;
  for (const p of parts) n += p.length;
  const o = new Uint8Array(n);
  let i = 0;
  for (const p of parts) { o.set(p, i); i += p.length; }
  return o;
}

/** Removes emulation prevention bytes (00 00 03 → 00 00). */
export function unescapeRbsp(b) {
  const o = [];
  for (let i = 0, z = 0; i < b.length; i++) {
    if (z >= 2 && b[i] === 3) { z = 0; continue; }
    z = b[i] === 0 ? z + 1 : 0;
    o.push(b[i]);
  }
  return Uint8Array.from(o);
}

/** WebCodecs codec string from the SPS NAL (with its NAL header). */
export function codecString(codec, sps) {
  if (!sps) return null;
  const hx = (v) => v.toString(16).padStart(2, '0');
  if (codec === 'h264') return sps.length >= 4 ? `avc1.${hx(sps[1])}${hx(sps[2])}${hx(sps[3])}` : null;
  const r = unescapeRbsp(sps.subarray(2, 40));
  if (r.length < 13) return null;
  const space = r[1] >> 6, tier = (r[1] >> 5) & 1, profile = r[1] & 0x1f;
  let compat = ((r[2] << 24) | (r[3] << 16) | (r[4] << 8) | r[5]) >>> 0, rev = 0;
  for (let i = 0; i < 32; i++) { rev = ((rev << 1) | (compat & 1)) >>> 0; compat >>>= 1; }
  const cons = Array.from(r.subarray(6, 12));
  while (cons.length && cons[cons.length - 1] === 0) cons.pop();
  return `hev1.${['', 'A', 'B', 'C'][space]}${profile}.${rev.toString(16)}.${tier ? 'H' : 'L'}${r[12]}${cons.map((c) => `.${c.toString(16).toUpperCase()}`).join('')}`;
}

/** Splits an Annex B buffer into NAL units (without start codes). */
export function splitAnnexB(b) {
  const out = [];
  let start = -1;
  for (let i = 0; i + 2 < b.length; i++) {
    if (b[i] === 0 && b[i + 1] === 0 && b[i + 2] === 1) {
      if (start >= 0) { let e = i; while (e > start && b[e - 1] === 0) e--; out.push(b.subarray(start, e)); }
      start = i + 3; i += 2;
    }
  }
  if (start >= 0 && start < b.length) out.push(b.subarray(start));
  return out;
}

/** RTP packetizer (single NAL / FU-A / H.265 FU), like GStreamer rtph26xpay with mtu 1440 (demo mode, tests). */
export class Packetizer {
  constructor({ codec = 'h264', mtu = 1440, pt = 96, ssrc = 0x0ffd0ffd, seq = 0 } = {}) { Object.assign(this, { codec, mtu, pt, ssrc, seq }); }
  /** One access unit (Annex B) → RTP packets; the last has the marker bit. */
  packetize(annexB, ts) {
    const nals = splitAnnexB(annexB), out = [], max = this.mtu - 12, h265 = this.codec === 'h265';
    const pkt = (payload, marker) => {
      const p = new Uint8Array(12 + payload.length), dv = new DataView(p.buffer);
      p[0] = 0x80; p[1] = this.pt | (marker ? 0x80 : 0);
      dv.setUint16(2, this.seq, false); this.seq = (this.seq + 1) & 0xffff;
      dv.setUint32(4, ts >>> 0, false); dv.setUint32(8, this.ssrc, false);
      p.set(payload, 12);
      out.push(p);
    };
    nals.forEach((n, ni) => {
      const last = ni === nals.length - 1;
      if (n.length <= max) { pkt(n, last); return; }
      const hl = h265 ? 2 : 1, type = h265 ? (n[0] >> 1) & 0x3f : n[0] & 0x1f;
      const ind = h265 ? [(n[0] & 0x81) | (49 << 1), n[1]] : [(n[0] & 0xe0) | 28];
      const room = max - hl - 1;
      for (let i = hl; i < n.length; i += room) {
        const chunk = n.subarray(i, Math.min(n.length, i + room)), s = i === hl, e = i + room >= n.length;
        const p = new Uint8Array(hl + 1 + chunk.length);
        p.set(ind); p[hl] = (s ? 0x80 : 0) | (e ? 0x40 : 0) | type; p.set(chunk, hl + 1);
        pkt(p, last && e);
      }
    });
    return out;
  }
}
