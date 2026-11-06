#!/usr/bin/env python3
"""libsodium reference vectors for the browser crypto (docs/13 §11) -> ohd-sodium.json.
Usage: python3 -I sodium_vectors.py   (needs libsodium; copy the output to test/js/fixtures/)"""
import ctypes, ctypes.util, json, random
so = ctypes.CDLL(ctypes.util.find_library("sodium")); assert so.sodium_init() >= 0
rnd = random.Random(1106)
rb = lambda n: bytes(rnd.randrange(256) for _ in range(n))
buf = ctypes.create_string_buffer
ull = ctypes.c_ulonglong
v = {}
so.sodium_version_string.restype = ctypes.c_char_p
v["generator"] = "libsodium " + so.sodium_version_string().decode()
L = [0, 1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 100, 255, 1000, 1500]
v["poly1305"] = []
for n in L:
    k = rb(32); m = rb(n); o = buf(16); so.crypto_onetimeauth(o, m, ull(n), k)
    v["poly1305"].append({"key": k.hex(), "msg": m.hex(), "tag": o.raw.hex()})
k = b"\xff" * 32; m = b"\xff" * 300; o = buf(16); so.crypto_onetimeauth(o, m, ull(300), k)
v["poly1305"].append({"key": k.hex(), "msg": m.hex(), "tag": o.raw.hex()})
v["hsalsa20"] = []; v["hchacha20"] = []
for _ in range(3):
    k, i = rb(32), rb(16); o = buf(32); so.crypto_core_hsalsa20(o, i, k, None); v["hsalsa20"].append({"key": k.hex(), "in": i.hex(), "out": o.raw.hex()})
    o = buf(32); so.crypto_core_hchacha20(o, i, k, None); v["hchacha20"].append({"key": k.hex(), "in": i.hex(), "out": o.raw.hex()})
v["chacha20"] = []
for n, ic in [(0, 0), (1, 0), (64, 0), (65, 1), (200, 4294967295), (1500, 7)]:
    k, nn, m = rb(32), rb(8), rb(n); o = buf(max(n, 1))
    so.crypto_stream_chacha20_xor_ic(o, m, ull(n), nn, ctypes.c_uint64(ic), k)
    v["chacha20"].append({"key": k.hex(), "nonce": nn.hex(), "ic": ic, "msg": m.hex(), "out": o.raw[:n].hex()})
v["secretbox"] = []
for n in [0, 1, 32, 100, 1000]:
    k, nn, m = rb(32), rb(24), rb(n); o = buf(16 + n)
    so.crypto_box_easy_afternm(o, m, ull(n), nn, k)
    v["secretbox"].append({"key": k.hex(), "nonce": nn.hex(), "msg": m.hex(), "box": o.raw.hex()})
v["aead"] = []
for n in [1, 16, 63, 64, 65, 1441]:
    k, m = rb(32), rb(n); nonce = rnd.randrange(2**64); o = buf(n + 16); ol = ull()
    so.crypto_aead_chacha20poly1305_encrypt(o, ctypes.byref(ol), m, ull(n), None, ull(0), None, nonce.to_bytes(8, "little"), k)
    v["aead"].append({"key": k.hex(), "nonce": str(nonce), "msg": m.hex(), "ct": o.raw[:ol.value].hex()})
v["x25519"] = []
for _ in range(4):
    s, u = rb(32), rb(32); o = buf(32); so.crypto_scalarmult(o, s, u)
    b = buf(32); so.crypto_scalarmult_base(b, s)
    v["x25519"].append({"scalar": s.hex(), "u": u.hex(), "out": o.raw.hex(), "base": b.raw.hex()})
v["box"] = []
for _ in range(3):
    seed = rb(32); pk, sk = buf(32), buf(32); so.crypto_box_seed_keypair(pk, sk, seed)
    pk2, sk2 = buf(32), buf(32); so.crypto_box_seed_keypair(pk2, sk2, rb(32))
    k = buf(32); so.crypto_box_beforenm(k, pk2.raw, sk.raw)
    v["box"].append({"seed": seed.hex(), "pk": pk.raw.hex(), "sk": sk.raw.hex(), "peer_pk": pk2.raw.hex(), "beforenm": k.raw.hex()})
v["blake2b"] = []
for n, ol in [(0, 64), (1, 32), (127, 64), (128, 64), (129, 17), (256, 64), (1000, 1), (1027, 48)]:
    m = rb(n); o = buf(ol); so.crypto_generichash(o, ull(ol), m, ull(n), None, ull(0))
    v["blake2b"].append({"msg": m.hex(), "outlen": ol, "out": o.raw.hex()})
v["argon2id"] = []
for ops, mem, ol, pw in [(2, 8192, 32, b"openhd"), (1, 1 << 20, 64, b""), (3, 256 * 1024, 32, b"capturetonkeli \xc3\xa4")]:
    salt = rb(16); o = buf(ol)
    r = so.crypto_pwhash(o, ull(ol), pw, ull(len(pw)), salt, ull(ops), ctypes.c_size_t(mem), ctypes.c_int(2))  # 2 = ARGON2ID13
    assert r == 0
    v["argon2id"].append({"pw": pw.hex(), "salt": salt.hex(), "t": ops, "m_kib": mem // 1024, "out": o.raw.hex()})
json.dump(v, open("ohd-sodium.json", "w"), indent=0)
print(v["generator"], {k: len(x) for k, x in v.items() if isinstance(x, list)})
