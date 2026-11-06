#!/usr/bin/env python3
"""Passive OpenHD air->ground receiver: reference implementation of doc 12 (rev 2).

Supports OpenHD 2.5.x/2.6.x (FramingGenA). 2.7 senders are detected and rejected;
adding 2.7 = one new Framing class (see L3 below).

Written from the spec, not from OpenHD code. Crypto primitives come from
libsodium via ctypes (the firmware/JS/Go ports must re-implement or link
equivalents). Run:  python3 -I openhd_rx_ref.py vectors-2.6.json
"""
import ctypes, ctypes.util, json, struct, sys

so = ctypes.CDLL(ctypes.util.find_library("sodium"))
assert so.sodium_init() >= 0

# ---------- constants (doc 12 §3) ----------
ID_AIR, ID_GND = 0x01, 0x02
PORT_TELE_AIR_TX, PORT_TELE_GND_TX = 4, 3
PORT_VIDEO0, PORT_VIDEO1 = 10, 11
PORT_MGMT_AIR, PORT_MGMT_GND = 20, 21
PORT_AUDIO, PORT_SESSION = 30, 127
TAG = 16


# ---------- §3 802.11 header ----------
def parse_80211(f):
    if len(f) < 24 + 1 or f[0] != 0x08 or f[1] != 0x01:
        return None
    if f[4:10] != b"\xff" * 6:
        return None
    if f[10] != f[16] or f[15] != f[21]:
        return None
    uid, rp = f[10], f[15]
    nonce = struct.unpack("<Q", f[11:15] + f[17:21])[0]
    return dict(uid=uid, encrypted=rp & 1, port=rp >> 1, nonce=nonce,
                payload=f[24:])


# ---------- §4 crypto ----------
def crypto_box_open_easy(c, n, pk, sk):
    m = ctypes.create_string_buffer(len(c) - 16)
    r = so.crypto_box_open_easy(m, c, ctypes.c_ulonglong(len(c)), n, pk, sk)
    return m.raw if r == 0 else None


def hchacha20(inp16, key32):
    out = ctypes.create_string_buffer(32)
    so.crypto_core_hchacha20(out, inp16, key32, None)
    return out.raw


def auth_only(nonce, data, session_key):
    body, tag = data[:-TAG], data[-TAG:]
    sub = hchacha20(struct.pack("<Q", nonce) + bytes(8), session_key)
    ok = so.crypto_onetimeauth_verify(tag, body, ctypes.c_ulonglong(len(body)), sub)
    return body if ok == 0 else None


def aead_decrypt(nonce, data, session_key):
    m = ctypes.create_string_buffer(len(data))
    mlen = ctypes.c_ulonglong()
    r = so.crypto_aead_chacha20poly1305_decrypt(
        m, ctypes.byref(mlen), None, data, ctypes.c_ulonglong(len(data)),
        None, ctypes.c_ulonglong(0), struct.pack("<Q", nonce), session_key)
    return m.raw[:mlen.value] if r == 0 else None


# ---------- §5 FEC: GF(2^8) poly 0x11D, Cauchy-like rows ----------
EXP, LOG = [0] * 512, [0] * 256
x = 1
for i in range(255):
    EXP[i] = x; LOG[x] = i
    x <<= 1
    if x & 0x100: x ^= 0x11D
for i in range(255, 512): EXP[i] = EXP[i - 255]
def gmul(a, b): return 0 if a == 0 or b == 0 else EXP[LOG[a] + LOG[b]]
def ginv(a): return EXP[255 - LOG[a]]
def coef(sec_idx, prim_idx): return ginv((128 + sec_idx) ^ prim_idx)


def solve(mat, rhs):
    """Gauss-Jordan over GF(256): mat k*k, rhs list of k byte arrays."""
    k = len(mat)
    mat = [row[:] for row in mat]; rhs = [bytearray(r) for r in rhs]
    for c in range(k):
        p = next(r for r in range(c, k) if mat[r][c])
        mat[c], mat[p] = mat[p], mat[c]; rhs[c], rhs[p] = rhs[p], rhs[c]
        iv = ginv(mat[c][c])
        mat[c] = [gmul(v, iv) for v in mat[c]]
        rhs[c] = bytearray(gmul(v, iv) for v in rhs[c])
        for r in range(k):
            if r != c and mat[r][c]:
                f = mat[r][c]
                mat[r] = [a ^ gmul(f, b) for a, b in zip(mat[r], mat[c])]
                rhs[r] = bytearray(a ^ gmul(f, b) for a, b in zip(rhs[r], rhs[c]))
    return rhs


