// OpenHD passive receiver (docs/13). Fixtures:
//   ohd-sodium.json   libsodium output for every primitive (generator: docs/13 §11)
//   ohd-gen-{a,b}.json frames built with OpenHD wifibroadcast's own Encryptor, FECEncoder,
//                      Ieee80211Header and FECDisabledEncoder + OpenHD MAVLink headers
//   ohd-rtp.json      GStreamer x264/x265 → rtph26xpay mtu=1440 packets and the elementary streams
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as C from '../../js/ohdcrypto.js';
import * as FEC from '../../js/ohdfec.js';
import * as O from '../../js/openhd.js';
import * as K from '../../js/ohdkeys.js';
import * as M from '../../js/mavlink.js';
import * as R from '../../js/rtp.js';
import * as MP4 from '../../js/mp4.js';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import * as Flt from '../../js/filter.js';

const fx = (n) => JSON.parse(readFileSync(new URL(`fixtures/${n}`, import.meta.url)));
const hx = O.hexToBytes, toHex = O.bytesToHex;
const S = fx('ohd-sodium.json');

test('OpenHD crypto primitives match libsodium', () => {
  for (const v of S.poly1305) assert.equal(toHex(C.poly1305(hx(v.key), hx(v.msg))), v.tag, `poly1305 len ${v.msg.length / 2}`);
  for (const v of S.hsalsa20) assert.equal(toHex(C.hsalsa20(hx(v.key), hx(v.in))), v.out);
  for (const v of S.hchacha20) assert.equal(toHex(C.hchacha20(hx(v.key), hx(v.in))), v.out);
  for (const v of S.chacha20) assert.equal(toHex(C.chacha20Xor(hx(v.key), hx(v.nonce), hx(v.msg), v.ic)), v.out, `chacha20 ic ${v.ic}`);
  for (const v of S.secretbox) {
    assert.equal(toHex(C.secretboxSeal(hx(v.msg), hx(v.nonce), hx(v.key))), v.box);
    assert.equal(toHex(C.secretboxOpen(hx(v.box), hx(v.nonce), hx(v.key))), v.msg);
    const bad = hx(v.box); bad[0] ^= 1;
    assert.equal(C.secretboxOpen(bad, hx(v.nonce), hx(v.key)), null);
  }
  for (const v of S.aead) {
    assert.equal(toHex(C.aeadSeal(hx(v.key), BigInt(v.nonce), hx(v.msg))), v.ct);
    assert.equal(toHex(C.aeadOpen(hx(v.key), BigInt(v.nonce), hx(v.ct))), v.msg);
    assert.equal(C.aeadOpen(hx(v.key), BigInt(v.nonce) + 1n, hx(v.ct)), null);
  }
});

test('OpenHD keys: X25519, BLAKE2b, Argon2id, seed keypair, beforenm match libsodium', async () => {
  for (const v of S.x25519) {
    assert.equal(toHex(K.x25519(hx(v.scalar), hx(v.u))), v.out);
    assert.equal(toHex(K.x25519(hx(v.scalar))), v.base);
  }
  for (const v of S.box) {
    const kp = await K.seedKeypair(hx(v.seed));
    assert.equal(toHex(kp.pk), v.pk); assert.equal(toHex(kp.sk), v.sk);
    assert.equal(toHex(K.beforenm(hx(v.peer_pk), kp.sk)), v.beforenm);
  }
  for (const v of S.blake2b) assert.equal(toHex(K.blake2b(v.outlen, hx(v.msg))), v.out, `blake2b ${v.msg.length / 2}/${v.outlen}`);
  for (const v of S.argon2id) assert.equal(toHex(await K.argon2id(hx(v.pw), hx(v.salt), { t: v.t, m: v.m_kib, tagLen: v.out.length / 2 })), v.out, `argon2id t=${v.t} m=${v.m_kib}`);
  // OpenHD's own keys (Encryption.cpp generate_keypair_from_bind_phrase / txrx.key layout)
  const k = fx('ohd-gen-a.json').keys;
  const file = hx(k.openhd.key1_pub + k.openhd.key1_sec + k.openhd.key2_pub + k.openhd.key2_sec);
  assert.deepEqual(K.keysFromKeyFile(file), { ...O.DEFAULT_KEYS });
  assert.throws(() => K.keysFromKeyFile(file.subarray(1)), /128 bytes/);
});

