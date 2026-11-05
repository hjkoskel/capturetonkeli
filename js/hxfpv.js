// hx-esp32-cam-fpv receiver (docs/12): 802.11 frame → Packet_Header →
// per-air-unit FEC blocks → Air2Ground packets → JPEG frames, OSD stats and
// config. Runs in the worker; pure (no DOM), unit-tested with node.
//
// Wire layout (little-endian, packed; hx-esp32-cam-fpv components/common):
//   802.11 data header (24 B, addr2 = 11:22:33:44:55:66)
//   Packet_Header (12 B): u8 version(2) u8 signature(56) u16 fromDeviceId
//     u16 toDeviceId u16 size u24 block_index u8 packet_index
//   FEC slot (mtu B, default 1464): one Air2Ground packet, zero-padded
//   Air2Ground_Header (12 B): u8 type u32 size u8 pong u8 version u8 crc
//     u16 airDeviceId u16 gsDeviceId; crc = CRC-8 (poly 0x07) over the
//     type's header struct with the crc byte zeroed.
import * as fec from './hxfec.js';

export const AIR_MAC = Uint8Array.from([0x11, 0x22, 0x33, 0x44, 0x55, 0x66]);
export const PACKET_VERSION = 2, PACKET_SIGNATURE = 56;
export const WLAN_HDR = 24, PKT_HDR = 12, A2G_HDR = 12, VIDEO_HDR = 18, OSD_SIZE = 1244, CONFIG_SIZE = 53;
export const TYPE = { VIDEO: 0, TELEMETRY: 1, OSD: 2, CONFIG: 3 };
export const TYPE_NAME = ['video', 'telemetry', 'OSD', 'config'];
export const RESOLUTIONS = ['QVGA 320×240', 'CIF 400×296', 'HVGA 480×320', 'VGA 640×480', 'VGA16 640×360', 'SVGA 800×600', 'SVGA16 800×456',
  'XGA 1024×768', 'XGA16 1024×576', 'SXGA 1280×960', 'HD 1280×720', 'UXGA 1600×1200'];
export const RATES = ['2M CCK', '2M CCK S', '5.5M CCK', '5.5M CCK S', '11M CCK', '11M CCK S', '6M', '9M', '12M', '18M', '24M', '36M', '48M', '54M',
  'MCS0 6.5M', 'MCS0 7.2M S', 'MCS1 13M', 'MCS1 14.4M S', 'MCS2 19.5M', 'MCS2 21.7M S', 'MCS3 26M', 'MCS3 28.9M S', 'MCS4 39M', 'MCS4 43.3M S',
  'MCS5 52M', 'MCS5 57.8M S', 'MCS6 58M', 'MCS6 65M S', 'MCS7 65M', 'MCS7 72M S'];
export const DEFAULT_K = 6, MAX_N = 32;
const MAX_QUEUED_BLOCKS = 3; // like the native GS: give up on a block once 3 newer ones are waiting
const RESTART_GAP = 100;      // a block this far in the past means the air unit restarted

// ---- CRC-8 (poly 0x07, init 0, no reflection) ------------------------------------------------
const CRC8 = new Uint8Array(256);
for (let i = 0; i < 256; i++) {
  let c = i;
  for (let j = 0; j < 8; j++) c = ((c << 1) ^ (c & 0x80 ? 0x07 : 0)) & 0xff;
  CRC8[i] = c;
}
export function crc8(b, n = b.length, crc = 0) {
  for (let i = 0; i < n; i++) crc = CRC8[crc ^ b[i]];
  return crc;
}
/** CRC check of an Air2Ground header struct of `n` bytes (crc byte at offset 7 counted as 0). */
export function crcOk(slot, n) {
  if (slot.length < n) return false;
  let crc = 0;
  for (let i = 0; i < n; i++) crc = CRC8[crc ^ (i === 7 ? 0 : slot[i])];
  return crc === slot[7];
}
/** Sets the crc byte of a header struct of `n` bytes (used by the demo sender and tests). */
export function sealCrc(slot, n) {
  slot[7] = 0;
  slot[7] = crc8(slot, n);
  return slot;
}

const u16 = (b, o) => b[o] | (b[o + 1] << 8);
const u32 = (b, o) => (b[o] | (b[o + 1] << 8) | (b[o + 2] << 16) | (b[o + 3] << 24)) >>> 0;

/**
 * 802.11 frame (no FCS) → hx packet or null. Returns {bad:'version'} for
 * frames that are hx but of another protocol version.
 */
