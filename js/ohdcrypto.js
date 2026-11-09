// Crypto primitives for the OpenHD receiver (docs/13 §4), written from the
// algorithm descriptions (Salsa20/XSalsa20, ChaCha20, Poly1305 as in libsodium).
// Only what a passive receiver (and the demo transmitter) needs:
//   - secretbox open/seal  (session key packet, crypto_box_*_afternm)
//   - auth-only packets    (Poly1305 with an HChaCha20 subkey)
//   - encrypted packets    (libsodium crypto_aead_chacha20poly1305, ORIGINAL 8-byte-nonce variant)
//   - beforenm             (HSalsa20(X25519 shared secret)), see js/ohdkeys.js
// test/js/openhd.test.mjs checks everything against libsodium output.

const rotl = (x, n) => (x << n) | (x >>> (32 - n));
const le32 = (b, i) => (b[i] | (b[i + 1] << 8) | (b[i + 2] << 16) | (b[i + 3] << 24)) >>> 0;
function put32(b, i, v) { b[i] = v; b[i + 1] = v >>> 8; b[i + 2] = v >>> 16; b[i + 3] = v >>> 24; }

const SIGMA = [0x61707865, 0x3320646e, 0x79622d32, 0x6b206574]; // "expand 32-byte k"

// ---- Salsa20 ----
function salsaRounds(x) {
  for (let i = 0; i < 20; i += 2) {
    x[4] ^= rotl(x[0] + x[12], 7); x[8] ^= rotl(x[4] + x[0], 9); x[12] ^= rotl(x[8] + x[4], 13); x[0] ^= rotl(x[12] + x[8], 18);
    x[9] ^= rotl(x[5] + x[1], 7); x[13] ^= rotl(x[9] + x[5], 9); x[1] ^= rotl(x[13] + x[9], 13); x[5] ^= rotl(x[1] + x[13], 18);
    x[14] ^= rotl(x[10] + x[6], 7); x[2] ^= rotl(x[14] + x[10], 9); x[6] ^= rotl(x[2] + x[14], 13); x[10] ^= rotl(x[6] + x[2], 18);
    x[3] ^= rotl(x[15] + x[11], 7); x[7] ^= rotl(x[3] + x[15], 9); x[11] ^= rotl(x[7] + x[3], 13); x[15] ^= rotl(x[11] + x[7], 18);
    x[1] ^= rotl(x[0] + x[3], 7); x[2] ^= rotl(x[1] + x[0], 9); x[3] ^= rotl(x[2] + x[1], 13); x[0] ^= rotl(x[3] + x[2], 18);
    x[6] ^= rotl(x[5] + x[4], 7); x[7] ^= rotl(x[6] + x[5], 9); x[4] ^= rotl(x[7] + x[6], 13); x[5] ^= rotl(x[4] + x[7], 18);
    x[11] ^= rotl(x[10] + x[9], 7); x[8] ^= rotl(x[11] + x[10], 9); x[9] ^= rotl(x[8] + x[11], 13); x[10] ^= rotl(x[9] + x[8], 18);
    x[12] ^= rotl(x[15] + x[14], 7); x[13] ^= rotl(x[12] + x[15], 9); x[14] ^= rotl(x[13] + x[12], 13); x[15] ^= rotl(x[14] + x[13], 18);
  }
}
function salsaInit(key, n16) {
  const s = new Uint32Array(16);
  s[0] = SIGMA[0]; s[5] = SIGMA[1]; s[10] = SIGMA[2]; s[15] = SIGMA[3];
  for (let i = 0; i < 4; i++) { s[1 + i] = le32(key, 4 * i); s[11 + i] = le32(key, 16 + 4 * i); s[6 + i] = le32(n16, 4 * i); }
  return s;
}
/** HSalsa20(key 32, in 16) → 32 bytes. */
export function hsalsa20(key, in16) {
  const x = salsaInit(key, in16);
  salsaRounds(x);
  const o = new Uint8Array(32), idx = [0, 5, 10, 15, 6, 7, 8, 9];
  idx.forEach((j, i) => put32(o, 4 * i, x[j]));
  return o;
}
/** XSalsa20 XOR with a 24-byte nonce, starting at keystream byte `skip` (multiple of 64 or 32 for secretbox). */
function xsalsa20Xor(key, n24, data, skip = 0) {
  const sub = hsalsa20(key, n24.subarray(0, 16));
  const n = new Uint8Array(16);
  n.set(n24.subarray(16, 24));
  const s0 = salsaInit(sub, n), x = new Uint32Array(16), ks = new Uint8Array(64), out = new Uint8Array(data.length);
  let ctr = 0, pos = 0, off = skip;
  while (pos < data.length) {
    s0[8] = ctr >>> 0; s0[9] = Math.floor(ctr / 4294967296);
    x.set(s0); salsaRounds(x);
    for (let i = 0; i < 16; i++) put32(ks, 4 * i, x[i] + s0[i]);
    for (let i = off; i < 64 && pos < data.length; i++) out[pos] = data[pos++] ^ ks[i];
    off = 0; ctr++;
  }
  return out;
}

