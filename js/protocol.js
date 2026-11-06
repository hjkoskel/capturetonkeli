// CTP client (docs/04 §4, docs/03 §5–7): req_id allocation, request/response
// matching with retry (same req_id), HELLO/version check, keepalive, config
// push, cfg_gen drift and EVT_RESET recovery. The worker decodes frames and
// hands non-PKT frames to ingest(); this module never parses packets.
import { encodeFrame, crc32 } from './codec.js';
import * as W from './wire.js';
import * as F from './filter.js';
import * as semver from './semver.js';

export class CtpError extends Error {
  /** kind: 'timeout' | 'device' | 'protocol' | 'closed' | 'version' */
  constructor(kind, message, extra = {}) {
    super(message);
    this.name = 'CtpError';
    this.kind = kind;
    Object.assign(this, extra);
  }
}

export const LINK = Object.freeze({ DOWN: 'down', UP: 'up', DEGRADED: 'degraded', DEAD: 'dead' });

export class Client extends EventTarget {
  /**
   * write(u8): Promise — transport write.
   * hostFw: {major,minor,patch} of the bundled firmware, or null (no bundle: no check).
   */
  constructor({ write, hostFw = null, requestTimeout = 200, retries = 3, pingInterval = 500, log = () => {} }) {
    super();
    this.write = write;
    this.hostFw = hostFw;
    this.requestTimeout = requestTimeout;
    this.retries = retries;
    this.pingInterval = pingInterval;
    this.log = log;
    this.pending = new Map();
    this.reqId = 1 + Math.floor(Math.random() * 0x7fff);
    this.seq = 0;
    this.info = null;
    this.config = null;
    this.knownGen = null;
    this.desired = null;
    this.rtts = [];
    this.pingFails = 0;
    this.link = LINK.DOWN;
    this.closed = false;
    this.lastRx = 0;
    this.busy = 0; // requests in flight (drift checks wait)
  }

  emit(type, detail) { this.dispatchEvent(new CustomEvent(type, { detail })); }

  setLink(s) { if (this.link !== s) { this.link = s; this.emit('link', s); } }

  /** Called by the worker bridge for every decoded non-PKT frame. */
  ingest(h, body) {
    this.lastRx = performance.now();
    switch (h.type) {
      case W.T.RSP_OK:
      case W.T.RSP_ERR: {
        const p = this.pending.get(h.reqId);
        if (p) p.resolve({ h, body });
        break;
      }
      case W.T.EVT_STATS: {
        try { this.emit('stats', { ...W.decStats(body), cfgGen: h.cfgGen }); } catch { /* ignore malformed */ }
        if (this.knownGen !== null && h.cfgGen !== this.knownGen && this.busy === 0) this.onDrift();
        break;
      }
      case W.T.EVT_LOG:
        try { this.emit('devlog', W.decLog(body)); } catch { /* ignore */ }
        break;
      case W.T.EVT_RESET: {
        let r = { reason: 0 };
        try { r = W.decReset(body); } catch { /* keep default */ }
        this.emit('reset', r);
        if (this.info) this.onReset(r);
        break;
      }
      default: break;
    }
  }

  nextId() { this.reqId = (this.reqId % 0xffff) + 1; return this.reqId; }

  /** Sends a request; resolves {h, body}; rejects CtpError (device errors carry code/espErr/detail). */
  async request(type, body = new Uint8Array(0), { timeout = this.requestTimeout, retries = this.retries } = {}) {
    if (this.closed) throw new CtpError('closed', 'Connection is closed');
    const id = this.nextId();
    this.busy++;
    try {
      for (let attempt = 0; attempt <= retries; attempt++) {
        // every attempt is a new frame (new header seq) with the same req_id,
        // so the device answers a duplicate from its cache (docs/03 §5)
        const f = encodeFrame({ type, reqId: id, seq: this.seq }, body);
        this.seq = (this.seq + 1) & 0xffff;
        const got = new Promise((resolve) => this.pending.set(id, { resolve }));
        try { await this.write(f); } catch (e) {
          this.pending.delete(id);
          throw new CtpError('closed', `Could not send to the dongle: ${e.message ?? e}`, { cause: e });
        }
        const res = await Promise.race([got, sleep(timeout).then(() => null)]);
        this.pending.delete(id);
        if (res) {
          if (res.h.type === W.T.RSP_ERR) {
            const e = W.decRspErr(res.body);
            throw new CtpError('device', `${W.typeName(type)}: ${W.errName(e.code)}${e.detail ? ' (' + e.detail + ')' : ''}`,
              { request: type, code: e.code, espErr: e.espErr, detail: e.detail });
          }
          return res;
        }
        if (this.closed) throw new CtpError('closed', 'Connection is closed');
        if (attempt < retries) this.log('debug', `${W.typeName(type)} req ${id}: no answer in ${timeout} ms, retry ${attempt + 1}`);
      }
      throw new CtpError('timeout', `${W.typeName(type)}: the dongle did not answer (${retries + 1} tries × ${timeout} ms)`, { request: type });
    } finally {
      this.busy--;
    }
  }

  async cfgReq(type, body) {
    const { body: b } = await this.request(type, body);
    const cfg = W.decConfig(b);
    this.config = cfg;
    this.knownGen = cfg.cfgGen;
    this.emit('config', cfg);
    return cfg;
  }

  // ---- session ----------------------------------------------------------------------------

