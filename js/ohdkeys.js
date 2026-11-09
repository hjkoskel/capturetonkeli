// OpenHD keys (docs/13 §4.4): bind phrase → Argon2id seeds → Curve25519 keypairs
// → beforenm keys K_air / K_gnd, or the same from a 128-byte txrx.key file.
// Written from RFC 7748 (X25519), RFC 7693 (BLAKE2b), RFC 9106 (Argon2id) and
// libsodium's crypto_box_seed_keypair / beforenm; test/js/openhd.test.mjs
// checks it against libsodium and OpenHD's own key derivation.
import { hsalsa20 } from './ohdcrypto.js';
import { DEFAULT_KEYS, hexToBytes, bytesToHex } from './openhd.js';

const SALT_AIR = Uint8Array.of(192, 189, 216, 102, 56, 153, 154, 92, 228, 26, 49, 209, 157, 7, 128, 207);
const SALT_GND = Uint8Array.of(179, 30, 150, 20, 17, 200, 225, 82, 48, 64, 18, 130, 89, 62, 83, 234);

// ---- X25519 (BigInt Montgomery ladder; used once per key) ----
const P = (1n << 255n) - 19n;
const md = (a) => ((a % P) + P) % P;
function pow(b, e) { let r = 1n; b = md(b); while (e > 0n) { if (e & 1n) r = (r * b) % P; b = (b * b) % P; e >>= 1n; } return r; }
const leToBig = (b) => { let v = 0n; for (let i = b.length - 1; i >= 0; i--) v = (v << 8n) | BigInt(b[i]); return v; };
const bigToLe = (v, n = 32) => { const o = new Uint8Array(n); for (let i = 0; i < n; i++) { o[i] = Number(v & 0xffn); v >>= 8n; } return o; };
export function x25519(scalar, u = Uint8Array.of(9)) {
  const k = Uint8Array.from(scalar);
  k[0] &= 248; k[31] &= 127; k[31] |= 64;
  const kn = leToBig(k), x1 = leToBig(u.length === 32 ? Uint8Array.from(u, (b, i) => (i === 31 ? b & 127 : b)) : u);
  let x2 = 1n, z2 = 0n, x3 = x1, z3 = 1n, swap = 0n;
  for (let t = 254n; t >= 0n; t--) {
    const kt = (kn >> t) & 1n;
    if (swap ^ kt) { [x2, x3] = [x3, x2]; [z2, z3] = [z3, z2]; }
    swap = kt;
    const A = md(x2 + z2), AA = md(A * A), B = md(x2 - z2), BB = md(B * B), E = md(AA - BB);
    const C = md(x3 + z3), D = md(x3 - z3), DA = md(D * A), CB = md(C * B);
    x3 = md((DA + CB) ** 2n); z3 = md(x1 * md((DA - CB) ** 2n));
    x2 = md(AA * BB); z2 = md(E * (AA + 121665n * E));
  }
  if (swap) { [x2, x3] = [x3, x2]; [z2, z3] = [z3, z2]; }
  return bigToLe(md(x2 * pow(z2, P - 2n)));
}
/** crypto_box_beforenm(pk, sk) = HSalsa20(X25519(sk, pk), 0). */
export const beforenm = (pk, sk) => hsalsa20(x25519(sk, pk), new Uint8Array(16));
/** crypto_box_seed_keypair: sk = SHA-512(seed)[0..32], pk = X25519(sk, 9). */
export async function seedKeypair(seed) {
  const h = new Uint8Array(await crypto.subtle.digest('SHA-512', seed));
  const sk = h.slice(0, 32);
  return { pk: x25519(sk), sk };
}

// ---- BLAKE2b (32-bit halves) ----
const IV = Uint32Array.of(0xf3bcc908, 0x6a09e667, 0x84caa73b, 0xbb67ae85, 0xfe94f82b, 0x3c6ef372, 0x5f1d36f1, 0xa54ff53a,
  0xade682d1, 0x510e527f, 0x2b3e6c1f, 0x9b05688c, 0xfb41bd6b, 0x1f83d9ab, 0x137e2179, 0x5be0cd19);
