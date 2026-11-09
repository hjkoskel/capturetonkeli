// App controller: connection state machine (docs/04 §4), worker bridge,
// config actions with error reporting, recording/export (§10).
import { Client, CtpError, LINK, sleep } from './protocol.js';
import { pickTransport, DemoTransport, SerialTransport, UsbTransport } from './transport.js';
import { describe } from './errors.js';
import * as W from './wire.js';
import * as semver from './semver.js';
import * as settings from './settings.js';
import { log } from './log.js';

/** Maximum dongles in a diversity group (docs/14: the format allows more, 4 are tested). */
export const MAX_DONGLES = 4;

/**
 * One connected dongle. The app has a primary member (what the panels show: app.client,
 * app.info, app.config…) and, for receive diversity (docs/14), up to 3 more that follow
 * the primary's configuration.
 */
class Member {
  constructor(id, transport) {
    Object.assign(this, { id, transport, client: null, info: null, config: null, devStats: null, state: 'opening', link: 'up', error: null, label: `Dongle ${id + 1}` });
  }
  get mac() { return this.info ? W.macString(this.info.mac) : ''; }
}

class App extends EventTarget {
  constructor() {
    super();
    this.conn = 'disconnected';
    this.primary = null;
    this.members = [];   // all connected dongles, primary included
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

  // the panels talk to the primary dongle
  get transport() { return this.primary?.transport ?? null; }
  get client() { return this.primary?.client ?? null; }
  get info() { return this.primary?.info ?? null; }
  get config() { return this.primary?.config ?? null; }
  get devStats() { return this.primary?.devStats ?? null; }
  /** Dongles besides the primary. */
  get extras() { return this.members.filter((m) => m !== this.primary); }

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
      case 'frame': {
        const mem = this.members.find((x) => x.id === (m.m ?? 0));
        if (settings.get().showRaw) log('debug', `rx ${W.typeName(m.h.type)} seq=${m.h.seq} req=${m.h.reqId} gen=${m.h.cfgGen} ${m.body.length} B${this.members.length > 1 ? ` (${mem?.label})` : ''}`, 'link');
        mem?.client?.ingest(m.h, new Uint8Array(m.body.buffer, m.body.byteOffset, m.body.byteLength));
        break;
      }
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
  freeId() { for (let i = 0; ; i++) if (!this.members.some((m) => m.id === i)) return i; }

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
    const mem = new Member(this.freeId(), t);
    this.primary = mem;
    this.members = [mem];
    this.reconnectPort = t;
    this.wireTransport(mem);
    return this.handshake(false);
  }

  wireTransport(mem) {
    const t = mem.transport;
    this.post({ t: 'resync', m: mem.id });
    t.onData((u8, err) => {
      if (!u8) { // recoverable stream error (BufferOverrunError…)
        this.post({ t: 'resync', m: mem.id, overrun: true, mark: 'Host overrun: bytes lost, stream resynchronised' });
        this.fail('link', err, this.members.length > 1 ? `Receiving (${mem.label})` : 'Receiving', { bannerKey: 'overrun' });
        return;
      }
      this.post({ t: 'chunk', m: mem.id, buf: u8 }, [u8.buffer]);
    });
    t.onLost((err) => this.onLost(err, mem));
  }

