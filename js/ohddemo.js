// Demo mode: a simulated OpenHD system (docs/13 §12) on channel 149 (5745 MHz),
// default bind phrase. The air unit sends H.264 video (WebCodecs VideoEncoder,
// when the browser has it) as RTP → FEC 20 % → authenticated frames, session
// packets, encrypted MAVLink telemetry of a simulated flight (ArduCopter, plus
// the OpenHD air unit's own messages) and management frames; the ground unit
// sends its session packet and GCS heartbeats. ~3 % of the frames are lost.
import * as O from './openhd.js';
import * as M from './mavlink.js';
import { Packetizer } from './rtp.js';

export const OHD_DEMO_CHANNEL = 149;
const W = 640, H = 360, FPS = 30, MAX_BLOCK = 20;

function payload(n, fill) { const b = new Uint8Array(n); fill(new DataView(b.buffer)); return b; }

export class OhdAirSim {
  constructor(rng) {
    this.rng = rng;
    this.air = new O.Sender({ rng, nonce: BigInt(Math.floor(rng() * 2 ** 32)) });
    this.gnd = new O.Sender({ rng, uid: O.UID.GND, K: O.DEFAULT_KEYS.gnd, nonce: BigInt(Math.floor(rng() * 2 ** 32)) });
    this.pk = new Packetizer({ codec: 'h264' });
    this.out = [];
    this.t0 = null; this.frame = 0; this.seq = 0;
    this.next = { video: 0, session: 0, mgmt: 0, tele: 0, slow: 0, gnd: 0 };
    this.encoder = null; this.encoderFailed = false; this.pending = 0;
    if (typeof OffscreenCanvas === 'function' && typeof VideoEncoder === 'function') {
      this.canvas = new OffscreenCanvas(W, H);
      this.ctx = this.canvas.getContext('2d');
      try {
        this.encoder = new VideoEncoder({ output: (chunk) => this.onChunk(chunk), error: () => { this.encoderFailed = true; } });
        this.encoder.configure({ codec: 'avc1.42001f', width: W, height: H, bitrate: 1_500_000, framerate: FPS, latencyMode: 'realtime', avc: { format: 'annexb' } });
      } catch { this.encoder = null; }
    }
  }

  /** 802.11 frames (no FCS) sent since the last call. */
  poll(now) {
    this.t0 ??= now;
    const n = this.next;
    if (now >= n.session) { n.session = now + 1000; this.out.push(this.air.session(), this.gnd.session()); }
    if (now >= n.mgmt) { n.mgmt = now + 500; this.out.push(this.air.mgmt(5745, 20)); }
    if (now >= n.tele) { n.tele = now + 100; this.telemetry(now); }
    if (now >= n.gnd) {
      n.gnd = now + 1000;
      const hb = payload(9, (d) => { d.setUint8(4, 6); d.setUint8(5, 8); d.setUint8(8, 3); });
      this.out.push(...this.gnd.telemetry(M.encode({ id: 0, sys: 255, comp: 190, seq: this.seq++ }, hb), { n: 1 }));
    }
    if (this.encoder && !this.encoderFailed && now >= n.video && this.pending < 2) {
      n.video = Math.max(now, n.video + 1000 / FPS);
      this.render(now);
    }
    const out = this.out;
    this.out = [];
    return out.filter(() => this.rng() >= 0.03);
  }

  render(now) {
    const c = this.ctx, t = (now - this.t0) / 1000, s = this.state(now);
    c.fillStyle = '#1d3b5a'; c.fillRect(0, 0, W, H);
    // artificial horizon from the simulated attitude
    c.save();
    c.translate(W / 2, H / 2); c.rotate(-s.roll); c.translate(0, s.pitch * 300);
    c.fillStyle = '#5b8a3c'; c.fillRect(-W, 0, 2 * W, 2 * H);
    c.strokeStyle = '#fff'; c.lineWidth = 2; c.beginPath(); c.moveTo(-W, 0); c.lineTo(W, 0); c.stroke();
    c.restore();
    c.fillStyle = '#ffd34d';
    c.beginPath(); c.arc(W / 2 + Math.cos(t * 1.7) * 200, 70 + Math.sin(t * 2.3) * 30, 18, 0, 2 * Math.PI); c.fill();
    c.fillStyle = '#fff'; c.font = 'bold 22px sans-serif';
    c.fillText('DEMO OpenHD air unit', 16, 34);
    c.font = '16px monospace';
    c.fillText(`frame ${this.frame}  ${new Date().toISOString().slice(11, 23)}`, 16, H - 18);
    const vf = new VideoFrame(this.canvas, { timestamp: Math.round((now - this.t0) * 1000) });
    this.pending++;
    try { this.encoder.encode(vf, { keyFrame: this.frame % FPS === 0 }); } catch { this.encoderFailed = true; }
    vf.close();
    this.frame++;
  }