test('OpenHD keys: custom bind phrase (full Argon2id, 2 × 64 MiB) matches OpenHD', { timeout: 120000 }, async () => {
  const c = fx('ohd-gen-a.json').keys.custom;
  const want = K.keysFromKeyFile(hx(c.key1_pub + c.key1_sec + c.key2_pub + c.key2_sec));
  let last = 0;
  assert.deepEqual(await K.keysFromPhrase(c.phrase, (p) => { assert.ok(p >= last); last = p; }), want);
  assert.equal(last, 1);
});

test('OpenHD FEC: Cauchy code encodes and recovers any pattern of losses', () => {
  const k = 6, m = 3, L = 300;
  const syms = Array.from({ length: k }, (_, j) => Uint8Array.from({ length: 50 + j * 40 }, (_, i) => (i * 7 + j * 13) & 0xff));
  const sec = FEC.encode(syms, m, L);
  for (let mask = 0; mask < 1 << k; mask++) {
    const lost = [...Array(k).keys()].filter((j) => mask & (1 << j));
    if (lost.length > m) continue;
    const prim = new Map(syms.map((s, j) => [j, s]).filter(([j]) => !lost.includes(j)));
    const rec = FEC.decode(k, prim, new Map(sec.map((s, r) => [r, s])), L);
    for (const j of lost) assert.deepEqual(rec.get(j).subarray(0, syms[j].length), syms[j]);
  }
});

for (const gen of ['a', 'b']) {
  test(`OpenHD receiver decodes OpenHD-generated frames (Gen ${gen.toUpperCase()})`, () => {
    const v = fx(`ohd-gen-${gen}.json`);
    const rtp = [[], []], tele = [], mgmt = [], sess = [];
    let t = 0;
    const rx = new O.Receiver({ now: () => t, onRtp: (u, cam, p) => rtp[cam].push(toHex(p)), onTelemetry: (u, d, dir) => tele.push({ dir, d: toHex(d) }),
      onMgmt: (u, x) => mgmt.push(x), onSession: (u, kind) => sess.push(kind) });
    let i = 0;
    for (const fr of v.frames) {
      if (fr.kind === 'session' && i++ === 1) t += 2000; // the restart happens 2 s later
      if (!fr.drop) rx.push(hx(fr.frame), { rssi: -60, channel: 149 });
    }
    assert.deepEqual(rtp[0], v.expect.rtp0);
    assert.deepEqual(rtp[1], v.expect.rtp1);
    assert.deepEqual(tele, [{ dir: 1, d: v.expect.telemetry[0] }, { dir: 2, d: tele[1]?.d }]); // air (deduplicated) + ground uplink
    assert.deepEqual(mgmt, [{ freq: 5745, bw: 20 }]);
    assert.deepEqual(sess, ['new', 'new', 'restart']);
    const s = rx.summary(t);
    const air = s.units.find((u) => u.dir === 1);
    assert.equal(air.gen, gen.toUpperCase());
    assert.equal(air.st.restarts, 1);
    assert.equal(s.g.authFail, 1); // the frame with the corrupted tag
    const f0 = air.fec[0];
    assert.equal(f0.blocksLost, 0);
    assert.ok(f0.recovered >= 0);
    // telemetry content: ArduPilot quad in Loiter, armed; OpenHD air unit 2.6.4
    const fs = new M.FlightState();
    fs.add(M.parse(hx(v.expect.telemetry[0])), 0);
    const sum = fs.summary(0);
    assert.equal(sum.badCrc, 0);
    assert.deepEqual({ ap: sum.flight.autopilot, v: sum.flight.vehicle, armed: sum.flight.armed, mode: sum.flight.mode, sats: sum.flight.gps.satellites },
      { ap: 'ArduPilot', v: 'quadcopter', armed: true, mode: 'Loiter', sats: 14 });
    assert.deepEqual(sum.flight.hud, { airspeed: 12.5, groundspeed: 11, alt: 42, climb: -0.5, heading: 270, throttle: 55 });
    assert.equal(sum.flight.battery.voltage, 15.8);
    assert.deepEqual(sum.openhd.OPENHD_VERSION_MESSAGE, { major: 2, minor: 6, patch: 4, releaseType: 0 });
    assert.equal(sum.openhd.OPENHD_CAMERA_STATUS_AIR.width, 1280);
    assert.equal(sum.openhd.OPENHD_CORE_STATUS.cpuTemp, 63);
    assert.equal(sum.texts[0].text, 'Hello from the air');
  });
}

