// hx-esp32-cam-fpv receiver (docs/12). Fixtures in test/js/fixtures/hxfpv-*.json
// were produced from the hx-esp32-cam-fpv C sources (fec.cpp, packets.h, crc.cpp).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as F from '../../js/hxfec.js';
import * as H from '../../js/hxfpv.js';
import * as Flt from '../../js/filter.js';

const fx = (n) => JSON.parse(readFileSync(new URL(`fixtures/${n}`, import.meta.url)));
const hex = (s) => Uint8Array.from(s.match(/../g), (x) => parseInt(x, 16));

test('hx FEC: encoding matrices and blocks match the C reference', () => {
  const v = fx('hxfpv-fec.json');
  for (const m of v.matrices) assert.deepEqual(F.encRows(m.k, m.n), hex(m.rows), `k=${m.k}`);
  for (const b of v.blocks) {
    const p = b.packets.map(hex);
    for (let i = b.k; i < b.n; i++) assert.deepEqual(F.encode(b.k, p.slice(0, b.k), i, b.size), p[i], `k=${b.k} fec ${i}`);
    // every way of losing data packets that leaves ≥ k packets: try a few patterns
    for (let drop = 1; drop <= Math.min(b.k, b.n - b.k); drop++) {
      const have = new Map();
      for (let i = 0; i < b.n; i++) if (i >= drop) have.set(i, p[i]);
      const rec = F.decode(b.k, have, b.size);
      for (let i = 0; i < drop; i++) assert.deepEqual(rec.get(i), p[i], `k=${b.k} lost ${drop}, packet ${i}`);
    }
  }
  assert.throws(() => F.decode(6, new Map([[0, new Uint8Array(4)]]), 4), /need 6 packets/);
});

test('hx packets: CRC-8 and parsers match the C structs', () => {
  const v = fx('hxfpv-packets.json');
  assert.equal(H.crc8(new TextEncoder().encode('123456789')), v.crc8_123456789);
  const video = hex(v.video);
  assert.ok(H.crcOk(video, H.VIDEO_HDR));
  assert.deepEqual(H.parseA2G(video), { type: 0, size: 23, pong: 7, version: 2, crc: video[7], air: 0xa1b2, gs: 0xc3d4 });
  assert.equal(video[13] & 0x7f, 3); assert.ok(video[13] & 0x80);
  const osd = hex(v.osd);
  assert.equal(osd.length, H.OSD_SIZE);
  assert.ok(H.crcOk(osd, H.OSD_SIZE));
  const s = H.parseAirStats(osd);
  assert.deepEqual({ rate: s.wifiRate, q: s.quality, ov: s.ov5640, free: s.sdFreeGB, total: s.sdTotalGB, rssi: s.rssiDbm, nf: s.noiseFloorDbm, fps: s.captureFps },
    { rate: 20, q: 17, ov: true, free: 20, total: 32, rssi: -63, nf: -92, fps: 30 });
  assert.deepEqual({ min: s.frameSizeMin, max: s.frameSizeMax, ch: s.wifiChannel, res: s.resolution, t: s.temperature, k: s.fecK, ses: s.inSession, asp: s.aspect },
    { min: 9000, max: 21000, ch: 7, res: 5, t: 51, k: 6, ses: true, asp: 2 });
  assert.deepEqual({ b: s.brightness, c: s.contrast, sa: s.saturation, sh: s.sharpness, ae: s.aeLevel, rec: s.airRecording, qmax: s.wifiQueueMax },
    { b: -2, c: 1, sa: -1, sh: 3, ae: 2, rec: true, qmax: 33 });
  const cfg = hex(v.config);
  assert.equal(cfg.length, H.CONFIG_SIZE);
  assert.ok(H.crcOk(cfg, H.CONFIG_SIZE));
  assert.deepEqual(H.parseConfig(cfg), { resolution: 10, fpsLimit: 50, quality: 12, wifiPower: -3, wifiRate: 22, wifiChannel: 11, fecK: 4, fecN: 8, mtu: 1464 });
  osd[20] ^= 1;
  assert.ok(!H.crcOk(osd, H.OSD_SIZE));
});

function fakeJpeg(n, seed) {
  const j = new Uint8Array(n);
  let x = seed;
  for (let i = 0; i < n; i++) { x = (x * 1103515245 + 12345) >>> 0; j[i] = x >>> 24; }
  j.set([0xff, 0xd8, 0xff, 0xc0, 0, 11, 8, 0x01, 0xe0, 0x02, 0x80], 0); // SOF0 480×640
  j.set([0xff, 0xd9], n - 2);
  return j;
}

function run({ frames = 20, size = 9000, k = 6, n = 12, loss = () => false }) {
  const tx = new H.Sender({ air: 0xbeef, gs: 0x4242, k, n });
  const got = [];
  const rx = new H.Receiver({ onFrame: (u, jpeg, info) => got.push({ id: u.id, jpeg, info }) });
  const sent = [];
  let seq = 0;
  for (let i = 0; i < frames; i++) {
    const j = fakeJpeg(size + i * 37, i + 1);
    sent.push(j);
    const slots = H.videoSlots(j, { frameIndex: i, resolution: 3, air: 0xbeef, gs: 0x4242 });
    if (i % 5 === 0) slots.push(H.osdSlot({ air: 0xbeef, gs: 0x4242, fecK: k }));
    for (const f of tx.push(slots)) if (!loss(seq++, f)) rx.push(f, { rssi: -50, channel: 7 });
  }
  return { got, sent, rx };
}