export function parseFrame(f) {
  if (f.length < WLAN_HDR + PKT_HDR + 1) return null;
  if ((f[0] & 0x0c) !== 0x08) return null; // data frame
  for (let i = 0; i < 6; i++) if (f[10 + i] !== AIR_MAC[i]) return null;
  const p = WLAN_HDR;
  if (f[p + 1] !== PACKET_SIGNATURE) return null;
  if (f[p] !== PACKET_VERSION) return { bad: 'version', version: f[p] };
  return {
    from: u16(f, p + 2), to: u16(f, p + 4), size: u16(f, p + 6),
    block: f[p + 8] | (f[p + 9] << 8) | (f[p + 10] << 16), index: f[p + 11],
    payload: f.subarray(p + PKT_HDR),
  };
}

/** Air2Ground header of a FEC slot. */
export function parseA2G(s) {
  return { type: s[0], size: u32(s, 1), pong: s[5], version: s[6], crc: s[7], air: u16(s, 8), gs: u16(s, 10) };
}

/** AirStats (32 B at offset 12 of an OSD packet). */
export function parseAirStats(s, o = A2G_HDR) {
  const b3 = u32(s, o + 3);
  const sb = (v, bits) => (v << (32 - bits)) >> (32 - bits);
  const w = u16(s, o + 30);
  return {
    sdDetected: !!(s[o] & 1), sdSlow: !!(s[o] & 2), sdError: !!(s[o] & 4), wifiRate: s[o] >> 3,
    wifiQueueMin: s[o + 1] & 0x7f, airRecording: !!(s[o + 1] & 0x80), wifiQueueMax: s[o + 2],
    sdFreeGB: (b3 & 0xfff) / 16, sdTotalGB: ((b3 >>> 12) & 0xfff) / 16, quality: (b3 >>> 24) & 0x3f, wifiOverflow: !!((b3 >>> 30) & 1), ov5640: !!(b3 >>> 31),
    outPacketRate: u16(s, o + 7), inPacketRate: u16(s, o + 9), inRejectedPacketRate: u16(s, o + 11),
    rssiDbm: -s[o + 13], noiseFloorDbm: -s[o + 14], captureFps: s[o + 15], camOverflows: s[o + 16],
    frameSizeMin: u16(s, o + 17), frameSizeMax: u16(s, o + 19), mavlinkIn: u16(s, o + 21), mavlinkOut: u16(s, o + 23),
    rcPeriodMax: s[o + 25], wifiChannel: s[o + 26] & 0x0f, resolution: s[o + 26] >> 4,
    temperature: s[o + 27] & 0x7f, overheat: !!(s[o + 27] & 0x80), suspended: !!(s[o + 28] & 0x80),
    fecK: s[o + 29] & 0x0f, inSession: !!(s[o + 29] & 0x10), aspect: s[o + 29] >> 5,
    brightness: sb(w & 7, 3), contrast: sb((w >> 3) & 7, 3), saturation: sb((w >> 6) & 7, 3), sharpness: sb((w >> 9) & 7, 3), aeLevel: sb((w >> 12) & 7, 3),
  };
}

/** Air2Ground_Config_Packet: CameraConfig (30 B at 12) + DataChannelConfig (11 B at 42). */
export function parseConfig(s) {
  const d = 42;
  return {
    resolution: s[12], fpsLimit: s[13], quality: s[14],
    wifiPower: (s[d] << 24) >> 24, wifiRate: s[d + 1], wifiChannel: s[d + 2], fecK: s[d + 3], fecN: s[d + 4], mtu: u16(s, d + 5),
  };
}

/** Width/height from a JPEG SOFn marker, or null. */
export function jpegSize(j) {
  if (j.length < 4 || j[0] !== 0xff || j[1] !== 0xd8) return null;
  let p = 2;
  while (p + 9 < j.length) {
    if (j[p] !== 0xff) { p++; continue; }
    const m = j[p + 1];
    if (m === 0xff) { p++; continue; }
    if (m === 0xd8 || m === 0x01 || (m >= 0xd0 && m <= 0xd7)) { p += 2; continue; }
    if (m === 0xd9 || m === 0xda) return null;
    const len = (j[p + 2] << 8) | j[p + 3];
    if ((m >= 0xc0 && m <= 0xcf) && m !== 0xc4 && m !== 0xc8 && m !== 0xcc) return { height: (j[p + 5] << 8) | j[p + 6], width: (j[p + 7] << 8) | j[p + 8] };
    p += 2 + len;
  }
  return null;
}

