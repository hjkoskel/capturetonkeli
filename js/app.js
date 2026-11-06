// App controller: connection state machine (docs/04 §4), worker bridge,
// config actions with error reporting, recording/export (§10).
import { Client, CtpError, LINK, sleep } from './protocol.js';
import { pickTransport, DemoTransport, SerialTransport, UsbTransport } from './transport.js';
import { describe } from './errors.js';
import * as W from './wire.js';
import * as semver from './semver.js';
import * as settings from './settings.js';
import { log } from './log.js';

class App extends EventTarget {
  constructor() {
    super();
    this.conn = 'disconnected';
    this.transport = null;
    this.client = null;
    this.info = null;
    this.config = null;
    this.devStats = null;
    this.host = null;
    this.manifest = null;
    this.hostFw = null;
    this.unsupportedFw = false;
    this.recording = null;
    this.pendingWorker = new Map();
    this.wid = 0;
    this.worker = new Worker(new URL('./worker.js', import.meta.url), { type: 'module' });
    this.worker.onmessage = (e) => this.onWorker(e.data);
    this.worker.onerror = (e) => this.fail('worker', new Error(`The packet worker crashed: ${e.message}`), 'Packet processing');
  }

  emit(type, detail) { this.dispatchEvent(new CustomEvent(type, { detail })); }
  setConn(s) { this.conn = s; this.emit('conn', s); }

  /** Reports an error: log + banner (link-level) or returns the description for inline display. */
  fail(where, err, context, { bannerKey = null, quiet = false } = {}) {
    const d = describe(err, context);
    log(d.level === 'info' ? 'info' : d.level, `${d.title}${d.detail ? ' — ' + d.detail : ''}`, where);
    if (!quiet && bannerKey) this.emit('banner', { key: bannerKey, d });
    return d;
  }

  async loadManifest() {
    try {
      const r = await fetch('fw/manifest.json', { cache: 'no-cache' });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      this.manifest = await r.json();
      this.hostFw = semver.parse(this.manifest.version);
    } catch {
      this.manifest = null;
      this.hostFw = null; // development page: no bundled firmware, no version check
      log('info', 'No bundled firmware (fw/manifest.json missing): firmware version check disabled', 'page');
    }
  }

  // ---- worker bridge ---------------------------------------------------------------------
  onWorker(m) {
    switch (m.t) {
      case 'frame':
        if (settings.get().showRaw) log('debug', `rx ${W.typeName(m.h.type)} seq=${m.h.seq} req=${m.h.reqId} gen=${m.h.cfgGen} ${m.body.length} B`, 'link');
        this.client?.ingest(m.h, new Uint8Array(m.body.buffer, m.body.byteOffset, m.body.byteLength));
        break;
      case 'tick': this.host = m.s; this.emit('host', m.s); break;
      case 'fpv-frame': this.emit('fpv-frame', m); break;
      case 'ohd-au': this.emit('ohd-au', m); break;
      case 'ohd-rec-data': this.emit('ohd-rec-data', m); break;
      case 'recdata': this.writeRec(m.buf); break;
      case 'rec-stopped': this.finishRec(m.packets); break;
      case 'error': this.fail('worker', new Error(m.message), 'Packet processing', { bannerKey: 'worker' }); break;
      default: {
        const p = this.pendingWorker.get(m.id);
        if (p) { this.pendingWorker.delete(m.id); p(m); }
      }
    }
  }
  ask(msg) { const id = ++this.wid; return new Promise((r) => { this.pendingWorker.set(id, r); this.worker.postMessage({ ...msg, id }); }); }
  post(msg, transfer) { this.worker.postMessage(msg, transfer ?? []); }

  // ---- connect ----------------------------------------------------------------------------
  /** mode: 'pick' (browser chooser), 'demo', or a transport instance (known port). */
  async connect(mode = 'pick') {
    if (this.conn !== 'disconnected') return;
    this.emit('banner-clear', 'conn');
    let t;
    try {
      this.setConn('choosing');
      if (mode === 'demo') t = new DemoTransport();
      else if (mode === 'pick') {
        const T = pickTransport(settings.get().transport);
        if (!T) throw Object.assign(new Error('No USB API in this browser'), { name: 'NotSupportedError' });
        t = await T.request();
      } else t = mode;
      this.setConn('opening');
      await t.open();
    } catch (e) {
      this.setConn('disconnected');
      const d = this.fail('connect', e, 'Connect', { bannerKey: e?.name === 'NotFoundError' || e?.name === 'AbortError' ? null : 'conn' });
      return d;
    }
    this.transport = t;
    this.reconnectPort = t;
    this.wireTransport(t);
    return this.handshake(false);
  }