  /** A protocol client for a member; events of the primary drive the panels. */
  makeClient(mem) {
    const s = settings.get();
    const tag = () => (this.members.length > 1 ? `${mem.label}: ` : '');
    const c = new Client({ write: (u8) => mem.transport.write(u8), hostFw: this.hostFw, pingInterval: s.pingInterval,
      log: (lvl, txt) => log(lvl, tag() + txt, 'proto') });
    mem.client = c;
    const primary = () => mem === this.primary;
    c.addEventListener('config', (e) => {
      mem.config = e.detail;
      this.post({ t: 'cfgnote', m: mem.id, gen: e.detail.cfgGen, text: this.cfgText(e.detail) });
      if (primary()) { this.emit('config', e.detail); this.noteConfig(e.detail); } else this.checkMembers();
    });
    c.addEventListener('stats', (e) => { mem.devStats = e.detail; if (primary()) this.emit('stats', e.detail); this.emit('members'); });
    c.addEventListener('devlog', (e) => log(['error', 'warn', 'info', 'debug'][e.detail.level] ?? 'info', tag() + e.detail.text, 'dongle'));
    c.addEventListener('link', (e) => { mem.link = e.detail; if (primary()) this.emit('link', e.detail); else this.emit('members'); });
    c.addEventListener('reset', (e) => {
      const r = e.detail;
      const why = W.resetName(r.reason) + (r.detail ? ` (${r.detail})` : '');
      if (this.conn === 'connected') {
        log('warn', `${tag()}Dongle restarted: ${why}`, 'dongle');
        this.emit('banner', { key: `reset${mem.id}`, d: { level: 'warn', title: this.members.length > 1 ? `${mem.label} restarted` : 'The dongle restarted', text: `Reset reason: ${why}. The page re-sent your configuration.`,
          actions: r.reason === 6 || r.reason === 3 || r.reason === 4 ? ['This was a crash; if it repeats, copy the log and update the firmware.'] : [] } });
      }
    });
    c.addEventListener('drift', (e) => log('info', `${tag()}Config changed on the dongle (cfg_gen ${e.detail.old?.cfgGen} → ${e.detail.cfg.cfgGen})`, 'proto'));
    c.addEventListener('repushed', () => log('info', `${tag()}Desired configuration re-pushed`, 'proto'));
    c.addEventListener('dead', (e) => this.onLost(e.detail ?? new CtpError('timeout', 'keepalive failed'), mem));
    c.addEventListener('error', (e) => this.fail('proto', e.detail, `${tag()}Resync after dongle restart`, { bannerKey: 'resync' }));
    return c;
  }

