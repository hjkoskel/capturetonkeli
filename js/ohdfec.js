// OpenHD (wifibroadcast) FEC: systematic erasure code over GF(2^8), poly 0x11d,
// Rizzo/Knaff "fec.c" Cauchy-style rows (docs/13 §5.3):
//   secondary r = Σ_j inv((128 + r) XOR j) · S_j,   S_j = LE16(size_j) ‖ data_j ‖ zero pad to L.
// Written from the math; test/js/openhd.test.mjs decodes blocks produced by
// OpenHD's own FECEncoder.
import { inv, invert, addmul } from './hxfec.js';

export const coef = (r, j) => inv((128 + r) ^ j);

/** Encode m secondaries of length L from k symbols (each ≤ L, zero padded). */
export function encode(symbols, m, L) {
  const out = [];
  for (let r = 0; r < m; r++) {
    const s = new Uint8Array(L);
    symbols.forEach((p, j) => addmul(s, p, coef(r, j), Math.min(p.length, L)));
    out.push(s);
  }
  return out;
}

/**
 * Recover missing primaries.
 * @param k number of primaries
 * @param prim Map j → symbol (Uint8Array, LE16 size ‖ data, any length ≤ L)
 * @param sec  Map r → secondary symbol (length L)
 * @returns Map j → recovered symbol (length L) for every missing j
 */
export function decode(k, prim, sec, L) {
  const missing = [];
  for (let j = 0; j < k; j++) if (!prim.has(j)) missing.push(j);
  const e = missing.length;
  if (!e) return new Map();
  const rows = [...sec.keys()].sort((a, b) => a - b).slice(0, e);
  if (rows.length < e) throw new Error(`FEC: need ${e} secondaries, have ${rows.length}`);
  // subtract the known primaries from each secondary
  const rhs = rows.map((r) => {
    const acc = new Uint8Array(L);
    acc.set(sec.get(r).subarray(0, L));
    for (const [j, p] of prim) addmul(acc, p, coef(r, j), Math.min(p.length, L));
    return acc;
  });
  const m = new Uint8Array(e * e);
  rows.forEach((r, a) => missing.forEach((j, b) => { m[a * e + b] = coef(r, j); }));
  const mi = invert(m, e);
  const res = new Map();
  missing.forEach((j, b) => {
    const s = new Uint8Array(L);
    for (let a = 0; a < e; a++) addmul(s, rhs[a], mi[b * e + a], L);
    res.set(j, s);
  });
  return res;
}
