// Small MP4 writer for OpenHD video recordings (docs/13 §13.2): H.264 ('avc1' + avcC) or
// H.265 ('hev1' + hvcC) access units in Annex B with their 90 kHz RTP timestamps →
// a plain (non-fragmented) ISO BMFF file, moov first ("fast start"), one chunk.
// Parameter sets stay in-band too, so damaged or restarted streams still decode.
// No B-frames assumed (OpenHD's encoders are low-latency): decode order = display order.
import { splitAnnexB, unescapeRbsp } from './rtp.js';

// ---------------- SPS parsing ----------------
class Bits {
  constructor(b) { this.b = b; this.i = 0; }
  u(n) { let v = 0; for (let k = 0; k < n; k++) { const byte = this.b[this.i >> 3]; if (byte === undefined) throw new Error('SPS too short'); v = v * 2 + ((byte >> (7 - (this.i & 7))) & 1); this.i++; } return v; }
  ue() { let z = 0; while (this.u(1) === 0) { if (++z > 31) throw new Error('bad Exp-Golomb code in SPS'); } return 2 ** z - 1 + this.u(z); }
  se() { const k = this.ue(); return k & 1 ? (k + 1) / 2 : -k / 2; }
}
const nalType = (codec, n) => (codec === 'h265' ? (n[0] >> 1) & 0x3f : n[0] & 0x1f);
const PARAM_TYPES = { h264: { 7: 'sps', 8: 'pps' }, h265: { 32: 'vps', 33: 'sps', 34: 'pps' } };

/** SPS NAL (with header) → { width, height, chroma, bitDepthLuma, bitDepthChroma, ... }. */
export function spsInfo(codec, sps) {
  if (codec === 'h264') {
    const r = new Bits(unescapeRbsp(sps.subarray(1)));
    const profile = r.u(8), compat = r.u(8), level = r.u(8);
    r.ue();
    let chroma = 1, bdl = 8, bdc = 8, sep = 0;
    if ([100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135].includes(profile)) {
      chroma = r.ue();
      if (chroma === 3) sep = r.u(1);
      bdl = r.ue() + 8; bdc = r.ue() + 8; r.u(1);
      if (r.u(1)) {
        for (let i = 0; i < (chroma === 3 ? 12 : 8); i++) {
          if (!r.u(1)) continue;
          let last = 8, next = 8;
          for (let j = 0; j < (i < 6 ? 16 : 64); j++) { if (next) next = (last + r.se() + 256) % 256; if (next) last = next; }
        }
      }
    }
    r.ue();
    const poc = r.ue();
    if (poc === 0) r.ue();
    else if (poc === 1) { r.u(1); r.se(); r.se(); const n = r.ue(); for (let i = 0; i < n; i++) r.se(); }
    r.ue(); r.u(1);
    const wMbs = r.ue() + 1, hMap = r.ue() + 1, frameMbsOnly = r.u(1);
    if (!frameMbsOnly) r.u(1);
    r.u(1);
    let cl = 0, cr = 0, ct = 0, cb = 0;
    if (r.u(1)) { cl = r.ue(); cr = r.ue(); ct = r.ue(); cb = r.ue(); }
    const arr = sep ? 0 : chroma;
    const cx = arr === 1 || arr === 2 ? 2 : 1, cy = (arr === 1 ? 2 : 1) * (2 - frameMbsOnly);
    return { width: wMbs * 16 - cx * (cl + cr), height: (2 - frameMbsOnly) * hMap * 16 - cy * (ct + cb), profile, compat, level, chroma, bitDepthLuma: bdl, bitDepthChroma: bdc };
  }
  const rb = unescapeRbsp(sps.subarray(2));
  const r = new Bits(rb);
  r.u(4);
  const maxSub = r.u(3);
  r.u(1);
  const ptl = rb.subarray(1, 13); // general profile_tier_level incl. level (12 bytes)
  r.i += 96;
  const subProf = [], subLvl = [];
  for (let i = 0; i < maxSub; i++) { subProf.push(r.u(1)); subLvl.push(r.u(1)); }
  if (maxSub > 0) for (let i = maxSub; i < 8; i++) r.u(2);
  for (let i = 0; i < maxSub; i++) { if (subProf[i]) r.i += 88; if (subLvl[i]) r.i += 8; }
  r.ue();
  const chroma = r.ue();
  if (chroma === 3) r.u(1);
  let width = r.ue(), height = r.ue();
  if (r.u(1)) {
    const sw = chroma === 1 || chroma === 2 ? 2 : 1, sh = chroma === 1 ? 2 : 1;
    const l = r.ue(), rt = r.ue(), t = r.ue(), b = r.ue();
    width -= sw * (l + rt); height -= sh * (t + b);
  }
  return { width, height, chroma, bitDepthLuma: r.ue() + 8, bitDepthChroma: r.ue() + 8, ptl: Uint8Array.from(ptl), maxSubLayers: maxSub + 1 };
}