  async handshake(reconnect) {
    const s = settings.get();
    const mem = this.primary;
    const c = this.makeClient(mem);
    this.setConn('handshaking');
    try {
      await sleep(mem.transport.kind === 'demo' ? 0 : 50);
      try {
        mem.info = await c.hello();
      } catch (e) {
        if (e.kind !== 'version') throw e;
        mem.info = e.info;
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
      mem.label = this.labelFor(mem);
      this.emit('info', mem.info);
      if (mem.info.stateFlags & W.STATE_SAFE_MODE) {
        this.emit('banner', { key: 'safe', d: describe(new CtpError('device', 'safe mode', { code: W.ERR.SAFE_MODE, espErr: 0 }), 'Dongle') });
      }
      await c.getConfig();
      await this.pushDesired();
      if (s.autoCapture && !(mem.info.stateFlags & W.STATE_SAFE_MODE)) await this.setCapture(true);
      c.startKeepalive();
      mem.state = 'ok';
      this.setConn('connected');
      this.publishMembers();
      log('info', `${reconnect ? 'Reconnected' : 'Connected'}: ${mem.transport.label}, firmware ${W.fwString(mem.info.fw)}, MAC ${mem.mac}`, 'page');
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

  cfgText(cfg) {
    return `Config (cfg_gen ${cfg.cfgGen}): ch ${cfg.channel} ${W.bwName(cfg.bw)}, country ${cfg.country.cc}${cfg.country.mode ? ' manual' : ''}, snaplen ${cfg.snaplen || 'full'}, filter ${cfg.nRules} rule${cfg.nRules === 1 ? '' : 's'} (crc ${cfg.rulesCrc32.toString(16).toUpperCase().padStart(8, '0')})`;
  }

  noteConfig() {
    if (this.client?.desired && this.conn === 'connected') {
      this.client.desired = this.deviceDesired();
      for (const m of this.extras) if (m.client?.desired) m.client.desired = this.deviceDesired(); // the group follows the primary
    }
    this.checkMembers();
  }

  /** Runs a config action on every dongle; returns null or an error description (for inline display). */
  async act(context, fn) {
    if (!this.client || this.conn !== 'connected') return { level: 'warn', title: `${context}: not connected`, text: 'Connect the dongle first.', actions: [] };
    try { await fn(this.client); } catch (e) { return this.fail('config', e, context); }
    await this.forExtras(context, fn);
    return null;
  }

  /** Runs an action on the primary dongle only (reboot, stats readback…). */
  async actOne(context, fn) {
    if (!this.client || this.conn !== 'connected') return { level: 'warn', title: `${context}: not connected`, text: 'Connect the dongle first.', actions: [] };
    try { await fn(this.client); return null; } catch (e) { return this.fail('config', e, context); }
  }

  /** Applies an action to the other dongles of the group; a failure marks that dongle degraded (docs/14 §4.2). */
  async forExtras(context, fn) {
    await Promise.all(this.extras.filter((m) => m.client && m.state !== 'joining').map(async (m) => {
      try { await fn(m.client); } catch (e) {
        m.error = this.fail('config', e, `${context} (${m.label})`);
        m.state = 'degraded';
        this.publishMembers();
      }
    }));
    this.checkMembers();
  }

  async setCapture(on) {
    const c = this.client;
    if (!c) return null;
    try {
      if (on) this.post({ t: 'capture-start' });
      await c.capture(on);
      if (c.desired) c.desired.capture = on;
      await this.forExtras(on ? 'Start capture' : 'Stop capture', async (x) => { await x.capture(on); if (x.desired) x.desired.capture = on; });
      this.emit('capture', on);
      return null;
    } catch (e) { return this.fail('capture', e, on ? 'Start capture' : 'Stop capture', { bannerKey: 'capture' }); }
  }

  // ---- diversity group (docs/14) --------------------------------------------------------------
  labelFor(mem) { return settings.get().dongleLabels?.[mem.mac] || `Dongle ${mem.id + 1}`; }
  setLabel(mem, label) {
    settings.update((s) => { s.dongleLabels = { ...(s.dongleLabels ?? {}) }; if (label) s.dongleLabels[mem.mac] = label; else delete s.dongleLabels[mem.mac]; });
    mem.label = this.labelFor(mem);
    this.publishMembers();
  }

  /** Same capture settings as the primary? (docs/14 §4.2 "verify the readback") */
  sameAsPrimary(mem) {
    const a = mem.config, b = this.config;
    if (!a || !b) return false;
    return a.channel === b.channel && a.bw === b.bw && a.country.cc === b.country.cc && a.country.mode === b.country.mode && a.snaplen === b.snaplen &&
      a.typeMask >>> 0 === b.typeMask >>> 0 && a.ctrlMask >>> 0 === b.ctrlMask >>> 0 && a.rulesCrc32 >>> 0 === b.rulesCrc32 >>> 0 && a.captureOn === b.captureOn;
  }
  checkMembers() {
    let changed = false;
    for (const m of this.extras) {
      if (m.state !== 'ok' && m.state !== 'degraded') continue;
      const st = this.sameAsPrimary(m) ? 'ok' : 'degraded';
      if (st !== m.state) { m.state = st; if (st === 'ok') m.error = null; changed = true; }
    }
    if (changed) this.publishMembers();
  }

  /** Tells the worker which dongles are merged (state ok), and the panels about all of them. */
  publishMembers() {
    const list = this.members.filter((m) => m === this.primary || m.state === 'ok').map((m) => ({ id: m.id, mac: m.mac, label: m.label }));
    this.post({ t: 'members', list });
    this.emit('members');
  }

  /** Adds a dongle to the group: 'pick' (browser chooser), 'demo' or a transport. */
  async addMember(mode = 'pick') {
    if (this.conn !== 'connected') return { level: 'warn', title: 'Connect the first dongle first', text: 'Extra dongles join the one that is connected and follow its settings.', actions: [] };
    if (this.members.length >= MAX_DONGLES) return { level: 'warn', title: `At most ${MAX_DONGLES} dongles`, text: 'More are not tested (docs/14).', actions: [] };
    let t;
    try {
      if (mode === 'demo') {
        let i = 1;
        while (this.members.some((m) => m.transport.kind === 'demo' && m.transport.index === i)) i++;
        t = new DemoTransport(i);
      } else if (mode === 'pick') {
        const T = pickTransport(settings.get().transport);
        if (!T) throw Object.assign(new Error('No USB API in this browser'), { name: 'NotSupportedError' });
        t = await T.request();
      } else t = mode;
      if (this.members.some((m) => m.transport.portId === t.portId)) throw new Error('This dongle is already connected. Pick another one (each dongle is its own USB device).');
      await t.open();
    } catch (e) {
      if (e?.name === 'NotFoundError' || e?.name === 'AbortError') return null;
      return this.fail('member', e, 'Add dongle');
    }
    return this.joinMember(new Member(this.freeId(), t));
  }

  /** HELLO, same config as the primary, capture, keepalive (docs/14 §4.2 start). */
  async joinMember(mem, expectMac = null) {
    mem.state = 'joining';
    this.members.push(mem);
    this.wireTransport(mem);
    this.emit('members');
    const c = this.makeClient(mem);
    try {
      await sleep(mem.transport.kind === 'demo' ? 0 : 50);
      try { mem.info = await c.hello(); } catch (e) {
        if (e.kind !== 'version') throw e;
        throw Object.assign(new Error(`Its firmware (${W.fwString(e.info.fw)}) does not match this page. Connect it alone first and update it, then add it to the group.`), { name: 'FirmwareMismatch' });
      }
      if (expectMac && mem.mac !== expectMac) throw Object.assign(new Error('another dongle answered'), { name: 'WrongDongle' });
      if (this.members.some((m) => m !== mem && m.mac === mem.mac)) throw new Error(`Dongle ${mem.mac} is already in the group.`);
      mem.label = this.labelFor(mem);
      await c.getConfig();
      if (this.config.channel > 14 && !(mem.info.caps & W.CAP.GHZ5)) throw new Error('It has no 5 GHz support; the group is on a 5 GHz channel.');
      await this.pushTo(mem);
      c.startKeepalive();
      mem.state = this.sameAsPrimary(mem) ? 'ok' : 'degraded';
      if (mem.state === 'degraded' && !mem.error) mem.error = { level: 'warn', title: 'Settings differ from the first dongle', text: 'It is not merged until its settings match.', actions: [] };
      this.publishMembers();
      log('info', `${mem.label} joined the group: ${mem.transport.label}, firmware ${W.fwString(mem.info.fw)}, MAC ${mem.mac}${mem.state === 'degraded' ? ' (settings differ: not merged)' : ''}`, 'page');
      return null;
    } catch (e) {
      await this.dropMember(mem, true);
      if (e.name === 'WrongDongle') return e;
      return this.fail('member', e, 'Add dongle');
    }
  }

  /** Pushes the primary's readback (the group config) to a member; per-step errors mark it degraded. */
  async pushTo(mem) {
    const c = mem.client, d = this.deviceDesired();
    mem.error = null;
    const step = async (what, fn) => { try { await fn(); } catch (e) { if (e.kind !== 'device') throw e; mem.error = this.fail('config', e, `${what} (${mem.label})`); } };
    await step('Country', () => c.setCountry(d.country.cc, d.country.mode, d.country.schan, d.country.nchan));
    await step(`Channel ${d.channel}`, () => c.setChannel(d.channel, mem.info.caps & W.CAP.HT40 ? d.bw : 0));
    await step('Snaplen', () => c.setSnaplen(d.snaplen));
    await step('Driver mask', () => c.setDriverMask(d.typeMask >>> 0, d.ctrlMask >>> 0));
    await step('Filter', () => c.setFilter(d.filter));
    await step('Capture', () => c.capture(d.capture));
    c.desired = { ...this.deviceDesired() };
  }

  /** Re-push to every dongle (Device tab). */
  async repushAll() {
    await this.pushDesired();
    await Promise.all(this.extras.filter((m) => m.client && m.state !== 'joining').map((m) => this.pushTo(m).catch((e) => { m.error = this.fail('config', e, `Re-push (${m.label})`); })));
    this.checkMembers();
  }

  async dropMember(mem, sendStop) {
    mem.client?.stopKeepalive();
    if (sendStop) { try { await mem.client?.close(); } catch { /* best effort */ } } else if (mem.client) { mem.client.closed = true; mem.client.setLink(LINK.DOWN); }
    try { await mem.transport.close(); } catch { /* ignore */ }
    this.members = this.members.filter((m) => m !== mem);
    mem.state = 'gone';
    this.publishMembers();
  }

  /** Removes a dongle from the group (UI). The primary hands over to the next one. */
  async removeMember(mem) {
    if (mem === this.primary) {
      if (!this.extras.length) return this.disconnect();
      this.promote(this.extras[0]);
    }
    await this.dropMember(mem, true);
    log('info', `${mem.label} removed from the group`, 'page');
    return null;
  }

  /** Makes another member the primary (the panels then show it); its config already matches. */
  promote(mem) {
    this.primary = mem;
    mem.state = 'ok';
    this.reconnectPort = mem.transport;
    this.emit('info', mem.info);
    if (mem.config) this.emit('config', mem.config);
    if (mem.devStats) this.emit('stats', mem.devStats);
    this.emit('link', mem.link);
    this.publishMembers();
  }

  // ---- link loss & reconnect (docs/04 §4 keepalive) -----------------------------------------
  async onLost(err, mem = this.primary) {
    if (this.flashing || this.conn === 'disconnected' || this.conn === 'reconnecting' || !mem || mem.state === 'gone') return;
    if (mem !== this.primary || this.extras.some((m) => m.state === 'ok')) return this.memberLost(err, mem);
    const wasRecording = !!this.recording;
    this.fail('link', err, 'Link lost', { quiet: true });
    for (const m of this.extras) await this.dropMember(m, true); // degraded leftovers: start again with the primary
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
      const m = new Member(this.freeId(), cand);
      this.primary = m;
      this.members = [m];
      this.wireTransport(m);
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

  /** A group member was lost: the others keep capturing; it rejoins when it comes back (docs/14 §4.2). */
  async memberLost(err, mem) {
    const wasPrimary = mem === this.primary;
    const mac = mem.mac, label = mem.label, kind = mem.transport.kind, index = mem.transport.index;
    this.fail('link', err, `${label} lost`, { quiet: true });
    if (wasPrimary) this.promote(this.extras.find((m) => m.state === 'ok'));
    await this.dropMember(mem, false);
    const left = this.members.length;
    this.emit('banner', { key: `lost${mac}`, d: { level: 'warn', title: `${label} lost, capture continues with ${left} dongle${left === 1 ? '' : 's'}`, text: describe(err).text,
      actions: ['It rejoins by itself when it comes back within 30 s (plug it in again).'] } });
    this.post({ t: 'mark', text: `${label} lost`, kind: 'reset' });
    const deadline = performance.now() + 30000;
    while (this.conn === 'connected' && performance.now() < deadline && !this.members.some((m) => m.mac === mac)) {
      await sleep(1000);
      if (this.members.length >= MAX_DONGLES) break;
      const cands = kind === 'demo' ? [new DemoTransport(index)] : await (kind === 'serial' ? SerialTransport.known() : UsbTransport.known());
      for (const t of cands) {
        if (this.members.some((m) => m.transport.portId === t.portId)) continue;
        try { await t.open(); } catch { continue; }
        const r = await this.joinMember(new Member(this.freeId(), t), mac);
        if (!r) {
          this.emit('banner-clear', `lost${mac}`);
          this.post({ t: 'mark', text: `${label} back in the group`, kind: 'reset' });
          return;
        }
      }
    }
  }

  async reconnectCandidate() {
    const p = this.reconnectPort;
    if (!p) return null;
    if (p.kind === 'demo') return new DemoTransport(p.index);
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
    const mem = this.primary;
    if (mem) {
      mem.client?.stopKeepalive();
      if (sendStop) { try { await mem.client?.close(); } catch { /* best effort */ } } else if (mem.client) { mem.client.closed = true; mem.client.setLink(LINK.DOWN); }
      try { await mem.transport?.close(); } catch { /* ignore */ }
    }
    this.primary = null;
    this.members = [];
    this.publishMembers();
  }

  async disconnect(silent = false) {
    if (this.conn === 'disconnected') return;
    this.setConn('disconnected'); // stops the reconnect loop
    for (const m of this.extras) await this.dropMember(m, true);
    await this.teardown(true);
    this.emit('info', null);
    if (!silent) log('info', 'Disconnected', 'page');
  }

  // ---- recording (docs/04 §10) -----------------------------------------------------------
  pcapOpts() {
    const fw = this.info ? W.fwString(this.info.fw) : '?';
    return { userAppl: `capturetonkeli web v1${this.unsupportedFw ? ' (unsupported firmware)' : ''}`, hardware: `XIAO ESP32-C5 fw ${fw}`,
      ifName: this.members.length > 1 ? `capturetonkeli diversity group: ${this.members.map((m) => `${m.label} ${m.mac}`).join(', ')}` : this.transport?.label ?? 'capturetonkeli',
      snaplen: this.config?.snaplen || 65535 };
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
