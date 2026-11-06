// MAVLink v1/v2 parsing for the OpenHD telemetry stream (docs/13 §7): framing,
// CRC with CRC_EXTRA (common + ardupilotmega + OpenHD dialect), decoders for
// the messages shown in the UI, a flight-state summary and .tlog output.
// Field offsets are MAVLink wire order (fields sorted by type size).
import { CRC_TABLE } from './mavlink-crc.js';

const TABLE = new Map(CRC_TABLE.trim().split(/\s+/).map((e) => { const [i, c, mn, mx] = e.split(':').map(Number); return [i, { crc: c, min: mn, max: mx }]; }));
export const msgInfo = (id) => TABLE.get(id);

/** X.25 / MCRF4XX CRC as used by MAVLink. */
export function crcX25(bytes, crc = 0xffff) {
  for (let i = 0; i < bytes.length; i++) {
    let t = (bytes[i] ^ crc) & 0xff;
    t ^= (t << 4) & 0xff;
    crc = ((crc >> 8) ^ (t << 8) ^ (t << 3) ^ (t >> 4)) & 0xffff;
  }
  return crc;
}

/**
 * Splits a buffer of concatenated MAVLink frames.
 * Each message: {v, id, sys, comp, seq, len, payload (zero-extended to the full length when known),
 * crcOk (true/false, or null for unknown message ids), raw (the frame bytes)}.
 */
export function parse(buf) {
  const out = [];
  let i = 0;
  while (i < buf.length) {
    const stx = buf[i];
    if (stx === 0xfd && i + 12 <= buf.length) {
      const len = buf[i + 1], signed = buf[i + 2] & 1, total = 12 + len + (signed ? 13 : 0);
      if (i + total > buf.length) break;
      const id = buf[i + 7] | (buf[i + 8] << 8) | (buf[i + 9] << 16);
      out.push(finish({ v: 2, id, seq: buf[i + 4], sys: buf[i + 5], comp: buf[i + 6], len, signed: !!signed }, buf, i, 10, len, total));
      i += total;
    } else if (stx === 0xfe && i + 8 <= buf.length) {
      const len = buf[i + 1], total = 8 + len;
      if (i + total > buf.length) break;
      out.push(finish({ v: 1, id: buf[i + 5], seq: buf[i + 2], sys: buf[i + 3], comp: buf[i + 4], len, signed: false }, buf, i, 6, len, total));
      i += total;
    } else i++;
  }
  return out;
}
function finish(m, buf, i, hdr, len, total) {
  const info = TABLE.get(m.id);
  const pl = buf.subarray(i + hdr, i + hdr + len);
  m.raw = buf.slice(i, i + total);
  if (info) {
    const crc = crcX25([info.crc], crcX25(buf.subarray(i + 1, i + hdr + len)));
    m.crcOk = crc === (buf[i + hdr + len] | (buf[i + hdr + len + 1] << 8));
    m.payload = new Uint8Array(Math.max(info.max, len));
    m.payload.set(pl); // v2 truncates trailing zero bytes
  } else { m.crcOk = null; m.payload = Uint8Array.from(pl); }
  return m;
}

/** MAVLink v2 frame (trailing zeros truncated), for demo mode and tests. */
export function encode({ id, sys = 1, comp = 1, seq = 0 }, payload) {
  const info = TABLE.get(id);
  if (!info) throw new Error(`MAVLink: unknown message ${id}`);
  let len = payload.length;
  while (len > 1 && payload[len - 1] === 0) len--;
  const f = new Uint8Array(12 + len);
  f.set([0xfd, len, 0, 0, seq & 0xff, sys, comp, id & 0xff, (id >> 8) & 0xff, id >> 16]);
  f.set(payload.subarray(0, len), 10);
  const crc = crcX25([info.crc], crcX25(f.subarray(1, 10 + len)));
  f[10 + len] = crc & 0xff; f[11 + len] = crc >> 8;
  return f;
}

