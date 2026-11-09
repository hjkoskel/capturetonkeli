// Simulated dongle (docs/04 §13): implements CTP v1 byte-for-byte with the
// same behaviour as ctp/sim (Go), so worker, protocol, UI and recording run
// unchanged. Generates synthetic 802.11 traffic; supports fault injection.
import { Decoder, encodeFrame, crc32 } from './codec.js';
import * as W from './wire.js';
import * as F from './filter.js';
import { COUNTRIES } from './regdom.js';
import { HxAirSim, DEMO_CHANNEL } from './hxdemo.js';
import { OhdAirSim, OHD_DEMO_CHANNEL } from './ohddemo.js';

const WATCHDOG_MS = 2000, STATS_MS = 1000, RING_BYTES = 96 * 1024, USB_BPS = 1_000_000;

function bootConfig() {
  return {
    cfgGen: 0, channel: 1, bw: 0, band: 0, country: { cc: '01', mode: 0, schan: 1, nchan: 11 }, snaplen: 0,
    typeMask: W.DEFAULT_TYPE_MASK, ctrlMask: W.DEFAULT_CTRL_MASK, captureOn: false, nRules: 0, rulesCrc32: W.EMPTY_FILTER_CRC,
  };
}

const SSIDS = ['Koti', 'Elisa-5G', 'DNA-WIFI-21A4', 'TP-Link_8C2E', 'Telia-ABCD', 'kahvila', 'iPhone (Liisa)', 'eduroam',
  'Guest', 'printer-DIRECT', 'Sauna', 'mokki', 'Office', 'AndroidAP', 'FRITZ!Box 7590', 'Zyxel_12', 'HUAWEI-5G', 'Ruokala',
  'Linksys', 'VIRUS-FREE'];

export class DemoDevice {
  /** onBytes(Uint8Array): device → host bytes. */
  constructor({ onBytes, seed = 1, caps = W.CAP.GHZ5 | W.CAP.HT40 | W.CAP.MANUAL_COUNTRY, fwVersion = 0x00010000 } = {}) {
    this.onBytes = onBytes;
    this.rng = mulberry32(seed);
    this.caps = caps;
    this.fwVersion = fwVersion;
    this.buildId = Uint8Array.from([0xde, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01]);
    this.mac = Uint8Array.from([0x02, 0xc5, 0xde, 0x40, 0x00, 0x01]);
    this.t0 = performance.now();
    this.faults = { drop: 0, corrupt: 0, garbage: false, stallUntil: 0, saturate: false };
    this.bss = {};
    this.seqNo = 0;
    this.attached = false;
    this.boot(W.RESET.indexOf('POWER_ON'));
  }

  nowUs() { return Math.floor((performance.now() - this.t0) * 1000); }

  boot(reason) {
    this.cfg = bootConfig();
    this.region = COUNTRIES['01'];
    this.flt = F.decode(new Uint8Array(8));
    this.session = false;
    this.lastRx = 0;
    this.cnt = { rxTotal: 0, rxPassed: 0, dropRing: 0, dropFcs: 0, txFrames: 0, txBytes: 0, hostRxCrcErr: 0, hostRxOversize: 0, highWater: 0 };
    this.ctrl = [];
    this.ring = [];
    this.ringBytes = 0;
    this.dedup = [];
    this.seq = 0;
    this.firstTx = true;
    this.lastGenTx = 0;
    this.rxSeq = 0;
    this.pendingReboot = false;
    this.bootReason = reason;
    this.dec = new Decoder();
    this.decSeen = { crc: 0, over: 0 };
    this.lastStats = 0;
    this.carry = 0;
    if (this.faults.garbage) this.emitRaw(new TextEncoder().encode('ESP-ROM:esp32c5-eco2-20250121\r\nrst:0xc (SW_CPU),boot:0x18 (SPI_FAST_FLASH_BOOT)\r\n'));
    this.queueCtrl(W.T.EVT_RESET, 0, W.encReset(reason, this.fwVersion, 0, ''));
  }