const SIGMA = [
  [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15], [14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3],
  [11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4], [7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8],
  [9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13], [2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9],
  [12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11], [13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10],
  [6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5], [10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0],
].map((r) => r.map((x) => x * 2));
function add64(v, a, b) { const lo = v[a] + v[b]; v[a + 1] = v[a + 1] + v[b + 1] + (lo > 0xffffffff ? 1 : 0); v[a] = lo; }
function add64m(v, a, lo, hi) { const l = v[a] + lo; v[a + 1] = v[a + 1] + hi + (l > 0xffffffff ? 1 : 0); v[a] = l; }
function xorRot(v, d, a, n) { // v[d] = rotr64(v[d] ^ v[a], n)
  const lo = v[d] ^ v[a], hi = v[d + 1] ^ v[a + 1];
  if (n === 32) { v[d] = hi; v[d + 1] = lo; } else if (n < 32) { v[d] = (lo >>> n) | (hi << (32 - n)); v[d + 1] = (hi >>> n) | (lo << (32 - n)); } else { const m = n - 32; v[d] = (hi >>> m) | (lo << (32 - m)); v[d + 1] = (lo >>> m) | (hi << (32 - m)); }
}
const bv = new Uint32Array(32), bm = new Uint32Array(32);
function compress(h, block, t, last) {
  for (let i = 0; i < 16; i++) { bv[i] = h[i]; bv[i + 16] = IV[i]; }
  bv[24] ^= t >>> 0; bv[25] ^= Math.floor(t / 4294967296);
  if (last) { bv[28] = ~bv[28]; bv[29] = ~bv[29]; }
  for (let i = 0; i < 32; i++) bm[i] = block[i * 4] | (block[i * 4 + 1] << 8) | (block[i * 4 + 2] << 16) | (block[i * 4 + 3] << 24);
  const G = (a, b, c, d, x, y) => {
    add64(bv, a, b); add64m(bv, a, bm[x], bm[x + 1]); xorRot(bv, d, a, 32);
    add64(bv, c, d); xorRot(bv, b, c, 24);
    add64(bv, a, b); add64m(bv, a, bm[y], bm[y + 1]); xorRot(bv, d, a, 16);
    add64(bv, c, d); xorRot(bv, b, c, 63);
  };
  for (let r = 0; r < 12; r++) {
    const s = SIGMA[r % 10];
    G(0, 8, 16, 24, s[0], s[1]); G(2, 10, 18, 26, s[2], s[3]); G(4, 12, 20, 28, s[4], s[5]); G(6, 14, 22, 30, s[6], s[7]);
    G(0, 10, 20, 30, s[8], s[9]); G(2, 12, 22, 24, s[10], s[11]); G(4, 14, 16, 26, s[12], s[13]); G(6, 8, 18, 28, s[14], s[15]);
  }
  for (let i = 0; i < 16; i++) h[i] ^= bv[i] ^ bv[i + 16];
}
/** BLAKE2b (no key) of the concatenated parts, outLen 1…64. */
export function blake2b(outLen, ...parts) {
  const h = Uint32Array.from(IV);
  h[0] ^= 0x01010000 ^ outLen;
  const buf = new Uint8Array(128);
  let fill = 0, t = 0;
  for (const p of parts) {
    for (let i = 0; i < p.length; i++) {
      if (fill === 128) { t += 128; compress(h, buf, t, false); fill = 0; }
      buf[fill++] = p[i];
    }
  }
  t += fill;
  buf.fill(0, fill);
  compress(h, buf, t, true);
  const out = new Uint8Array(outLen);
  for (let i = 0; i < outLen; i++) out[i] = h[i >> 2] >>> (8 * (i & 3));
  return out;
}
const le32b = (n) => Uint8Array.of(n, n >>> 8, n >>> 16, n >>> 24);
/** Argon2 variable-length hash H'. */
function hPrime(T, ...parts) {
  if (T <= 64) return blake2b(T, le32b(T), ...parts);
  const out = new Uint8Array(T), r = Math.ceil(T / 32) - 2;
  let v = blake2b(64, le32b(T), ...parts);
  out.set(v.subarray(0, 32), 0);
  for (let i = 1; i < r; i++) { v = blake2b(64, v); out.set(v.subarray(0, 32), 32 * i); }
  out.set(blake2b(T - 32 * r, v), 32 * r);
  return out;
}