function newStats() {
  return {
    packets: 0, dataPackets: 0, fecPackets: 0, fecUnneeded: 0, duplicates: 0, late: 0, badIndex: 0,
    blocksOk: 0, blocksFec: 0, blocksLost: 0, slotsLost: 0, crcErrors: 0, badSize: 0, restarts: 0,
    framesOk: 0, framesFec: 0, framesLost: 0, badJpeg: 0, videoBytes: 0, telemetryBytes: 0, osd: 0, configs: 0, wireBytes: 0,
  };
}

/**
 * Receiver for all air units in range. push() every captured 802.11 frame
 * (without FCS); onFrame(unit, jpeg, info) is called for every complete JPEG
 * frame of every unit (the caller decides which unit to show).
 */
export class Receiver {
  constructor({ onFrame = () => {}, now = () => Date.now() } = {}) {
    this.onFrame = onFrame;
    this.now = now;
    this.units = new Map();
    this.wrongVersion = 0;
    this.lastWrongVersion = null;
  }

  reset() { this.units.clear(); this.wrongVersion = 0; }

  /** meta: {rssi, channel} of the captured frame. Returns true if the frame was an hx packet. */
  push(frame, meta = {}) {
    const p = parseFrame(frame);
    if (!p) return false;
    if (p.bad) { this.wrongVersion++; this.lastWrongVersion = p.version; return true; }
    let u = this.units.get(p.from);
    if (!u) {
      u = {
        id: p.from, gs: p.to, k: DEFAULT_K, kSource: 'default', mtu: p.payload.length, next: -1, blocks: new Map(),
        st: newStats(), first: this.now(), last: 0, rssi: null, channel: meta.channel ?? null,
        frame: { index: -1, nextPart: 0, parts: [], size: 0, fec: false },
        osd: null, config: null, lastFrame: null, pong: null,
      };
      this.units.set(p.from, u);
    }
    const st = u.st;
    st.packets++; st.wireBytes += frame.length;
    u.last = this.now(); u.gs = p.to;
    if (meta.rssi !== undefined) u.rssi = u.rssi === null ? meta.rssi : u.rssi * 0.9 + meta.rssi * 0.1;
    if (meta.channel !== undefined) u.channel = meta.channel;
    if (p.payload.length !== u.mtu) { // MTU changed (air unit reconfigured): restart decoding
      u.mtu = p.payload.length;
      this.restart(u);
    }
    if (p.index >= MAX_N) { st.badIndex++; return true; }
    if (p.index < u.k) st.dataPackets++; else st.fecPackets++;
    if (u.next < 0) u.next = p.block;
    if (p.block < u.next) {
      if (u.next - p.block > RESTART_GAP) { st.restarts++; this.restart(u); u.next = p.block; } else { if (p.index < u.k) st.late++; else st.fecUnneeded++; return true; }
    }
    let b = u.blocks.get(p.block);
    if (!b) { b = { index: p.block, have: new Map(), emitted: 0 }; u.blocks.set(p.block, b); }
    if (b.have.has(p.index)) { st.duplicates++; return true; }
    b.have.set(p.index, p.payload.slice());
    this.process(u);
    return true;
  }

  restart(u) { u.blocks.clear(); u.next = -1; u.frame = { index: -1, nextPart: 0, parts: [], size: 0, fec: false }; }

  setK(u, k, source) {
    if (!(k >= 1 && k <= 16) || k === u.k) { if (k === u.k) u.kSource = source; return; }
    u.k = k; u.kSource = source;
    const next = u.next;
    this.restart(u);
    u.next = next;
  }

  process(u) {
    const st = u.st;
    while (u.blocks.size) {
      const idx = Math.min(...u.blocks.keys());
      const b = u.blocks.get(idx);
      const k = u.k;
      // data packets in order, as soon as they arrive (low latency)
      while (b.emitted < k && b.have.has(b.emitted)) { this.slot(u, b.have.get(b.emitted), false); b.emitted++; if (u.k !== k) return; }
      const nData = [...b.have.keys()].filter((i) => i < k).length;
      if (nData >= k) { st.blocksOk++; this.done(u, idx); continue; }
      if (b.have.size >= k) {
        let rec;
        try { rec = fec.decode(k, b.have, u.mtu); } catch { rec = null; }
        if (rec) {
          st.blocksFec++;
          for (let i = b.emitted; i < k; i++) {
            const d = b.have.get(i) ?? rec.get(i);
            this.slot(u, d, !b.have.has(i));
            if (u.k !== k) return;
          }
          this.done(u, idx);
          continue;
        }
      }
      if (u.blocks.size > MAX_QUEUED_BLOCKS) {
        st.blocksLost++; st.slotsLost += k - b.emitted;
        this.done(u, idx);
        continue;
      }
      break;
    }
  }