// ---- decoders ----
const MSG = {
  0: ['HEARTBEAT', (d) => ({ customMode: d.getUint32(0, true), type: d.getUint8(4), autopilot: d.getUint8(5), baseMode: d.getUint8(6), systemStatus: d.getUint8(7) })],
  1: ['SYS_STATUS', (d) => ({ load: d.getUint16(12, true) / 10, voltage: d.getUint16(14, true) / 1000, current: d.getInt16(16, true) / 100, dropRate: d.getUint16(18, true) / 100, batteryRemaining: d.getInt8(30) })],
  24: ['GPS_RAW_INT', (d) => ({ lat: d.getInt32(8, true) / 1e7, lon: d.getInt32(12, true) / 1e7, alt: d.getInt32(16, true) / 1000, eph: d.getUint16(20, true), vel: d.getUint16(24, true) / 100, cog: d.getUint16(26, true) / 100, fixType: d.getUint8(28), satellites: d.getUint8(29) })],
  30: ['ATTITUDE', (d) => ({ roll: d.getFloat32(4, true), pitch: d.getFloat32(8, true), yaw: d.getFloat32(12, true) })],
  33: ['GLOBAL_POSITION_INT', (d) => ({ lat: d.getInt32(4, true) / 1e7, lon: d.getInt32(8, true) / 1e7, alt: d.getInt32(12, true) / 1000, relAlt: d.getInt32(16, true) / 1000, vx: d.getInt16(20, true) / 100, vy: d.getInt16(22, true) / 100, vz: d.getInt16(24, true) / 100, hdg: d.getUint16(26, true) === 65535 ? null : d.getUint16(26, true) / 100 })],
  65: ['RC_CHANNELS', (d) => ({ chancount: d.getUint8(40), rssi: d.getUint8(41) })],
  74: ['VFR_HUD', (d) => ({ airspeed: d.getFloat32(0, true), groundspeed: d.getFloat32(4, true), alt: d.getFloat32(8, true), climb: d.getFloat32(12, true), heading: d.getInt16(16, true), throttle: d.getUint16(18, true) })],
  109: ['RADIO_STATUS', (d) => ({ rssi: d.getUint8(4), remrssi: d.getUint8(5), noise: d.getUint8(7), remnoise: d.getUint8(8) })],
  147: ['BATTERY_STATUS', (d) => {
    let mv = 0;
    for (let i = 0; i < 10; i++) { const c = d.getUint16(10 + 2 * i, true); if (c === 0xffff) break; mv += c; }
    return { consumed: d.getInt32(0, true), voltage: mv / 1000, current: d.getInt16(30, true) / 100, remaining: d.getInt8(35) };
  }],
  253: ['STATUSTEXT', (d, p) => ({ severity: p[0], text: new TextDecoder().decode(p.subarray(1, 51)).replace(/\0.*$/s, '') })],
  1211: ['OPENHD_STATS_MONITOR_MODE_WIFI_LINK', (d) => ({ txBps: d.getInt32(0, true), rxBps: d.getInt32(4, true), txPps: d.getInt16(20, true), rxPps: d.getInt16(22, true), freq: d.getUint16(26, true), rateKbit: d.getUint16(28, true), lossPerc: d.getInt8(32), width: d.getUint8(33), mcs: d.getUint8(34), pollution: d.getUint8(37), cardTemp: d.getInt8(40) })],
  1213: ['OPENHD_STATS_TELEMETRY', () => ({})],
  1214: ['OPENHD_STATS_WB_VIDEO_AIR', (d) => ({ encoderBitrate: d.getInt32(0, true), injectedBitrate: d.getInt32(4, true), injectedPps: d.getInt32(8, true), droppedFrames: d.getInt32(12, true), recommendedBitrate: d.getInt16(20, true), fecPerc: d.getInt16(22, true), link: d.getUint8(26) })],
  1215: ['OPENHD_STATS_WB_VIDEO_AIR_FEC_PERFORMANCE', () => ({})],
  1218: ['OPENHD_ONBOARD_COMPUTER_STATUS_EXTENSION', (d) => ({ cpuCoreMv: d.getUint16(0, true), overCurrent: d.getUint8(10) })],
  1219: ['OPENHD_CAMERA_STATUS_AIR', (d) => ({ bitrateKbit: d.getUint16(4, true), width: d.getUint16(6, true), height: d.getUint16(8, true), fps: d.getUint16(10, true), camType: d.getUint8(14), camStatus: d.getUint8(15), recording: d.getUint8(17), encoding: d.getUint8(18), keyframeInterval: d.getUint8(19) })],
  1221: ['OPENHD_VERSION_MESSAGE', (d) => ({ major: d.getUint8(0), minor: d.getUint8(1), patch: d.getUint8(2), releaseType: d.getUint8(3) })],
  1226: ['OPENHD_SYS_STATUS1', () => ({})],
  1227: ['OPENHD_CORE_STATUS', (d) => ({ storageLeftMb: d.getUint32(0, true), cpuClock: d.getUint16(4, true), cpuTemp: d.getInt8(14), wifi0Temp: d.getInt8(15), undervolt: d.getUint8(17), platform: d.getUint8(18), encryption: d.getUint8(19), fcSysId: d.getUint8(20), mode: d.getUint8(21) })],
  1228: ['OPENHD_POWER_STATUS', (d) => ({ voltage: d.getUint16(0, true) / 1000, current: d.getInt16(2, true) / 1000, charging: d.getUint8(4), percent: d.getUint8(5) })],
};
export const msgName = (id) => MSG[id]?.[0] ?? `#${id}`;
/** Decoded fields of m (from parse()) or null when the message is not decoded here. */
export function decode(m) {
  const d = MSG[m.id];
  if (!d || m.crcOk !== true) return null;
  return d[1](new DataView(m.payload.buffer, m.payload.byteOffset, m.payload.length), m.payload);
}