// ---------------- boxes ----------------
const enc = new TextEncoder();
function box(type, ...parts) {
  let n = 8;
  for (const p of parts) n += p.length;
  const o = new Uint8Array(n), dv = new DataView(o.buffer);
  dv.setUint32(0, n); o.set(enc.encode(type), 4);
  let i = 8;
  for (const p of parts) { o.set(p, i); i += p.length; }
  return o;
}
const fullbox = (type, version, flags, ...parts) => box(type, u32((version << 24) | flags), ...parts);
function u8(...v) { return Uint8Array.from(v); }
function u16(...v) { const o = new Uint8Array(2 * v.length), dv = new DataView(o.buffer); v.forEach((x, i) => dv.setUint16(2 * i, x)); return o; }
function u32(...v) { const o = new Uint8Array(4 * v.length), dv = new DataView(o.buffer); v.forEach((x, i) => dv.setUint32(4 * i, x >>> 0)); return o; }
const MATRIX = u32(0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000);
const withLen16 = (n) => [u16(n.length), n];

function avcC(sps, pps, info) {
  const ext = [100, 110, 122, 144].includes(info.profile) ? [u8(0xfc | info.chroma, 0xf8 | (info.bitDepthLuma - 8), 0xf8 | (info.bitDepthChroma - 8), 0)] : [];
  return box('avcC', u8(1, sps[1], sps[2], sps[3], 0xff, 0xe0 | 1), ...withLen16(sps), u8(1), ...withLen16(pps), ...ext);
}
function hvcC(vps, sps, pps, info) {
  const arr = (type, n) => [u8(0x80 | type), u16(1), ...withLen16(n)];
  return box('hvcC', u8(1), info.ptl.subarray(0, 11), u8(info.ptl[11]), u16(0xf000), u8(0xfc, 0xfc | info.chroma, 0xf8 | (info.bitDepthLuma - 8), 0xf8 | (info.bitDepthChroma - 8)),
    u16(0), u8(((info.maxSubLayers & 7) << 3) | 3), u8(3), ...arr(32, vps), ...arr(33, sps), ...arr(34, pps));
}

/** Per-sample durations (90 kHz) from RTP timestamps; gaps and jumps fall back to the typical frame time. */
function durations(ts) {
  const d = [];
  for (let i = 0; i + 1 < ts.length; i++) d.push((ts[i + 1] - ts[i]) >>> 0);
  const ok = d.filter((x) => x > 0 && x <= 90000).sort((a, b) => a - b);
  const typical = ok.length ? ok[ok.length >> 1] : 3000;
  const out = d.map((x) => (x > 0 && x <= 5 * 90000 ? x : typical));
  out.push(typical);
  return out;
}

function runs(values) { // stts entries
  const e = [];
  for (const v of values) { const l = e[e.length - 1]; if (l && l[1] === v) l[0]++; else e.push([1, v]); }
  return e;
}

