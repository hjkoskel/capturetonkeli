// Demo mode: a simulated hx-esp32-cam-fpv air unit (docs/12 §7) on channel 7.
// Draws an animated test picture, encodes it as JPEG (OffscreenCanvas, when
// the browser has it), packs it like the real air unit (video parts, OSD
// packets, FEC k=6/n=12) and loses ~3 % of the packets on the "air".
import * as H from './hxfpv.js';

export const DEMO_AIR_ID = 0x1234, DEMO_CHANNEL = 7;
const FPS = 15, W = 480, HGT = 360;

export class HxAirSim {
  constructor(rng) {
    this.rng = rng;
    this.tx = new H.Sender({ air: DEMO_AIR_ID, gs: 0, k: 6, n: 12 });
    this.frameIndex = 0;
    this.ready = [];   // encoded JPEGs waiting to be sent
    this.busy = false;
    this.nextFrameAt = 0;
    this.nextOsdAt = 0;
    this.canvas = typeof OffscreenCanvas === 'function' ? new OffscreenCanvas(W, HGT) : null;
    this.ctx = this.canvas?.getContext('2d') ?? null;
  }

  /** 802.11 frames (no FCS) that the air unit sent since the last call. */
  poll(nowMs) {
    if (nowMs >= this.nextFrameAt && !this.busy && this.ctx) {
      this.nextFrameAt = Math.max(nowMs, this.nextFrameAt + 1000 / FPS);
      this.render(nowMs);
    }
    const slots = [];
    while (this.ready.length) slots.push(...H.videoSlots(this.ready.shift(), { frameIndex: this.frameIndex++, resolution: 3, air: DEMO_AIR_ID }));
    if (nowMs >= this.nextOsdAt) {
      this.nextOsdAt = nowMs + 200;
      slots.push(H.osdSlot({ air: DEMO_AIR_ID, fecK: 6, rssiDbm: -48 - Math.floor(this.rng() * 6), captureFps: FPS, temperature: 47, quality: 18, resolution: 3, wifiChannel: DEMO_CHANNEL }));
    }
    return this.tx.push(slots).filter(() => this.rng() >= 0.03);
  }

  render(nowMs) {
    const c = this.ctx, t = nowMs / 1000;
    const g = c.createLinearGradient(0, 0, W, HGT);
    g.addColorStop(0, `hsl(${(t * 20) % 360} 60% 35%)`);
    g.addColorStop(1, `hsl(${(t * 20 + 120) % 360} 60% 20%)`);
    c.fillStyle = g;
    c.fillRect(0, 0, W, HGT);
    c.strokeStyle = 'rgba(255,255,255,.35)';
    c.lineWidth = 2;
    c.beginPath(); c.moveTo(0, HGT / 2 + Math.sin(t) * 30); c.lineTo(W, HGT / 2 - Math.sin(t) * 30); c.stroke(); // horizon
    c.fillStyle = '#ffd34d';
    c.beginPath(); c.arc(W / 2 + Math.cos(t * 1.3) * 150, HGT / 2 + Math.sin(t * 2.1) * 90, 22, 0, 2 * Math.PI); c.fill();
    c.fillStyle = '#fff';
    c.font = 'bold 22px sans-serif';
    c.fillText('DEMO air unit 0x1234', 16, 34);
    c.font = '16px monospace';
    c.fillText(`frame ${this.frameIndex}  ${new Date().toISOString().slice(11, 23)}`, 16, HGT - 18);
    this.busy = true;
    this.canvas.convertToBlob({ type: 'image/jpeg', quality: 0.55 })
      .then((b) => b.arrayBuffer())
      .then((buf) => { if (this.ready.length < 3) this.ready.push(new Uint8Array(buf)); })
      .catch(() => { this.ctx = null; }) // no JPEG encoder: OSD packets only
      .finally(() => { this.busy = false; });
  }
}