// ---- flight modes (ArduPilot copter/plane, PX4) ----
const COPTER = ['Stabilize', 'Acro', 'AltHold', 'Auto', 'Guided', 'Loiter', 'RTL', 'Circle', '', 'Land', '', 'Drift', '', 'Sport', 'Flip', 'AutoTune', 'PosHold', 'Brake', 'Throw', 'Avoid_ADSB', 'Guided_NoGPS', 'Smart_RTL', 'FlowHold', 'Follow', 'ZigZag', 'SystemID', 'Autorotate', 'Auto RTL'];
const PLANE = ['Manual', 'Circle', 'Stabilize', 'Training', 'Acro', 'FBWA', 'FBWB', 'Cruise', 'Autotune', '', 'Auto', 'RTL', 'Loiter', 'Takeoff', 'Avoid_ADSB', 'Guided', '', 'QStabilize', 'QHover', 'QLoiter', 'QLand', 'QRTL', 'QAutotune', 'QAcro', 'Thermal', 'Loiter2QLand'];
const PX4 = ['', 'Manual', 'Altitude', 'Position', 'Auto', 'Acro', 'Offboard', 'Stabilized', 'Rattitude'];
const PX4_AUTO = ['', 'Ready', 'Takeoff', 'Hold', 'Mission', 'RTL', 'Land', '', 'Follow', 'Precland'];
const PLANE_TYPES = new Set([1, 16, 19, 20, 21, 22, 23, 24, 25]);
export const AUTOPILOT = { 0: 'generic', 3: 'ArduPilot', 8: 'none', 12: 'PX4' };
export const MAV_TYPE = { 1: 'fixed wing', 2: 'quadcopter', 3: 'coaxial', 4: 'helicopter', 6: 'GCS', 10: 'rover', 13: 'hexacopter', 14: 'octocopter', 15: 'tricopter', 18: 'onboard computer', 19: 'VTOL', 20: 'VTOL', 21: 'VTOL', 22: 'VTOL', 26: 'gimbal', 27: 'ADSB' };
export function flightMode(hb) {
  if (!hb) return null;
  if (!(hb.baseMode & 1)) return null; // custom mode not enabled
  if (hb.autopilot === 3) return (PLANE_TYPES.has(hb.type) ? PLANE : COPTER)[hb.customMode] || `mode ${hb.customMode}`;
  if (hb.autopilot === 12) {
    const main = (hb.customMode >> 16) & 0xff, sub = (hb.customMode >>> 24) & 0xff;
    return main === 4 ? `Auto ${PX4_AUTO[sub] || sub}` : PX4[main] || `mode ${main}`;
  }
  return `mode ${hb.customMode}`;
}

