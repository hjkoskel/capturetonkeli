// ESP32-C5 mask-ROM flasher (docs/04 §11.1), same behaviour as ctp/romflash
// (Go). ROM loader only, no stub. Hardware rules (docs/10 §3, M3):
//   · never change DTR/RTS        · never send non-SLIP bytes to the ROM
//   · no FLASH_END                · leave the ROM with the LP watchdog reset
// Works on any transport with write(u8) and onData(cb).

export const CMD = { FLASH_BEGIN: 0x02, FLASH_DATA: 0x03, SYNC: 0x08, WRITE_REG: 0x09, READ_REG: 0x0a,
  SPI_SET_PARAMS: 0x0b, SPI_ATTACH: 0x0d, SPI_FLASH_MD5: 0x13, SECURITY_INFO: 0x14 };
export const FLASH_WRITE_SIZE = 0x400, SECTOR = 0x1000, CHIP_ID_C5 = 23;
const LPWDT = 0x600b1c00, WDT_KEY = 0x50d83aa1;
const CHIPS = { 5: 'ESP32-C3', 9: 'ESP32-S3', 12: 'ESP32-C2', 13: 'ESP32-C6', 16: 'ESP32-H2', 18: 'ESP32-P4', 20: 'ESP32-C61', 23: 'ESP32-C5' };

export class RomError extends Error {
  /** kind: 'timeout' | 'status' | 'chip' | 'verify' | 'abort' */
  constructor(kind, message, extra = {}) { super(message); this.name = 'RomError'; this.kind = kind; Object.assign(this, extra); }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export function slipEncode(p) {
  const out = [0xc0];
  for (const b of p) { if (b === 0xc0) out.push(0xdb, 0xdc); else if (b === 0xdb) out.push(0xdb, 0xdd); else out.push(b); }
  out.push(0xc0);
  return Uint8Array.from(out);
}

/** Streaming SLIP splitter. */
export class SlipReader {
  constructor(onFrame) { this.on = onFrame; this.buf = []; this.inF = false; this.esc = false; }
  feed(u8) {
    for (const b of u8) {
      if (!this.inF) { if (b === 0xc0) { this.inF = true; this.buf = []; } continue; }
      if (this.esc) {
        this.esc = false;
        if (b === 0xdc) this.buf.push(0xc0); else if (b === 0xdd) this.buf.push(0xdb); else this.inF = false;
      } else if (b === 0xdb) this.esc = true;
      else if (b === 0xc0) { if (this.buf.length) { this.on(Uint8Array.from(this.buf)); this.buf = []; } }
      else this.buf.push(b);
    }
  }
}

const le32 = (v) => [v & 0xff, (v >>> 8) & 0xff, (v >>> 16) & 0xff, v >>> 24];
const words = (...v) => Uint8Array.from(v.flatMap(le32));
const checksum = (d) => d.reduce((c, b) => c ^ b, 0xef);

export class RomLoader {
  constructor(transport) {
    this.t = transport;
    this.waiters = [];
    this.slip = new SlipReader((p) => this.onPacket(p));
    transport.onData((u8) => { if (u8) this.slip.feed(u8); });
  }

  onPacket(p) {
    if (p.length < 8 || p[0] !== 1) return;
    const w = this.waiters.find((x) => x.op === p[1]);
    if (!w) return;
    this.waiters.splice(this.waiters.indexOf(w), 1);
    const dv = new DataView(p.buffer, p.byteOffset);
    const n = dv.getUint16(2, true);
    w.resolve({ val: dv.getUint32(4, true), body: p.subarray(8, 8 + n) });
  }

  async command(op, data = new Uint8Array(0), chk = 0, timeout = 1000) {
    const pkt = new Uint8Array(8 + data.length);
    const dv = new DataView(pkt.buffer);
    pkt[0] = 0; pkt[1] = op; dv.setUint16(2, data.length, true); dv.setUint32(4, chk, true);
    pkt.set(data, 8);
    const got = new Promise((resolve) => this.waiters.push({ op, resolve }));
    await this.t.write(slipEncode(pkt));
    const r = await Promise.race([got, sleep(timeout).then(() => null)]);
    if (!r) { this.waiters = this.waiters.filter((w) => w.op !== op); throw new RomError('timeout', `ROM did not answer command 0x${op.toString(16)}`); }
    return r;
  }

  /** Command + 2 status bytes after respLen bytes of data. */
  async check(op, data, chk = 0, respLen = 0, timeout = 1000) {
    const { val, body } = await this.command(op, data, chk, timeout);
    if (body.length < respLen + 2) {
      if (body.length >= 2 && body[0]) throw new RomError('status', `ROM rejected command 0x${op.toString(16)} (status ${body[0]}, reason 0x${body[1].toString(16)})`);
      throw new RomError('status', `short ROM reply to 0x${op.toString(16)}`);
    }
    if (body[respLen]) throw new RomError('status', `ROM rejected command 0x${op.toString(16)} (status ${body[respLen]}, reason 0x${body[respLen + 1].toString(16)})`);
    return { val, data: body.subarray(0, respLen) };
  }

