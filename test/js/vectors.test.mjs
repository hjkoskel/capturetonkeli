// JS codec/wire/filter/pcapng vs the shared golden vectors (docs/05 §9).
// Run: node --test test/js/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as codec from '../../js/codec.js';
import * as wire from '../../js/wire.js';
import * as filter from '../../js/filter.js';
import * as pcapng from '../../js/pcapng.js';
import * as dot11 from '../../js/dot11.js';
import * as semver from '../../js/semver.js';

const V = (f) => JSON.parse(readFileSync(new URL(`../../testvectors/${f}`, import.meta.url)));
const { hex, unhex } = codec;

test('cobs', () => {
  for (const v of V('cobs.json')) {
    assert.equal(hex(codec.cobsEncode(unhex(v.in_hex))), v.out_hex, v.name);
    assert.equal(hex(codec.cobsDecode(unhex(v.out_hex))), v.in_hex, v.name);
  }
});

test('crc32', () => {
  for (const v of V('crc32.json')) assert.equal(codec.crc32(unhex(v.in_hex)), v.crc);
});

test('frames: encode + streams', () => {
  const f = V('frames.json');
  for (const v of f.valid) {
    const h = { ver: v.header.Ver, type: v.header.Type, seq: v.header.Seq, reqId: v.header.ReqID, flags: v.header.Flags, cfgGen: v.header.CfgGen };
    assert.equal(hex(codec.encodeFrame(h, unhex(v.body_hex), false)), v.wire_hex);
  }
  for (const s of f.streams) {
    for (const chunk of [1, 7, 1 << 20]) {
      const d = new codec.Decoder();
      const ids = [];
      const b = unhex(s.stream_hex);
      for (let o = 0; o < b.length; o += chunk) d.feed(b.subarray(o, o + chunk), (h) => ids.push(h.reqId));
      assert.deepEqual(ids, s.req_ids, s.name);
      assert.equal(d.stats.crcErrors, s.crc_errors, s.name);
      assert.equal(d.stats.cobsErrors, s.cobs_errors, s.name);
      assert.equal(d.stats.oversize, s.oversize, s.name);
      assert.equal(d.stats.short, s.short, s.name);
    }
  }
});

test('messages', () => {
  const byName = Object.fromEntries(V('messages.json').map((m) => [m.name, m]));
  const body = (n) => unhex(byName[n].body_hex);
  const F = (n) => byName[n].fields;
  assert.deepEqual(wire.decHello(body('HELLO')), { hostNonce: F('HELLO')[0], hostProtoVer: 1 });
  assert.equal(hex(wire.encHello(0xdeadbeef)), byName.HELLO.body_hex);
  assert.equal(hex(wire.encSetChannel(149, 2)), byName.SET_CHANNEL.body_hex);
  assert.equal(hex(wire.encCountry('FI', 1, 1, 13)), byName.SET_COUNTRY.body_hex);
  assert.equal(hex(wire.encSnaplen(1500)), byName.SET_SNAPLEN.body_hex);
  assert.equal(hex(wire.encDriverMask(7, 0xff800000)), byName.SET_DRIVER_MASK.body_hex);
  assert.deepEqual(wire.decSetChannel(body('SET_CHANNEL (+trailing)')), { channel: 149, bw: 2 });
  const di = wire.decDeviceInfo(body('RSP_OK DeviceInfo'));
  assert.equal(di.fwVersion, 0x01030002); assert.equal(di.hostNonce, 0xcafebabe); assert.equal(di.resetReason, 6);
  assert.equal(di.deviceTimeUs, 0x0102030405060708n);
  assert.equal(hex(wire.encDeviceInfo(di)), byName['RSP_OK DeviceInfo'].body_hex);
  for (const n of ['RSP_OK Config', 'RSP_OK boot Config']) {
    const c = wire.decConfig(body(n));
    assert.equal(hex(wire.encConfig(c)), byName[n].body_hex, n);
  }
  const cfg = wire.decConfig(body('RSP_OK Config'));
  assert.equal(cfg.country.cc, 'FI'); assert.equal(cfg.rulesCrc32, 0x1a2b3c4d); assert.equal(cfg.captureOn, true);
  const st = wire.decStats(body('EVT_STATS'));
  assert.equal(st.noiseFloor, -95); assert.equal(st.ringHighWater, 42);
  assert.equal(hex(wire.encStats(st)), byName.EVT_STATS.body_hex);
  const e = wire.decRspErr(body('RSP_ERR'));
  assert.deepEqual(e, { code: 4, espErr: 0x3002, detail: 'not in country table' });
  assert.equal(hex(wire.encRspErr(4, 0x3002, 'not in country table')), byName.RSP_ERR.body_hex);
  const r = wire.decReset(body('EVT_RESET'));
  assert.equal(r.detail, 'proto.c:123');
  assert.equal(hex(wire.encReset(6, 0x01030002, 1, 'proto.c:123')), byName.EVT_RESET.body_hex);
  const p = wire.decPkt(body('PKT'));
  assert.equal(p.meta.rssi, -61); assert.equal(p.meta.rxSeq, 65535); assert.equal(hex(p.data), '88022c00');
  assert.equal(hex(wire.encPkt(p.meta, p.data)), byName.PKT.body_hex);
  assert.throws(() => wire.decHello(Uint8Array.of(1)), wire.ShortBody);
});