  /** Start/stop the device clock (attach = port opened). */
  attach() {
    if (this.attached) return;
    this.attached = true;
    this.lastTick = performance.now();
    this.timer = setInterval(() => this.tick(), 10);
  }
  detach() { this.attached = false; clearInterval(this.timer); }

  /** Host → device bytes. */
  write(bytes) {
    this.dec.feed(bytes, (h, body) => {
      if (h.ver !== W.PROTO_VER) { this.cnt.hostRxCrcErr++; return; }
      this.lastRx = performance.now();
      this.handle(h, body);
    });
    const st = this.dec.stats;
    this.cnt.hostRxCrcErr += st.crcErrors + st.cobsErrors + st.short - this.decSeen.crc;
    this.cnt.hostRxOversize += st.oversize - this.decSeen.over;
    this.decSeen = { crc: st.crcErrors + st.cobsErrors + st.short, over: st.oversize };
  }

  // ---- requests -------------------------------------------------------------------
  queueCtrl(type, reqId, body) { this.ctrl.push({ type, reqId, cfgGen: this.cfg.cfgGen, body }); }
  err(code, detail = '', espErr = 0) { return [W.T.RSP_ERR, W.encRspErr(code, espErr, detail)]; }
  cfgRsp() { return [W.T.RSP_OK, W.encConfig(this.cfg)]; }

  handle(h, body) {
    const crc = crc32(body);
    const hit = this.dedup.find((d) => d.reqId === h.reqId && d.type === h.type && d.crc === crc);
    if (hit) { this.queueCtrl(hit.rtype, h.reqId, hit.rbody); return; }
    let rtype, rbody;
    try { [rtype, rbody] = this.exec(h.type, body); } catch (e) {
      if (e instanceof W.ShortBody) [rtype, rbody] = this.err(W.ERR.BAD_BODY); else throw e;
    }
    this.dedup.push({ reqId: h.reqId, type: h.type, crc, rtype, rbody });
    if (this.dedup.length > 4) this.dedup.shift();
    this.queueCtrl(rtype, h.reqId, rbody);
  }

  apply(fn) {
    const old = { ...this.cfg, country: { ...this.cfg.country } };
    fn(this.cfg);
    if (!W.sameSettings(old, this.cfg)) {
      this.cfg.cfgGen = (old.cfgGen + 1) & 0xff;
      if (old.captureOn && !this.cfg.captureOn) { this.ring = []; this.ringBytes = 0; }
    }
  }

