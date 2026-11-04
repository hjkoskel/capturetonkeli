// CTP v1 message set (docs/03 §6, §6.1, §9) and shared constants (docs/04
// App. B). Mirrors ctp/wire (Go). Decoders accept longer bodies and throw
// ShortBody for truncated mandatory fields.

export const PROTO_VER = 1;
export const MAX_FRAME = 4128;

export const T = Object.freeze({
  HELLO: 0x01, PING: 0x02, GET_CONFIG: 0x10, SET_CHANNEL: 0x11, SET_COUNTRY: 0x12, SET_FILTER: 0x13,
  SET_SNAPLEN: 0x14, SET_DRIVER_MASK: 0x15, CAPTURE: 0x20, GET_STATS: 0x21, REBOOT: 0x30,
  RSP_OK: 0x81, RSP_ERR: 0x82, EVT_STATS: 0x90, EVT_LOG: 0x91, EVT_RESET: 0x92, PKT: 0xa0,
});
export const typeName = (t) => Object.keys(T).find((k) => T[k] === t) ?? `0x${t.toString(16).padStart(2, '0')}`;

export const HFLAG_CFG_CHANGED = 0x02;
export const CAP = Object.freeze({ GHZ5: 1, HT40: 2, MANUAL_COUNTRY: 4, INJECTOR: 0x80000000 });
export const capNames = (c) => [[CAP.GHZ5, '5 GHz'], [CAP.HT40, 'HT40'], [CAP.MANUAL_COUNTRY, 'manual country'], [CAP.INJECTOR, 'injector']]
  .filter(([b]) => (c & b) >>> 0).map(([, n]) => n);
export const PKT = Object.freeze({ FCS_ERR: 1, TRUNCATED: 2, GHZ5: 4, HT40: 8, AMPDU: 16, RATE_UNKNOWN: 32, NO_FCS: 64 });
export const FW_FLAG = Object.freeze({ DIRTY: 1, IDF_MISMATCH: 2, DEV: 4 });
export const STATE_SAFE_MODE = 1;
export const BW = Object.freeze({ HT20: 0, HT40_PLUS: 1, HT40_MINUS: 2 });
export const bwName = (b) => ['HT20', 'HT40+', 'HT40−'][b] ?? `bw${b}`;
export const BB = Object.freeze({ B11: 0, G11: 1, HT: 2, VHT: 3, HE_SU: 4, HE_MU: 5, HE_ER_SU: 6, HE_TB: 7, VHT_MU: 11 });
export const bbName = (f) => ({ 0: '11b', 1: '11a/g', 2: 'HT', 3: 'VHT', 4: 'HE-SU', 5: 'HE-MU', 6: 'HE-ER-SU', 7: 'HE-TB', 11: 'VHT-MU' })[f] ?? '?';
export const DEFAULT_TYPE_MASK = 0xffffffff;
export const DEFAULT_CTRL_MASK = 0xff800000;
export const EMPTY_FILTER_CRC = 0x6522df69;

// ESP-IDF promiscuous masks (esp_wifi_types_generic.h, IDF v6.0.2)
export const PROMIS_TYPE = [['MGMT', 1], ['CTRL', 2], ['DATA', 4], ['MISC', 8], ['DATA_MPDU', 16], ['DATA_AMPDU', 32], ['FCSFAIL', 64]];
export const PROMIS_CTRL = [['Control Wrapper', 1 << 23], ['Block Ack Req', 1 << 24], ['Block Ack', 1 << 25], ['PS-Poll', 1 << 26],
  ['RTS', 1 << 27], ['CTS', 1 << 28], ['ACK', 1 << 29], ['CF-End', 1 << 30], ['CF-End+CF-Ack', 2 ** 31]];

export const ERR = Object.freeze({
  UNKNOWN_TYPE: 1, BAD_BODY: 2, INVALID_ARG: 3, CHANNEL_NOT_ALLOWED: 4, COUNTRY_INVALID: 5,
  NOT_SUPPORTED: 6, FILTER_INVALID: 7, NO_SESSION: 8, RADIO: 9, SAFE_MODE: 10,
});
export const errName = (c) => 'ERR_' + (Object.keys(ERR).find((k) => ERR[k] === c) ?? String(c));

export const RESET = ['UNKNOWN', 'POWER_ON', 'SOFTWARE', 'PANIC', 'WATCHDOG', 'BROWNOUT', 'ASSERT', 'EXTERNAL'];
export const resetName = (r) => RESET[r] ?? `RESET_${r}`;