  async sync(attempts = 10) {
    const d = Uint8Array.from([0x07, 0x07, 0x12, 0x20, ...new Array(32).fill(0x55)]);
    for (let i = 0; i < attempts; i++) {
      try {
        await this.command(CMD.SYNC, d, 0, 200);
        await sleep(50); // the ROM answers SYNC several times
        this.waiters = [];
        return;
      } catch (e) { if (e.kind !== 'timeout') throw e; }
    }
    throw new RomError('timeout', 'No answer from the ROM download mode');
  }

  writeReg(addr, value, timeout = 1000) { return this.check(CMD.WRITE_REG, words(addr, value, 0xffffffff, 0), 0, 0, timeout); }

  async securityInfo() {
    const { data } = await this.check(CMD.SECURITY_INFO, new Uint8Array(0), 0, 20);
    const dv = new DataView(data.buffer, data.byteOffset);
    return { flags: dv.getUint32(0, true), flashCrypt: data[4], chipId: dv.getUint32(12, true), eco: dv.getUint32(16, true) };
  }

  spiAttach() { return this.check(CMD.SPI_ATTACH, new Uint8Array(8), 0, 0, 3000); }
  setParams(size) { return this.check(CMD.SPI_SET_PARAMS, words(0, size, 64 * 1024, SECTOR, 256, 0xffff)); }

  async md5(addr, size) {
    const { data } = await this.check(CMD.SPI_FLASH_MD5, words(addr, size, 0, 0), 0, 32, 3000 + (size >> 20) * 8000);
    return new TextDecoder().decode(data).toLowerCase();
  }

  async flashBegin(off, size) {
    const blocks = Math.ceil(size / FLASH_WRITE_SIZE);
    await this.check(CMD.FLASH_BEGIN, words(size, blocks, FLASH_WRITE_SIZE, off, 0), 0, 0, 3000 + Math.ceil(size * 30000 / (1 << 20)));
    return blocks;
  }

  async flashData(seq, block) {
    const b = new Uint8Array(FLASH_WRITE_SIZE).fill(0xff);
    b.set(block);
    const d = new Uint8Array(16 + b.length);
    d.set(words(b.length, seq, 0, 0)); d.set(b, 16);
    let err;
    for (let a = 0; a < 3; a++) { try { await this.check(CMD.FLASH_DATA, d, checksum(b), 0, 5000); return; } catch (e) { err = e; } }
    throw err;
  }