  exec(type, b) {
    const T = W.T;
    switch (type) {
      case T.HELLO: {
        const m = W.decHello(b);
        this.session = true;
        this.lastStats = performance.now();
        return [T.RSP_OK, W.encDeviceInfo({
          protoVer: 1, fwVersion: this.fwVersion, fwFlags: W.FW_FLAG.DEV, buildId: this.buildId, mac: this.mac, caps: this.caps,
          maxFrame: W.MAX_FRAME, maxRules: 16, deviceTimeUs: this.nowUs(), hostNonce: m.hostNonce, resetReason: this.bootReason, stateFlags: 0,
        })];
      }
      case T.PING: return [T.RSP_OK, W.encPong(W.decPing(b).hostTimeMs, this.nowUs())];
      case T.GET_CONFIG: return this.cfgRsp();
      case T.SET_CHANNEL: {
        const m = W.decSetChannel(b);
        const r = this.resolve(this.region, m.channel, m.bw);
        if (r.err) return this.err(r.err, r.detail, r.esp ?? 0);
        this.apply((c) => { c.channel = m.channel; c.bw = r.bw; c.band = r.band; });
        return this.cfgRsp();
      }
      case T.SET_COUNTRY: {
        const m = W.decCountry(b);
        let region;
        if (m.mode === 0) {
          region = COUNTRIES[m.cc];
          if (!region) return this.err(W.ERR.COUNTRY_INVALID, `unknown country ${m.cc}`, 0x102);
        } else if (m.mode === 1) {
          if (!(this.caps & W.CAP.MANUAL_COUNTRY)) return this.err(W.ERR.NOT_SUPPORTED, 'manual country');
          if (m.schan < 1 || m.nchan < 1 || m.schan + m.nchan - 1 > 14) return this.err(W.ERR.COUNTRY_INVALID, 'bad manual range');
          region = { schan: m.schan, nchan: m.nchan, ch5: (COUNTRIES[m.cc] ?? COUNTRIES['01']).ch5 };
        } else return this.err(W.ERR.INVALID_ARG, 'mode');
        this.region = region;
        let ch = this.cfg.channel;
        let r = this.resolve(region, ch, this.cfg.bw);
        if (r.err) { r = this.resolve(region, ch, 0); if (r.err) { ch = region.schan; r = { bw: 0, band: 0 }; } }
        this.apply((c) => { c.country = { cc: m.cc, mode: m.mode, schan: region.schan, nchan: region.nchan }; c.channel = ch; c.bw = r.bw; c.band = r.band; });
        return this.cfgRsp();
      }
      case T.SET_FILTER: {
        let f;
        try { f = F.decode(b); } catch (e) { return this.err(W.ERR.FILTER_INVALID, e.message.slice(0, 64)); }
        this.flt = f;
        this.apply((c) => { c.nRules = f.rules.length; c.rulesCrc32 = crc32(b); });
        return this.cfgRsp();
      }
      case T.SET_SNAPLEN: { const n = W.decU16(b, 'SET_SNAPLEN'); this.apply((c) => { c.snaplen = n; }); return this.cfgRsp(); }
      case T.SET_DRIVER_MASK: { const m = W.decDriverMask(b); this.apply((c) => { c.typeMask = m.typeMask; c.ctrlMask = m.ctrlMask; }); return this.cfgRsp(); }
      case T.CAPTURE: {
        const on = W.decU8(b, 'CAPTURE') !== 0;
        if (on && !this.session) return this.err(W.ERR.NO_SESSION, 'HELLO first');
        if (on && !this.cfg.captureOn) this.rxSeq = 0;
        this.apply((c) => { c.captureOn = on; });
        return this.cfgRsp();
      }
      case T.GET_STATS: {
        const reset = W.decU8(b, 'GET_STATS') !== 0;
        const s = this.stats();
        if (reset) { const hw = this.cnt.highWater; for (const k of Object.keys(this.cnt)) this.cnt[k] = 0; this.cnt.highWater = hw; }
        this.cnt.highWater = 0;
        return [T.RSP_OK, W.encStats(s)];
      }
      case T.REBOOT: {
        const mode = W.decU8(b, 'REBOOT');
        if (mode > 1) return this.err(W.ERR.INVALID_ARG, 'mode');
        this.pendingReboot = true;
        return [T.RSP_OK, new Uint8Array(0)];
      }
      default: return this.err(W.ERR.UNKNOWN_TYPE);
    }
  }

  allowed(region, ch) {
    if (ch >= 1 && ch <= 14) return ch >= region.schan && ch < region.schan + region.nchan;
    return region.ch5.includes(ch);
  }