class FecBlock:
    def __init__(self):
        self.k = None; self.frags = {}; self.forwarded = 0; self.done = False


class FecRx:
    """Per-radio-port decoder. Emits primaries in order (doc 12 §5.4)."""
    QUEUE = 20

    def __init__(self, out):
        self.out, self.blocks = out, {}
        self.stats = dict(recovered=0, lost_blocks=0)

    def push(self, p):
        if len(p) < 8: return
        blk, fi, k = struct.unpack_from("<IBB", p)
        if k == 0 or fi >= 255: return
        b = self.blocks.setdefault(blk, FecBlock())
        if b.done or fi in b.frags: return
        b.k = k
        b.frags[fi] = bytes(p[6:])  # u16 data_size + data (FEC-protected part)
        self._flush(blk)
        while len(self.blocks) > self.QUEUE:
            old = min(self.blocks)
            if not self.blocks[old].done: self.stats["lost_blocks"] += 1
            del self.blocks[old]

    def _forward_in_order(self, b):
        while b.forwarded < b.k and b.forwarded in b.frags:
            raw = b.frags[b.forwarded]
            n = struct.unpack_from("<H", raw)[0]
            self.out(raw[2:2 + n])
            b.forwarded += 1
        if b.forwarded == b.k: b.done = True

    def _flush(self, blk):
        b = self.blocks[blk]
        self._forward_in_order(b)
        if b.done or len(b.frags) < b.k: return
        missing = [i for i in range(b.k) if i not in b.frags]
        secs = [i for i in sorted(b.frags) if i >= b.k][:len(missing)]
        size = len(b.frags[secs[0]])
        pad = lambda d: d + bytes(size - len(d))
        rhs = []
        for s in secs:
            acc = bytearray(b.frags[s])
            for j in range(b.k):
                if j in b.frags:
                    c, d = coef(s - b.k, j), pad(b.frags[j])
                    for t in range(size): acc[t] ^= gmul(c, d[t])
            rhs.append(acc)
        mat = [[coef(s - b.k, m) for m in missing] for s in secs]
        for m, data in zip(missing, solve(mat, rhs)):
            b.frags[m] = bytes(data); self.stats["recovered"] += 1
        self._forward_in_order(b)


# ---------- L3 framing (the ONLY generation-specific layer, doc 12 §2/§10.5) ----------
NO_FRAMING_PORTS = {PORT_SESSION, PORT_MGMT_AIR, PORT_MGMT_GND}  # handled before/without L3
EXPECTED_TYPE = {PORT_VIDEO0: 0, PORT_VIDEO1: 0, PORT_TELE_AIR_TX: 1, PORT_TELE_GND_TX: 1}


class FramingGenA:
    """OpenHD 2.5.x / 2.6.x: stream payload is used as is."""
    name = "2.6"

    def strip(self, port, body):
        return {}, body


# Future: class FramingGenB (OpenHD 2.7): drop the 8-byte WBPacketHeader,
# return {"packet_type":..,"stream_idx":..,"retransmit":..} as meta, and
# register it in FRAMINGS. Nothing else in this file needs to change.
FRAMINGS = {"2.6": FramingGenA}


def looks_like_gen_b(port, body):
    """Incompatible-sender diagnostic (doc 12 §2)."""
    t = EXPECTED_TYPE.get(port)
    return (t is not None and len(body) >= 8 and body[0] in (0, 1) and body[1] == t
            and struct.unpack_from("<H", body, 6)[0] == len(body))