  onChunk(chunk) {
    this.pending = Math.max(0, this.pending - 1);
    const b = new Uint8Array(chunk.byteLength);
    chunk.copyTo(b);
    const pkts = this.pk.packetize(b, Math.round(chunk.timestamp * 0.09));
    // one FEC block per frame, split like OpenHD when k > max block size
    for (let i = 0; i < pkts.length; i += MAX_BLOCK) this.out.push(...this.air.video(pkts.slice(i, i + MAX_BLOCK)));
  }

  /** Simulated flight: a 120 m circle at 40 m, ArduCopter Loiter. */
  state(now) {
    const t = (now - (this.t0 ?? now)) / 1000, w = 0.05, r = 120;
    const lat0 = 60.1699, lon0 = 24.9384;
    const dn = r * Math.sin(w * t), de = r * Math.cos(w * t);
    const heading = ((Math.atan2(Math.cos(w * t), -Math.sin(w * t)) * 180) / Math.PI + 360) % 360;
    return { t, lat: lat0 + dn / 111320, lon: lon0 + de / (111320 * Math.cos((lat0 * Math.PI) / 180)), alt: 40 + 3 * Math.sin(t / 7), speed: r * w, heading,
      roll: 0.15 + 0.05 * Math.sin(t), pitch: -0.05 + 0.03 * Math.sin(t * 1.3), yaw: (heading * Math.PI) / 180, volt: 16.4 - t * 0.002, batt: Math.max(5, 98 - Math.floor(t / 20)) };
  }

  telemetry(now) {
    const s = this.state(now), msgs = [];
    const fc = (id, p) => msgs.push(M.encode({ id, sys: 1, comp: 1, seq: this.seq++ }, p));
    fc(30, payload(28, (d) => { d.setUint32(0, s.t * 1000, true); d.setFloat32(4, s.roll, true); d.setFloat32(8, s.pitch, true); d.setFloat32(12, s.yaw, true); }));
    fc(74, payload(20, (d) => { d.setFloat32(0, s.speed, true); d.setFloat32(4, s.speed, true); d.setFloat32(8, s.alt, true); d.setFloat32(12, 0.2, true); d.setInt16(16, Math.round(s.heading), true); d.setUint16(18, 46, true); }));
    fc(33, payload(28, (d) => { d.setUint32(0, s.t * 1000, true); d.setInt32(4, Math.round(s.lat * 1e7), true); d.setInt32(8, Math.round(s.lon * 1e7), true); d.setInt32(12, Math.round((s.alt + 12) * 1000), true); d.setInt32(16, Math.round(s.alt * 1000), true); d.setUint16(26, Math.round(s.heading * 100), true); }));
    if (now >= this.next.slow) {
      this.next.slow = now + 1000;
      fc(0, payload(9, (d) => { d.setUint32(0, 5, true); d.setUint8(4, 2); d.setUint8(5, 3); d.setUint8(6, 128 | 1 | 16); d.setUint8(7, 4); d.setUint8(8, 3); }));
      fc(1, payload(31, (d) => { d.setUint16(12, 220, true); d.setUint16(14, Math.round(s.volt * 1000), true); d.setInt16(16, 1250, true); d.setInt8(30, s.batt); }));
      fc(24, payload(30, (d) => { d.setInt32(8, Math.round(s.lat * 1e7), true); d.setInt32(12, Math.round(s.lon * 1e7), true); d.setInt32(16, Math.round((s.alt + 12) * 1000), true); d.setUint16(20, 70, true); d.setUint16(24, Math.round(s.speed * 100), true); d.setUint16(26, Math.round(s.heading * 100), true); d.setUint8(28, 3); d.setUint8(29, 15); }));
      const ohd = (id, p) => msgs.push(M.encode({ id, sys: 101, comp: 191, seq: this.seq++ }, p));
      ohd(1221, Uint8Array.of(2, 6, 4, 0, 0));
      ohd(1219, payload(21, (d) => { d.setUint16(4, 1500, true); d.setUint16(6, W, true); d.setUint16(8, H, true); d.setUint16(10, FPS, true); d.setUint8(14, 3); d.setUint8(15, 1); d.setUint8(18, 0); d.setUint8(19, FPS); }));
      ohd(1227, payload(22, (d) => { d.setUint32(0, 12000, true); d.setUint16(4, 1500, true); d.setInt8(14, 55 + Math.round(3 * Math.sin(s.t / 9))); d.setInt8(15, 48); d.setUint8(19, 1); d.setUint8(20, 1); }));
      if (Math.floor(s.t) % 15 === 0) msgs.push(M.encode({ id: 253, sys: 1, comp: 1, seq: this.seq++ }, payload(54, (d) => { d.setUint8(0, 6); new Uint8Array(d.buffer).set(new TextEncoder().encode('Demo: still circling'), 1); })));
    }
    let len = 0;
    for (const m of msgs) len += m.length;
    const agg = new Uint8Array(len);
    let o = 0;
    for (const m of msgs) { agg.set(m, o); o += m.length; }
    this.out.push(...this.air.telemetry(agg, { n: 2 }));
  }
}