// ---- Argon2id (version 0x13, one lane) ----
// fBlaMka: v[a] = v[a] + v[b] + 2·lo32(v[a])·lo32(v[b])
function mulAdd(v, a, b) {
  const x = v[a], y = v[b];
  const xl = x & 0xffff, xh = x >>> 16, yl = y & 0xffff, yh = y >>> 16;
  const mid = xh * yl + xl * yh; // < 2^33
  const low = xl * yl + (mid % 65536) * 65536; // < 2^33
  let lo = low % 4294967296, hi = xh * yh + Math.floor(mid / 65536) + Math.floor(low / 4294967296);
  hi = (hi * 2 + (lo >= 2147483648 ? 1 : 0)) % 4294967296; lo = (lo * 2) % 4294967296;
  add64(v, a, b); add64m(v, a, lo, hi);
}
function GB(v, a, b, c, d) {
  mulAdd(v, a, b); xorRot(v, d, a, 32);
  mulAdd(v, c, d); xorRot(v, b, c, 24);
  mulAdd(v, a, b); xorRot(v, d, a, 16);
  mulAdd(v, c, d); xorRot(v, b, c, 63);
}
const R = new Uint32Array(256), Q = new Uint32Array(256);
const PR = Array.from({ length: 8 }, (_, i) => Array.from({ length: 16 }, (_, j) => 2 * (16 * i + j)));
const PC = Array.from({ length: 8 }, (_, i) => Array.from({ length: 16 }, (_, j) => 2 * (2 * i + (j & 1) + 16 * (j >> 1))));
function permute(v, w) { // w = 16 word indices (×2)
  GB(v, w[0], w[4], w[8], w[12]); GB(v, w[1], w[5], w[9], w[13]); GB(v, w[2], w[6], w[10], w[14]); GB(v, w[3], w[7], w[11], w[15]);
  GB(v, w[0], w[5], w[10], w[15]); GB(v, w[1], w[6], w[11], w[12]); GB(v, w[2], w[7], w[8], w[13]); GB(v, w[3], w[4], w[9], w[14]);
}
/** dst = G(X, Y) (xor dst when `xor`). Blocks are offsets (in u32) into mem, or separate arrays. */
function G(mem, x, y, dstArr, dst, xor) {
  for (let i = 0; i < 256; i++) R[i] = mem[x + i] ^ mem[y + i];
  Q.set(R);
  for (const w of PR) permute(Q, w);
  for (const w of PC) permute(Q, w);
  if (xor) for (let i = 0; i < 256; i++) dstArr[dst + i] ^= Q[i] ^ R[i];
  else for (let i = 0; i < 256; i++) dstArr[dst + i] = Q[i] ^ R[i];
}
const toU32 = (b) => new Uint32Array(b.buffer.slice(b.byteOffset, b.byteOffset + b.length));

/**
 * Argon2id(password, salt, t passes, m KiB, 1 lane) → tagLen bytes. onProgress(0…1) is called per slice.
 * Yields to the event loop between slices so a worker stays responsive.
 */