test('OpenHD receiver: wrong bind phrase, unverified video, two systems', () => {
  const v = fx('ohd-gen-a.json');
  const other = { air: '11'.repeat(32), gnd: null };
  let n = 0;
  const rx = new O.Receiver({ keys: other, onRtp: () => n++ });
  for (const fr of v.frames) if (!fr.drop) rx.push(hx(fr.frame));
  const s = rx.summary();
  assert.equal(n, 0);
  assert.ok(s.g.badSession[1] >= 2 && s.g.noSession > 0);
  // keyless: video can still be shown, marked unverified
  const rx2 = new O.Receiver({ keys: other, unverified: true, onRtp: () => n++ });
  for (const fr of v.frames) if (!fr.drop) rx2.push(hx(fr.frame));
  assert.ok(n > 0);
  assert.equal(rx2.summary().units.find((u) => !u.verified).dir, 1);
  // two OpenHD systems with the default phrase on one channel: two units
  const got = new Map();
  const rx3 = new O.Receiver({ onRtp: (u, cam, p) => got.set(u.id, (got.get(u.id) ?? 0) + 1) });
  const rnd = mulberry(5);
  const a = new O.Sender({ rng: rnd, nonce: 1000n }), b = new O.Sender({ rng: rnd, nonce: 7000000000n });
  rx3.push(a.session()); rx3.push(b.session());
  for (let i = 0; i < 5; i++) {
    for (const f of a.video([rtpPkt(i, 300)])) rx3.push(f);
    for (const f of b.video([rtpPkt(i, 400)])) rx3.push(f);
  }
  assert.deepEqual([...got.values()], [5, 5]);
});

test('OpenHD FEC stream: in-order low-latency forwarding, Q, loss and duplicates', () => {
  const tx = new O.Sender({ rng: mulberry(9) });
  const out = [];
  const rx = new O.Receiver({ onRtp: (u, cam, p, info) => out.push({ seq: (p[2] << 8) | p[3], ...info }) });
  rx.push(tx.session());
  let seq = 0;
  const blocks = [];
  for (let b = 0; b < 6; b++) blocks.push(tx.video(Array.from({ length: 5 }, () => rtpPkt(seq++, 200 + (seq * 37) % 900)), { m: 2 }));
  // block 0 clean; block 1 loses 2 primaries (recovered); block 2 loses 3 (lost); block 3 duplicated
  blocks[0].forEach((f) => rx.push(f));
  // first primary is forwarded immediately, before the block is complete
  blocks[1].forEach((f, i) => { if (i !== 1 && i !== 3) rx.push(f); });
  blocks[2].forEach((f, i) => { if (i > 2) rx.push(f); });
  blocks[3].forEach((f) => { rx.push(f); rx.push(f); });
  blocks[4].forEach((f) => rx.push(f));
  const u = rx.summary().units[0], st = u.fec[0];
  assert.deepEqual(out.map((o) => o.seq), [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24]);
  assert.ok(out.find((o) => o.seq === 6).recovered);
  assert.ok(out.find((o) => o.seq === 15).gap);
  assert.deepEqual({ ok: st.blocksOk, fec: st.blocksFec, lost: st.blocksLost }, { ok: 3, fec: 1, lost: 1 });
  assert.ok(st.duplicates > 0 && st.secUnneeded > 0);
  // forwardGapped: primaries after the gap of a lost block are delivered too
  const out2 = [];
  const rx2 = new O.Receiver({ forwardGapped: true, onRtp: (uu, cam, p) => out2.push((p[2] << 8) | p[3]) });
  rx2.push(tx.session());
  blocks[2].forEach((f, i) => { if (i !== 1 && i !== 2 && i !== 3) rx2.push(f); });
  blocks[3].forEach((f) => rx2.push(f));
  assert.deepEqual(out2, [10, 14, 15, 16, 17, 18, 19]);
});

