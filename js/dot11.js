// Minimal 802.11 decoder (docs/04 §8), mirrors ctp/dot11 (Go). Never throws
// on truncated input; fields beyond the captured bytes are reported missing.

const NAMES = {
  '0/0': 'Assoc Request', '0/1': 'Assoc Response', '0/2': 'Reassoc Request', '0/3': 'Reassoc Response',
  '0/4': 'Probe Request', '0/5': 'Probe Response', '0/6': 'Timing Advert', '0/8': 'Beacon', '0/9': 'ATIM',
  '0/10': 'Disassoc', '0/11': 'Auth', '0/12': 'Deauth', '0/13': 'Action', '0/14': 'Action No Ack',
  '1/2': 'Trigger', '1/4': 'Beamforming Poll', '1/5': 'NDP Announcement', '1/7': 'Control Wrapper',
  '1/8': 'Block Ack Request', '1/9': 'Block Ack', '1/10': 'PS-Poll', '1/11': 'RTS', '1/12': 'CTS', '1/13': 'ACK',
  '1/14': 'CF-End', '1/15': 'CF-End+CF-Ack', '2/0': 'Data', '2/4': 'Null', '2/8': 'QoS Data', '2/12': 'QoS Null',
  '3/0': 'DMG Beacon',
};
export const typeName = (t, s) => NAMES[`${t}/${s}`] ?? `t${t}/s${s}`;
export const TYPE_CLASS = ['mgmt', 'ctrl', 'data', 'ext'];

const mac = (b, o) => (o + 6 <= b.length ? Array.from(b.subarray(o, o + 6), (x) => x.toString(16).padStart(2, '0')).join(':') : null);

/** Byte ranges of decoded fields, for hex-dump highlighting: [name, start, len]. */
export function parse(b) {
  const h = { type: 0, subtype: 0, name: 'runt', fields: [], truncated: true, headerLen: 0 };
  if (b.length < 2) return h;
  const fc0 = b[0], fc1 = b[1];
  h.type = (fc0 >> 2) & 3; h.subtype = fc0 >> 4; h.name = typeName(h.type, h.subtype);
  h.toDS = !!(fc1 & 1); h.fromDS = !!(fc1 & 2); h.moreFrag = !!(fc1 & 4); h.retry = !!(fc1 & 8);
  h.pwrMgmt = !!(fc1 & 16); h.moreData = !!(fc1 & 32); h.protected = !!(fc1 & 64); h.order = !!(fc1 & 128);
  h.fields.push(['Frame Control', 0, 2]);
  if (b.length >= 4) { h.duration = b[2] | (b[3] << 8); h.fields.push(['Duration/ID', 2, 2]); }
  h.addr1 = mac(b, 4); if (h.addr1) h.fields.push(['Addr1 (receiver)', 4, 6]);
  let need = 10;
  if (h.type === 1) {
    if ([2, 4, 5, 8, 9, 10, 11, 14, 15].includes(h.subtype)) {
      h.addr2 = mac(b, 10); need = 16;
      if (h.addr2) h.fields.push(['Addr2 (transmitter)', 10, 6]);
    }
  } else {
    h.addr2 = mac(b, 10); h.addr3 = mac(b, 16); need = 24;
    if (h.addr2) h.fields.push(['Addr2 (transmitter)', 10, 6]);
    if (h.addr3) h.fields.push(['Addr3', 16, 6]);
    if (b.length >= 24) {
      const sc = b[22] | (b[23] << 8);
      h.seq = sc >> 4; h.frag = sc & 15; h.fields.push(['Sequence Control', 22, 2]);
    }
    if (h.type === 2 && h.toDS && h.fromDS) { h.addr4 = mac(b, 24); need = 30; if (h.addr4) h.fields.push(['Addr4', 24, 6]); }
    if (h.type === 2 && h.subtype & 8) {
      h.qos = true;
      if (b.length >= need + 2) { h.tid = b[need] & 15; h.fields.push(['QoS Control', need, 2]); }
      need += 2;
    }
  }
  if (b.length < need) return h;
  h.truncated = false; h.headerLen = need;
  h.ies = ies(h, b.subarray(need), need);
  for (const ie of h.ies) {
    if (ie.id === 0) h.ssid = ie.text;
    if (ie.id === 3 && ie.data.length === 1) h.dsChannel = ie.data[0];
    if (ie.id === 48) h.rsn = true;
  }
  h.bssid = h.type === 0 ? h.addr3 : h.type === 2 ? (h.toDS && !h.fromDS ? h.addr1 : !h.toDS && h.fromDS ? h.addr2 : !h.toDS ? h.addr3 : null) : null;
  return h;
}

const IE_NAMES = { 0: 'SSID', 1: 'Supported Rates', 3: 'DS Parameter Set', 5: 'TIM', 7: 'Country', 42: 'ERP', 45: 'HT Capabilities',
  48: 'RSN', 50: 'Extended Rates', 61: 'HT Operation', 127: 'Extended Capabilities', 191: 'VHT Capabilities', 192: 'VHT Operation',
  221: 'Vendor Specific', 255: 'Extension' };
const td = new TextDecoder('utf-8', { fatal: false });

function ies(h, body, base) {
  if (h.type !== 0 || h.protected) return [];
  let skip;
  switch (h.subtype) {
    case 8: case 5: skip = 12; break;
    case 0: skip = 4; break;
    case 1: case 3: skip = 6; break;
    case 2: skip = 10; break;
    case 4: skip = 0; break;
    default: return [];
  }
  if (body.length < skip) return [];
  if (skip) h.fields.push(['Fixed parameters', base, skip]);
  const out = [];
  let p = skip;
  while (p + 2 <= body.length) {
    const id = body[p], n = body[p + 1];
    if (p + 2 + n > body.length) { out.push({ id, name: (IE_NAMES[id] ?? `IE ${id}`) + ' [truncated]', data: body.subarray(p + 2), start: base + p, len: body.length - p }); break; }
    const data = body.subarray(p + 2, p + 2 + n);
    const ie = { id, name: IE_NAMES[id] ?? `IE ${id}`, data, start: base + p, len: n + 2 };
    if (id === 0) ie.text = td.decode(data);
    if (id === 221 && n >= 3) ie.oui = Array.from(data.subarray(0, 3), (x) => x.toString(16).padStart(2, '0')).join(':');
    out.push(ie);
    p += 2 + n;
  }
  return out;
}

/** Short flags string for the Info column. */
export function flagsText(h) {
  return [h.retry && 'Retry', h.protected && 'Protected', h.moreData && 'More Data', h.pwrMgmt && 'PwrMgt'].filter(Boolean).join(' ');
}
