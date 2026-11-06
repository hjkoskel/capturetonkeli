// JS ROM flasher against an in-memory fake ROM (same model as ctp/romflash FakeROM).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import * as RF from '../../js/romflash.js';

class FakeRom {
  constructor() { this.flash = new Uint8Array(8 << 20).fill(0xff); this.chipId = 23; this.writes = 0; this.resets = 0; this.corrupt = 0; this.begin = null; this.nonSlip = 0; }
  transport() {
    const rom = this;
    const t = {
      cb: () => {},
      onData(cb) { this.cb = cb; },
      async write(u8) { queueMicrotask(() => rom.rx(u8, (out) => t.cb(out))); },
    };
    this.slip = new RF.SlipReader((p) => this.exec(p, (out) => t.cb(out)));
    return t;
  }
  rx(u8, send) { if (u8[0] !== 0xc0) this.nonSlip++; this.send = send; this.slip.feed(u8); }
  exec(p, sendIgnored) {
    const send = this.send ?? sendIgnored;
    const dv = new DataView(p.buffer, p.byteOffset);
    const op = p[1], d = p.subarray(8), chk = dv.getUint32(4, true), dd = new DataView(d.buffer, d.byteOffset, d.byteLength);
    let body = new Uint8Array(0), st = [0, 0];
    switch (op) {
      case RF.CMD.SYNC: case RF.CMD.SPI_ATTACH: case RF.CMD.SPI_SET_PARAMS: break;
      case RF.CMD.SECURITY_INFO: body = new Uint8Array(20); new DataView(body.buffer).setUint32(12, this.chipId, true); break;
      case RF.CMD.WRITE_REG: if (dd.getUint32(0, true) === 0x600b1c00 && dd.getUint32(4, true) >>> 31) this.resets++; break;
      case RF.CMD.FLASH_BEGIN: {
        const size = dd.getUint32(0, true), off = dd.getUint32(12, true);
        this.flash.fill(0xff, off, Math.ceil((off + size) / 4096) * 4096);
        this.begin = { off, next: 0, blocks: dd.getUint32(4, true) };
        break;
      }
      case RF.CMD.FLASH_DATA: {
        const seq = dd.getUint32(4, true), blk = d.slice(16);
        if (!this.begin || seq !== this.begin.next || blk.reduce((c, b) => c ^ b, 0xef) !== chk) { st = [1, 5]; break; }
        if (this.corrupt > 0) { this.corrupt--; blk[0] ^= 0xff; }
        this.flash.set(blk.subarray(0, Math.min(blk.length, this.flash.length - (this.begin.off + seq * 1024))), this.begin.off + seq * 1024);
        this.begin.next++; this.writes++;
        break;
      }
      case RF.CMD.SPI_FLASH_MD5: {
        const a = dd.getUint32(0, true), n = dd.getUint32(4, true);
        body = new TextEncoder().encode(createHash('md5').update(this.flash.subarray(a, a + n)).digest('hex'));
        break;
      }
      default: st = [1, 5];
    }
    const out = new Uint8Array(8 + body.length + 4);
    const o = new DataView(out.buffer);
    out[0] = 1; out[1] = op; o.setUint16(2, body.length + 4, true);
    out.set(body, 8); out[8 + body.length] = st[0]; out[9 + body.length] = st[1];
    queueMicrotask(() => send(RF.slipEncode(out)));
  }
}

test('md5 matches node crypto', () => {
  for (const n of [0, 1, 55, 56, 63, 64, 65, 4096, 70001]) {
    const b = randomBytes(n);
    assert.equal(RF.md5(b), createHash('md5').update(b).digest('hex'), `n=${n}`);
  }
});

test('flash: differential, unchanged = 0 sectors, single sector change, verify', async () => {
  const rom = new FakeRom();
  const l = await RF.probe(rom.transport());
  assert.ok(l, 'probe');
  const chip = await RF.connect(l);
  assert.equal(chip.name, 'ESP32-C5');
  const app = new Uint8Array(randomBytes(70 * 1024 + 123)), boot = new Uint8Array(randomBytes(15000));
  const imgs = [{ name: 'bootloader', offset: 0x2000, data: boot }, { name: 'app', offset: 0x10000, data: app }];
  let r = await RF.writeImages(l, imgs);
  assert.equal(r.sectorsWritten, 4 + 18);
  const w0 = rom.writes;
  r = await RF.writeImages(l, imgs);
  assert.equal(r.sectorsWritten, 0); assert.equal(rom.writes, w0);
  app[5 * 4096 + 7] ^= 1;
  r = await RF.writeImages(l, imgs);
  assert.equal(r.sectorsWritten, 1);
  assert.deepEqual(rom.flash.subarray(0x10000, 0x10000 + app.length), app);
  await l.watchdogReset();
  assert.equal(rom.resets, 1);
  assert.equal(rom.nonSlip, 0, 'only SLIP bytes reach the ROM');
});

test('flash: retry once on verify failure; wrong chip refused', async () => {
  const rom = new FakeRom();
  rom.corrupt = 1;
  const l = await RF.probe(rom.transport());
  await RF.connect(l);
  const r = await RF.writeImages(l, [{ name: 'app', offset: 0x10000, data: new Uint8Array(5000).fill(0x42) }], { differential: false });
  assert.ok(r.parts[0].retried && r.parts[0].verified);
  const c6 = new FakeRom(); c6.chipId = 13;
  const l2 = await RF.probe(c6.transport());
  await assert.rejects(RF.connect(l2), (e) => e.kind === 'chip' && /ESP32-C6, not an ESP32-C5. Nothing was written/.test(e.message));
});

test('app descriptor of the release bundle', async () => {
  const { readFileSync } = await import('node:fs');
  const m = JSON.parse(readFileSync(new URL('../../fw/manifest.json', import.meta.url)));
  const app = new Uint8Array(readFileSync(new URL(`../../fw/${m.parts.find((p) => p.name === 'app').file}`, import.meta.url)));
  const d = RF.parseAppDesc(app);
  assert.equal(d.version, m.version); assert.equal(d.buildId, m.build_id); assert.equal(d.idf, m.idf);
});
