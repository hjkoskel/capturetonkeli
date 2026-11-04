import { test } from 'node:test';
import assert from 'node:assert/strict';
import { compileDisplayFilter as C } from '../../js/display-filter.js';
import { unhex } from '../../js/codec.js';

const beacon = { meta: { channel: 6, rssi: -50, noise: -95, origLen: 60, capLen: 60, flags: 0, rate: 12, bbFormat: 1 },
  data: unhex('80000000ffffffffffff020102030405020102030405300100000000000000006400110400046b6f746903010630020100') };
const ack = { meta: { channel: 6, rssi: -80, noise: -95, origLen: 14, capLen: 14, flags: 1, rate: 2, bbFormat: 0 }, data: unhex('d4000000aabbccddeeff') };
const run = (e, p) => C(e)({ ...p });

test('display filter expressions', () => {
  assert.ok(run('type==beacon', beacon));
  assert.ok(!run('type==beacon', ack));
  assert.ok(run('ssid~"KO"', beacon));
  assert.ok(run('rssi>-60 and ch==6', beacon));
  assert.ok(run('fcs_err || type==ack', ack));
  assert.ok(run('not mgmt', ack));
  assert.ok(run('src==02-01-02-03-04-05', beacon));
  assert.ok(run('(len>200 or ssid=="koti") && !fcs_err', beacon));
  assert.ok(run('type==probe-req', { ...beacon, data: unhex('40000000ffffffffffff021122334455ffffffffffff1000') }));
});

test('display filter errors are explained', () => {
  assert.throws(() => C('rssi>'), /value expected/);
  assert.throws(() => C('bogus==1'), /unknown field "bogus"/);
  assert.throws(() => C('rssi>abc'), /not a number/);
  assert.throws(() => C('(type==beacon'), /"\)" expected/);
  assert.throws(() => C('ssid<3'), /only works on numbers/);
  assert.throws(() => C('rssi'), /needs a comparison/);
});
