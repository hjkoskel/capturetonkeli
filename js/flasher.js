// Browser firmware flashing (docs/04 §11.1): flow A (update a CTP dongle),
// flow B (bare / unknown ESP32-C5), flow C (from file). Uses js/romflash.js
// and the M3 hardware rules (docs/10 §3): REBOOT 1 to enter the ROM, SLIP
// probe before anything else, watchdog reset to leave, no DTR/RTS.
import { app } from './app.js';
import * as RF from './romflash.js';
import { SerialTransport, UsbTransport, pickTransport } from './transport.js';
import { Decoder } from './codec.js';
import { Client, sleep } from './protocol.js';
import * as W from './wire.js';
import * as semver from './semver.js';
import * as settings from './settings.js';
import { log } from './log.js';
import { h, dialog, confirmDlg } from './ui/dom.js';

export class FlashError extends Error {
  constructor(title, text, actions = []) { super(title); this.name = 'FlashError'; this.title = title; this.text = text; this.actions = actions; }
}

// ---- firmware sources -----------------------------------------------------------------------

async function sha256hex(u8) {
  const d = await crypto.subtle.digest('SHA-256', u8);
  return Array.from(new Uint8Array(d), (b) => b.toString(16).padStart(2, '0')).join('');
}

/** The firmware bundled with this page (fw/manifest.json + images), sha256-checked. */
export async function bundledFirmware() {
  if (!app.manifest) throw new FlashError('This page has no bundled firmware', 'It is a development page without fw/manifest.json.', ['Use "Flash from file" or ctpbench fw flash.']);
  return loadManifest(app.manifest, async (f) => {
    const r = await fetch(`fw/${f}`, { cache: 'no-cache' });
    if (!r.ok) throw new FlashError(`Could not download fw/${f}`, `HTTP ${r.status}`, ['Reload the page and try again.']);
    return new Uint8Array(await r.arrayBuffer());
  });
}

async function loadManifest(m, get) {
  if (m.chip && m.chip !== 'esp32c5') throw new FlashError('Firmware is not for the ESP32-C5', `The manifest says "${m.chip}".`);
  const images = [];
  for (const p of m.parts) {
    const data = await get(p.file);
    if (p.sha256 && (await sha256hex(data)) !== p.sha256) {
      throw new FlashError(`${p.file} is corrupted`, 'Its SHA-256 does not match the manifest.', ['Reload the page (or re-download the files) and try again.']);
    }
    images.push({ name: p.name, offset: parseInt(p.offset, 16), data });
  }
  const app_ = images.find((i) => i.name === 'app');
  return { manifest: m, images, desc: app_ ? RF.parseAppDesc(app_.data) : null, flashSize: parseInt(m.flash?.size, 10) * (1 << 20) || 8 << 20 };
}

/** Flow C: user-chosen files: manifest.json + .bin files, or a single app .bin (offset 0x10000). */
export async function firmwareFromFiles(files) {
  const byName = new Map([...files].map((f) => [f.name, f]));
  const mf = byName.get('manifest.json');
  if (mf) {
    const m = JSON.parse(await mf.text());
    return loadManifest(m, async (f) => {
      const file = byName.get(f) ?? byName.get(f.split('/').pop());
      if (!file) throw new FlashError(`File ${f} is missing`, 'Select manifest.json together with all .bin files it lists.');
      return new Uint8Array(await file.arrayBuffer());
    });
  }
  if (files.length === 1 && files[0].name.endsWith('.bin')) {
    const data = new Uint8Array(await files[0].arrayBuffer());
    const desc = RF.parseAppDesc(data);
    if (!desc) throw new FlashError('Not an ESP32 app image', `${files[0].name} has no app descriptor. For a merged image use ctpbench fw flash --image FILE --offset 0x0.`);
    return { manifest: { version: desc.version, build_id: desc.buildId }, images: [{ name: 'app', offset: 0x10000, data }], desc, flashSize: 8 << 20 };
  }
  throw new FlashError('Unrecognised selection', 'Pick manifest.json plus its .bin files, or one app .bin.');
}

// ---- progress dialog ------------------------------------------------------------------------------