  wireTransport(t) {
    this.post({ t: 'resync' });
    t.onData((u8, err) => {
      if (!u8) { // recoverable stream error (BufferOverrunError…)
        this.post({ t: 'resync', overrun: true, mark: 'Host overrun: bytes lost, stream resynchronised' });
        this.fail('link', err, 'Receiving', { bannerKey: 'overrun' });
        return;
      }
      this.post({ t: 'chunk', buf: u8 }, [u8.buffer]);
    });
    t.onLost((err) => this.onLost(err));
  }

  async handshake(reconnect) {
    const s = settings.get();
    const c = new Client({ write: (u8) => this.transport.write(u8), hostFw: this.hostFw, pingInterval: s.pingInterval,
      log: (lvl, txt) => log(lvl, txt, 'proto') });
    this.client = c;
    c.addEventListener('config', (e) => { this.config = e.detail; this.emit('config', e.detail); this.noteConfig(e.detail); });
    c.addEventListener('stats', (e) => { this.devStats = e.detail; this.emit('stats', e.detail); });
    c.addEventListener('devlog', (e) => log(['error', 'warn', 'info', 'debug'][e.detail.level] ?? 'info', e.detail.text, 'dongle'));
    c.addEventListener('link', (e) => this.emit('link', e.detail));
    c.addEventListener('reset', (e) => {
      const r = e.detail;
      const why = W.resetName(r.reason) + (r.detail ? ` (${r.detail})` : '');
      if (this.conn === 'connected') {
        log('warn', `Dongle restarted: ${why}`, 'dongle');
        this.emit('banner', { key: 'reset', d: { level: 'warn', title: 'The dongle restarted', text: `Reset reason: ${why}. The page re-sent your configuration.`,
          actions: r.reason === 6 || r.reason === 3 || r.reason === 4 ? ['This was a crash; if it repeats, copy the log and update the firmware.'] : [] } });
      }
    });
    c.addEventListener('drift', (e) => log('info', `Config changed on the dongle (cfg_gen ${e.detail.old?.cfgGen} → ${e.detail.cfg.cfgGen})`, 'proto'));
    c.addEventListener('repushed', () => log('info', 'Desired configuration re-pushed', 'proto'));
    c.addEventListener('dead', (e) => this.onLost(e.detail ?? new CtpError('timeout', 'keepalive failed')));
    c.addEventListener('error', (e) => this.fail('proto', e.detail, 'Resync after dongle restart', { bannerKey: 'resync' }));

    this.setConn('handshaking');
    try {
      await sleep(this.transport.kind === 'demo' ? 0 : 50);
      try {
        this.info = await c.hello();
      } catch (e) {
        if (e.kind !== 'version') throw e;
        this.info = e.info;
        const choice = await this.versionDecision(e);
        if (choice === 'update' || choice === 'flash') {
          // hand over to the flasher: it sends REBOOT 1 on this session (update) or uses the BOOT flow
          this.setConn('connected');
          this.emit('request-flash', choice);
          return null;
        }
        if (choice !== 'continue') { await this.disconnect(true); return null; }
        this.unsupportedFw = true;
      }
      this.emit('info', this.info);
      if (this.info.stateFlags & W.STATE_SAFE_MODE) {
        this.emit('banner', { key: 'safe', d: describe(new CtpError('device', 'safe mode', { code: W.ERR.SAFE_MODE, espErr: 0 }), 'Dongle') });
      }
      await c.getConfig();
      await this.pushDesired();
      if (s.autoCapture && !(this.info.stateFlags & W.STATE_SAFE_MODE)) await this.setCapture(true);
      c.startKeepalive();
      this.setConn('connected');
      log('info', `${reconnect ? 'Reconnected' : 'Connected'}: ${this.transport.label}, firmware ${W.fwString(this.info.fw)}, MAC ${W.macString(this.info.mac)}`, 'page');
      if (reconnect) this.post({ t: 'mark', text: 'Reconnected', kind: 'reset' });
      this.emit('banner-clear', 'conn');
      return null;
    } catch (e) {
      const d = this.fail('connect', e, reconnect ? 'Reconnect' : 'Connect', { bannerKey: 'conn' });
      await this.teardown();
      this.setConn('disconnected');
      return d;
    }
  }

  /** Firmware/protocol mismatch decision (docs/04 §4.1). Returns 'continue' | 'cancel'. */
  async versionDecision(e) {
    const key = `${W.macString(e.info.mac)}@${e.info.fwVersion}`;
    if (!e.protocol && settings.get().dontAskFw[key]) return 'continue';
    const r = await new Promise((resolve) => this.emit('version-mismatch', { err: e, resolve }));
    if (r.dontAsk && r.choice === 'continue') settings.update((s) => { s.dontAskFw[key] = true; });
    return r.choice;
  }