export const ESP_ERR = {
  0x0: 'ESP_OK', [-1]: 'ESP_FAIL', 0x101: 'ESP_ERR_NO_MEM', 0x102: 'ESP_ERR_INVALID_ARG', 0x103: 'ESP_ERR_INVALID_STATE',
  0x104: 'ESP_ERR_INVALID_SIZE', 0x105: 'ESP_ERR_NOT_FOUND', 0x106: 'ESP_ERR_NOT_SUPPORTED', 0x107: 'ESP_ERR_TIMEOUT',
  0x3001: 'ESP_ERR_WIFI_NOT_INIT', 0x3002: 'ESP_ERR_WIFI_NOT_STARTED', 0x3003: 'ESP_ERR_WIFI_NOT_STOPPED', 0x3004: 'ESP_ERR_WIFI_IF',
  0x3005: 'ESP_ERR_WIFI_MODE', 0x3006: 'ESP_ERR_WIFI_STATE', 0x3008: 'ESP_ERR_WIFI_NVS', 0x300a: 'ESP_ERR_WIFI_ARG',
};
export const espErrName = (e) => e === 0 ? '' : `${ESP_ERR[e] ?? 'esp_err'} 0x${(e >>> 0).toString(16)}`;

/** 2.4 GHz: 2407 + 5·ch (ch14 = 2484); 5 GHz: 5000 + 5·ch. */
export function channelFreq(ch) {
  if (ch >= 1 && ch <= 13) return 2407 + 5 * ch;
  if (ch === 14) return 2484;
  if (ch >= 32 && ch <= 177) return 5000 + 5 * ch;
  return 0;
}
/** DFS channels (display only; receive is always fine). */
export const DFS = new Set([52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144]);

export class ShortBody extends Error {
  constructor(what) { super(`CTP: ${what} body too short`); this.name = 'ShortBody'; }
}

// ---- little cursor helpers -------------------------------------------------------

class R {
  constructor(u8, what) { this.dv = new DataView(u8.buffer, u8.byteOffset, u8.byteLength); this.o = 0; this.what = what; this.u8 = u8; }
  need(n) { if (this.o + n > this.dv.byteLength) throw new ShortBody(this.what); }
  left() { return this.dv.byteLength - this.o; }
  u8_() { this.need(1); return this.dv.getUint8(this.o++); }
  i8() { this.need(1); return this.dv.getInt8(this.o++); }
  u16() { this.need(2); const v = this.dv.getUint16(this.o, true); this.o += 2; return v; }
  u32() { this.need(4); const v = this.dv.getUint32(this.o, true); this.o += 4; return v; }
  i32() { this.need(4); const v = this.dv.getInt32(this.o, true); this.o += 4; return v; }
  // Number when exact (always for real device µs counters), BigInt beyond 2^53
  u64() { this.need(8); const v = this.dv.getBigUint64(this.o, true); this.o += 8; return v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : v; }
  bytes(n) { this.need(n); const b = this.u8.slice(this.o, this.o + n); this.o += n; return b; }
}

class W {
  constructor() { this.a = []; }
  u8(v) { this.a.push(v & 0xff); return this; }
  u16(v) { this.a.push(v & 0xff, (v >>> 8) & 0xff); return this; }
  u32(v) { v >>>= 0; this.a.push(v & 0xff, (v >>> 8) & 0xff, (v >>> 16) & 0xff, v >>> 24); return this; }
  bytes(b) { for (const x of b) this.a.push(x); return this; }
  out() { return Uint8Array.from(this.a); }
}

const txt = new TextDecoder();
const enc = new TextEncoder();

// ---- requests ----------------------------------------------------------------------

export const encHello = (nonce, protoVer = PROTO_VER) => new W().u32(nonce).u8(protoVer).out();
export const encPing = (ms) => new W().u32(ms).out();
export const encSetChannel = (ch, bw) => new W().u8(ch).u8(bw).out();
/** cc: "FI" / "01"; mode 0 validated, 1 manual. */
export const encCountry = (cc, mode = 0, schan = 0, nchan = 0) => {
  const c = enc.encode(cc.toUpperCase().padEnd(2, ' ').slice(0, 2));
  return new W().u8(c[0]).u8(c[1]).u8(0).u8(mode).u8(schan).u8(nchan).out();
};
export const encSnaplen = (n) => new W().u16(n).out();
export const encDriverMask = (t, c) => new W().u32(t).u32(c).out();
export const encU8 = (v) => Uint8Array.of(v ? 1 : 0);