  resolve(region, ch, bw) {
    if (bw > 2) return { err: W.ERR.INVALID_ARG, detail: 'bw' };
    if (!W.channelFreq(ch)) return { err: W.ERR.CHANNEL_NOT_ALLOWED, detail: 'no such channel' };
    const band = ch > 14 ? 1 : 0;
    if (band && !(this.caps & W.CAP.GHZ5)) return { err: W.ERR.NOT_SUPPORTED, detail: '5 GHz' };
    if (!this.allowed(region, ch)) return { err: W.ERR.CHANNEL_NOT_ALLOWED, detail: 'not in country table', esp: 0x300a };
    if (bw === 0) return { bw: 0, band };
    if (!(this.caps & W.CAP.HT40)) return { err: W.ERR.NOT_SUPPORTED, detail: 'HT40' };
    let partner, plus;
    if (ch <= 14) {
      plus = bw === 1;
      partner = plus ? ch + 4 : ch - 4;
      if (plus ? partner > 13 : ch < 5) return { err: W.ERR.CHANNEL_NOT_ALLOWED, detail: 'HT40 partner channel' };
    } else {
      const idx = ch >= 149 ? (ch - 149) / 4 : (ch - 36) / 4;
      plus = idx % 2 === 0;
      partner = plus ? ch + 4 : ch - 4;
    }
    if (!this.allowed(region, partner)) return { err: W.ERR.CHANNEL_NOT_ALLOWED, detail: 'HT40 partner channel', esp: 0x300a };
    return { bw: plus ? 1 : 2, band };
  }

  stats() {
    return { deviceTimeUs: this.nowUs(), ...this.cnt, ringHighWater: this.cnt.highWater, channel: this.cfg.channel, noiseFloor: -95, flashWrites: 0 };
  }

  // ---- traffic ------------------------------------------------------------------------
  aps(ch) {
    if (!this.bss[ch]) {
      const n = ch <= 14 ? 3 + Math.floor(this.rng() * 5) : 1 + Math.floor(this.rng() * 3);
      this.bss[ch] = Array.from({ length: n }, () => ({
        mac: Uint8Array.from([2 | (Math.floor(this.rng() * 64) << 2), ...Array.from({ length: 5 }, () => Math.floor(this.rng() * 256))]),
        ssid: SSIDS[Math.floor(this.rng() * SSIDS.length)], rssi: -35 - Math.floor(this.rng() * 55),
      }));
    }
    return this.bss[ch];
  }

  frame(ch) {
    const r = this.rng, aps = this.aps(ch), ap = aps[Math.floor(r() * aps.length)];
    const sta = Uint8Array.from([0x02, 0x11, 0x22, 0x33, ch, Math.floor(r() * 6)]);
    const bc = new Uint8Array(6).fill(0xff);
    const hdr = (fc0, fc1, a1, a2, a3) => {
      this.seqNo = (this.seqNo + 1) & 0xfff;
      return [fc0, fc1, 0, 0, ...a1, ...a2, ...a3, (this.seqNo << 4) & 0xff, this.seqNo >> 4];
    };
    let f, bb = ch > 14 || r() < 0.75 ? W.BB.G11 : W.BB.B11, rate = bb === W.BB.B11 ? 2 : 12;
    const x = r() * 100;
    const ssid = new TextEncoder().encode(ap.ssid);
    if (x < 14) {
      f = [...hdr(0x80, 0, bc, ap.mac, ap.mac), ...new Array(8).fill(0), 0x64, 0, 0x31, 0x04, 0, ssid.length, ...ssid,
        1, 8, 0x82, 0x84, 0x8b, 0x96, 0x0c, 0x12, 0x18, 0x24, 3, 1, ch, 48, 20, 1, 0, 0, 0x0f, 0xac, 4, 1, 0, 0, 0x0f, 0xac, 4, 1, 0, 0, 0x0f, 0xac, 2, 0, 0];
    } else if (x < 19) {
      f = [...hdr(0x40, 0, bc, sta, bc), 0, 0, 1, 4, 0x02, 0x04, 0x0b, 0x16];
    } else if (x < 23) {
      f = [...hdr(0x50, 0, sta, ap.mac, ap.mac), ...new Array(8).fill(0), 0x64, 0, 0x31, 0x04, 0, ssid.length, ...ssid, 3, 1, ch];
    } else if (x < 40) {
      f = [0xd4, 0, 0, 0, ...sta];
    } else if (x < 46) {
      f = [0xb4, 0, 0x2c, 0, ...ap.mac, ...sta];
    } else if (x < 47) {
      f = [...hdr(0x88, 0x02, sta, ap.mac, ap.mac), 0, 0, 0xaa, 0xaa, 3, 0, 0, 0, 0x88, 0x8e, 2, 3, 0, 0x5f, ...new Array(91).fill(0)];
    } else if (x < 47.3) {
      f = [...hdr(0xc0, 0, sta, ap.mac, ap.mac), 7, 0];
    } else {
      const n = 40 + Math.floor(r() * 1400);
      f = [...hdr(0x88, r() < 0.5 ? 0x41 : 0x42, ap.mac, sta, ap.mac), 0, 0];
      for (let i = 0; i < n; i++) f.push(Math.floor(r() * 256));
      if (r() < 0.6) { bb = W.BB.HT; rate = Math.floor(r() * 8); }
    }
    const data = Uint8Array.from(f);
    const fcs = crc32(data);
    const out = new Uint8Array(data.length + 4);
    out.set(data);
    new DataView(out.buffer).setUint32(data.length, fcs, true);
    const fcsErr = r() < 0.01;
    if (fcsErr) out[out.length - 1] ^= 0x5a;
    return { data: out, rssi: Math.max(-95, Math.min(-20, ap.rssi + Math.floor(r() * 7) - 3)), rate, bb, fcsErr };
  }

