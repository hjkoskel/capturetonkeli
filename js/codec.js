// CTP framing (docs/03 §3–4): COBS, CRC-32, frame encode, streaming decoder.
// Byte-identical to ctp/codec (Go) and ctp_core frame.c; tested against
// testvectors/{cobs,crc32,frames}.json (test/js/codec.test.mjs).

export const HEADER_LEN = 8;
export const MAX_FRAME = 4096 + 32;
export const maxEncodedLen = (n) => n + Math.floor(n / 254) + 2;

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[i] = c >>> 0;
  }
  return t;
})();

/** CRC-32 IEEE; pass the previous return value as `crc` to chain. */
export function crc32(bytes, crc = 0) {
  let c = (crc ^ 0xffffffff) >>> 0;
  for (let i = 0; i < bytes.length; i++) c = CRC_TABLE[(c ^ bytes[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

/** COBS-encode src (no delimiter). */
export function cobsEncode(src) {
  const out = new Uint8Array(maxEncodedLen(src.length));
  let codeIdx = 0, o = 1, code = 1;
  for (let i = 0; i < src.length; i++) {
    const b = src[i];
    if (b === 0) {
      out[codeIdx] = code; codeIdx = o++; code = 1;
      continue;
    }
    out[o++] = b; code++;
    if (code === 0xff) { out[codeIdx] = code; codeIdx = o++; code = 1; }
  }
  out[codeIdx] = code;
  return out.subarray(0, o);
}

/** COBS-decode one frame (no delimiter); returns null if invalid. */
export function cobsDecode(src) {
  const out = new Uint8Array(src.length);
  let i = 0, o = 0;
  while (i < src.length) {
    const code = src[i++];
    if (code === 0) return null;
    const n = code - 1;
    if (i + n > src.length || o + n > out.length) return null;
    for (let k = 0; k < n; k++) {
      const b = src[i + k];
      if (b === 0) return null;
      out[o++] = b;
    }
    i += n;
    if (code !== 0xff && i < src.length) {
      if (o >= out.length) return null;
      out[o++] = 0;
    }
  }
  return out.subarray(0, o);
}

/** Header object → 8 bytes. */
export function putHeader(dv, off, h) {
  dv.setUint8(off, h.ver ?? 1);
  dv.setUint8(off + 1, h.type);
  dv.setUint16(off + 2, h.seq ?? 0, true);
  dv.setUint16(off + 4, h.reqId ?? 0, true);
  dv.setUint8(off + 6, h.flags ?? 0);
  dv.setUint8(off + 7, h.cfgGen ?? 0);
}

/** Wire bytes for one frame: [0x00 if lead] COBS(header|body|crc) 0x00. */
export function encodeFrame(h, body = new Uint8Array(0), lead = true) {
  const raw = new Uint8Array(HEADER_LEN + body.length + 4);
  const dv = new DataView(raw.buffer);
  putHeader(dv, 0, h);
  raw.set(body, HEADER_LEN);
  dv.setUint32(HEADER_LEN + body.length, crc32(raw.subarray(0, HEADER_LEN + body.length)), true);
  const enc = cobsEncode(raw);
  const out = new Uint8Array(enc.length + (lead ? 2 : 1));
  out.set(enc, lead ? 1 : 0);
  return out; // leading and trailing bytes stay 0
}

/** Streaming decoder with the doc 03 resync rule. */
export class Decoder {
  constructor(maxFrame = MAX_FRAME) {
    this.maxFrame = maxFrame;
    this.maxEnc = maxEncodedLen(maxFrame);
    this.enc = new Uint8Array(this.maxEnc);
    this.n = 0;
    this.synced = false;
    this.overrun = false;
    this.stats = { frames: 0, crcErrors: 0, cobsErrors: 0, oversize: 0, short: 0, skippedBytes: 0, resyncs: 0 };
  }
  /** Drop partial state; wait for a fresh delimiter (after reopen/overrun). */
  reset() { this.n = 0; this.synced = false; this.overrun = false; this.stats.resyncs++; }
  /** fn(header, body) per valid frame; body is a fresh copy. */
  feed(bytes, fn) {
    for (let i = 0; i < bytes.length; i++) {
      const b = bytes[i];
      if (!this.synced) {
        if (b === 0) this.synced = true; else this.stats.skippedBytes++;
        continue;
      }
      if (b !== 0) {
        if (this.overrun) continue;
        if (this.n >= this.maxEnc) { this.overrun = true; this.stats.oversize++; continue; }
        this.enc[this.n++] = b;
        continue;
      }
      if (this.overrun) { this.overrun = false; this.n = 0; continue; }
      if (this.n === 0) continue;
      this._handle(fn);
      this.n = 0;
    }
  }
  _handle(fn) {
    const dec = cobsDecode(this.enc.subarray(0, this.n));
    if (dec === null) { this.stats.cobsErrors++; return; }
    if (dec.length > this.maxFrame) { this.stats.oversize++; return; }
    if (dec.length < HEADER_LEN + 4) { this.stats.short++; return; }
    const m = dec.length - 4;
    const dv = new DataView(dec.buffer, dec.byteOffset, dec.length);
    if (crc32(dec.subarray(0, m)) !== dv.getUint32(m, true)) { this.stats.crcErrors++; return; }
    this.stats.frames++;
    fn({ ver: dec[0], type: dec[1], seq: dv.getUint16(2, true), reqId: dv.getUint16(4, true), flags: dec[6], cfgGen: dec[7] },
      dec.slice(HEADER_LEN, m));
  }
}

export const hex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, '0')).join('');
export const unhex = (s) => {
  const out = new Uint8Array(s.length >> 1);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.substr(i * 2, 2), 16);
  return out;
};