test('OpenHD Sender ↔ Receiver (Gen B, encrypted video, telemetry, management)', () => {
  const tx = new O.Sender({ rng: mulberry(3), genB: true });
  const got = { rtp: 0, tele: [], mg: null };
  const rx = new O.Receiver({ onRtp: () => got.rtp++, onTelemetry: (u, d) => got.tele.push(toHex(d)), onMgmt: (u, x) => { got.mg = x; } });
  rx.push(tx.session());
  for (const f of tx.video([rtpPkt(1, 1441), rtpPkt(2, 10)], { enc: true })) rx.push(f);
  for (const f of tx.telemetry(Uint8Array.of(1, 2, 3))) rx.push(f);
  rx.push(tx.mgmt(5785, 40));
  assert.equal(got.rtp, 2);
  assert.deepEqual(got.tele, ['010203']);
  assert.deepEqual(got.mg, { freq: 5785, bw: 40 });
  assert.equal(rx.summary().units[0].gen, 'B');
});

test('RTP: GStreamer H.264/H.265 packets → the encoder\'s access units', () => {
  const v = fx('ohd-rtp.json');
  for (const c of ['h264', 'h265']) {
    const aus = [];
    const d = new R.Depacketizer({ onAU: (a) => aus.push(a) });
    for (const p of v[c].rtp) d.push(hx(p));
    const nals = aus.flatMap((a) => R.splitAnnexB(a.data).map(toHex));
    assert.deepEqual(nals, R.splitAnnexB(hx(v[c].es)).map(toHex), c);
    assert.equal(aus[0].codec, c);
    assert.ok(aus[0].key && aus[0].hasParams);
    assert.equal(aus[0].codecString, c === 'h264' ? 'avc1.42c014' : 'hev1.1.6.L60.90');
    // our packetizer (demo mode) round-trips
    const pk = new R.Packetizer({ codec: c }), back = [];
    const d2 = new R.Depacketizer({ codec: c, onAU: (a) => back.push(toHex(a.data)) });
    for (const a of aus) for (const p of pk.packetize(a.data, a.ts)) { assert.ok(p.length <= 1440); d2.push(p); }
    assert.deepEqual(back, aus.map((a) => toHex(a.data)));
  }
  // a lost packet marks the access unit damaged; parameter sets are re-sent with the next key frame
  const aus = [];
  const d = new R.Depacketizer({ codec: 'h264', onAU: (a) => aus.push(a) });
  v.h264.rtp.forEach((p, i) => { if (i !== 3) d.push(hx(p)); });
  assert.ok(aus.some((a) => a.damaged));
});

function boxes(b, from = 0, to = b.length) { // top-level ISO BMFF boxes → {type: [start, end]}
  const dv = new DataView(b.buffer, b.byteOffset), out = {};
  for (let i = from; i + 8 <= to;) { const n = dv.getUint32(i), t = String.fromCharCode(...b.subarray(i + 4, i + 8)); out[t] = [i, i + n]; assert.ok(n >= 8, t); i += n; }
  return out;
}
const cat = (parts) => { const o = new Uint8Array(parts.reduce((a, p) => a + p.length, 0)); let i = 0; for (const p of parts) { o.set(p, i); i += p.length; } return o; };

