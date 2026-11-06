// pcapng + radiotap writer (docs/04 §10), byte-identical to ctp/pcapng (Go);
// tested against testvectors/pcapng.json.
import { PKT, BB, channelFreq } from './wire.js';

const SHB = 0x0a0d0d0a, IDB = 1, EPB_T = 6;
const te = new TextEncoder();
const pad4 = (n) => (4 - (n % 4)) % 4;

class Buf {
  constructor() { this.a = []; }
  u8(v) { this.a.push(v & 0xff); return this; }
  u16(v) { this.a.push(v & 0xff, (v >>> 8) & 0xff); return this; }
  u32(v) { v >>>= 0; this.a.push(v & 0xff, (v >>> 8) & 0xff, (v >>> 16) & 0xff, v >>> 24); return this; }
  u64(v) { const b = BigInt(v); return this.u32(Number(b & 0xffffffffn)).u32(Number(b >> 32n)); }
  bytes(b) { for (const x of b) this.a.push(x); return this; }
  opt(code, val) { this.u16(code).u16(val.length).bytes(val); for (let i = pad4(val.length); i > 0; i--) this.u8(0); return this; }
  get length() { return this.a.length; }
}

function block(type, body) {
  const total = 12 + body.length;
  return new Buf().u32(type).u32(total).bytes(body.a).u32(total);
}

/** SHB + IDB (LINKTYPE 127, µs resolution). */
export function header({ userAppl = '', hardware = '', ifName = '', snaplen = 0 } = {}) {
  const shb = new Buf().u32(0x1a2b3c4d).u16(1).u16(0).u32(0xffffffff).u32(0xffffffff);
  if (hardware) shb.opt(2, te.encode(hardware));
  if (userAppl) shb.opt(4, te.encode(userAppl));
  shb.opt(0, []);
  const idb = new Buf().u16(127).u16(0).u32(snaplen || 65535);
  if (ifName) idb.opt(2, te.encode(ifName));
  idb.opt(9, [6]).opt(0, []);
  return Uint8Array.from([...block(SHB, shb).a, ...block(IDB, idb).a]);
}

/** Radiotap header for a PKT meta (same field set as Go). */
export function radiotap(m) {
  const legacy = !(m.flags & PKT.RATE_UNKNOWN) && (m.bbFormat === BB.B11 || m.bbFormat === BB.G11);
  const mcs = !(m.flags & PKT.RATE_UNKNOWN) && m.bbFormat === BB.HT;
  let present = 1 | 2 | 8 | 32 | 64;
  if (legacy) present |= 4;
  if (mcs) present |= 1 << 19;
  const b = new Buf().u8(0).u8(0).u16(0).u32(present).u64(m.tsUs);
  let fl = 0;
  if (!(m.flags & PKT.NO_FCS) && m.capLen === m.origLen && m.capLen >= 4) fl |= 0x10;
  if (m.flags & PKT.FCS_ERR) fl |= 0x40;
  b.u8(fl);
  if (legacy) b.u8(m.rate);
  if (b.length % 2) b.u8(0);
  const mhz = channelFreq(m.channel);
  const five = m.channel > 14;
  const cf = five ? 0x0100 | 0x0040 : 0x0080 | (m.bbFormat === BB.B11 ? 0x0020 : 0x0040);
  b.u16(mhz).u16(cf).u8(m.rssi).u8(m.noise);
  if (mcs) b.u8(0x02).u8(0).u8(m.rate);
  const out = Uint8Array.from(b.a);
  out[2] = out.length & 0xff; out[3] = out.length >> 8;
  return out;
}

/** One Enhanced Packet Block. */
export function epb(m, data, comment = '') {
  const rt = radiotap(m);
  const caplen = rt.length + data.length;
  const body = new Buf().u32(0).u32(Math.floor(m.tsUs / 2 ** 32)).u32(m.tsUs % 2 ** 32).u32(caplen).u32(rt.length + m.origLen)
    .bytes(rt).bytes(data);
  for (let i = pad4(caplen); i > 0; i--) body.u8(0);
  if (comment) body.opt(1, te.encode(comment)).opt(0, []);
  return Uint8Array.from(block(EPB_T, body).a);
}