  done(u, idx) { u.blocks.delete(idx); u.next = idx + 1; }

  /** One decoded FEC slot = one Air2Ground packet. */
  slot(u, s, recovered) {
    const st = u.st;
    if (s.length < A2G_HDR) { st.badSize++; return; }
    const h = parseA2G(s);
    if (h.type === TYPE.VIDEO) {
      if (h.size < VIDEO_HDR || h.size > s.length) { st.badSize++; return; }
      if (!crcOk(s, VIDEO_HDR)) { st.crcErrors++; return; }
      u.pong = h.pong;
      this.video(u, s, h, recovered);
    } else if (h.type === TYPE.OSD) {
      if (h.size < OSD_SIZE || h.size > s.length) { st.badSize++; return; }
      if (!crcOk(s, OSD_SIZE)) { st.crcErrors++; return; }
      st.osd++;
      u.osd = { ...parseAirStats(s), at: this.now() };
      if (u.osd.fecK) this.setK(u, u.osd.fecK, 'OSD');
    } else if (h.type === TYPE.CONFIG) {
      if (h.size < CONFIG_SIZE || h.size > s.length) { st.badSize++; return; }
      if (!crcOk(s, CONFIG_SIZE)) { st.crcErrors++; return; }
      st.configs++;
      u.config = { ...parseConfig(s), at: this.now() };
      if (u.config.fecK) this.setK(u, u.config.fecK, 'config');
    } else if (h.type === TYPE.TELEMETRY) {
      if (h.size < A2G_HDR || h.size > s.length) { st.badSize++; return; }
      if (!crcOk(s, A2G_HDR)) { st.crcErrors++; return; }
      st.telemetryBytes += h.size - A2G_HDR;
    } else {
      st.badSize++;
    }
  }

  /** Frame assembly, same rules as the native GS (main.cpp). */
  video(u, s, h, recovered) {
    const st = u.st, f = u.frame;
    const resolution = s[12], part = s[13] & 0x7f, last = !!(s[13] & 0x80), index = u32(s, 14);
    if (index + 200 < f.index || index > f.index) {
      if (f.nextPart) st.framesLost++;
      if (f.index >= 0 && index > f.index + 1) st.framesLost += index - f.index - 1;
      f.index = index; f.nextPart = 0; f.parts = []; f.size = 0; f.fec = false;
    }
    if (index !== f.index || part !== f.nextPart) return;
    const data = s.subarray(VIDEO_HDR, h.size);
    f.parts.push(data); f.size += data.length; f.nextPart++; f.fec ||= recovered;
    if (!last) return;
    const jpeg = new Uint8Array(f.size);
    let o = 0;
    for (const d of f.parts) { jpeg.set(d, o); o += d.length; }
    f.nextPart = 0; f.parts = []; f.size = 0;
    st.videoBytes += jpeg.length;
    if (jpeg[0] !== 0xff || jpeg[1] !== 0xd8) { st.badJpeg++; return; }
    if (f.fec) st.framesFec++; else st.framesOk++;
    const info = { index, size: jpeg.length, fec: f.fec, resolution, parts: part + 1, at: this.now() };
    u.lastFrame = info;
    f.fec = false;
    this.onFrame(u, jpeg, info);
  }

  /** Plain-object snapshot for the UI (postMessage-safe). */
  summary() {
    const now = this.now();
    return {
      wrongVersion: this.wrongVersion, lastWrongVersion: this.lastWrongVersion,
      units: [...this.units.values()].map((u) => ({
        id: u.id, gs: u.gs, k: u.k, kSource: u.kSource, mtu: u.mtu, rssi: u.rssi === null ? null : Math.round(u.rssi), channel: u.channel,
        ageMs: now - u.last, st: { ...u.st }, osd: u.osd, config: u.config, lastFrame: u.lastFrame, queued: u.blocks.size,
      })),
    };
  }
}

// ---- sender side (demo mode and tests) ----------------------------------------------------------

