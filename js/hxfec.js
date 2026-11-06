// Systematic Reed-Solomon erasure code over GF(2^8) (Vandermonde, Rizzo/zfec
// construction) as used by hx-esp32-cam-fpv (docs/12 §3). Written from the
// math; test/js/hxfpv.test.mjs checks it byte-for-byte against vectors from
// the hx-esp32-cam-fpv C implementation.
//
// Block numbers 0…k-1 are the data packets themselves, k…n-1 are FEC packets.
// Row r of the encoding matrix does not depend on n, so one table serves any n.

const exp = new Uint8Array(510), log = new Int16Array(256);
{
  let x = 1;
  for (let i = 0; i < 255; i++) {
    exp[i] = x; log[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d; // x^8 + x^4 + x^3 + x^2 + 1
  }
  for (let i = 255; i < 510; i++) exp[i] = exp[i - 255];
  log[0] = -1;
}
/** 64 KB multiplication table: MUL[a << 8 | b] = a·b. */
const MUL = new Uint8Array(65536);
for (let a = 1; a < 256; a++) for (let b = 1; b < 256; b++) MUL[(a << 8) | b] = exp[log[a] + log[b]];

export const mul = (a, b) => MUL[(a << 8) | b];
function inv(a) { if (!a) throw new Error('GF(256): inverse of 0'); return exp[255 - log[a]]; }

/** Gauss-Jordan inverse of a k×k matrix (Uint8Array, row-major). Throws if singular. */
export function invert(m, k) {
  const a = Uint8Array.from(m), r = new Uint8Array(k * k);
  for (let i = 0; i < k; i++) r[i * k + i] = 1;
  for (let c = 0; c < k; c++) {
    let p = c;
    while (p < k && !a[p * k + c]) p++;
    if (p === k) throw new Error('FEC: singular matrix');
    if (p !== c) for (let j = 0; j < k; j++) {
      [a[p * k + j], a[c * k + j]] = [a[c * k + j], a[p * k + j]];
      [r[p * k + j], r[c * k + j]] = [r[c * k + j], r[p * k + j]];
    }
    const s = inv(a[c * k + c]);
    for (let j = 0; j < k; j++) { a[c * k + j] = mul(a[c * k + j], s); r[c * k + j] = mul(r[c * k + j], s); }
    for (let i = 0; i < k; i++) {
      const f = a[i * k + c];
      if (i === c || !f) continue;
      for (let j = 0; j < k; j++) { a[i * k + j] ^= mul(f, a[c * k + j]); r[i * k + j] ^= mul(f, r[c * k + j]); }
    }
  }
  return r;
}

const cache = new Map();
/**
 * Encoding rows k…maxN-1 (each k bytes) for data count k: the bottom of the
 * Vandermonde matrix V (row 0 = e0, row i+1 = [α^(i·j)]) times inv(V top k×k).
 */
export function encRows(k, maxN = 32) {
  const key = `${k}/${maxN}`;
  let rows = cache.get(key);
  if (rows) return rows;
  if (!(k >= 1 && k <= 16 && maxN > k && maxN <= 32)) throw new Error(`FEC: unsupported k=${k} n=${maxN}`);
  const v = new Uint8Array(maxN * k);
  v[0] = 1;
  for (let r = 0; r < maxN - 1; r++) for (let c = 0; c < k; c++) v[(r + 1) * k + c] = exp[(r * c) % 255];
  const top = invert(v.subarray(0, k * k), k);
  rows = new Uint8Array((maxN - k) * k);
  for (let r = k; r < maxN; r++) for (let c = 0; c < k; c++) {
    let s = 0;
    for (let j = 0; j < k; j++) s ^= mul(v[r * k + j], top[j * k + c]);
    rows[(r - k) * k + c] = s;
  }
  cache.set(key, rows);
  return rows;
}

/** dst ^= c·src */
export function addmul(dst, src, c, n = dst.length) {
  if (!c) return;
  const o = c << 8;
  for (let i = 0; i < n; i++) dst[i] ^= MUL[o | src[i]];
}

/** FEC packet number idx (k ≤ idx < 32) from the k data packets (all `size` bytes). */
export function encode(k, data, idx, size) {
  const row = encRows(k).subarray((idx - k) * k, (idx - k + 1) * k);
  const out = new Uint8Array(size);
  for (let j = 0; j < k; j++) addmul(out, data[j], row[j], size);
  return out;
}

/**
 * Recovers missing data packets. `have`: Map packetIndex → Uint8Array(size)
 * with at least k entries (data and/or FEC). Returns Map index → data for the
 * missing data packets 0…k-1.
 */
export function decode(k, have, size) {
  const rows = encRows(k);
  const present = [], missing = [];
  for (let i = 0; i < k; i++) (have.has(i) ? present : missing).push(i);
  const out = new Map();
  if (!missing.length) return out;
  const fecIdx = [...have.keys()].filter((i) => i >= k).sort((a, b) => a - b).slice(0, missing.length);
  if (fecIdx.length < missing.length) throw new Error(`FEC: need ${k} packets, have ${present.length + fecIdx.length}`);
  // Row i of M is the encoding row of the packet used in slot i (identity for data packets).
  const m = new Uint8Array(k * k), src = new Array(k);
  let f = 0;
  for (let i = 0; i < k; i++) {
    if (have.has(i)) { m[i * k + i] = 1; src[i] = have.get(i); continue; }
    const idx = fecIdx[f++];
    m.set(rows.subarray((idx - k) * k, (idx - k + 1) * k), i * k);
    src[i] = have.get(idx);
  }
  const d = invert(m, k);
  for (const i of missing) {
    const buf = new Uint8Array(size);
    for (let j = 0; j < k; j++) addmul(buf, src[j], d[i * k + j], size);
    out.set(i, buf);
  }
  return out;
}