test('MP4: GStreamer H.264/H.265 access units → playable MP4 with RTP timing', () => {
  const v = fx('ohd-rtp.json');
  const tmp = mkdtempSync(join(tmpdir(), 'ctk-mp4-'));
  try {
    for (const c of ['h264', 'h265']) {
      const aus = [];
      const d = new R.Depacketizer({ onAU: (a) => aus.push(a) });
      for (const p of v[c].rtp) d.push(hx(p));
      const sps = R.splitAnnexB(aus[0].data).find((n) => (c === 'h264' ? (n[0] & 0x1f) === 7 : ((n[0] >> 1) & 0x3f) === 33));
      const info = MP4.spsInfo(c, sps);
      assert.equal(`${info.width}x${info.height}`, '320x240', c);
      const files = MP4.mp4Files(c, aus);
      assert.equal(files.length, 1);
      assert.equal(files[0].frames, aus.length);
      const f = cat(files[0].parts), top = boxes(f);
      assert.deepEqual(Object.keys(top), ['ftyp', 'moov', 'mdat'], c);
      // the stco offset points at the first sample: a 4-byte NAL length, not a start code
      const mdatStart = top.mdat[0] + 8, dv = new DataView(f.buffer);
      assert.equal(dv.getUint32(mdatStart) + 4 <= f.length - mdatStart, true);
      const moov = f.subarray(top.moov[0], top.moov[1]), s = toHex(moov);
      assert.ok(s.includes(toHex(new TextEncoder().encode(c === 'h264' ? 'avcC' : 'hvcC'))));
      // independent check with ffprobe/ffmpeg when installed (CI may not have it)
      const file = join(tmp, `t-${c}.mp4`);
      writeFileSync(file, f);
      const pr = spawnSync('ffprobe', ['-v', 'error', '-count_frames', '-show_entries', 'stream=codec_name,width,height,avg_frame_rate,nb_read_frames', '-of', 'compact=p=0:nk=1', file], { encoding: 'utf8' });
      if (pr.error) continue;
      assert.equal(pr.stderr, '', c);
      const [codec, w, hh, rate, n] = pr.stdout.trim().split('|'), [rn, rd] = rate.split('/').map(Number);
      assert.deepEqual([codec, w, hh, Number(n)], [c === 'h264' ? 'h264' : 'hevc', '320', '240', aus.length], c);
      assert.ok(Math.abs(rn / rd - 30) < 0.1, `${c} frame rate ${rate}`); // GStreamer's RTP ticks alternate 2999/3000
      const dec = spawnSync('ffmpeg', ['-v', 'error', '-i', file, '-f', 'null', '-'], { encoding: 'utf8' });
      assert.equal(dec.stderr, '', c);
    }
  } finally { rmSync(tmp, { recursive: true, force: true }); }
});

test('MP4: timing gaps, SPS change splits the file, no key frame → nothing', () => {
  const v = fx('ohd-rtp.json');
  const aus = [];
  const d = new R.Depacketizer({ onAU: (a) => aus.push(a) });
  for (const p of v.h264.rtp) d.push(hx(p));
  // drop two frames: the timeline keeps the gap (stts carries the longer duration)
  const lost = aus.filter((_, i) => i !== 4 && i !== 5);
  const f = cat(MP4.mp4Files('h264', lost)[0].parts), s = toHex(f);
  assert.ok(s.includes(toHex(new Uint8Array([0, 0, 0x23, 0x28]))), 'a 9000-tick (3 frame) sample duration');
  // starting without a key frame: units are skipped until one arrives
  assert.equal(MP4.mp4Files('h264', aus.filter((a) => !a.key)).length, 0);
  // a different SPS (e.g. another resolution; here the last byte changed) starts a second file
  const mod = aus.map((a) => ({ ...a }));
  const k2 = mod.findIndex((a, i) => i > 0 && a.key);
  const nals = R.splitAnnexB(mod[k2].data).map((n) => ((n[0] & 0x1f) === 7 ? Uint8Array.from(n, (x, i) => (i === n.length - 1 ? x ^ 1 : x)) : n));
  mod[k2] = { ...mod[k2], data: cat(nals.flatMap((n) => [Uint8Array.of(0, 0, 0, 1), n])) };
  assert.equal(MP4.mp4Files('h264', mod).length, 2);
});