// decoders for requests (used by the demo device)
export function decHello(b) { const r = new R(b, 'HELLO'); return { hostNonce: r.u32(), hostProtoVer: r.u8_() }; }
export function decPing(b) { const r = new R(b, 'PING'); return { hostTimeMs: r.u32() }; }
export function decSetChannel(b) { const r = new R(b, 'SET_CHANNEL'); return { channel: r.u8_(), bw: r.u8_() }; }
export function decCountry(b) {
  const r = new R(b, 'SET_COUNTRY');
  const cc = r.bytes(3);
  return { cc: String.fromCharCode(cc[0], cc[1]), mode: r.u8_(), schan: r.u8_(), nchan: r.u8_() };
}
export function decU16(b, what) { return new R(b, what).u16(); }
export function decU8(b, what) { return new R(b, what).u8_(); }
export function decDriverMask(b) { const r = new R(b, 'SET_DRIVER_MASK'); return { typeMask: r.u32(), ctrlMask: r.u32() }; }

// ---- responses / events ------------------------------------------------------------------

export function decDeviceInfo(b) {
  const r = new R(b, 'HELLO response');
  const d = {
    protoVer: r.u8_(), fwVersion: r.u32(), fwFlags: r.u8_(), buildId: r.bytes(8), mac: r.bytes(6), caps: r.u32(),
    maxFrame: r.u16(), maxRules: r.u16(), deviceTimeUs: r.u64(), hostNonce: r.u32(), resetReason: 0, stateFlags: 0,
  };
  if (r.left() >= 2) { d.resetReason = r.u8_(); d.stateFlags = r.u8_(); }
  return d;
}
export function encDeviceInfo(d) {
  const w = new W().u8(d.protoVer).u32(d.fwVersion).u8(d.fwFlags).bytes(d.buildId).bytes(d.mac).u32(d.caps)
    .u16(d.maxFrame).u16(d.maxRules);
  const t = BigInt(typeof d.deviceTimeUs === 'bigint' ? d.deviceTimeUs : Math.floor(d.deviceTimeUs));
  w.u32(Number(t & 0xffffffffn)).u32(Number(t >> 32n));
  return w.u32(d.hostNonce).u8(d.resetReason ?? 0).u8(d.stateFlags ?? 0).out();
}

export function decPong(b) { const r = new R(b, 'PING response'); return { hostTimeMs: r.u32(), deviceTimeUs: r.u64() }; }
export function encPong(hostMs, devUs) {
  const t = BigInt(typeof devUs === 'bigint' ? devUs : Math.floor(devUs));
  return new W().u32(hostMs).u32(Number(t & 0xffffffffn)).u32(Number(t >> 32n)).out();
}

export function decConfig(b) {
  const r = new R(b, 'CONFIG');
  const c = { cfgGen: r.u8_(), channel: r.u8_(), bw: r.u8_(), band: r.u8_() };
  const cc = r.bytes(3);
  c.country = { cc: String.fromCharCode(...cc.filter((x) => x !== 0 && x !== 0x20)), mode: r.u8_(), schan: r.u8_(), nchan: r.u8_() };
  c.snaplen = r.u16(); c.typeMask = r.u32(); c.ctrlMask = r.u32(); c.captureOn = r.u8_() !== 0; c.nRules = r.u8_(); c.rulesCrc32 = r.u32();
  return c;
}
export function encConfig(c) {
  const cc = enc.encode((c.country.cc + '\0\0').slice(0, 2));
  return new W().u8(c.cfgGen).u8(c.channel).u8(c.bw).u8(c.band).u8(cc[0]).u8(cc[1]).u8(0)
    .u8(c.country.mode).u8(c.country.schan).u8(c.country.nchan).u16(c.snaplen).u32(c.typeMask).u32(c.ctrlMask)
    .u8(c.captureOn ? 1 : 0).u8(c.nRules).u32(c.rulesCrc32).out();
}
export const sameSettings = (a, b) => {
  const k = (c) => JSON.stringify({ ...c, cfgGen: 0 });
  return k(a) === k(b);
};