// ---- ChaCha20 (original: 64-bit counter, 64-bit nonce) ----
function chachaRounds(x) {
  const qr = (a, b, c, d) => {
    x[a] += x[b]; x[d] = rotl(x[d] ^ x[a], 16);
    x[c] += x[d]; x[b] = rotl(x[b] ^ x[c], 12);
    x[a] += x[b]; x[d] = rotl(x[d] ^ x[a], 8);
    x[c] += x[d]; x[b] = rotl(x[b] ^ x[c], 7);
  };
  for (let i = 0; i < 20; i += 2) {
    qr(0, 4, 8, 12); qr(1, 5, 9, 13); qr(2, 6, 10, 14); qr(3, 7, 11, 15);
    qr(0, 5, 10, 15); qr(1, 6, 11, 12); qr(2, 7, 8, 13); qr(3, 4, 9, 14);
  }
}
function chachaInit(key) {
  const s = new Uint32Array(16);
  s.set(SIGMA);
  for (let i = 0; i < 8; i++) s[4 + i] = le32(key, 4 * i);
  return s;
}
/** HChaCha20(key 32, in 16) → 32 bytes (libsodium crypto_core_hchacha20 with c = NULL). */
export function hchacha20(key, in16) {
  const x = chachaInit(key);
  for (let i = 0; i < 4; i++) x[12 + i] = le32(in16, 4 * i);
  chachaRounds(x);
  const o = new Uint8Array(32);
  [0, 1, 2, 3, 12, 13, 14, 15].forEach((j, i) => put32(o, 4 * i, x[j]));
  return o;
}
/** ChaCha20 keystream XOR, 8-byte nonce, starting at block `ic`. */
export function chacha20Xor(key, n8, data, ic = 0) {
  const s0 = chachaInit(key), x = new Uint32Array(16), ks = new Uint8Array(64), out = new Uint8Array(data.length);
  s0[14] = le32(n8, 0); s0[15] = le32(n8, 4);
  let ctr = ic, pos = 0;
  while (pos < data.length) {
    s0[12] = ctr >>> 0; s0[13] = Math.floor(ctr / 4294967296);
    x.set(s0); chachaRounds(x);
    for (let i = 0; i < 16; i++) put32(ks, 4 * i, x[i] + s0[i]);
    for (let i = 0; i < 64 && pos < data.length; i++) out[pos] = data[pos++] ^ ks[i];
    ctr++;
  }
  return out;
}

// ---- Poly1305: 10 limbs of 13 bits (all intermediate products stay < 2^53) ----
const P1305 = (1n << 130n) - 5n;
function limbs13(v) { const l = new Float64Array(10); for (let i = 0; i < 10; i++) { l[i] = Number(v & 0x1fffn); v >>= 13n; } return l; }
function leBig(b, off, n) { let v = 0n; for (let i = n - 1; i >= 0; i--) v = (v << 8n) | BigInt(b[off + i]); return v; }

