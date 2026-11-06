// Client ↔ demo device end-to-end in node (no browser): the same stack the UI
// uses, minus the worker (frames are decoded directly here).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Decoder } from '../../js/codec.js';
import { DemoDevice } from '../../js/demo-device.js';
import { Client, CtpError, sleep } from '../../js/protocol.js';
import * as W from '../../js/wire.js';
import * as F from '../../js/filter.js';
import { describe as describeErr } from '../../js/errors.js';

function connect(opts = {}) {
  const dec = new Decoder();
  const pkts = [];
  let client;
  const dev = new DemoDevice({
    onBytes: (u8) => dec.feed(u8, (h, body) => (h.type === W.T.PKT ? pkts.push(W.decPkt(body)) : client.ingest(h, body))),
    ...opts.device,
  });
  client = new Client({ write: async (u8) => dev.write(u8), ...opts.client });
  dev.attach();
  return { dev, client, pkts };
}

test('handshake, config readback, capture with filter', async () => {
  const { dev, client, pkts } = connect();
  try {
    const info = await client.hello();
    assert.equal(info.protoVer, 1);
    assert.equal(info.caps & W.CAP.GHZ5, W.CAP.GHZ5);
    let cfg = await client.getConfig();
    assert.equal(cfg.country.cc, '01');
    cfg = await client.push({ country: { cc: 'FI' }, channel: 13, snaplen: 64, filter: { version: 1, rules: F.PRESETS.beacons.rules() }, capture: true });
    assert.equal(cfg.channel, 13); assert.equal(cfg.snaplen, 64); assert.equal(cfg.nRules, 1); assert.ok(cfg.captureOn);
    client.startKeepalive();
    await sleep(1500);
    assert.ok(pkts.length > 5, `got ${pkts.length} packets`);
    for (const p of pkts) {
      assert.equal(p.data[0] & 0xfc, 0x80, 'only beacons pass');
      assert.ok(p.meta.capLen <= 64);
      assert.equal(p.meta.channel, 13);
    }
    assert.ok(client.rttStats().median < 50);
  } finally { await client.close(); dev.detach(); }
});

test('device errors carry codes and map to helpful messages', async () => {
  const { dev, client } = connect();
  try {
    await client.hello();
    await assert.rejects(client.setChannel(13), (e) => e instanceof CtpError && e.kind === 'device' && e.code === W.ERR.CHANNEL_NOT_ALLOWED);
    const d = describeErr(await client.setChannel(13).catch((e) => e), 'Set channel 13');
    assert.match(d.title, /not allowed by the country table/);
    assert.ok(d.actions.length > 0);
    await assert.rejects(client.setCountry('ZZ'), (e) => e.code === W.ERR.COUNTRY_INVALID);
    await assert.rejects(client.capture(true).then(() => client.request(0x7e)), (e) => e.code === W.ERR.UNKNOWN_TYPE);
  } finally { await client.close(); dev.detach(); }
});

test('timeout when the device stalls; recovery after', async () => {
  const { dev, client } = connect({ client: { requestTimeout: 100, retries: 1 } });
  try {
    await client.hello();
    dev.stall(400);
    await assert.rejects(client.getConfig(), (e) => e.kind === 'timeout');
    assert.match(describeErr(new CtpError('timeout', 'x')).title, /did not answer/);
    await sleep(400);
    assert.equal((await client.getConfig()).channel, 1);
  } finally { await client.close(); dev.detach(); }
});

test('device reboot: EVT_RESET triggers re-handshake and re-push', async () => {
  const { dev, client } = connect();
  try {
    await client.hello();
    await client.push({ country: { cc: 'FI' }, channel: 11, snaplen: 100 });
    const repushed = new Promise((r) => client.addEventListener('repushed', (e) => r(e.detail), { once: true }));
    dev.reboot();
    const cfg = await Promise.race([repushed, sleep(3000).then(() => null)]);
    assert.ok(cfg, 'repushed');
    assert.equal(cfg.channel, 11); assert.equal(cfg.snaplen, 100); assert.equal(cfg.country.cc, 'FI');
  } finally { await client.close(); dev.detach(); }
});

test('firmware mismatch is reported as a version error', async () => {
  const { dev, client } = connect({ client: { hostFw: { major: 1, minor: 3, patch: 0 } } });
  try {
    await assert.rejects(client.hello(), (e) => e.kind === 'version' && !e.protocol && e.info.fw.minor === 1);
  } finally { await client.close(); dev.detach(); }
});

test('error descriptions for browser failures', () => {
  const ex = (name, message) => Object.assign(new Error(message), { name });
  const t = (e, re) => assert.match(describeErr(e, 'X').title, re);
  t(ex('SecurityError', "Failed to execute 'requestPort' on 'Serial': Must be handling a user gesture to show a permission request."), /needs a click/);
  t(ex('SecurityError', 'Access denied'), /blocked access to USB/);
  t(ex('NetworkError', 'Failed to open serial port.'), /in use by another program/);
  t(ex('NetworkError', 'The device was disconnected'), /disconnected/);
  t(ex('NotFoundError', 'No port selected by the user.'), /No dongle selected/);
  t(ex('BufferOverrunError', 'overrun'), /could not keep up/);
  t(new Error('Unable to claim the USB interface: busy'), /operating system holds/);
  t(ex('SecurityError', "Failed to execute 'showSaveFilePicker' on 'Window': nope"), /file for writing/);
  t(new CtpError('device', 'x', { code: W.ERR.SAFE_MODE, espErr: 0 }), /safe mode/);
  for (const d of [describeErr(undefined), describeErr('plain string'), describeErr({ weird: 1 })]) assert.ok(d.title && d.level);
});