  /** HELLO + validation (docs/04 §4 step 3–4). Throws CtpError('version'|'protocol'). */
  async hello() {
    const nonce = (Math.random() * 2 ** 32) >>> 0;
    const { body } = await this.request(W.T.HELLO, W.encHello(nonce), { timeout: 300, retries: 4 });
    const info = W.decDeviceInfo(body);
    if (info.hostNonce !== nonce) throw new CtpError('protocol', 'HELLO answer did not echo our nonce (another program is talking to this port?)');
    if (info.protoVer !== W.PROTO_VER) {
      throw new CtpError('version', `Dongle speaks protocol v${info.protoVer}, this page v${W.PROTO_VER}`, { protocol: true, info });
    }
    info.fw = W.fwFromWire(info.fwVersion);
    info.clockOffsetUs = Date.now() * 1000 - info.deviceTimeUs;
    this.info = info;
    this.emit('info', info);
    if (this.hostFw && !semver.compatible(this.hostFw, info.fw)) {
      throw new CtpError('version', `Firmware ${W.fwString(info.fw)} ≠ page ${this.hostFw.major}.${this.hostFw.minor}.x`, { protocol: false, info });
    }
    return info;
  }

  getConfig() { return this.cfgReq(W.T.GET_CONFIG); }
  setCountry(cc, mode = 0, schan = 0, nchan = 0) { return this.cfgReq(W.T.SET_COUNTRY, W.encCountry(cc, mode, schan, nchan)); }
  setChannel(ch, bw = 0) { return this.cfgReq(W.T.SET_CHANNEL, W.encSetChannel(ch, bw)); }
  setSnaplen(n) { return this.cfgReq(W.T.SET_SNAPLEN, W.encSnaplen(n)); }
  setDriverMask(t, c) { return this.cfgReq(W.T.SET_DRIVER_MASK, W.encDriverMask(t, c)); }
  capture(on) { return this.cfgReq(W.T.CAPTURE, W.encU8(on)); }

  /** Applies a filter set (JSON model); verifies rules_crc32. */
  async setFilter(set) {
    const body = F.encode(set, this.info?.maxRules ?? F.MAX_RULES);
    const cfg = await this.cfgReq(W.T.SET_FILTER, body);
    if (cfg.rulesCrc32 !== crc32(body)) throw new CtpError('protocol', 'The dongle reports a different filter checksum than was sent');
    return cfg;
  }

  async stats(reset = false) {
    const { body } = await this.request(W.T.GET_STATS, W.encU8(reset));
    return W.decStats(body);
  }

  async ping() {
    const t0 = performance.now();
    await this.request(W.T.PING, W.encPing(Date.now() >>> 0), { retries: 0 });
    const rtt = performance.now() - t0;
    this.rtts.push(rtt);
    if (this.rtts.length > 20) this.rtts.shift();
    return rtt;
  }

  rttStats() {
    const s = [...this.rtts].sort((a, b) => a - b);
    if (!s.length) return null;
    return { median: s[Math.floor(s.length / 2)], p95: s[Math.min(s.length - 1, Math.floor(s.length * 0.95))] };
  }

  reboot(mode = 0) { return this.request(W.T.REBOOT, Uint8Array.of(mode)); }

  /** Pushes desired config in doc 04 §4 step 5 order. desired = {country:{cc,mode,schan,nchan}, channel, bw, snaplen, typeMask, ctrlMask, filter, capture}. */
  async push(d) {
    this.desired = d;
    if (d.country) await this.setCountry(d.country.cc, d.country.mode ?? 0, d.country.schan ?? 0, d.country.nchan ?? 0);
    if (d.channel) await this.setChannel(d.channel, d.bw ?? 0);
    if (d.snaplen !== undefined) await this.setSnaplen(d.snaplen);
    if (d.typeMask !== undefined) await this.setDriverMask(d.typeMask >>> 0, (d.ctrlMask ?? W.DEFAULT_CTRL_MASK) >>> 0);
    if (d.filter) await this.setFilter(d.filter);
    if (d.capture !== undefined) await this.capture(d.capture);
    return this.config;
  }

  startKeepalive() {
    this.stopKeepalive();
    const loop = async () => {
      if (this.closed) return;
      try {
        await this.ping();
        this.pingFails = 0;
        this.setLink(LINK.UP);
      } catch (e) {
        if (this.closed) return;
        this.pingFails++;
        this.setLink(LINK.DEGRADED);
        if (this.pingFails >= 3) { this.setLink(LINK.DEAD); this.emit('dead', e); return; }
      }
      this.pingTimer = setTimeout(loop, this.pingInterval);
    };
    this.pingTimer = setTimeout(loop, this.pingInterval);
  }

  stopKeepalive() { clearTimeout(this.pingTimer); }

  async onDrift() {
    if (this.drifting) return;
    this.drifting = true;
    try {
      const old = this.config;
      const cfg = await this.getConfig();
      this.emit('drift', { old, cfg });
      if (this.desired && old && !W.sameSettings(old, cfg)) {
        // the dongle's watchdog (or something else) changed it: restore what the user wants
        await this.push(this.desired);
        this.emit('repushed', this.config);
      }
    } catch (e) { this.log('warn', `config drift check failed: ${e.message}`); } finally { this.drifting = false; }
  }

  async onReset(r) {
    if (this.resetting) return;
    this.resetting = true;
    try {
      await sleep(100);
      await this.hello().catch((e) => { if (e.kind !== 'version') throw e; });
      await this.getConfig();
      if (this.desired) { await this.push(this.desired); this.emit('repushed', this.config); }
    } catch (e) { this.emit('error', e); } finally { this.resetting = false; }
  }

  /** CAPTURE 0 best effort, then stop. */
  async close() {
    this.stopKeepalive();
    if (!this.closed && this.info) {
      try { await this.request(W.T.CAPTURE, W.encU8(false), { retries: 0, timeout: 200 }); } catch { /* best effort */ }
    }
    this.closed = true;
    for (const p of this.pending.values()) p.resolve(null);
    this.pending.clear();
    this.setLink(LINK.DOWN);
  }
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