function progressDialog(title) {
  const bar = h('progress', { max: 100, value: 0, style: { width: '100%' } });
  const line = h('div.small.mono'), step = h('div'), msg = h('div');
  const cancel = h('button', {}, 'Cancel');
  const close = h('button.primary.hidden', {}, 'Close');
  const dlg = h('dialog', h('h2', title), step, bar, line, msg, h('menu', cancel, close));
  document.body.append(dlg);
  dlg.addEventListener('cancel', (e) => e.preventDefault()); // Esc must not hide a running flash
  dlg.showModal();
  const ac = new AbortController();
  cancel.onclick = () => { ac.abort(); cancel.disabled = true; step.textContent = 'Cancelling…'; };
  return {
    signal: ac.signal,
    step: (t) => { step.textContent = t; log('info', t, 'flash'); },
    progress: (p) => {
      bar.value = Math.round((100 * p.done) / Math.max(1, p.total));
      line.textContent = `${p.phase === 'compare' ? 'comparing' : p.phase === 'write' ? 'writing' : p.phase} ${p.part ?? ''} · ${(p.done / 1024).toFixed(0)} / ${(p.total / 1024).toFixed(0)} KB · ${p.sectorsWritten} sectors written`;
    },
    finish: (d, ok) => new Promise((resolve) => {
      cancel.classList.add('hidden');
      close.classList.remove('hidden');
      msg.replaceChildren(h(`div.inline-msg.${ok ? 'ok' : 'error'}`, h('div.b-title', d.title), d.text && h('div', d.text),
        d.actions?.length ? h('ul', d.actions.map((a) => h('li', a))) : null));
      close.onclick = () => { dlg.close(); dlg.remove(); resolve(); };
    }),
  };
}

// ---- finding the dongle across re-enumeration --------------------------------------------------------

async function knownPorts() {
  const T = pickTransport(settings.get().transport);
  return T === SerialTransport ? SerialTransport.known() : T === UsbTransport ? UsbTransport.known() : [];
}

/** Waits for the dongle to (re)appear; asks the user to pick it if the browser lost the permission. */
async function waitForPort(ui, what, ms = 10000) {
  const t0 = performance.now();
  while (performance.now() - t0 < ms) {
    const k = await knownPorts();
    if (k.length === 1) return k[0];
    await sleep(300);
  }
  ui.step(`${what}: please select the dongle again`);
  const pick = await dialog('Select the dongle again', [
    'The dongle restarted and the browser needs you to choose it once more (USB re-enumeration).',
    'In the next window pick "USB JTAG/serial debug unit".'], [{ label: 'Cancel', value: '' }, { label: 'Select dongle', value: 'ok', primary: true }]);
  if (pick !== 'ok') throw new FlashError('Cancelled', 'The dongle is left as it is (it may be in download mode: flash again or unplug it).');
  return pickTransport(settings.get().transport).request();
}

/** Tries the transport as a ROM loader; returns {transport, loader} or null (transport closed). */
async function openRom(t) {
  await t.open();
  const loader = await RF.probe(t, 5);
  if (loader) return { t, loader };
  return { t, loader: null };
}

/** CTP HELLO on an open transport (flow B: is it our firmware?) → client or null. */
async function tryCtp(t) {
  const dec = new Decoder();
  const c = new Client({ write: (u8) => t.write(u8), retries: 1, requestTimeout: 250, pingInterval: 0 });
  t.onData((u8) => { if (u8) dec.feed(u8, (hh, body) => c.ingest(hh, body)); });
  try { await c.hello().catch((e) => { if (e.kind !== 'version') throw e; }); return c; } catch { return null; }
}

// ---- the flash procedure ----------------------------------------------------------------------------

/**
 * fw: {manifest, images, desc, flashSize}; mode: 'update' (A) | 'bare' (B) | 'file' (C).
 * Returns null on success, or {title,text,actions} describing the failure.
 */