test('MAVLink: CRC_EXTRA framing, v1, signing, truncation, tlog', () => {
  const hb = new Uint8Array(9);
  new DataView(hb.buffer).setUint32(0, 3, true); hb[4] = 1; hb[5] = 3; hb[6] = 129;
  const f = M.encode({ id: 0, sys: 1, comp: 1, seq: 7 }, hb);
  const [m] = M.parse(f);
  assert.equal(m.crcOk, true);
  assert.equal(M.flightMode(M.decode(m)), 'Training'); // plane, custom mode 3
  const bad = Uint8Array.from(f); bad[12] ^= 1;
  assert.equal(M.parse(bad)[0].crcOk, false);
  assert.equal(M.parse(Uint8Array.of(0xfd, 1, 0, 0, 0, 1, 1, 0x39, 0x30, 0, 5, 0, 0))[0].crcOk, null); // unknown id 12345
  // MAVLink v1 heartbeat
  const v1 = Uint8Array.of(0xfe, 9, 0, 1, 1, 0, ...hb, 0, 0);
  const crc = M.crcX25([50], M.crcX25(v1.subarray(1, 15)));
  v1[15] = crc & 0xff; v1[16] = crc >> 8;
  assert.equal(M.parse(v1)[0].crcOk, true);
  // garbage between frames is skipped
  assert.equal(M.parse(Uint8Array.from([1, 2, ...f, 3, ...f])).length, 2);
  const rec = M.tlogRecord(f, 1700000000000);
  assert.equal(new DataView(rec.buffer).getBigUint64(0, false), 1700000000000000n);
});

test('OpenHD capture filter and channel helpers', () => {
  const tx = new O.Sender({ rng: mulberry(1) }), gnd = new O.Sender({ rng: mulberry(2), uid: O.UID.GND });
  const frames = [tx.session(), ...tx.video([rtpPkt(1, 100)]), ...tx.video([rtpPkt(2, 100)], { enc: true }), ...tx.telemetry(Uint8Array.of(1)), tx.mgmt(5745, 20)];
  const gf = [gnd.session(), ...gnd.telemetry(Uint8Array.of(1))];
  const ctx = (f) => ({ fcsErr: false, rssi: -50, origLen: f.length });
  for (const ground of [false, true]) {
    const c = Flt.compile(O.captureFilter({ ground }));
    assert.ok(Flt.encode(O.captureFilter({ ground })).length > 0);
    assert.ok(frames.every((f) => Flt.match(c, f, ctx(f))));
    assert.equal(gf.filter((f) => Flt.match(c, f, ctx(f))).length, ground ? gf.length : 0);
    const beacon = new Uint8Array(60); beacon[0] = 0x80;
    assert.ok(!Flt.match(c, beacon, ctx(beacon)));
  }
  assert.deepEqual(O.RACE_BAND_MHZ.map(O.freqToChannel), [140, 149, 157, 165, 173, 52, 56]);
  assert.equal(O.freqToChannel(2452), 9);
  assert.equal(O.freqToChannel(5000), 0);
});

function rtpPkt(seq, len) {
  const p = new Uint8Array(len);
  p[0] = 0x80; p[1] = 96; p[2] = seq >> 8; p[3] = seq & 0xff;
  for (let i = 12; i < len; i++) p[i] = (i * seq) & 0xff;
  return p;
}
function mulberry(a) {
  return () => { a |= 0; a = (a + 0x6d2b79f5) | 0; let t = Math.imul(a ^ (a >>> 15), 1 | a); t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t; return ((t ^ (t >>> 14)) >>> 0) / 4294967296; };
}