export async function argon2id(pw, salt, { t = 2, m = 65536, tagLen = 32, onProgress } = {}) {
  const lanes = 1, mm = 4 * Math.floor(m / 4), q = mm, SL = q / 4;
  const h0 = blake2b(64, le32b(lanes), le32b(tagLen), le32b(m), le32b(t), le32b(0x13), le32b(2), le32b(pw.length), pw, le32b(salt.length), salt, le32b(0), le32b(0));
  const mem = new Uint32Array(256 * mm);
  mem.set(toU32(hPrime(1024, h0, le32b(0), le32b(0))), 0);
  mem.set(toU32(hPrime(1024, h0, le32b(1), le32b(0))), 256);
  const addr = new Uint32Array(256), input = new Uint32Array(256);
  for (let r = 0; r < t; r++) {
    for (let s = 0; s < 4; s++) {
      const indep = r === 0 && s < 2;
      if (indep) { input.fill(0); input[0] = r; input[2] = 0; input[4] = s; input[6] = mm; input[8] = t; input[10] = 2; input[12] = 0; }
      const start = r === 0 && s === 0 ? 2 : 0;
      if (indep && start) nextAddresses();
      for (let i = start; i < SL; i++) {
        const cur = s * SL + i, prev = cur === 0 ? q - 1 : cur - 1;
        let j1, j2;
        if (indep) { if (i % 128 === 0) nextAddresses(); j1 = addr[2 * (i % 128)]; j2 = addr[2 * (i % 128) + 1]; } else { j1 = mem[256 * prev]; j2 = mem[256 * prev + 1]; }
        void j2; // one lane: the reference lane is always 0
        const W = r === 0 ? s * SL + i - 1 : q - SL + i - 1;
        const x = Number((BigInt(j1) * BigInt(j1)) >> 32n);
        const y = Math.floor((W * x) / 4294967296);
        const zz = W - 1 - y, st0 = r === 0 ? 0 : ((s + 1) * SL) % q;
        const ref = (st0 + zz) % q;
        G(mem, 256 * prev, 256 * ref, mem, 256 * cur, r > 0);
      }
      onProgress?.((r * 4 + s + 1) / (4 * t));
      await new Promise((res) => setTimeout(res, 0));
    }
  }
  function nextAddresses() { // addresses = G(0, G(0, input)); G(X, Y) reads X at 0 and Y at 256
    input[12]++;
    const a = new Uint32Array(512), b = new Uint32Array(512);
    a.set(input, 256);
    G(a, 0, 256, b, 256, false);
    G(b, 0, 256, addr, 0, false);
  }
  const last = mem.subarray(256 * (q - 1), 256 * q);
  return hPrime(tagLen, new Uint8Array(last.buffer, last.byteOffset, 1024));
}

/** Bind phrase → keys {air, gnd} as hex beforenm keys (docs/13 §4.4). ~64 MiB, a few seconds. */
export async function keysFromPhrase(phrase, onProgress) {
  if (phrase === 'openhd') return { ...DEFAULT_KEYS };
  const pw = new TextEncoder().encode(phrase);
  const half = (f) => (p) => onProgress?.(f + p / 2);
  const seedAir = await argon2id(pw, SALT_AIR, { onProgress: half(0) });
  const seedGnd = await argon2id(pw, SALT_GND, { onProgress: half(0.5) });
  const a = await seedKeypair(seedAir), g = await seedKeypair(seedGnd);
  return { air: bytesToHex(beforenm(a.pk, a.sk)), gnd: bytesToHex(beforenm(g.pk, g.sk)) };
}
/** 128-byte txrx.key (key_1.pub ‖ key_1.sec ‖ key_2.pub ‖ key_2.sec) → keys. */
export function keysFromKeyFile(b) {
  if (b.length !== 128) throw new Error(`txrx.key must be 128 bytes (this file has ${b.length})`);
  return { air: bytesToHex(beforenm(b.subarray(0, 32), b.subarray(32, 64))), gnd: bytesToHex(beforenm(b.subarray(64, 96), b.subarray(96, 128))) };
}
export { hexToBytes };