/** Air2Ground video packets for one JPEG (each fits one FEC slot of `mtu` bytes). */
export function videoSlots(jpeg, { frameIndex, resolution = 0, air = 0, gs = 0, mtu = 1464 }) {
  const max = mtu - VIDEO_HDR, out = [];
  const parts = Math.max(1, Math.ceil(jpeg.length / max));
  for (let i = 0; i < parts; i++) {
    const data = jpeg.subarray(i * max, Math.min(jpeg.length, (i + 1) * max));
    const s = new Uint8Array(mtu);
    const dv = new DataView(s.buffer);
    s[0] = TYPE.VIDEO; dv.setUint32(1, VIDEO_HDR + data.length, true); s[6] = PACKET_VERSION;
    dv.setUint16(8, air, true); dv.setUint16(10, gs, true);
    s[12] = resolution; s[13] = (i & 0x7f) | (i === parts - 1 ? 0x80 : 0); dv.setUint32(14, frameIndex >>> 0, true);
    s.set(data, VIDEO_HDR);
    out.push(sealCrc(s, VIDEO_HDR));
  }
  return out;
}

/** OSD packet slot with the given AirStats fields (subset). */
export function osdSlot({ air = 0, gs = 0, mtu = 1464, fecK = DEFAULT_K, rssiDbm = -60, captureFps = 30, temperature = 45, quality = 20, resolution = 0, wifiChannel = 7, wifiRate = 20 }) {
  const s = new Uint8Array(mtu);
  const dv = new DataView(s.buffer), o = A2G_HDR;
  s[0] = TYPE.OSD; dv.setUint32(1, OSD_SIZE, true); s[6] = PACKET_VERSION;
  dv.setUint16(8, air, true); dv.setUint16(10, gs, true);
  s[o] = (wifiRate & 0x1f) << 3;
  dv.setUint32(o + 3, (quality & 0x3f) << 24, true);
  s[o + 13] = -rssiDbm; s[o + 14] = 95; s[o + 15] = captureFps;
  s[o + 26] = (wifiChannel & 0x0f) | ((resolution & 0x0f) << 4);
  s[o + 27] = temperature & 0x7f;
  s[o + 29] = (fecK & 0x0f) | 0x10;
  return sealCrc(s, OSD_SIZE);
}

/** Wraps FEC slots into 802.11 frames: k data packets per block plus n-k FEC packets. */
export class Sender {
  constructor({ air = 0x1234, gs = 0, k = DEFAULT_K, n = 12, mtu = 1464 } = {}) {
    Object.assign(this, { air, gs, k, n, mtu });
    this.block = 0; this.pending = []; this.seq = 0;
  }

  /** Adds slots; returns the 802.11 frames (without FCS) of every completed block. */
  push(slots) {
    const frames = [];
    for (const s of slots) {
      this.pending.push(s);
      frames.push(this.frame(s, this.pending.length - 1));
      if (this.pending.length === this.k) {
        for (let i = this.k; i < this.n; i++) frames.push(this.frame(fec.encode(this.k, this.pending, i, this.mtu), i));
        this.pending = [];
        this.block = (this.block + 1) & 0xffffff;
      }
    }
    return frames;
  }

  frame(payload, index) {
    const f = new Uint8Array(WLAN_HDR + PKT_HDR + this.mtu);
    f.set([0x08, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, ...AIR_MAC, ...AIR_MAC]);
    this.seq = (this.seq + 1) & 0xfff;
    f[22] = (this.seq << 4) & 0xff; f[23] = this.seq >> 4;
    const p = WLAN_HDR, dv = new DataView(f.buffer);
    f[p] = PACKET_VERSION; f[p + 1] = PACKET_SIGNATURE;
    dv.setUint16(p + 2, this.air, true); dv.setUint16(p + 4, this.gs, true); dv.setUint16(p + 6, this.mtu, true);
    f[p + 8] = this.block & 0xff; f[p + 9] = (this.block >> 8) & 0xff; f[p + 10] = (this.block >> 16) & 0xff; f[p + 11] = index;
    f.set(payload.subarray(0, this.mtu), p + PKT_HDR);
    return f;
  }
}

/** Dongle capture filter for hx air→ground traffic (docs/12 §5). keepBelow: keep packet_index < 8 or < 16; 0 = keep all. */
export function captureFilter(keepBelow = 8) {
  const terms = [
    { offset: 0, value: '08', mask: '0c', negate: false },
    { offset: 10, value: '11 22 33 44 55 66', negate: false },
    { offset: 24, value: '02 38', negate: false },
  ];
  if (keepBelow === 8 || keepBelow === 16) terms.push({ offset: 35, value: '00', mask: keepBelow === 8 ? 'f8' : 'f0', negate: false });
  return {
    version: 1,
    rules: [{ name: `hx-esp32-cam-fpv air→ground${keepBelow ? ` (packets 0…${keepBelow - 1} of each block)` : ''}`, enabled: true, terms }],
    global: { drop_fcs_err: true, rssi_min: null, len_min: null, len_max: null },
  };
}