/** Keeps the latest state per MAVLink system; feed it parse() output. */
export class FlightState {
  constructor() { this.reset(); }
  reset() { this.sys = new Map(); this.total = 0; this.badCrc = 0; this.unknown = 0; this.texts = []; }
  add(msgs, now = Date.now()) {
    for (const m of msgs) {
      this.total++;
      if (m.crcOk === false) { this.badCrc++; continue; }
      if (m.crcOk === null) this.unknown++;
      let s = this.sys.get(m.sys);
      if (!s) { s = { sys: m.sys, comps: new Set(), counts: {}, last: 0, d: {} }; this.sys.set(m.sys, s); }
      s.comps.add(m.comp); s.last = now;
      s.counts[m.id] = (s.counts[m.id] ?? 0) + 1;
      const f = decode(m);
      if (!f) continue;
      s.d[msgName(m.id)] = f;
      if (m.id === 253) {
        this.texts.push({ t: now, sys: m.sys, severity: f.severity, text: f.text });
        if (this.texts.length > 30) this.texts.shift();
      }
    }
  }
  /** Plain snapshot: per system the decoded messages, message counts, and a flight summary for the FC. */
  summary(now = Date.now()) {
    const systems = [...this.sys.values()].map((s) => ({ sys: s.sys, comps: [...s.comps], ageMs: now - s.last, counts: { ...s.counts }, d: { ...s.d } }));
    const fc = systems.filter((s) => s.d.HEARTBEAT && s.d.HEARTBEAT.type !== 18 && s.d.HEARTBEAT.type !== 6 && s.sys !== 100 && s.sys !== 101 && s.sys !== 255)
      .sort((a, b) => a.ageMs - b.ageMs)[0] ?? systems.find((s) => s.d.GPS_RAW_INT || s.d.ATTITUDE) ?? null;
    let flight = null;
    if (fc) {
      const d = fc.d, hb = d.HEARTBEAT;
      flight = {
        sys: fc.sys, ageMs: fc.ageMs, autopilot: hb ? AUTOPILOT[hb.autopilot] ?? `autopilot ${hb.autopilot}` : null,
        vehicle: hb ? MAV_TYPE[hb.type] ?? `type ${hb.type}` : null, armed: hb ? !!(hb.baseMode & 128) : null, mode: flightMode(hb),
        gps: d.GPS_RAW_INT ?? null, pos: d.GLOBAL_POSITION_INT ?? null, att: d.ATTITUDE ?? null, hud: d.VFR_HUD ?? null,
        battery: d.SYS_STATUS ? { voltage: d.SYS_STATUS.voltage, current: d.SYS_STATUS.current, remaining: d.SYS_STATUS.batteryRemaining } : d.BATTERY_STATUS ?? null,
        rcRssi: d.RC_CHANNELS?.rssi ?? null,
      };
    }
    const ohd = systems.find((s) => s.sys === 101) ?? null;
    return { total: this.total, badCrc: this.badCrc, unknown: this.unknown, systems, flight, openhd: ohd ? ohd.d : null, texts: this.texts.slice(-10) };
  }
}

/** One .tlog record (Mission Planner / QGroundControl): u64 big-endian µs since epoch + frame. */
export function tlogRecord(raw, ms = Date.now()) {
  const r = new Uint8Array(8 + raw.length);
  new DataView(r.buffer).setBigUint64(0, BigInt(Math.round(ms * 1000)), false);
  r.set(raw, 8);
  return r;
}