function oneFile(codec, samples, params, info) {
  const sizes = samples.map((s) => s.data.length), mdatLen = sizes.reduce((a, b) => a + b, 0);
  if (mdatLen + 8 > 0xffffffff) throw new Error('recording too large for one MP4 (4 GiB)');
  const durs = durations(samples.map((s) => s.ts)), total = durs.reduce((a, b) => a + b, 0), ms = Math.round(total / 90);
  const keys = [];
  samples.forEach((s, i) => { if (s.key) keys.push(i + 1); });
  const tt = runs(durs);
  const entry = box(codec === 'h265' ? 'hev1' : 'avc1', new Uint8Array(6), u16(1, 0, 0), u32(0, 0, 0), u16(info.width, info.height), u32(0x480000, 0x480000, 0), u16(1),
    new Uint8Array(32), u16(0x18, 0xffff), codec === 'h265' ? hvcC(params.vps, params.sps, params.pps, info) : avcC(params.sps, params.pps, info));
  const stbl = (chunkOffset) => box('stbl',
    fullbox('stsd', 0, 0, u32(1), entry),
    fullbox('stts', 0, 0, u32(tt.length), ...tt.map(([c, v]) => u32(c, v))),
    ...(keys.length === samples.length ? [] : [fullbox('stss', 0, 0, u32(keys.length, ...keys))]),
    fullbox('stsz', 0, 0, u32(0, samples.length, ...sizes)),
    fullbox('stsc', 0, 0, u32(1, 1, samples.length, 1)),
    fullbox('stco', 0, 0, u32(1, chunkOffset)));
  const moov = (chunkOffset) => box('moov',
    fullbox('mvhd', 0, 0, u32(0, 0, 1000, ms, 0x10000), u16(0x100, 0), u32(0, 0), MATRIX, u32(0, 0, 0, 0, 0, 0, 2)),
    box('trak',
      fullbox('tkhd', 0, 3, u32(0, 0, 1, 0, ms, 0, 0), u16(0, 0, 0, 0), MATRIX, u32(info.width * 0x10000, info.height * 0x10000)),
      box('mdia',
        fullbox('mdhd', 0, 0, u32(0, 0, 90000, total), u16(0x55c4, 0)),
        fullbox('hdlr', 0, 0, u32(0), enc.encode('vide'), u32(0, 0, 0), enc.encode('VideoHandler\0')),
        box('minf', fullbox('vmhd', 0, 1, u16(0, 0, 0, 0)), box('dinf', fullbox('dref', 0, 0, u32(1), fullbox('url ', 0, 1))), stbl(chunkOffset)))));
  const ftyp = box('ftyp', enc.encode('isom'), u32(0x200), enc.encode(`isomiso2${codec === 'h265' ? 'hev1' : 'avc1'}mp41`));
  const head = moov(0).length; // stco is 32-bit: the size does not depend on the value
  return [ftyp, moov(ftyp.length + head + 8), u32(mdatLen + 8), enc.encode('mdat'), ...samples.map((s) => s.data)];
}

/**
 * Access units [{data: Annex B, key, ts: 90 kHz}] → MP4 files [{parts: Uint8Array[], width, height, frames, ms}].
 * A new file starts when the SPS changes (e.g. the camera resolution). Units before the
 * first key frame with parameter sets are skipped.
 */
export function mp4Files(codec, aus) {
  const PT = PARAM_TYPES[codec], files = [];
  let cur = null;
  const finish = () => { if (cur?.samples.length) { const parts = oneFile(codec, cur.samples, cur.params, cur.info); files.push({ parts, width: cur.info.width, height: cur.info.height, frames: cur.samples.length, ms: Math.round(durations(cur.samples.map((s) => s.ts)).reduce((a, b) => a + b, 0) / 90) }); } cur = null; };
  for (const au of aus) {
    const nals = splitAnnexB(au.data), p = {};
    for (const n of nals) { const k = PT[nalType(codec, n)]; if (k) p[k] = n; }
    const complete = p.sps && p.pps && (codec === 'h264' || p.vps);
    if (au.key && complete && (!cur || !same(cur.params.sps, p.sps))) {
      finish();
      cur = { params: { vps: p.vps && Uint8Array.from(p.vps), sps: Uint8Array.from(p.sps), pps: Uint8Array.from(p.pps) }, info: spsInfo(codec, p.sps), samples: [] };
    }
    if (!cur) continue;
    let n = 0;
    for (const x of nals) n += 4 + x.length;
    const data = new Uint8Array(n), dv = new DataView(data.buffer);
    let o = 0;
    for (const x of nals) { dv.setUint32(o, x.length); data.set(x, o + 4); o += 4 + x.length; }
    cur.samples.push({ data, key: !!au.key, ts: au.ts >>> 0 });
  }
  finish();
  return files;
}
function same(a, b) { if (a.length !== b.length) return false; for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false; return true; }
