// docs/01 §6: WebUSB read loop statistics (simulated device).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { UsbTransport } from '../../js/transport.js';

test('WebUSB read loop: bytes, full reads, delivered data, re-queue', async () => {
  const waiting = [];
  const dev = { transferIn: (ep, size) => new Promise((resolve) => waiting.push({ resolve, size })) };
  const t = new UsbTransport(dev);
  Object.assign(t, { epIn: 1, inflight: 4, size: 64, closed: false });
  t.resetLinkStats();
  const got = [];
  t.onData((u8) => got.push(u8.length));
  for (let i = 0; i < 4; i++) t.inLoop();
  const done = (n) => ({ status: 'ok', data: new DataView(new ArrayBuffer(n)) });
  assert.equal(waiting.length, 4);
  assert.equal(waiting[0].size, 64);
  waiting.splice(0).forEach((w, i) => w.resolve(done(i === 0 ? 64 : i === 3 ? 0 : 10)));
  await new Promise((r) => setTimeout(r, 0));
  const l = t.linkStats();
  assert.deepEqual([l.reads, l.bytes, l.full, l.empty], [4, 84, 1, 1]);
  assert.deepEqual(got, [64, 10, 10], 'empty reads deliver nothing');
  assert.equal(waiting.length, 4, 'every completed read is queued again');
  t.closed = true;
});