  /** Pushes the user's desired config; per-step errors are reported but do not abort the session. */
  async pushDesired() {
    const d = settings.get().desired;
    const c = this.client;
    c.desired = null;
    const step = async (what, fn) => {
      try { await fn(); } catch (e) {
        if (e.kind !== 'device') throw e;
        const desc = this.fail('config', e, what);
        this.emit('config-error', { what, d: desc });
      }
    };
    await step('Country', () => c.setCountry(d.country.cc, d.country.mode, d.country.schan, d.country.nchan));
    const want5 = d.channel > 14 && !(this.info.caps & W.CAP.GHZ5);
    await step(`Channel ${d.channel}`, () => c.setChannel(want5 ? 1 : d.channel, this.info.caps & W.CAP.HT40 ? d.bw : 0));
    await step('Snaplen', () => c.setSnaplen(d.snaplen));
    await step('Driver mask', () => c.setDriverMask(d.typeMask >>> 0, d.ctrlMask >>> 0));
    await step('Filter', () => c.setFilter(d.filter));
    c.desired = { ...this.deviceDesired() };
  }

  /** What the client re-pushes after a reset/drift: the current readback (what the user ended up with). */
  deviceDesired() {
    const cfg = this.config;
    const d = settings.get().desired;
    return { country: { ...cfg.country }, channel: cfg.channel, bw: cfg.bw, snaplen: cfg.snaplen, typeMask: cfg.typeMask, ctrlMask: cfg.ctrlMask,
      filter: d.filter, capture: cfg.captureOn };
  }

  noteConfig(cfg) {
    const text = `Config (cfg_gen ${cfg.cfgGen}): ch ${cfg.channel} ${W.bwName(cfg.bw)}, country ${cfg.country.cc}${cfg.country.mode ? ' manual' : ''}, snaplen ${cfg.snaplen || 'full'}, filter ${cfg.nRules} rule${cfg.nRules === 1 ? '' : 's'} (crc ${cfg.rulesCrc32.toString(16).toUpperCase().padStart(8, '0')})`;
    this.post({ t: 'cfgnote', gen: cfg.cfgGen, text });
    if (this.client?.desired && this.conn === 'connected') this.client.desired = this.deviceDesired();
  }

  /** Runs a config action; returns null or an error description (for inline display). */
  async act(context, fn) {
    if (!this.client || this.conn !== 'connected') return { level: 'warn', title: `${context}: not connected`, text: 'Connect the dongle first.', actions: [] };
    try { await fn(this.client); return null; } catch (e) { return this.fail('config', e, context); }
  }

  async setCapture(on) {
    const c = this.client;
    if (!c) return null;
    try {
      if (on) this.post({ t: 'capture-start' });
      await c.capture(on);
      if (c.desired) c.desired.capture = on;
      this.emit('capture', on);
      return null;
    } catch (e) { return this.fail('capture', e, on ? 'Start capture' : 'Stop capture', { bannerKey: 'capture' }); }
  }

  // ---- link loss & reconnect (docs/04 §4 keepalive) -----------------------------------------
  async onLost(err) {
    if (this.flashing || this.conn === 'disconnected' || this.conn === 'reconnecting') return;
    const wasRecording = !!this.recording;
    this.fail('link', err, 'Link lost', { quiet: true });
    await this.teardown(false);
    if (!settings.get().autoReconnect) {
      this.setConn('disconnected');
      this.emit('banner', { key: 'conn', d: { ...describe(err, 'Link lost'), level: 'warn' }, reconnect: true });
      return;
    }
    this.setConn('reconnecting');
    this.emit('banner', { key: 'conn', d: { level: 'warn', title: 'Connection to the dongle lost, reconnecting…', text: describe(err).text,
      actions: ['Check the USB cable and plug the dongle back in if it was removed.', wasRecording ? 'The recording continues once the link is back.' : ''].filter(Boolean) } });
    const deadline = performance.now() + 30000;
    while (this.conn === 'reconnecting' && performance.now() < deadline) {
      await sleep(1000);
      const cand = await this.reconnectCandidate();
      if (!cand) continue;
      try { await cand.open(); } catch { continue; }
      this.transport = cand;
      this.wireTransport(cand);
      const d = await this.handshake(true);
      if (!d) return;
      if (this.conn === 'disconnected') break;
      this.setConn('reconnecting');
    }
    if (this.conn === 'reconnecting') {
      this.setConn('disconnected');
      this.emit('banner', { key: 'conn', d: { level: 'error', title: 'Could not reconnect to the dongle within 30 s', text: 'It may be unplugged, in download mode, or used by another program.',
        actions: ['Plug the dongle in and press Reconnect.', 'If it keeps failing, unplug it for a few seconds.'] }, reconnect: true });
    }
  }

