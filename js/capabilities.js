// Browser capability probe (docs/04 §1, §20): which USB API reaches the dongle, plus the
// features the page uses. Shown on the start page when the page loads.
//
// Android: Chrome ships Web Serial there (Chrome 148), but wired ports only work through
// Android's serial API, available on a limited set of devices from 2026 (docs/01). Older
// builds list only Bluetooth RFCOMM ports. So on Android, Web Serial is used only once it
// has shown a USB port on this phone (a granted port with a USB vendor id, or the
// "Test Web Serial" chooser); otherwise WebUSB (own CDC-ACM driver). The result is kept
// per Chrome major version, so an update tests again.
import * as settings from './settings.js';

export function platform(nav = globalThis.navigator ?? {}) {
  const ua = nav.userAgent ?? '';
  const android = /Android/i.test(ua);
  const ios = /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && nav.maxTouchPoints > 1);
  const brand = nav.userAgentData?.brands?.find((b) => /Chrome|Chromium|Edge|Opera/.test(b.brand) && !/Not/.test(b.brand));
  const chrome = Number(brand?.version ?? ua.match(/Chrom(?:e|ium)\/(\d+)/)?.[1] ?? 0);
  const firefox = Number(ua.match(/Firefox\/(\d+)/)?.[1] ?? 0);
  const name = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : firefox ? 'Firefox' : chrome ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'browser';
  return { android, ios, chrome, firefox, name, version: chrome || firefox || null, os: android ? 'Android' : ios ? 'iOS' : /Windows/.test(ua) ? 'Windows' : /Mac OS X/.test(ua) ? 'macOS' : /CrOS/.test(ua) ? 'ChromeOS' : /Linux/.test(ua) ? 'Linux' : '' };
}

/** What we learned about Web Serial on this Android browser: 'works' | 'none' | 'untested'. */
export function androidSerialState(p = platform()) {
  const k = settings.get().androidSerial;
  return k && k.chrome === p.chrome ? k.state : 'untested';
}
export function setAndroidSerial(state, p = platform()) { settings.update((s) => { s.androidSerial = { state, chrome: p.chrome }; }); }

/**
 * The USB API for the dongle: { api: 'serial' | 'webusb' | null, why }.
 * pref: the Settings override ('auto' | 'serial' | 'webusb').
 */
export function chooseTransport(pref = 'auto', nav = globalThis.navigator ?? {}, androidSerial = 'untested') {
  const p = platform(nav), serial = !!nav.serial, usb = !!nav.usb;
  if (pref === 'serial') return { api: serial ? 'serial' : null, why: 'chosen in Settings' };
  if (pref === 'webusb') return { api: usb ? 'webusb' : null, why: 'chosen in Settings' };
  if (!p.android) return serial ? { api: 'serial', why: 'operating system driver' } : usb ? { api: 'webusb', why: 'no Web Serial in this browser' } : { api: null, why: 'neither Web Serial nor WebUSB' };
  if (serial && androidSerial === 'works') return { api: 'serial', why: 'Web Serial showed the USB port on this phone' };
  if (usb) {
    return { api: 'webusb', why: !serial ? 'this Chrome has no Web Serial' : androidSerial === 'none' ? 'Web Serial offered no USB port on this phone' : 'Web Serial has not yet shown a USB port on this phone' };
  }
  return serial ? { api: 'serial', why: 'no WebUSB; Web Serial may only list Bluetooth ports' } : { api: null, why: 'neither Web Serial nor WebUSB' };
}

/** Full probe for the start page. Never throws; each item { name, ok: true|false|null, text }. */
export async function probe(env = globalThis) {
  const nav = env.navigator ?? {}, p = platform(nav), items = [];
  const add = (name, ok, text = '') => items.push({ name, ok, text });
  // Web Serial: on Android, a granted port with a USB vendor id proves wired support
  if (nav.serial) {
    let txt = '';
    if (p.android) {
      try {
        const ports = await nav.serial.getPorts();
        if (ports.some((x) => x.getInfo?.().usbVendorId)) setAndroidSerial('works', p);
      } catch { /* not allowed here */ }
      const st = androidSerialState(p);
      txt = st === 'works' ? 'USB ports: yes' : st === 'none' ? 'USB ports: none offered (Bluetooth only?)' : 'USB ports: untested';
    }
    add('Web Serial', true, txt);
  } else add('Web Serial', false);
  let usbTxt = '';
  if (nav.usb) { try { const n = (await nav.usb.getDevices()).filter((d) => d.vendorId === 0x303a).length; usbTxt = n ? `${n} dongle${n > 1 ? 's' : ''} allowed` : ''; } catch { /* policy */ } }
  add('WebUSB', !!nav.usb, usbTxt);
  const ch = chooseTransport(settings.get().transport, nav, p.android ? androidSerialState(p) : 'untested');
  const dec = env.VideoDecoder;
  const codec = async (c) => { try { return !!(await dec.isConfigSupported({ codec: c })).supported; } catch { return false; } };
  add('H.264 video', dec ? await codec('avc1.42E01F') : false, dec ? '' : 'no WebCodecs');
  add('H.265 video', dec ? await codec('hev1.1.6.L93.B0') : false, dec ? '' : 'no WebCodecs');
  add('Keep screen on', 'wakeLock' in nav);
  add('Offline / install', !!nav.serviceWorker && env.location?.protocol === 'https:', !nav.serviceWorker ? '' : env.location?.protocol !== 'https:' ? 'only on https' : nav.serviceWorker.controller ? 'active' : 'from the next load');
  add('Recording', true, env.showSaveFilePicker ? 'straight to a file' : nav.storage?.getDirectory ? 'browser storage, then download' : 'in memory, then download');
  return { platform: p, secure: !!env.isSecureContext, transport: ch, items };
}
