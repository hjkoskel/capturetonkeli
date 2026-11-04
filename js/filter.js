// User filters (docs/04 §6 + App. A): JSON model, pattern parser, presets,
// SET_FILTER encode/decode and the evaluator. Must agree with ctp/filter
// (Go) on testvectors/filter_*.json (test/js/filter.test.mjs).
import { crc32 } from './codec.js';

export const MAX_RULES = 16, MAX_TERMS = 4, MAX_TERM_LEN = 16, MAX_OFFSET = 4095;
const G_DROP_FCS = 1, G_RSSI = 2, G_LEN_MIN = 4, G_LEN_MAX = 8, T_NEGATE = 1;

export class FilterError extends Error {
  constructor(msg) { super(msg); this.name = 'FilterError'; }
}

/** Parses "80", "8?", "aa:bb:cc:dd:ee:ff", "88-8e ?? 03", "80/fc" → {value, mask}. */
export function parsePattern(pattern) {
  const [vp, mp] = pattern.split('/');
  const p = parseHex(vp, true);
  if (mp !== undefined) {
    const m = parseHex(mp, false);
    if (m.value.length !== p.value.length) throw new FilterError(`mask has ${m.value.length} bytes, value has ${p.value.length}`);
    for (let i = 0; i < p.mask.length; i++) p.mask[i] &= m.value[i];
  }
  for (let i = 0; i < p.value.length; i++) p.value[i] &= p.mask[i];
  return p;
}

function parseHex(s, allowWild) {
  let d = s.toLowerCase().replace(/[\s:,\-]/g, '');
  if (d.startsWith('0x')) d = d.slice(2);
  if (!/^[0-9a-f?]*$/.test(d) || (!allowWild && d.includes('?'))) throw new FilterError(`"${s}": only hex digits${allowWild ? ', ?' : ''} and : - space allowed`);
  if (d.length === 0 || d.length % 2) throw new FilterError(`"${s}": need an even, non-zero number of hex digits`);
  const n = d.length / 2, value = new Uint8Array(n), mask = new Uint8Array(n);
  for (let i = 0; i < n; i++) {
    for (let k = 0; k < 2; k++) {
      const c = d[i * 2 + k];
      value[i] <<= 4; mask[i] <<= 4;
      if (c !== '?') { mask[i] |= 0xf; value[i] |= parseInt(c, 16); }
    }
  }
  return { value, mask };
}

export const hexBytes = (b) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join(' ');

/** JSON term {offset, value, mask?, negate} → compiled term. */
export function compileTerm(t) {
  const off = Number(t.offset);
  if (!Number.isInteger(off) || off < 0 || off > MAX_OFFSET) throw new FilterError(`offset ${t.offset} must be 0…${MAX_OFFSET}`);
  const p = parsePattern(t.mask ? `${t.value}/${t.mask}` : t.value);
  if (p.value.length > MAX_TERM_LEN) throw new FilterError(`term is ${p.value.length} bytes (max ${MAX_TERM_LEN})`);
  return { offset: off, value: p.value, mask: p.mask, negate: !!t.negate };
}

/** Validates a JSON set; returns compiled enabled rules. Throws FilterError with a precise message. */
export function compile(set, maxRules = MAX_RULES) {
  const rules = [];
  (set.rules ?? []).forEach((r, i) => {
    if (r.enabled === false) return;
    const name = r.name ? `rule "${r.name}"` : `rule ${i + 1}`;
    const terms = r.terms ?? [];
    if (terms.length < 1 || terms.length > MAX_TERMS) throw new FilterError(`${name}: needs 1…${MAX_TERMS} terms (has ${terms.length})`);
    rules.push(terms.map((t, k) => {
      try { return compileTerm(t); } catch (e) { throw new FilterError(`${name}, term ${k + 1}: ${e.message}`); }
    }));
  });
  if (rules.length > maxRules) throw new FilterError(`${rules.length} enabled rules, the dongle accepts ${maxRules}`);
  const g = set.global ?? {};
  return { rules, dropFcs: !!g.drop_fcs_err, rssiMin: g.rssi_min ?? null, lenMin: g.len_min ?? null, lenMax: g.len_max ?? null };
}

/** SET_FILTER body (Appendix A). */
export function encode(set, maxRules) {
  const c = compile(set, maxRules);
  const out = [c.rules.length, (c.dropFcs ? G_DROP_FCS : 0) | (c.rssiMin !== null ? G_RSSI : 0) |
    (c.lenMin !== null ? G_LEN_MIN : 0) | (c.lenMax !== null ? G_LEN_MAX : 0), (c.rssiMin ?? 0) & 0xff, 0,
  (c.lenMin ?? 0) & 0xff, (c.lenMin ?? 0) >> 8, (c.lenMax ?? 0) & 0xff, (c.lenMax ?? 0) >> 8];
  for (const r of c.rules) {
    out.push(r.length);
    for (const t of r) {
      out.push(t.offset & 0xff, t.offset >> 8, t.value.length, t.negate ? T_NEGATE : 0, ...t.value, ...t.mask);
    }
  }
  return Uint8Array.from(out);
}
export const crcOf = (set, maxRules) => crc32(encode(set, maxRules));