  /** Wraps an 802.11 frame without FCS as a captured HT MCS3 frame (hx / OpenHD demo). */
  withFcs(data, rssi) {
    const out = new Uint8Array(data.length + 4);
    out.set(data);
    new DataView(out.buffer).setUint32(data.length, crc32(data), true);
    return { data: out, rssi, rate: 3, bb: W.BB.HT, fcsErr: false };
  }

  driverPass(f) {
    const t = (f[0] >> 2) & 3, sub = f[0] >> 4, m = this.cfg.typeMask >>> 0;
    if (t === 0) return !!(m & 1);
    if (t === 1) return !!(m & 2) && (sub < 7 || !!((this.cfg.ctrlMask >>> 0) & ((1 << (16 + sub)) >>> 0)));
    if (t === 2) return !!(m & 4);
    return !!(m & 8);
  }

  capture(fr, now) {
    if (!this.driverPass(fr.data)) return;
    this.cnt.rxTotal++;
    const ol = fr.data.length;
    const meta = { fcsErr: fr.fcsErr, rssi: fr.rssi, origLen: ol };
    if (!F.match(this.flt, fr.data, meta)) { if (fr.fcsErr && this.flt.dropFcs) this.cnt.dropFcs++; return; }
    this.cnt.rxPassed++;
    let cap = ol;
    if (this.cfg.snaplen && this.cfg.snaplen < cap) cap = this.cfg.snaplen;
    cap = Math.min(cap, 4096);
    let flags = (fr.fcsErr ? W.PKT.FCS_ERR : 0) | (cap < ol ? W.PKT.TRUNCATED : 0) | (this.cfg.band ? W.PKT.GHZ5 : 0) | (this.cfg.bw ? W.PKT.HT40 : 0);
    const seq = this.rxSeq; this.rxSeq = (this.rxSeq + 1) & 0xffff;
    const rec = 4 + 20 + cap;
    if (this.ringBytes + rec > RING_BYTES) { this.cnt.dropRing++; return; }
    const body = W.encPkt({ tsUs: Math.floor((now - this.t0) * 1000), channel: this.cfg.channel, flags, rssi: fr.rssi, noise: -95,
      rate: fr.rate, bbFormat: fr.bb, origLen: ol, capLen: cap, rxSeq: seq }, fr.data.subarray(0, cap));
    this.ring.push({ type: W.T.PKT, reqId: 0, cfgGen: this.cfg.cfgGen, body });
    this.ringBytes += rec;
    this.cnt.highWater = Math.max(this.cnt.highWater, Math.floor(this.ringBytes * 100 / RING_BYTES));
  }

