// docs/14 multi-dongle diversity, host side: merge, clock alignment, bad copies.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Merger, HOLD_MS } from '../../js/diversity.js';
import * as P from '../../js/pcapng.js';

function rng(seed) { let a = seed; return () => { a = (a + 0x6d2b79f5) | 0; let t = Math.imul(a ^ (a >>> 15), 1 | a); t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t; return ((t ^ (t >>> 14)) >>> 0) / 4294967296; }; }
const meta = (tsUs, data, extra = {}) => ({ tsUs, channel: 6, flags: 0, rssi: -50, origLen: data.length, ...extra });
let seqNo = 0;
function dataFrame(r, n = 60) { // QoS data with a sequence number: unique content
  const f = new Uint8Array(n); f[0] = 0x88; f[1] = 0x02; f[22] = (++seqNo << 4) & 0xff; f[23] = seqNo >> 4;
  for (let i = 24; i < n; i++) f[i] = Math.floor(r() * 256);
  return f;
}
const ACK = Uint8Array.from([0xd4, 0, 0, 0, 2, 0x11, 0x22, 0x33, 6, 1]); // identical every time

/**
 * Air: frames at air time t (µs). Dongle A (ref, lower MAC) has device time = t; B has
 * t·(1+ppm) + 5 s offset and ±jitter; each copy reaches the host after 1–9 ms (USB batching).
 * Returns the copies in host arrival order.
 */
function scenario({ seconds = 3, ppm = 23, jitter = 4, frames, lossA = 0.1, lossB = 0.1, seed = 1 }) {
  const r = rng(seed), out = [];
  for (const f of frames(r, seconds)) {
    const ta = f.t, tb = f.t * (1 + ppm * 1e-6) + 5e6 + (r() - 0.5) * 2 * jitter;
    if (r() >= lossA) out.push({ id: 0, ts: Math.round(ta), host: f.t / 1000 + 1 + r() * 8, data: f.data, bad: f.badA });
    if (r() >= lossB) out.push({ id: 1, ts: Math.round(tb), host: f.t / 1000 + 1 + r() * 8, data: f.data, bad: f.badB });
  }
  return out.sort((a, b) => a.host - b.host);
}
function run(copies, m = new Merger()) {
  if (!m.size) m.setMembers([{ id: 1, mac: '02:c5:de:40:00:02' }, { id: 0, mac: '02:c5:de:40:00:01' }]);
  const emitted = [];
  for (const c of copies) {
    const data = c.bad ? Uint8Array.from(c.data, (x, i) => (i === 30 ? x ^ 4 : x)) : c.data;
    const r = m.offer(c.id, meta(c.ts, data, { flags: c.bad ? 1 : 0 }), data, c.host);
    if (r.dup === false) { emitted.push({ data, entry: r.entry }); m.bind(r.entry, emitted.length - 1); }
    for (const h of m.due(c.host)) emitted.push({ data: h.data, bad: true });
  }
  m.flush();
  for (const h of m.due(Infinity)) emitted.push({ data: h.data, bad: true });
  return { m, emitted };
}

test('Diversity merge: each air frame once, clock lock with drift, unique contribution', () => {
  const frames = function* (r, s) { for (let t = 0; t < s * 1e6; t += 2000 + r() * 2000) yield { t, data: dataFrame(r) }; };
  const copies = scenario({ frames });
  const nAir = new Set(copies.map((c) => c.data)).size;
  const { m, emitted } = run(copies);
  assert.equal(emitted.length, nAir, 'every air frame exactly once');
  const s = m.summary(), b = s.members.find((x) => x.id === 1);
  assert.equal(s.ref, 0, 'the lowest MAC is the reference');
  assert.ok(b.clock.locked, 'B locked');
  assert.ok(Math.abs(b.clock.ppm - 23) < 3, `ppm ${b.clock.ppm}`);
  assert.ok(b.clock.rmsUs < 6, `rms ${b.clock.rmsUs}`);
  // frames heard by only one dongle are its unique contribution (~10 % loss on the other)
  const a = s.members.find((x) => x.id === 0);
  assert.ok(a.unique > nAir * 0.05 && b.unique > nAir * 0.05, `unique A ${a.unique} B ${b.unique} of ${nAir}`);
  assert.equal(s.out, nAir);
  assert.equal(s.out + s.dups, copies.length, 'every copy is either the output frame or a duplicate');
});