export async function flash(fw, mode) {
  const ui = progressDialog(mode === 'update' ? `Update firmware to ${fw.manifest.version}` : `Flash firmware ${fw.manifest.version}`);
  app.flashing = true;
  let t = null;
  try {
    // 1. get the chip into ROM download mode
    if (app.conn === 'connected' && app.client) {
      ui.step('Asking the dongle to enter download mode (REBOOT 1)…');
      await app.client.reboot(1).catch(() => {}); // the reply may be lost while it reboots
      await app.teardown(false);
      app.setConn('disconnected');
      await sleep(800);
      t = await waitForPort(ui, 'Download mode');
    } else {
      if (app.conn !== 'disconnected') await app.disconnect(true);
      const k = await knownPorts();
      t = k.length === 1 ? k[0] : await pickTransport(settings.get().transport).request();
    }
    ui.step('Connecting to the ROM loader…');
    let { loader } = await openRom(t);
    if (!loader) {
      // flow B: maybe our firmware is running → REBOOT 1; otherwise the user needs the BOOT button
      const c = await tryCtp(t);
      if (c) {
        ui.step('capturetonkeli firmware found: entering download mode (REBOOT 1)…');
        await c.reboot(1).catch(() => {});
        await t.close();
        await sleep(800);
        t = await waitForPort(ui, 'Download mode');
        ({ loader } = await openRom(t));
      }
    }
    if (!loader) {
      throw new FlashError('The dongle does not answer in download mode',
        'It runs firmware this page cannot ask to restart, or it is busy.',
        ['Unplug the dongle, hold its BOOT button, plug it in, release BOOT, then press "Flash firmware to a new dongle" again.']);
    }
    const chip = await RF.connect(loader);
    log('info', `ROM: ${chip.name} (eco ${chip.eco})${chip.secureBoot ? ', secure boot' : ''}`, 'flash');
    if (chip.flashEncryption || chip.secureBoot) {
      throw new FlashError('This ESP32-C5 has flash encryption or secure boot enabled', 'Plain images cannot be written. Nothing was written.');
    }
    // 2. write (differential for updates)
    ui.step(mode === 'bare' ? 'Writing the firmware (all sectors, MD5-verified)…' : 'Comparing and writing changed sectors (MD5-verified)…');
    const rep = await RF.writeImages(loader, fw.images, { differential: mode !== 'bare', flashSize: fw.flashSize, progress: ui.progress, signal: ui.signal });
    log('info', `Flash: ${rep.sectorsWritten} sectors written, ${rep.sectorsSkipped} unchanged`, 'flash');
    app.sectorsWritten = (app.sectorsWritten ?? 0) + rep.sectorsWritten;
    // 3. leave the ROM and verify
    ui.step('Restarting the dongle…');
    await loader.watchdogReset();
    await t.close();
    t = null;
    await sleep(1500);
    const nt = await waitForPort(ui, 'Restart');
    app.flashing = false;
    ui.step('Connecting to the new firmware…');
    const d = await app.connect(nt);
    if (d) throw new FlashError('Flashed, but the new firmware does not answer', d.title, ['Unplug and replug the dongle, then Connect.']);
    const got = Array.from(app.info.buildId, (b) => b.toString(16).padStart(2, '0')).join('');
    const want = fw.manifest.build_id ?? fw.desc?.buildId;
    if (want && got !== want) throw new FlashError('The dongle runs a different build than was written', `expected ${want}, got ${got}`, ['Flash again; if it repeats, copy the log and report it.']);
    await ui.finish({ title: `Done: firmware ${W.fwString(app.info.fw)} (build ${got.slice(0, 8)}…)`,
      text: `${rep.sectorsWritten} of ${rep.sectorsWritten + rep.sectorsSkipped} sectors written.` }, true);
    return null;
  } catch (e) {
    const d = e instanceof FlashError ? e : e?.name === 'RomError'
      ? { title: e.kind === 'chip' ? e.message : e.kind === 'abort' ? 'Flashing cancelled' : `Flashing failed: ${e.message}`,
        text: e.kind === 'chip' ? '' : 'The dongle is safe to retry: it stays in download mode until it is flashed or unplugged.',
        actions: e.kind === 'chip' ? ['Use an ESP32-C5 (Seeed XIAO ESP32-C5).'] : ['Press the flash button again to finish.'] }
      : { title: 'Flashing failed', text: e?.message ?? String(e), actions: ['Retry. If it repeats, unplug the dongle, hold BOOT while plugging it in, and use "Flash firmware to a new dongle".'] };
    log('error', `${d.title}${d.text ? ' — ' + d.text : ''}`, 'flash');
    try { await t?.close(); } catch { /* ignore */ }
    await ui.finish(d, false);
    return d;
  } finally {
    app.flashing = false;
  }
}

/** Device tab / mismatch dialog entry points. */
export async function updateToBundled() {
  let fw;
  try { fw = await bundledFirmware(); } catch (e) { return dialog(e.title ?? 'Error', [e.text ?? e.message, ...(e.actions ?? [])]); }
  return flash(fw, 'update');
}

export async function flashBare() {
  let fw;
  try { fw = await bundledFirmware(); } catch (e) { return dialog(e.title ?? 'Error', [e.text ?? e.message, ...(e.actions ?? [])]); }
  const ok = await confirmDlg('Replace the existing firmware on this ESP32-C5?',
    [`Writes capturetonkeli ${fw.manifest.version} to the whole application area. This is the only flow that overwrites non-capturetonkeli firmware. eFuses, secure boot and flash encryption are never touched.`,
      'If the dongle runs other firmware: unplug it, hold BOOT, plug it in, release BOOT, then continue.'], 'Flash', true);
  return ok ? flash(fw, 'bare') : null;
}

export async function flashFromFiles(files) {
  let fw;
  try { fw = await firmwareFromFiles(files); } catch (e) { return dialog(e.title ?? 'Could not use these files', [e.text ?? e.message]); }
  const v = semver.parse(fw.manifest.version ?? fw.desc?.version ?? '');
  const lines = [`Version ${fw.manifest.version ?? '?'}, build ${(fw.manifest.build_id ?? fw.desc?.buildId ?? '?').slice(0, 16)}, IDF ${fw.desc?.idf ?? fw.manifest.idf ?? '?'}.`,
    `Parts: ${fw.images.map((i) => `${i.name} @ 0x${i.offset.toString(16)} (${i.data.length} B)`).join(', ')}.`];
  if (app.hostFw && v && !semver.compatible(app.hostFw, v)) lines.push(h('strong', `This firmware (${semver.toString(v)}) does not match this page (${app.hostFw.major}.${app.hostFw.minor}.x).`));
  if (!(await confirmDlg('Flash this firmware?', lines, 'Flash anyway', true))) return null;
  return flash(fw, 'file');
}