test('hx receiver: clean link delivers every frame unchanged', () => {
  const { got, sent, rx } = run({});
  const done = got.length;
  assert.ok(done >= 18, `${done} frames`); // the last block may still be waiting for data
  for (const g of got) assert.deepEqual(g.jpeg, sent[g.info.index]);
  const u = rx.summary().units[0];
  assert.equal(u.id, 0xbeef); assert.equal(u.gs, 0x4242); assert.equal(u.k, 6); assert.equal(u.kSource, 'OSD');
  assert.equal(u.st.framesFec, 0); assert.equal(u.st.crcErrors, 0); assert.equal(u.st.framesLost, 0);
  assert.equal(u.osd.captureFps, 30);
  assert.deepEqual(H.jpegSize(got[0].jpeg), { width: 640, height: 480 });
});

test('hx receiver: FEC restores lost packets, too many losses lose the frame only', () => {
  // lose up to 6 of 12 packets per block (data packets first): all frames still arrive via FEC
  const r1 = run({ loss: (i) => i % 12 < 4 || i % 12 === 7 });
  assert.ok(r1.got.length >= 18);
  for (const g of r1.got) assert.deepEqual(g.jpeg, r1.sent[g.info.index]);
  const u1 = r1.rx.summary().units[0];
  assert.ok(u1.st.blocksFec > 0 && u1.st.framesFec > 0 && u1.st.blocksLost === 0, JSON.stringify(u1.st));
  // block 3 loses 7 packets: unrecoverable; frames touching it are lost, later frames recover
  const r2 = run({ loss: (i) => Math.floor(i / 12) === 3 && i % 12 < 7 });
  const u2 = r2.rx.summary().units[0];
  assert.ok(u2.st.blocksLost >= 1 && u2.st.framesLost >= 1, JSON.stringify(u2.st));
  assert.ok(r2.got.some((g) => g.info.index > 6));
  for (const g of r2.got) assert.deepEqual(g.jpeg, r2.sent[g.info.index]);
});

test('hx receiver: duplicates, foreign frames, wrong version, two air units', () => {
  const rx = new H.Receiver();
  const tx1 = new H.Sender({ air: 1 }), tx2 = new H.Sender({ air: 2 });
  for (const f of tx1.push(H.videoSlots(fakeJpeg(3000, 1), { frameIndex: 0 }))) { rx.push(f); rx.push(f); }
  for (const f of tx2.push(H.videoSlots(fakeJpeg(3000, 2), { frameIndex: 0 }))) rx.push(f);
  const s = rx.summary();
  assert.deepEqual(s.units.map((u) => u.id).sort(), [1, 2]);
  assert.ok(s.units.find((u) => u.id === 1).st.duplicates > 0);
  const beacon = new Uint8Array(60); beacon[0] = 0x80;
  assert.equal(rx.push(beacon), false);
  const old = tx1.push(H.videoSlots(fakeJpeg(100, 3), { frameIndex: 1 }))[0];
  old[24] = 1;
  assert.equal(rx.push(old), true);
  assert.equal(rx.summary().wrongVersion, 1);
});

test('hx receiver: k follows the air unit (config/OSD) and restart is detected', () => {
  const got = [];
  const rx = new H.Receiver({ onFrame: (u, j, info) => got.push(info.index) });
  const tx = new H.Sender({ air: 7, k: 4, n: 8 });
  // first an OSD packet announcing k=4 (packet 0 of the block is always a data packet)
  for (const f of tx.push([H.osdSlot({ air: 7, fecK: 4 })])) rx.push(f);
  for (let i = 0; i < 6; i++) for (const f of tx.push(H.videoSlots(fakeJpeg(5000, i + 1), { frameIndex: i }))) rx.push(f);
  assert.equal(rx.summary().units[0].k, 4);
  assert.ok(got.length >= 5, String(got));
  // air unit restarts: block index and frame index back to 0
  const tx2 = new H.Sender({ air: 7, k: 4, n: 8 });
  for (let i = 0; i < 300; i++) tx.block++; // make the old session far ahead
  const before = got.length;
  const rx2 = rx;
  for (let i = 0; i < 400; i++) for (const f of tx.push(H.videoSlots(fakeJpeg(200, i), { frameIndex: 10 + i }))) rx2.push(f);
  for (let i = 0; i < 5; i++) for (const f of tx2.push(H.videoSlots(fakeJpeg(5000, i + 9), { frameIndex: i }))) rx2.push(f);
  assert.ok(rx2.summary().units[0].st.restarts >= 1);
  assert.ok(got.slice(before).includes(3), 'frames after the restart are delivered');
});

test('hx capture filter: valid for the dongle and selects hx packets by index', () => {
  for (const keep of [0, 8, 16]) {
    const c = Flt.compile(H.captureFilter(keep));
    const tx = new H.Sender({ k: 6, n: 12 });
    const frames = tx.push(Array.from({ length: 6 }, () => new Uint8Array(1464)));
    const passed = frames.filter((f) => Flt.match(c, f, { fcsErr: false, rssi: -50, origLen: f.length })).length;
    assert.equal(passed, keep === 8 ? 8 : 12, `keep ${keep}`);
    assert.ok(Flt.encode(H.captureFilter(keep)).length > 0);
  }
  const beacon = new Uint8Array(60); beacon[0] = 0x80;
  assert.ok(!Flt.match(Flt.compile(H.captureFilter(8)), beacon, { fcsErr: false, rssi: -50, origLen: 60 }));
});