const STAT_FIELDS = ['rxTotal', 'rxPassed', 'dropRing', 'dropFcs', 'txFrames', 'txBytes', 'hostRxCrcErr', 'hostRxOversize'];
export function decStats(b) {
  const r = new R(b, 'STATS');
  const s = { deviceTimeUs: r.u64() };
  for (const f of STAT_FIELDS) s[f] = r.u32();
  s.ringHighWater = r.u16(); s.channel = r.u8_(); s.noiseFloor = r.i8(); s.flashWrites = r.left() >= 4 ? r.u32() : 0;
  return s;
}
export function encStats(s) {
  const t = BigInt(typeof s.deviceTimeUs === 'bigint' ? s.deviceTimeUs : Math.floor(s.deviceTimeUs));
  const w = new W().u32(Number(t & 0xffffffffn)).u32(Number(t >> 32n));
  for (const f of STAT_FIELDS) w.u32(s[f]);
  return w.u16(s.ringHighWater).u8(s.channel).u8(s.noiseFloor).u32(s.flashWrites ?? 0).out();
}

export function decRspErr(b) {
  const r = new R(b, 'RSP_ERR');
  const e = { code: r.u16(), espErr: r.i32(), detail: '' };
  if (r.left() >= 1) { const n = Math.min(r.u8_(), r.left()); e.detail = txt.decode(r.bytes(n)); }
  return e;
}
export function encRspErr(code, espErr = 0, detail = '') {
  const d = enc.encode(detail).slice(0, 64);
  return new W().u16(code).u32(espErr).u8(d.length).bytes(d).out();
}

export function decReset(b) {
  const r = new R(b, 'EVT_RESET');
  const e = { reason: r.u8_(), fwVersion: r.u32(), stateFlags: 0, detail: '' };
  if (r.left() >= 1) e.stateFlags = r.u8_();
  if (r.left() >= 1) { const n = Math.min(r.u8_(), r.left()); e.detail = txt.decode(r.bytes(n)); }
  return e;
}
export function encReset(reason, fwVersion, stateFlags = 0, detail = '') {
  const d = enc.encode(detail).slice(0, 64);
  return new W().u8(reason).u32(fwVersion).u8(stateFlags).u8(d.length).bytes(d).out();
}

export function decLog(b) { const r = new R(b, 'EVT_LOG'); const level = r.u8_(); return { level, text: txt.decode(b.subarray(1)) }; }
export const encLog = (level, text) => new W().u8(level).bytes(enc.encode(text).slice(0, 200)).out();

export const PKT_META_LEN = 20;
/** Decodes the PKT header; data aliases b. */
export function decPkt(b) {
  const r = new R(b, 'PKT');
  const m = {
    tsUs: r.u64(), channel: r.u8_(), flags: r.u8_(), rssi: r.i8(), noise: r.i8(), rate: r.u8_(), bbFormat: r.u8_(),
    origLen: r.u16(), capLen: r.u16(), rxSeq: r.u16(),
  };
  if (m.capLen > b.length - PKT_META_LEN) throw new ShortBody('PKT data');
  return { meta: m, data: b.subarray(PKT_META_LEN, PKT_META_LEN + m.capLen) };
}
export function encPkt(m, data) {
  const t = BigInt(typeof m.tsUs === 'bigint' ? m.tsUs : Math.floor(m.tsUs));
  return new W().u32(Number(t & 0xffffffffn)).u32(Number(t >> 32n)).u8(m.channel).u8(m.flags).u8(m.rssi).u8(m.noise)
    .u8(m.rate).u8(m.bbFormat).u16(m.origLen).u16(m.capLen).u16(m.rxSeq).bytes(data).out();
}

// ---- firmware versions (docs/03 §11) -----------------------------------------------------

export const fwFromWire = (u) => ({ major: u >>> 24, minor: (u >>> 16) & 0xff, patch: u & 0xffff });
export const fwToWire = (v) => (((v.major & 0xff) << 24) | ((v.minor & 0xff) << 16) | (v.patch & 0xffff)) >>> 0;
export const fwString = (v) => `${v.major}.${v.minor}.${v.patch}`;
export const macString = (m) => Array.from(m, (b) => b.toString(16).padStart(2, '0')).join(':');

/** The 802.11 frame without its trailing FCS (if the FCS is present in data). */
export function frameBytes(meta, data) {
  return !(meta.flags & PKT.NO_FCS) && meta.capLen === meta.origLen && data.length >= 4 ? data.subarray(0, data.length - 4) : data;
}
