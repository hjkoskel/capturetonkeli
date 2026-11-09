// docs/04 §1/§20: transport choice and the capability probe, with simulated browsers.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chooseTransport, platform, probe, androidSerialState, setAndroidSerial } from '../../js/capabilities.js';

const UA = {
  androidChrome: 'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Mobile Safari/537.36',
  linuxChrome: 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36',
  firefox: 'Mozilla/5.0 (X11; Linux x86_64; rv:152.0) Gecko/20100101 Firefox/152.0',
  iphone: 'Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.0 Mobile/15E148 Safari/604.1',
};
const port = (vid) => ({ getInfo: () => (vid ? { usbVendorId: vid, usbProductId: 0x1001 } : {}) });
const nav = (ua, { serial = false, usb = false, ports = [] } = {}) => ({ userAgent: ua, serial: serial ? { getPorts: async () => ports } : undefined, usb: usb ? { getDevices: async () => [] } : undefined });

test('platform detection', () => {
  assert.deepEqual([platform(nav(UA.androidChrome)).android, platform(nav(UA.androidChrome)).chrome, platform(nav(UA.androidChrome)).os], [true, 148, 'Android']);
  assert.equal(platform(nav(UA.firefox)).name, 'Firefox');
  assert.equal(platform(nav(UA.iphone)).ios, true);
});

test('transport choice: desktop Web Serial; Android WebUSB until Web Serial shows a USB port', () => {
  assert.equal(chooseTransport('auto', nav(UA.linuxChrome, { serial: true, usb: true })).api, 'serial');
  assert.equal(chooseTransport('auto', nav(UA.firefox, { serial: true })).api, 'serial');
  assert.equal(chooseTransport('auto', nav(UA.iphone)).api, null);
  const a = nav(UA.androidChrome, { serial: true, usb: true });
  assert.equal(chooseTransport('auto', a, 'untested').api, 'webusb');
  assert.equal(chooseTransport('auto', a, 'none').api, 'webusb');
  assert.equal(chooseTransport('auto', a, 'works').api, 'serial');
  assert.equal(chooseTransport('auto', nav(UA.androidChrome, { usb: true }), 'untested').api, 'webusb', 'older Chrome: no Web Serial');
  assert.equal(chooseTransport('serial', a).api, 'serial', 'Settings override');
  assert.equal(chooseTransport('webusb', nav(UA.linuxChrome, { serial: true, usb: true })).api, 'webusb');
});

test('probe: an allowed USB serial port on Android proves Web Serial; per Chrome version', async () => {
  const env = (ports) => ({ navigator: nav(UA.androidChrome, { serial: true, usb: true, ports }), isSecureContext: true, location: { protocol: 'https:' } });
  setAndroidSerial('untested', platform(nav(UA.androidChrome)));
  let c = await probe(env([port(0)])); // a Bluetooth port only
  assert.equal(c.transport.api, 'webusb');
  assert.match(c.items.find((x) => x.name === 'Web Serial').text, /untested/);
  c = await probe(env([port(0x303a)]));
  assert.equal(c.transport.api, 'serial');
  assert.equal(androidSerialState(platform(nav(UA.androidChrome))), 'works');
  // a Chrome update tests again
  assert.equal(androidSerialState(platform(nav(UA.androidChrome.replace('148', '149')))), 'untested');
  assert.equal(c.items.find((x) => x.name === 'H.264 video').ok, false, 'no WebCodecs in this environment');
});