/** Poly1305 tag of a list of byte arrays (concatenated) under a 32-byte one-time key. */
export function poly1305(key32, ...parts) {
  const r = limbs13(leBig(key32, 0, 16) & 0x0ffffffc0ffffffc0ffffffc0fffffffn);
  const r5 = r.map((v) => v * 5);
  const h = new Float64Array(10), d = new Float64Array(10), blk = new Uint8Array(16);
  let fill = 0;
  const block = (b, final) => {
    // h += block (+ 2^128 for full blocks, + 1 after the last byte for the final partial block)
    let lo = 0, nb = 0, li = 0;
    const add = (byte) => { lo |= byte << nb; nb += 8; while (nb >= 13) { h[li++] += lo & 0x1fff; lo >>>= 13; nb -= 13; } };
    for (let i = 0; i < 16; i++) add(b[i]);
    add(final ? 0 : 1); // bit 128
    if (li < 10) h[li] += lo;
    for (let i = 0; i < 10; i++) {
      let s = 0;
      for (let j = 0; j <= i; j++) s += h[j] * r[i - j];
      for (let j = i + 1; j < 10; j++) s += h[j] * r5[i + 10 - j];
      d[i] = s;
    }
    let c = 0;
    for (let i = 0; i < 10; i++) { d[i] += c; c = Math.floor(d[i] / 8192); d[i] -= c * 8192; }
    d[0] += c * 5;
    c = Math.floor(d[0] / 8192); d[0] -= c * 8192; d[1] += c;
    h.set(d);
  };
  for (const p of parts) {
    for (let i = 0; i < p.length; i++) {
      blk[fill++] = p[i];
      if (fill === 16) { block(blk, false); fill = 0; }
    }
  }
  if (fill) { blk[fill] = 1; blk.fill(0, fill + 1); block(blk, true); }
  let acc = 0n;
  for (let i = 9; i >= 0; i--) acc = (acc << 13n) + BigInt(h[i]);
  acc %= P1305;
  acc = (acc + leBig(key32, 16, 16)) & ((1n << 128n) - 1n);
  const tag = new Uint8Array(16);
  for (let i = 0; i < 16; i++) { tag[i] = Number(acc & 0xffn); acc >>= 8n; }
  return tag;
}
/** Constant-time-ish compare (timing does not matter for a passive receiver, but keep it honest). */
export function equal(a, b) {
  if (a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= a[i] ^ b[i];
  return d === 0;
}

// ---- secretbox (XSalsa20-Poly1305): crypto_box_open_easy_afternm / crypto_box_easy_afternm ----
/** box = tag16 ‖ ciphertext. Returns the plaintext or null. */
export function secretboxOpen(box, n24, k) {
  if (box.length < 16) return null;
  const c = box.subarray(16);
  const polyKey = xsalsa20Xor(k, n24, new Uint8Array(32));
  if (!equal(poly1305(polyKey, c), box.subarray(0, 16))) return null;
  return xsalsa20Xor(k, n24, c, 32);
}
export function secretboxSeal(m, n24, k) {
  const c = xsalsa20Xor(k, n24, m, 32);
  const out = new Uint8Array(16 + m.length);
  out.set(poly1305(xsalsa20Xor(k, n24, new Uint8Array(32)), c), 0);
  out.set(c, 16);
  return out;
}

// ---- OpenHD per-packet protection (docs/13 §4.6) ----
const nonce8 = (nonce) => { const n = new Uint8Array(8); new DataView(n.buffer).setBigUint64(0, BigInt(nonce), true); return n; };
const lenLE64 = (n) => { const b = new Uint8Array(8); new DataView(b.buffer).setBigUint64(0, BigInt(n), true); return b; };

/** Auth-only packet (radio port bit0 = 0): body ‖ tag. Returns body (a view) or null. */
export function authOpen(sessionKey, nonce, payload) {
  if (payload.length < 17) return null;
  const body = payload.subarray(0, payload.length - 16);
  const sub = hchacha20(sessionKey, Uint8Array.from([...nonce8(nonce), 0, 0, 0, 0, 0, 0, 0, 0]));
  return equal(poly1305(sub, body), payload.subarray(payload.length - 16)) ? body : null;
}
export function authSeal(sessionKey, nonce, body) {
  const sub = hchacha20(sessionKey, Uint8Array.from([...nonce8(nonce), 0, 0, 0, 0, 0, 0, 0, 0]));
  const out = new Uint8Array(body.length + 16);
  out.set(body); out.set(poly1305(sub, body), body.length);
  return out;
}
/** crypto_aead_chacha20poly1305_decrypt (original construction, no AD). Returns plaintext or null. */
export function aeadOpen(sessionKey, nonce, payload) {
  if (payload.length < 17) return null;
  const n = nonce8(nonce), c = payload.subarray(0, payload.length - 16);
  const polyKey = chacha20Xor(sessionKey, n, new Uint8Array(32), 0);
  if (!equal(poly1305(polyKey, lenLE64(0), c, lenLE64(c.length)), payload.subarray(payload.length - 16))) return null;
  return chacha20Xor(sessionKey, n, c, 1);
}
export function aeadSeal(sessionKey, nonce, m) {
  const n = nonce8(nonce), c = chacha20Xor(sessionKey, n, m, 1);
  const polyKey = chacha20Xor(sessionKey, n, new Uint8Array(32), 0);
  const out = new Uint8Array(c.length + 16);
  out.set(c); out.set(poly1305(polyKey, lenLE64(0), c, lenLE64(c.length)), c.length);
  return out;
}