test('Diversity merge: identical ACKs 300 µs apart are not merged once locked', () => {
  // 2 s of data frames to lock, then ACK bursts (same bytes) between them
  const frames = function* (r, s) {
    for (let t = 0; t < s * 1e6; t += 3000) {
      yield { t, data: dataFrame(r) };
      if (t > 2e6) for (let k = 1; k <= 3; k++) yield { t: t + 300 * k, data: ACK };
    }
  };
  const copies = scenario({ frames, lossA: 0, lossB: 0, seconds: 3 });
  const acksInAir = copies.filter((c) => c.data === ACK).length / 2;
  const { emitted } = run(copies);
  assert.equal(emitted.filter((e) => e.data === ACK).length, acksInAir, 'no false merges, no duplicates');
});

test('Diversity merge: a bad copy only adds RSSI when another dongle got the frame right', () => {
  const frames = function* (r, s) { for (let t = 0; t < s * 1e6; t += 4000) yield { t, data: dataFrame(r), badA: r() < 0.2 }; };
  const copies = scenario({ frames, lossA: 0, lossB: 0, seconds: 2 });
  const { m, emitted } = run(copies);
  assert.equal(emitted.filter((e) => e.bad).length, 0, 'no bad frame emitted: B had a good copy of each');
  assert.equal(emitted.length, copies.length / 2);
  assert.equal(m.summary().rescued, copies.filter((c) => c.bad).length);
  // bad and alone: emitted after the hold
  const m2 = new Merger();
  m2.setMembers([{ id: 0, mac: 'a' }, { id: 1, mac: 'b' }]);
  const d = dataFrame(rng(3));
  assert.deepEqual(m2.offer(0, meta(1000, d, { flags: 1 }), d, 0), { held: true });
  assert.equal(m2.due(HOLD_MS - 1).length, 0);
  assert.equal(m2.due(HOLD_MS).length, 1);
});

test('pcapng: per-antenna RSSI in radiotap extended namespaces (single dongle unchanged)', () => {
  const m = { tsUs: 123456, channel: 6, flags: 0, rssi: -40, noise: -95, rate: 12, bbFormat: 1, origLen: 30, capLen: 30 };
  assert.deepEqual(P.radiotap(m), P.radiotap(m, null), 'no antennas → same bytes as before');
  const rt = P.radiotap(m, [{ ant: 0, rssi: -40 }, { ant: 2, rssi: -61 }]);
  const dv = new DataView(rt.buffer);
  assert.equal(dv.getUint16(2, true), rt.length);
  const w0 = dv.getUint32(4, true), w1 = dv.getUint32(8, true), w2 = dv.getUint32(12, true);
  assert.ok(w0 & (1 << 31) && w0 & (1 << 29), 'first word: ext + radiotap namespace');
  assert.equal(w1, ((1 << 5) | (1 << 11) | (1 << 29) | (1 << 31)) >>> 0, 'antenna 1: signal + antenna, more follow');
  assert.equal(w2, (1 << 5) | (1 << 11));
  // TSFT is 8-aligned after 3 presence words (16 B): starts at 16
  assert.equal(Number(dv.getBigUint64(16, true)), 123456);
  // the antenna fields come last: signal, antenna index per namespace
  const tail = Array.from(rt.subarray(rt.length - 4));
  assert.deepEqual(tail, [(-40) & 0xff, 0, (-61) & 0xff, 2]);
});