test('filter encode: JSON → body + CRC', () => {
  for (const v of V('filter_encode.json')) {
    const b = filter.encode(v.filter);
    assert.equal(hex(b), v.body_hex, v.name);
    assert.equal(codec.crc32(b), v.crc, v.name);
  }
});

test('filter match vectors', () => {
  const enc = V('filter_encode.json');
  const m = V('filter_match.json');
  const compiled = enc.map((v) => filter.decode(unhex(v.body_hex)));
  const frames = m.frames_hex.map(unhex);
  for (const c of m.cases) {
    const got = filter.match(compiled[c.filter], frames[c.frame], { fcsErr: !!(c.flags & 1), rssi: c.rssi, origLen: c.orig_len });
    assert.equal(got, c.expect, `filter ${enc[c.filter].name} frame ${c.frame}`);
  }
});

test('filter invalid bodies rejected', () => {
  for (const v of V('filter_invalid.json')) assert.throws(() => filter.decode(unhex(v.body_hex)), filter.FilterError, v.name);
});

test('filter pattern parser', () => {
  assert.deepEqual(filter.parsePattern('8?'), { value: Uint8Array.of(0x80), mask: Uint8Array.of(0xf0) });
  assert.equal(filter.parsePattern('aa:bb:cc:dd:ee:ff').value.length, 6);
  assert.deepEqual(filter.parsePattern('80/fc').mask, Uint8Array.of(0xfc));
  for (const bad of ['', '8', 'zz', '80/f']) assert.throws(() => filter.parsePattern(bad), filter.FilterError, bad);
});

test('pcapng EPB bytes', () => {
  const v = V('pcapng.json');
  assert.equal(hex(pcapng.header({ userAppl: 'capturetonkeli', hardware: 'XIAO ESP32-C5' })), v.header_hex);
  for (const p of v.packets) {
    const M = p.meta;
    const m = { tsUs: M.TsUS, channel: M.Channel, flags: M.Flags, rssi: M.RSSI, noise: M.Noise, rate: M.Rate, bbFormat: M.BBFormat, origLen: M.OrigLen, capLen: M.CapLen, rxSeq: M.RxSeq };
    const ants = p.antennas?.map((a) => ({ ant: a.Index, rssi: a.RSSI })) ?? null;
    assert.equal(hex(pcapng.epb(m, unhex(p.data_hex), p.comment ?? '', ants)), p.epb_hex, p.name);
  }
});

test('dot11 + semver', () => {
  const b = unhex('80000000ffffffffffff020102030405020102030405300100000000000000006400110400046b6f746903010630020100');
  const h = dot11.parse(b);
  assert.equal(h.name, 'Beacon'); assert.equal(h.ssid, 'koti'); assert.equal(h.dsChannel, 6); assert.equal(h.rsn, true);
  assert.equal(dot11.parse(Uint8Array.of(0x80)).name, 'runt');
  assert.ok(semver.compatible(semver.parse('1.3.0'), semver.parse('v1.3.7')));
  assert.ok(!semver.compatible(semver.parse('1.3.0'), semver.parse('1.4.0')));
  assert.deepEqual(semver.parse('0.1.0-dev'), { major: 0, minor: 1, patch: 0, pre: 'dev' });
});