# ---------- receiver (L1 + L2 + glue to L4/L5) ----------
class Receiver:
    def __init__(self, key_1_pub, key_1_sec):
        self.pk, self.sk = key_1_pub, key_1_sec  # decrypt AIR traffic
        self.session_key = None
        self.framing_name = "2.6"
        self.framing = FRAMINGS[self.framing_name]()
        self.checked_gen = False
        self.unsupported = None  # set to "2.7" by the diagnostic
        self.video, self.tele, self.mgmt = [], [], []
        self.fec = {PORT_VIDEO0: FecRx(lambda d: self.video.append((0, d))),
                    PORT_VIDEO1: FecRx(lambda d: self.video.append((1, d)))}
        self.tele_seen = set()

    def on_frame(self, f):
        h = parse_80211(f)
        if not h or h["uid"] != ID_AIR: return "foreign/ground"
        port, pl = h["port"], h["payload"]
        if port == PORT_SESSION:
            if h["encrypted"] or len(pl) != 72: return "bad session"
            k = crypto_box_open_easy(pl[24:], pl[:24], self.pk, self.sk)
            if k is None: return "session key: wrong bind phrase"
            new = k != self.session_key
            self.session_key = k
            if new:
                self.checked_gen = False; self.unsupported = None
            return "session NEW" if new else "session same"
        if self.session_key is None or len(pl) < TAG + 1: return "no session yet"
        dec = aead_decrypt if h["encrypted"] else auth_only
        body = dec(h["nonce"], pl, self.session_key)
        if body is None: return "auth fail"
        if port == PORT_MGMT_AIR and len(body) == 6 and body[0] == 0:
            freq, bw = struct.unpack_from("<IB", body, 1)
            self.mgmt.append((freq, bw)); return f"mgmt {freq}MHz {bw}MHz"
        if not self.checked_gen and port in EXPECTED_TYPE:
            self.checked_gen = True
            if looks_like_gen_b(port, body): self.unsupported = "2.7"
        if self.unsupported:
            return f"OpenHD {self.unsupported} sender - not supported yet"
        _meta, body = self.framing.strip(port, body)
        if port in self.fec:
            self.fec[port].push(body); return "video"
        if port == PORT_TELE_AIR_TX:
            seq = struct.unpack_from("<Q", body)[0]
            if seq in self.tele_seen: return "telemetry dup"
            self.tele_seen.add(seq); self.tele.append(body[8:]); return "telemetry"
        return f"port {port}"


def parse_mavlink(buf):
    out, i = [], 0
    while i < len(buf):
        if buf[i] == 0xFD and i + 12 <= len(buf):
            n = buf[i + 1]; sig = 13 if buf[i + 2] & 1 else 0
            sysid, comp = buf[i + 5], buf[i + 6]
            msgid = buf[i + 7] | buf[i + 8] << 8 | buf[i + 9] << 16
            out.append((sysid, comp, msgid)); i += 12 + n + sig
        elif buf[i] == 0xFE and i + 8 <= len(buf):
            n = buf[i + 1]; out.append((buf[i + 3], buf[i + 4], buf[i + 5])); i += 8 + n
        else:
            i += 1
    return out


def main(path):
    v = json.load(open(path))
    rx = Receiver(bytes.fromhex(v["key_1_air_pub"]), bytes.fromhex(v["key_1_air_sec"]))
    expected_video = []
    for fr in v["frames"]:
        f = bytes.fromhex(fr["frame"])
        if fr["kind"] == "video" and v["generation"] == "2.6":
            hdr = bytes.fromhex(fr["plain"])
            blk, fi, k = struct.unpack_from("<IBB", hdr)
            if fi < k:
                n = struct.unpack_from("<H", hdr, 6)[0]
                expected_video.append(hdr[8:8 + n])
        if fr["drop"]:
            print(f"  [dropped on air] {fr['kind']}"); continue
        print(f"  {fr['kind']:15s} port_byte=0x{fr['radio_port_byte']:02x} -> {rx.on_frame(f)}")
    assert rx.mgmt == [(5745, 20)]  # management is generation-independent
    if v["generation"] == "2.7":
        assert rx.unsupported == "2.7" and not rx.video and not rx.tele
        print("OK 2.7 sender detected and rejected cleanly (Gen B = future FramingGenB)")
        return
    got = [d for _, d in rx.video]
    assert rx.unsupported is None
    assert got == expected_video, "video mismatch"
    assert len(rx.tele) == 1 and parse_mavlink(rx.tele[0]) == [(1, 1, 0)]
    print(f"OK framing={rx.framing.name}: {len(got)} RTP fragments in order, "
          f"{rx.fec[10].stats['recovered']} recovered by FEC, telemetry MAVLink "
          f"{parse_mavlink(rx.tele[0])}, mgmt {rx.mgmt}")


if __name__ == "__main__":
    for p in sys.argv[1:]:
        print(p); main(p)