  async reconnectCandidate() {
    const p = this.reconnectPort;
    if (!p) return null;
    if (p.kind === 'demo') return new DemoTransport();
    // re-enumeration can produce a new port object: prefer a known matching device
    const known = p.kind === 'serial' ? await SerialTransport.known() : await UsbTransport.known();
    return known[0] ?? (p.closed ? p : null);
  }

  async reconnect() {
    this.emit('banner-clear', 'conn');
    const cand = await this.reconnectCandidate();
    if (cand) return this.connect(cand);
    return this.connect('pick');
  }

  async teardown(sendStop = true) {
    this.client?.stopKeepalive();
    if (sendStop) { try { await this.client?.close(); } catch { /* best effort */ } } else if (this.client) { this.client.closed = true; this.client.setLink(LINK.DOWN); }
    try { await this.transport?.close(); } catch { /* ignore */ }
    this.client = null;
    this.transport = null;
  }

  async disconnect(silent = false) {
    if (this.conn === 'disconnected') return;
    this.setConn('disconnected'); // stops the reconnect loop
    await this.teardown(true);
    this.info = null;
    this.emit('info', null);
    if (!silent) log('info', 'Disconnected', 'page');
  }

  // ---- recording (docs/04 §10) -----------------------------------------------------------
  pcapOpts() {
    const fw = this.info ? W.fwString(this.info.fw) : '?';
    return { userAppl: `capturetonkeli web v1${this.unsupportedFw ? ' (unsupported firmware)' : ''}`, hardware: `XIAO ESP32-C5 fw ${fw}`,
      ifName: this.transport?.label ?? 'capturetonkeli', snaplen: this.config?.snaplen || 65535 };
  }

  fileName() {
    const d = new Date(), p = (n) => String(n).padStart(2, '0');
    const date = `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
    return settings.get().recName.replace('{date}', date).replace('{ch}', this.config?.channel ?? 'x');
  }

  async startRecording() {
    const name = this.fileName();
    let sink;
    try {
      if (window.showSaveFilePicker) {
        const fh = await window.showSaveFilePicker({ suggestedName: name, types: [{ description: 'pcapng capture', accept: { 'application/octet-stream': ['.pcapng'] } }] });
        sink = { kind: 'file', name: fh.name, w: await fh.createWritable() };
      } else if (navigator.storage?.getDirectory) {
        const dir = await navigator.storage.getDirectory();
        const fh = await dir.getFileHandle(name, { create: true });
        sink = { kind: 'opfs', name, fh, w: await fh.createWritable() };
      } else {
        sink = { kind: 'memory', name, parts: [] };
      }
    } catch (e) {
      if (e.name === 'AbortError') return null;
      return this.fail('record', e, 'Start recording', { bannerKey: 'rec' });
    }
    this.recording = { ...sink, bytes: 0, writing: Promise.resolve() };
    this.post({ t: 'rec-start', opts: this.pcapOpts(), filtered: settings.get().recFiltered });
    log('info', `Recording to ${sink.kind === 'file' ? 'file' : sink.kind === 'opfs' ? 'browser storage' : 'memory'}: ${sink.name}`, 'rec');
    this.emit('rec', this.recording);
    return null;
  }

  writeRec(buf) {
    const r = this.recording;
    if (!r) return;
    r.bytes += buf.length;
    if (r.kind === 'memory') { r.parts.push(buf); return; }
    r.writing = r.writing.then(() => r.w.write(buf)).catch((e) => {
      this.fail('record', e, 'Writing the recording', { bannerKey: 'rec' });
      this.recording = null;
      this.post({ t: 'rec-stop' });
      this.emit('rec', null);
    });
    this.emit('rec', r);
  }

  stopRecording() { if (this.recording) this.post({ t: 'rec-stop' }); }

  async finishRec(packets) {
    const r = this.recording;
    if (!r) return;
    this.recording = null;
    try {
      await r.writing;
      if (r.kind === 'memory') {
        const { download } = await import('./ui/dom.js');
        download(r.name, new Blob(r.parts));
      } else {
        await r.w.close();
        if (r.kind === 'opfs') {
          const { download } = await import('./ui/dom.js');
          download(r.name, await r.fh.getFile());
        }
      }
      log('info', `Recording saved: ${r.name} (${packets} packets, ${r.bytes} bytes)`, 'rec');
      this.emit('rec-done', { name: r.name, packets, bytes: r.bytes });
    } catch (e) {
      this.fail('record', e, 'Saving the recording', { bannerKey: 'rec' });
    }
    this.emit('rec', null);
  }

  async exportBuffer(filtered) {
    const r = await this.ask({ t: 'export', opts: this.pcapOpts(), filtered });
    const { download } = await import('./ui/dom.js');
    download(this.fileName().replace('capture_', 'buffer_'), new Blob([r.buf]));
    log('info', `Exported ${r.count} packets`, 'rec');
    return r.count;
  }
}

export const app = new App();