  // ---- clock: traffic, watchdog, stats, USB writer --------------------------------------------
  tick() {
    const now = performance.now();
    const dt = Math.min(0.1, (now - this.lastTick) / 1000);
    this.lastTick = now;
    if (this.cfg.captureOn && this.session) {
      const rate = (this.cfg.channel <= 14 ? 600 : 250) * (this.faults.saturate ? 40 : 1);
      this.carry += rate * dt;
      while (this.carry >= 1) { this.carry--; this.capture(this.frame(this.cfg.channel), now); }
      if (this.cfg.channel === DEMO_CHANNEL) {
        this.hx ??= new HxAirSim(this.rng);
        for (const f of this.hx.poll(now)) this.capture(this.withFcs(f, -50 - Math.floor(this.rng() * 5)), now);
      }
      if (this.cfg.channel === OHD_DEMO_CHANNEL) {
        this.ohd ??= new OhdAirSim(this.rng);
        for (const f of this.ohd.poll(now)) this.capture(this.withFcs(f, -56 - Math.floor(this.rng() * 6)), now);
      }
    }
    if (this.session && now - this.lastRx > WATCHDOG_MS) {
      this.session = false;
      this.apply((c) => { c.captureOn = false; });
      this.ring = []; this.ringBytes = 0;
    }
    if (this.session && now - this.lastStats >= STATS_MS) {
      this.lastStats = now - this.lastStats > 2 * STATS_MS ? now : this.lastStats + STATS_MS;
      this.queueCtrl(W.T.EVT_STATS, 0, W.encStats(this.stats()));
      this.cnt.highWater = 0;
    }
    this.flush(dt);
  }

  flush(dt) {
    if (now() < this.faults.stallUntil) return;
    let budget = USB_BPS * dt + (this.usbCarry ?? 0);
    const parts = [];
    let n = 0;
    while (this.ctrl.length) { const f = this.ctrl.shift(); const w = this.enc(f); parts.push(w); n += w.length; }
    while (this.session && this.ring.length && n < budget) {
      const f = this.ring.shift();
      this.ringBytes -= 4 + f.body.length;
      const w = this.enc(f); parts.push(w); n += w.length;
      this.cnt.txFrames++; this.cnt.txBytes += w.length;
    }
    this.usbCarry = Math.max(0, Math.min(8192, budget - n));
    if (parts.length) {
      const out = new Uint8Array(n);
      let o = 0;
      for (const p of parts) { out.set(p, o); o += p.length; }
      this.emitRaw(out);
    }
    if (this.pendingReboot && !this.ctrl.length) this.boot(W.RESET.indexOf('SOFTWARE'));
  }

  enc(f) {
    let flags = 0;
    if (!this.firstTx && f.cfgGen !== this.lastGenTx) flags |= W.HFLAG_CFG_CHANGED;
    this.firstTx = false;
    this.lastGenTx = f.cfgGen;
    const w = encodeFrame({ ver: 1, type: f.type, seq: this.seq, reqId: f.reqId, flags, cfgGen: f.cfgGen }, f.body, true);
    this.seq = (this.seq + 1) & 0xffff;
    if (this.faults.drop && this.rng() < this.faults.drop) return new Uint8Array(0);
    if (this.faults.corrupt && this.rng() < this.faults.corrupt && w.length > 3) {
      const i = 1 + Math.floor(this.rng() * (w.length - 2));
      w[i] = w[i] ^ 0x10 || 1;
    }
    return w;
  }

  emitRaw(u8) { if (this.attached && u8.length) this.onBytes(u8); }

  // ---- fault injection (Settings → Developer) --------------------------------------------
  stall(ms = 3000) { this.faults.stallUntil = now() + ms; }
  reboot() { this.boot(W.RESET.indexOf('WATCHDOG')); }
  garbage() { this.emitRaw(Uint8Array.from({ length: 300 }, () => 1 + Math.floor(this.rng() * 255))); }
}

const now = () => performance.now();

function mulberry32(a) {
  return () => {
    a |= 0; a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