  /** Leaves the ROM (the RTS hard reset does not on the C5). The device re-enumerates. */
  async watchdogReset() {
    await this.writeReg(LPWDT + 0x18, WDT_KEY);
    await this.writeReg(LPWDT + 0x04, 2000);
    await this.writeReg(LPWDT + 0x00, ((1 << 31) | (5 << 28) | (1 << 8) | 2) >>> 0, 300).catch(() => {});
    await this.writeReg(LPWDT + 0x18, 0, 300).catch(() => {});
  }
}

/** Probe for a ROM loader with SLIP SYNC (harmless to CTP firmware). */
export async function probe(transport, attempts = 3) {
  const l = new RomLoader(transport);
  try { await l.sync(attempts); return l; } catch { return null; }
}

/** SYNC, chip check (ESP32-C5 only, nothing written otherwise), SPI attach. */
export async function connect(loader) {
  const si = await loader.securityInfo();
  const name = CHIPS[si.chipId] ?? `chip id ${si.chipId}`;
  if (si.chipId !== CHIP_ID_C5) throw new RomError('chip', `This is a ${name}, not an ESP32-C5. Nothing was written.`, { chip: name });
  await loader.spiAttach();
  return { chipId: si.chipId, name, eco: si.eco, secureBoot: !!(si.flags & 1), flashEncryption: !!(si.flashCrypt & 1) };
}

/**
 * Writes images [{name, offset, data}], differential by 4 KB sector MD5,
 * MD5-verifies each image and retries it once. progress({phase, done, total, sectorsWritten}).
 */
export async function writeImages(loader, imgs, { differential = true, flashSize = 8 << 20, progress = () => {}, signal } = {}) {
  await loader.setParams(flashSize);
  const total = imgs.reduce((a, i) => a + i.data.length, 0);
  const rep = { sectorsWritten: 0, sectorsSkipped: 0, parts: [] };
  let done = 0;
  for (const im of imgs) {
    if (im.offset % SECTOR) throw new RomError('verify', `${im.name}: offset not sector aligned`);
    if (im.offset + im.data.length > flashSize) throw new RomError('verify', `${im.name} does not fit into ${flashSize >> 20} MB flash`);
    const want = md5(im.data);
    let ok = false, retried = false, w = 0, s = 0;
    for (let attempt = 0; attempt < 2 && !ok; attempt++) {
      if (attempt) retried = true;
      ({ w, s } = await writeOne(loader, im, differential && attempt === 0, (k, phase) => progress({ phase, part: im.name, done: done + k, total, sectorsWritten: rep.sectorsWritten + w }), signal));
      ok = (await loader.md5(im.offset, im.data.length)) === want;
    }
    rep.parts.push({ name: im.name, offset: im.offset, size: im.data.length, sectorsWritten: w, sectorsSkipped: s, verified: ok, retried });
    rep.sectorsWritten += w; rep.sectorsSkipped += s;
    if (!ok) throw new RomError('verify', `${im.name}: flash content does not match after writing (MD5)`, { report: rep });
    done += im.data.length;
  }
  progress({ phase: 'done', done: total, total, sectorsWritten: rep.sectorsWritten });
  return rep;
}

async function writeOne(loader, im, diff, prog, signal) {
  const n = Math.ceil(im.data.length / SECTOR);
  const changed = new Array(n).fill(true);
  if (diff) {
    for (let i = 0; i < n; i++) {
      if (signal?.aborted) throw new RomError('abort', 'Cancelled');
      const lo = i * SECTOR, hi = Math.min(im.data.length, lo + SECTOR);
      changed[i] = (await loader.md5(im.offset + lo, hi - lo)) !== md5(im.data.subarray(lo, hi));
      prog(lo, 'compare');
    }
  }
  let w = 0, s = 0;
  for (let i = 0; i < n;) {
    if (!changed[i]) { s++; i++; continue; }
    let j = i;
    while (j < n && changed[j]) j++;
    const lo = i * SECTOR, hi = Math.min(im.data.length, j * SECTOR);
    const run = im.data.subarray(lo, hi);
    const blocks = await loader.flashBegin(im.offset + lo, run.length);
    for (let q = 0; q < blocks; q++) {
      if (signal?.aborted) throw new RomError('abort', 'Cancelled (the dongle stays in download mode; flash again to finish)');
      await loader.flashData(q, run.subarray(q * FLASH_WRITE_SIZE, (q + 1) * FLASH_WRITE_SIZE));
      prog(lo + Math.min(run.length, (q + 1) * FLASH_WRITE_SIZE), 'write');
    }
    w += j - i;
    i = j;
  }
  return { w, s };
}

/** esp_app_desc_t from an app image → {version, project, idf, buildId(hex16)}; null if not an app image. */
export function parseAppDesc(app) {
  const off = 32;
  if (app.length < off + 256 || app[0] !== 0xe9) return null;
  const dv = new DataView(app.buffer, app.byteOffset + off);
  if (dv.getUint32(0, true) !== 0xabcd5432) return null;
  const str = (a, b) => new TextDecoder().decode(app.subarray(off + a, off + b)).replace(/\0.*$/s, '');
  const sha = app.subarray(off + 144, off + 152);
  return { version: str(16, 48), project: str(48, 80), idf: str(112, 144), buildId: Array.from(sha, (b) => b.toString(16).padStart(2, '0')).join('') };
}

// ---- MD5 (RFC 1321): browsers' WebCrypto has no MD5; the ROM speaks MD5 -------------------
const K = Array.from({ length: 64 }, (_, i) => Math.floor(Math.abs(Math.sin(i + 1)) * 2 ** 32) >>> 0);
const R = [7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
  4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21];
export function md5(data) {
  const n = data.length, padLen = ((n + 8) >> 6) + 1 << 6;
  const buf = new Uint8Array(padLen);
  buf.set(data); buf[n] = 0x80;
  const bits = n * 8;
  const dv = new DataView(buf.buffer);
  dv.setUint32(padLen - 8, bits >>> 0, true); dv.setUint32(padLen - 4, Math.floor(bits / 2 ** 32), true);
  let a0 = 0x67452301, b0 = 0xefcdab89, c0 = 0x98badcfe, d0 = 0x10325476;
  const M = new Uint32Array(16);
  for (let o = 0; o < padLen; o += 64) {
    for (let i = 0; i < 16; i++) M[i] = dv.getUint32(o + i * 4, true);
    let A = a0, B = b0, C = c0, D = d0;
    for (let i = 0; i < 64; i++) {
      let F, g;
      if (i < 16) { F = (B & C) | (~B & D); g = i; } else if (i < 32) { F = (D & B) | (~D & C); g = (5 * i + 1) & 15; }
      else if (i < 48) { F = B ^ C ^ D; g = (3 * i + 5) & 15; } else { F = C ^ (B | ~D); g = (7 * i) & 15; }
      F = (F + A + K[i] + M[g]) >>> 0;
      A = D; D = C; C = B;
      B = (B + ((F << R[i]) | (F >>> (32 - R[i])))) >>> 0;
    }
    a0 = (a0 + A) >>> 0; b0 = (b0 + B) >>> 0; c0 = (c0 + C) >>> 0; d0 = (d0 + D) >>> 0;
  }
  return [a0, b0, c0, d0].map((v) => [v & 255, (v >>> 8) & 255, (v >>> 16) & 255, v >>> 24].map((x) => x.toString(16).padStart(2, '0')).join('')).join('');
}