test('Diversity clock: stale anchor pairs do not break the fit; a member leaving mid-window is safe', () => {
  const r = rng(9), m = new Merger();
  m.setMembers([{ id: 0, mac: 'a' }, { id: 1, mac: 'b' }]);
  let host = 0;
  for (let t = 0; t < 3e6; t += 3000) {
    const d = dataFrame(r);
    host = t / 1000 + 2;
    m.offer(0, meta(t, d), d, host);
    m.offer(1, meta(Math.round(t * (1 + 20e-6) + 7e6), d), d, host + 1);
    if (t % 300000 === 0) m.members.get(1).clock.anchor(t * (1 + 20e-6) + 7e6, t + 3e5); // 300 ms wrong (stale backlog)
  }
  const c = m.summary().members.find((x) => x.id === 1).clock;
  assert.ok(c.locked && Math.abs(c.ppm - 20) < 3, JSON.stringify(c));
  // B's first copy is open, B leaves, A's copy of the same frame arrives: no crash, still one frame
  const d = dataFrame(r);
  m.offer(1, meta(Math.round(3.1e6 * (1 + 20e-6) + 7e6), d), d, host + 5);
  m.setMembers([{ id: 0, mac: 'a' }, { id: 2, mac: 'c' }]);
  assert.equal(m.offer(0, meta(3.1e6, d), d, host + 6).dup, true);
});

test('Diversity + OpenHD: late copies from another dongle complete FEC blocks (hold); loss counts the union', async () => {
  const O = await import('../../js/openhd.js');
  const r = rng(5);
  const rtp = (seq, n) => { const p = new Uint8Array(n); p[0] = 0x80; p[1] = 96; p[2] = seq >> 8; p[3] = seq & 255; return p; };
  // OpenHD air unit: video frames of 3 FEC blocks (k = 8, +20 %) back to back, every 33 ms
  const tx = new O.Sender({ rng: r });
  const air = [{ t: -50, f: tx.session() }]; // the session key is known before the video starts
  let seq = 0;
  for (let i = 0; i < 300; i++) for (let b = 0; b < 3; b++) for (const f of tx.video(Array.from({ length: 8 }, () => rtp(seq++, 1000)))) air.push({ t: i * 33 + b * 0.5 + air.length * 0.01, f, blk: i * 3 + b });
  // two dongles, 15 % independent loss each, USB delivery 1–9 ms later; first copy wins
  const copies = [];
  for (const a of air) for (let d = 0; d < 2; d++) if (a.t < 0 || r() >= 0.15) copies.push({ host: a.t + 1 + r() * 8, f: a.f, id: a });
  copies.sort((x, y) => x.host - y.host);
  const result = {};
  for (const holdMs of [0, HOLD_MS]) {
    let rtpOut = 0, now = 0;
    const rx = new O.Receiver({ onRtp: () => rtpOut++, now: () => now });
    rx.setOptions({ holdMs });
    const seen = new Set();
    for (const c of copies) if (!seen.has(c.id)) { seen.add(c.id); now = c.host; rx.push(c.f); }
    const u = rx.summary().units[0];
    result[holdMs] = { rtpOut, lost: u.fec[0].blocksLost, nonceLoss: u.st.nonceGaps / u.st.nonceSpan };
  }
  const got = new Set(copies.map((c) => c.id)), unionLoss = 1 - got.size / air.length;
  // blocks neither dongle can rebuild: more than m = 2 of their 10 fragments heard by nobody
  const missing = new Map();
  for (const a of air) if (a.blk !== undefined && !got.has(a)) missing.set(a.blk, (missing.get(a.blk) ?? 0) + 1);
  const hopeless = [...missing.values()].filter((n) => n > 2).length;
  assert.ok(result[0].lost > 100, `without hold the late copies are useless: ${JSON.stringify(result)}`);
  assert.equal(result[HOLD_MS].lost, hopeless, `with the hold only blocks both dongles lost beyond FEC are lost: ${JSON.stringify(result)}`);
  assert.ok(Math.abs(result[HOLD_MS].nonceLoss - unionLoss) < 0.005, `loss ${result[HOLD_MS].nonceLoss} vs union ${unionLoss}`);
});