/** Strict decode of a SET_FILTER body into a compiled filter (demo device, tests). */
export function decode(b) {
  const bad = (why) => { throw new FilterError(`invalid SET_FILTER body: ${why}`); };
  if (b.length < 8) bad('shorter than 8 bytes');
  const n = b[0], g = b[1];
  if (n > MAX_RULES) bad(`${n} rules`);
  if (g & ~0x0f) bad('unknown gflags');
  if (b[3] !== 0) bad('reserved byte');
  const c = {
    rules: [], dropFcs: !!(g & G_DROP_FCS), rssiMin: g & G_RSSI ? (b[2] << 24) >> 24 : null,
    lenMin: g & G_LEN_MIN ? b[4] | (b[5] << 8) : null, lenMax: g & G_LEN_MAX ? b[6] | (b[7] << 8) : null,
  };
  let p = 8;
  for (let i = 0; i < n; i++) {
    if (p >= b.length) bad('truncated rule');
    const nt = b[p++];
    if (nt < 1 || nt > MAX_TERMS) bad(`rule with ${nt} terms`);
    const r = [];
    for (let k = 0; k < nt; k++) {
      if (p + 4 > b.length) bad('truncated term');
      const off = b[p] | (b[p + 1] << 8), len = b[p + 2], tf = b[p + 3];
      p += 4;
      if (len < 1 || len > MAX_TERM_LEN) bad(`term length ${len}`);
      if (tf & ~T_NEGATE) bad('unknown tflags');
      if (p + 2 * len > b.length) bad('truncated value/mask');
      const value = b.slice(p, p + len), mask = b.slice(p + len, p + 2 * len);
      for (let j = 0; j < len; j++) value[j] &= mask[j];
      r.push({ offset: off, value, mask, negate: !!(tf & T_NEGATE) });
      p += 2 * len;
    }
    c.rules.push(r);
  }
  if (p !== b.length) bad('trailing bytes');
  return c;
}

/**
 * Evaluator (App. A): pass = globals AND (no rules OR any rule with all
 * terms matching). A term matches iff (in range AND masked bytes equal) XOR negate.
 * meta: {fcsErr, rssi, origLen}.
 */
export function match(c, frame, meta) {
  if (c.dropFcs && meta.fcsErr) return false;
  if (c.rssiMin !== null && meta.rssi < c.rssiMin) return false;
  if (c.lenMin !== null && meta.origLen < c.lenMin) return false;
  if (c.lenMax !== null && meta.origLen > c.lenMax) return false;
  if (c.rules.length === 0) return true;
  return c.rules.some((r) => r.every((t) => termMatch(t, frame)));
}

export function termMatch(t, frame) {
  let eq = t.offset + t.value.length <= frame.length;
  for (let i = 0; eq && i < t.value.length; i++) if ((frame[t.offset + i] & t.mask[i]) !== t.value[i]) eq = false;
  return eq !== t.negate;
}

// ---- presets (same definitions as ctp/filter) ------------------------------------------
const rule = (name, ...terms) => ({ name, enabled: true, terms });
const term = (offset, value, mask, negate = false) => ({ offset, value, ...(mask ? { mask } : {}), negate });
const LLC_EAPOL = 'aa aa 03 00 00 00 88 8e';

export const PRESETS = {
  beacons: { label: 'Beacons only', rules: () => [rule('Beacons', term(0, '80', 'fc'))] },
  probereq: { label: 'Probe requests', rules: () => [rule('Probe requests', term(0, '40', 'fc'))] },
  proberesp: { label: 'Probe responses', rules: () => [rule('Probe responses', term(0, '50', 'fc'))] },
  mgmt: { label: 'All management', rules: () => [rule('Management', term(0, '00', '0c'))] },
  ctrl: { label: 'All control', rules: () => [rule('Control', term(0, '04', '0c'))] },
  data: { label: 'All data', rules: () => [rule('Data', term(0, '08', '0c'))] },
  dropbeacons: { label: 'Drop beacons', rules: () => [rule('Drop beacons', term(0, '80', 'fc', true))] },
  from: { label: 'From MAC… (addr2)', arg: 'mac', rules: (m) => [rule(`From ${m}`, term(10, m))] },
  to: { label: 'To MAC… (addr1)', arg: 'mac', rules: (m) => [rule(`To ${m}`, term(4, m))] },
  bssid: { label: 'BSSID… (addr3)', arg: 'mac', rules: (m) => [rule(`BSSID ${m}`, term(16, m))] },
  deauth: { label: 'Deauth/disassoc', rules: () => [rule('Deauth', term(0, 'c0', 'fc')), rule('Disassoc', term(0, 'a0', 'fc'))] },
  eapol: {
    label: 'EAPOL (2 rules)',
    rules: () => [rule('EAPOL', term(0, '08', 'fc'), term(24, LLC_EAPOL)), rule('EAPOL (QoS)', term(0, '88', 'fc'), term(26, LLC_EAPOL))],
  },
};

export const emptySet = () => ({ version: 1, rules: [], global: { drop_fcs_err: true, rssi_min: null, len_min: null, len_max: null } });
