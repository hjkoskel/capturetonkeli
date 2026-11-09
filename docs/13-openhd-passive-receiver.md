# 13 – OpenHD air protocol: passive receiver specification (2026-11-06)

Status: web UI receiver implemented 2026-11-06 (§13). Not yet: firmware "OpenHD assist" (§10.3), Go library (§10.5), hardware test.

Goal: the capturetonkeli dongle (plus the browser / Go host) **passively** receives an OpenHD drone's downlink: video, telemetry and link metadata.
It works like an OpenHD ground station in "listen-only" mode and never transmits.

This doc covers:
- the exact on-air format, from the 802.11 header down to RTP and MAVLink;
- what can be identified with and without the bind phrase;
- the receive algorithm (crypto, FEC, ordering);
- what this means for the dongle firmware, the USB link (doc 03) and the host libraries (docs 04/05).

Everything here was read from OpenHD source code, not from wiki pages.
It was then **verified end to end**: golden vectors generated with OpenHD's own encryption and FEC code were decoded by an independent reference decoder (§11).

---

## 0. Key findings (read this first)

1. **Video needs no key to *read*.** By default, OpenHD video is only *authenticated* (Poly1305 tag), not encrypted.
   - The FEC headers and the RTP/H.264/H.265 payload are plaintext.
   - Without the bind phrase you can still decode video; you just cannot verify that it is genuine.
   - The exception is the camera setting "ultra secure encryption", which is off by default (§5.5).
2. **Telemetry and management frames are always encrypted** (ChaCha20-Poly1305).
   - You need the bind phrase. The default is `openhd`, and most users never change it.
   - Equivalently you can use the 128-byte `txrx.key` file from the air unit.
   - The bind phrase reduces to **one 32-byte precomputed key per direction** (§4.2). Default keys are listed in §4.6.
3. **There are two incompatible wire generations** (§2).
   - Gen A: releases 2.5.x and 2.6.x.
   - Gen B: the 2.7-evo development branch since Jan 2026. It adds an 8-byte `WBPacketHeader` in front of every stream payload.
   - The receiver can tell them apart automatically from the first authenticated packet.
4. **USB is the bottleneck, not the radio.** ⚠️ high
   - At OpenHD defaults (5745 MHz, 20 MHz, MCS2, FEC 20%) the air link carries up to ≈13 Mbit/s. The video stream alone is ≈11 Mbit/s ≈ 1.4 MB/s.
   - ESP32-C5 USB Serial/JTAG is USB Full-Speed. Bulk throughput is capped at a theoretical 1.216 MB/s, and doc 08 C2 only *requires* ≥0.5 MB/s.
   - Full-rate default video **cannot** cross the USB link, even with perfect firmware. Mitigations in §10.2: an on-dongle FEC/secondary drop, plus a recommended OpenHD bitrate cap.
5. **RF compatibility is good, with caveats.**
   - OpenHD transmits 802.11n HT MCS frames (single stream by default) at 20 or 40 MHz. LDPC and STBC are off by default.
   - The ESP32-C5 receives HT20/HT40 MCS0–7 on 2.4 GHz and 5180–5885 MHz.
   - It cannot receive the 5/10 MHz modes, 2-stream MCS ≥ 8, or (probably) LDPC/STBC if a user enables them (§3).

---

## 1. Scope and source baseline

| Item | Value |
|---|---|
| OpenHD repo | `OpenHD/OpenHD`, branch `2.7-evo` HEAD `c75c37e6` (2026-10-20); branch `2.6-evo` HEAD `90cf8e6f` (2025-11-09) |
| Link library | `OpenHD/wifibroadcast` (Consti10's fork), submodule `ohd_interface/lib/wifibroadcast` |
| Gen A link lib | `99e53e7` (2025-05-26) used by `2.6-evo`; `a64e5467` (2024-04) used by tag v2.6.0; `c22c564d` (2024-01) used by v2.5.3. Same wire format (checked: header, session packet, FEC header, crypto) |
| Gen B link lib | `14d5fdb` (2026-01-12) used by `2.7-evo` |
| Not covered | OpenHD ≤ 2.4 (older wifibroadcast layout, not verified); Microhard / Ethernet links; ground→air RC details beyond identification |

All multi-byte integers on air are **little-endian**. OpenHD `static_assert`s little-endian hosts and copies packed structs directly.

---

## 2. Protocol generations

| | Gen A (v2.5.x, v2.6.x) | Gen B (2.7-evo dev, wb ≥ 2026-01-11) |
|---|---|---|
| 802.11 header, radio ports, crypto, session key | identical | identical |
| Stream payload (after decrypt/auth) | `FECPayloadHdr` / `FECDisabledHeader` + data | **`WBPacketHeader` (8 B)** + `FECPayloadHdr` / `FECDisabledHeader` + data |
| Optional ARQ | – | ground may send retransmission requests (type 3), air may resend with flag bit0 |
| Max video FEC payload | 1441 B data per fragment | same (header fits in 1457 B budget) |

**Gen B `WBPacketHeader`** (packed, 8 bytes, in front of *every* `WBStreamTx` payload):

```
off size field
0   1    packet_flags       bit0 = RETRANSMITTED
1   1    packet_type        0=VIDEO 1=TELEMETRY 2=RC 3=RETRANSMISSION_REQ 4=VIDEO_KEYFRAME(reserved)
2   4    stream_packet_idx  u32, per (stream, packet_type) counter, +1 per packet
6   2    total_length       = 8 + rest of payload  (i.e. length of the authenticated plaintext)
```

**Auto-detection (normative for our host).**
- On the first authenticated packet of a stream (video or telemetry), check `p[0] ∈ {0,1}`, `p[1] == expected_type` and `u16(p[6..8]) == len(p)`. If all hold, the stream is Gen B, else Gen A.
- In Gen A the same bytes are `block_idx`/`data_size` or a u64 sequence number, and they never satisfy `u16@6 == len`.
- Detect per air unit (reset on a new session key).
- Gen B retransmissions (flag bit0) are duplicates. The FEC and dedupe layers drop them naturally.

---

## 3. RF layer

| Parameter | OpenHD behaviour | ESP32-C5 dongle |
|---|---|---|
| Default frequency | 5745 MHz (ch 149, "race band 2"); 2.4 GHz default 2452 MHz (ch 9) | ✅ |
| OpenHD 5.8 "race band" list | 5700, 5745, 5785, 5825, 5865, 5260, 5280 MHz | ✅ within 5180–5885 (5865 = ch 173 must be allowed by our country setting, doc 02 §5) |
| Non-standard freqs (AR9271 2.3/2.5 GHz, etc.) | possible with patched drivers | ❌ not tunable |
| Channel width | 20 MHz default; 40 MHz option (HT40+ or HT40− per channel table); 10 MHz experimental (8812eu) | 20 ✅, 40 ✅ (dongle must be set to HT40 with the same secondary side), 10/5 ❌ |
| What is sent at 40 MHz | video + telemetry use the data width; **session key and management frames are always sent at 20 MHz** so a 20 MHz receiver can always discover the link | an HT40 receiver also gets 20 MHz frames on the primary channel |
| Modulation | 802.11n HT MCS via radiotap TX header (`TX_FLAGS` + `MCS`), MCS 0–31 allowed, **default MCS 2** (QPSK 3/4); ground uplink always MCS 0 | MCS 0–7 ✅ (1T1R); MCS ≥ 8 (2 streams) ❌ |
| LDPC / STBC / short GI | default off / off / off; user-selectable | long/short GI ✅; **LDPC and STBC RX support on C5: unverified** → doc 08 spike C1 must test |
| ACK | air: `TX_NOACK` (no retries) unless a dev option is set | n/a (passive) |
| Frame size | ≤ 1510 B incl. 13-byte TX radiotap ⇒ **802.11 frame ≤ 1497 B** (+4 FCS) | ✅ < doc 03 max frame |

Frequency/width discovery: the air unit announces its frequency and width in management frames (§7.3), which are encrypted.
Without the key: scan the race-band list, look for the fingerprint (§4.1) and the 72-byte session packets that appear about every 1 s, then try HT40+/− if video frames are missing but session frames are present.

---

## 4. 802.11 frame and crypto layer (`WBTxRx`)

### 4.1 Frame layout (as delivered by a monitor-mode RX, FCS stripped)

```
off  size  field                          value / meaning
0    2     frame control                  08 01  (data frame, ToDS=1). Anything else: not OpenHD
2    2     duration                       00 00 (driver may alter; ignore)
4    6     addr1                          FF FF FF FF FF FF
10   1     unique_id   (addr2[0])         01 = sent by AIR unit, 02 = sent by GROUND unit
11   4     nonce[0..3] (addr2[1..4])      low 32 bits of the 64-bit packet nonce (LE)
15   1     radio_port  (addr2[5])         bit0 = encrypted, bits1..7 = stream index (0..127)
16   1     unique_id copy (addr3[0])      must equal byte 10
17   4     nonce[4..7] (addr3[1..4])      high 32 bits of the nonce
21   1     radio_port copy (addr3[5])     must equal byte 15
22   2     sequence control               written by OpenHD, often overwritten by the driver; ignore
24   N     payload                        see §4.3–4.5
```

Validity test (what OpenHD itself does, in this order):
1. FC == `08 01`;
2. 0 < N ≤ 1473;
3. byte10 == byte16 (and == the expected direction);
4. byte15 == byte21.

On a busy channel this is a near-perfect fingerprint. OpenHD's own "likely OpenHD packet" counter uses exactly this test.
⚠️ The ESP32 promiscuous buffer **includes the 4-byte FCS**. Strip it before computing N (confirm in spike C1).

### 4.2 Radio ports (stream map)

`radio_port byte = (stream_index << 1) | encrypted` (GCC bitfield, LSB first).

| Stream | Index | Direction (unique_id) | Encrypted (default) | Port byte | Payload stack |
|---|---|---|---|---|---|
| Session key | 127 | both (01 and 02) | never | `FE` | 72-byte `SessionKeyPacket`, **no** tag |
| Telemetry air→gnd | 4 | 01 | **always** | `09` | [Gen B hdr] `FECDisabledHeader` + MAVLink |
| Telemetry gnd→air (incl. RC) | 3 | 02 | **always** | `07` | same |
| Video primary cam | 10 | 01 | no (yes if "ultra secure") | `14` (`15`) | [Gen B hdr] `FECPayloadHdr` + RTP |
| Video secondary cam | 11 | 01 | no (yes if "ultra secure") | `16` (`17`) | same |
| Management air→gnd | 20 | 01 | **always** | `29` | 1-byte id + struct (§7.3) |
| Management gnd→air | 21 | 02 | **always** | `2B` | 1-byte id + struct |
| Audio | 30 | 01 | no | `3C` | [Gen B hdr] `FECDisabledHeader` + audio packet (not analysed) |

The ground unit transmits too (telemetry uplink, RC, management at 10 Hz, its own session key, Gen B retransmission requests) unless it runs in listen-only mode.
The dongle sees both directions; `unique_id` separates them.

### 4.3 Nonce

- A 64-bit counter **per transmitting unit**, shared by all its streams *including* session-key packets.
- Starts at a random 32-bit value when the OpenHD process starts (`randombytes_random()`), then +1 per injected frame.
- Uses:
  - (a) the AEAD / auth nonce;
  - (b) **link loss estimation**: gaps in the nonce sequence = frames the dongle missed (OpenHD computes its loss % this way);
  - (c) **TX instance identity**: a jump back or to another range = the air unit restarted.
- Duplicated telemetry injections (§7.2) get distinct nonces; dedupe happens one layer up.

### 4.4 Keys and the bind phrase

- **Bind phrase → two Curve25519 keypairs.**
  - `seed_air = Argon2id(pw, SALT_AIR, ops=2, mem=64 MiB, out=32)` and `seed_gnd` likewise with `SALT_GND`. These are libsodium `crypto_pwhash` with `OPSLIMIT/MEMLIMIT_INTERACTIVE` and the default algorithm.
  - `key_1 = crypto_box_seed_keypair(seed_air)`, `key_2 = crypto_box_seed_keypair(seed_gnd)`.
  ```
  SALT_AIR = c0 bd d8 66 38 99 9a 5c e4 1a 31 d1 9d 07 80 cf
  SALT_GND = b3 1e 96 14 11 c8 e1 52 30 40 12 82 59 3e 53 ea
  ```
- **Key file.** `/usr/local/share/openhd/txrx.key` holds 128 raw bytes: `key_1.pub ‖ key_1.sec ‖ key_2.pub ‖ key_2.sec`.
  - It is created from `password.txt` on the boot partition at first boot (that file is then deleted), or from the default phrase `openhd`.
  - Letting the user load this file is the simplest way to support custom phrases without Argon2 in the browser.
- **Usage.** The air unit encrypts with `key_1` and the ground with `key_2`. In both cases the *same* keypair is used as sender and recipient: `crypto_box(m, n, pk=key.pub, sk=key.sec)`.
  - A receiver of **air** traffic therefore needs only `K_air = crypto_box_beforenm(key_1.pub, key_1.sec)`, i.e. `HSalsa20(X25519(sec, pub), 0)`.
  - It needs no X25519 at runtime.
- **The whole bind phrase reduces to two 32-byte constants**, `K_air` and `K_gnd`. These are what the host passes to whatever does decryption.

### 4.5 Session key packet (radio port 127)

- Sent by each unit once per second while it is transmitting data (`session_key_packet_interval = 1 s`).
- Always unencrypted-flagged, always 20 MHz, payload exactly **72 bytes**:
  ```
  0   24  nonce24           random
  24  48  box               crypto_box_easy output = Poly1305 tag(16) ‖ XSalsa20(session_key)(32)
  ```
- `session_key = crypto_box_open_afternm(box, nonce24, K_dir)` (XSalsa20-Poly1305 secretbox open).
  - Failure ⇒ wrong bind phrase. OpenHD flags "likely mismatching encryption key" when it sees invalid and no valid session packets.
- The session key is random per TX process start and **stays the same** until restart. The same 72 bytes repeat every second.
  - The receiver can byte-compare and skip the crypto for repeats.
  - A *different* valid key = new session: reset the FEC and dedupe state of all streams from that unit.

### 4.6 Per-packet protection (all ports except 127)

`payload = body ‖ tag16`, with `body_len = N − 16 ≥ 1`. `nonce8 = LE64(nonce)` from §4.3.

| radio_port bit0 | Algorithm | How to verify / decrypt |
|---|---|---|
| 0 (authenticated only) | `subkey = HChaCha20(key=session_key, in16 = nonce8 ‖ 00×8)`; `tag = Poly1305(subkey, body)` | body is **plaintext**; tag check optional for display, required for trust |
| 1 (encrypted) | libsodium `crypto_aead_chacha20poly1305` (*original* 8-byte-nonce variant), no AD | see below |

ChaCha20-Poly1305 *original* construction, written out so it can be implemented with any ChaCha20 core. Verified against libsodium:
```
ks      = ChaCha20(key=session_key, nonce=nonce8, 64-bit counter)    // = IETF ChaCha20 with nonce12 = 00 00 00 00 ‖ nonce8
polykey = ks[block 0][0..32]
pt      = body XOR ks[starting at block 1]
tag     = Poly1305(polykey, LE64(0) ‖ body ‖ LE64(len(body)))        // AD-length (0) comes FIRST, no padding
```
⚠️ This is **not** RFC 8439 AEAD: no 16-byte padding, and the length fields sit differently. Go `x/crypto/chacha20poly1305` and mbedTLS `chachapoly` (IETF) cannot be used directly. Use the raw `chacha20` + `poly1305` primitives as above.

**Default bind phrase `openhd`** (computed with libsodium; matches the OpenHD-generated vectors):
```
key_1 (air) pub ce6d6effed68581cb6b6591ebdba2d2d727b0115ac1515bf8643890f9c6d351e
key_1 (air) sec 7e25c645723eaf93928db4b2d73ea1fd90927cb0ad78a23b72cf46bfbf4b5794
key_2 (gnd) pub 01beabbf16453456bd98f5d32983ad70e930ef35871790dd4cc450ee08fea012
key_2 (gnd) sec 80cab20ae62680a45cb520ff542cf41349f0b164a60eaaf5df6931b8a47dab2f
K_air = beforenm(key_1) 978a890d4b79f33f700fcdb7e4b91be6d6eb903a6638d2381d9b8e3ea9b20125
K_gnd = beforenm(key_2) 5be985599ab9271ee02aef52532bf4f5bd83eba8915934373f0ef1468a33f003
```
These are effectively public: the default phrase is in the OpenHD source. Ship them as the built-in default.

---

## 5. Stream layer

### 5.1 FEC-disabled streams (telemetry, audio)

```
[Gen B WBPacketHeader 8 B]
0  8  sequence_number   u64, starts at 0 per TX stream, +1 per *logical* packet
8  …  data              ≥ 1 byte
```
The same logical packet may be injected several times (same `sequence_number`, new nonce). The receiver keeps a set of the last 100 sequence numbers and drops duplicates, like OpenHD's `FECDisabledDecoder`.
Packets are forwarded in arrival order; reordering is possible in principle but practically absent.

### 5.2 FEC streams (video) – header

```
[Gen B WBPacketHeader 8 B]
0  4  block_idx            u32, per stream, +1 per FEC block (encoder keeps u64; the wire is u32)
4  1  fragment_idx         0..k−1 = primary, k..k+m−1 = secondary
5  1  n_primary_fragments  k (1..128), identical in every fragment of the block
6  2  data_size            PRIMARY: length of data that follows. SECONDARY: **parity bytes, not a length**
8  …  data                 PRIMARY: exactly data_size bytes (no padding on air). SECONDARY: L−2 bytes
```

### 5.3 FEC code

- Systematic erasure code over **GF(2⁸), polynomial 0x11D** (x⁸+x⁴+x³+x²+1).
  - This is Rizzo/Knaff "fec.c" with a Cauchy-style matrix. The same code is used in svpcom/wfb-ng–style links, but the matrix layout must be taken from here.
- Per block:
  - `L = max(data_size over primaries) + 2`.
  - Symbol `S_j = LE16(data_size_j) ‖ data_j ‖ zero-pad to L` for j < k. The 2-byte size is **inside** the protected symbol, which is why secondaries carry parity at offset 6.
  - Secondary r (0 ≤ r < m) = `Σ_j  inv((128 + r) XOR j) · S_j` (all products in GF(2⁸)). On air it is sent as `FECPayloadHdr(first 6 bytes) ‖ parity[0..L)`.
    In other words header bytes 6–7 *are* `parity[0..2)`, and every secondary of a block has length `6 + L`.
- Decode with `e` missing primaries and ≥ `e` secondaries:
  - subtract known primaries from each secondary, then
  - solve the `e×e` system with coefficients `inv((128+r) XOR j_missing)` (Gauss–Jordan).
  - Recovered symbols give `data_size` from their first 2 bytes.
- `m = round(k · fec_perc / 100)`, minimum 1 if `fec_perc > 0`. Default `fec_perc = 20`.
- Block size:
  - One video frame (an access unit, as RTP packets) becomes one block.
  - It is split into several blocks if `k > max_block_size`. The platform default is 20 (Rockchip/X20), 30 (old Pi), 50 (Pi 4/CM4), 80 (x86).
  - Secondaries are sent right after the block's primaries.

### 5.4 Receiver block logic (low latency, mirrors OpenHD `FECDecoder`)

- Keep a queue of up to `Q` open blocks per stream.
  - The OpenHD ground uses **`Q = 1`** (`fec_rx_queue_depth` default). As soon as a fragment of the next block arrives, the previous block is given up.
  - That is right for latency, because secondaries are sent immediately after their block's primaries. Recommendation: `Q = 1` for live view, `Q = 2..4` only for recording over a jittery USB/host path.
  - When giving up a block, OpenHD (`forward_gapped_fragments = false`) forwards only the primaries *before* the first gap. Forwarding the ones after the gap as well is a valid choice for H.264 (the decoder conceals errors). Make it a host option.
- **Forward each primary immediately** if all earlier primaries of that block were already forwarded. Never wait for the block to complete on a clean link.
- When `received primaries + secondaries ≥ k`: reconstruct, forward the rest in order, close the block.
- If a *newer* block becomes complete or recoverable, flush the older ones: forward what is available and count them lost.
- Same if more than `Q` blocks are open.
- `fragment_idx ≥ 255` → drop. A duplicate fragment → drop. A block older than the last closed one → drop.
- Blocks `≤` the last known block that are no longer in the queue are ignored (late packets).
- New session (§4.5) → clear the queue.

### 5.5 Video encryption option

The camera setting `enable_ultra_secure_encryption` makes the air encrypt video (port byte `15`/`17`).
The FEC header is then inside the ciphertext, so filtering or trimming on the dongle (§10.3) needs decryption first.

---

## 6. Video payload: RTP

- Each **primary fragment's data = exactly one RTP packet** (the GStreamer `rtph264pay`/`rtph265pay` with `mtu=1440`, or OpenHD's internal `RTPHelper` on some platforms).
  - RTP v2, 12-byte header, PT 96 (H.264); H.265 uses the same packetiser family.
  - Marker bit = last packet of the access unit.
  - The RTP timestamp unit differs by path: 90 kHz in GStreamer, ms in `RTPHelper`. Don't rely on the clock rate; use arrival time for display.
  - SSRC is arbitrary (0 in `RTPHelper`).
- Payload formats:
  - H.264 RFC 6184: single NAL, FU-A (28), STAP-A (24).
  - H.265 RFC 7798: single NAL, FU (49), AP (48).
- Config data:
  - GStreamer path: `h264parse/h265parse config-interval=-1` puts SPS/PPS(/VPS) before every IDR.
  - `RTPHelper` path: config is re-sent at least every 1 s.
  - ⚠️ With **intra-refresh** enabled (`h26x_intra_refresh_type ≠ −1`) there may be no IDR frames. A WebCodecs decoder must then start on SPS/PPS + the first frame and accept initial artifacts (WebCodecs requires a `key` chunk first: mark the first chunk after SPS/PPS as key and test on Chrome).
- Ground-side equivalent: OpenHD forwards exactly these RTP packets to UDP 5600 (primary) / 5601 (secondary).
  - That means a reassembled stream can be fed to any player expecting OpenHD's UDP 5600 (QOpenHD, GStreamer `udpsrc ! rtph264depay`).
  - This is useful as the Go library's output (doc 05).
- Some platforms send non-RTP frames ("dirty_frame" path, `try_enqueue_frame`): the data is a raw frame cut into ≤ 1440-byte pieces. Not seen on the RPi/Rockchip camera paths. Detect it as "first byte not RTP v2" and treat it as an Annex-B byte stream.

---

## 7. Telemetry and metadata payloads

### 7.1 MAVLink packing

- The data after the `FECDisabledHeader` is **one or more concatenated MAVLink frames**, aggregated up to 1024 bytes.
- In OpenHD's dialect these are mostly v2 (`0xFD`). Accept v1 (`0xFE`) too.
- Parse with the standard framing and check CRCs with CRC_EXTRA (common + OpenHD dialect, table below).
- Frames never span two wifibroadcast packets.

### 7.2 Who is who

| sysid | Meaning |
|---|---|
| 1 (configurable `FC_SYS_ID`, Betaflight = 0) | flight controller (ArduPilot/PX4/INAV): `HEARTBEAT`, `GPS_RAW_INT`, `GLOBAL_POSITION_INT`, `ATTITUDE`, `SYS_STATUS`, `VFR_HUD`, … |
| 101 | OpenHD air unit |
| 100 | OpenHD ground unit (gnd→air direction) |
| 255 | QOpenHD / GCS (gnd→air) |

The air sends important messages twice (`recommended_n_injections = 2`): same `sequence_number`, so dedupe them (§5.1).
In Gen B, RC messages from the ground go out as `packet_type = 2` on port 3.

OpenHD dialect messages (from `OpenHD/mavlink-headers`, generated 2026-01-16) with their CRC_EXTRA values:

| ID | Name | len | CRC_EXTRA |
|---|---|---|---|
| 1211 | OPENHD_STATS_MONITOR_MODE_WIFI_LINK | 42 | 134 |
| 1212 | OPENHD_STATS_MONITOR_MODE_WIFI_CARD | 38 | 205 |
| 1213 | OPENHD_STATS_TELEMETRY | 21 | 172 |
| 1214 | OPENHD_STATS_WB_VIDEO_AIR | 28 | 137 |
| 1215 | OPENHD_STATS_WB_VIDEO_AIR_FEC_PERFORMANCE | 32 | 210 |
| 1216 | OPENHD_STATS_WB_VIDEO_GROUND | 28 | 45 |
| 1217 | OPENHD_STATS_WB_VIDEO_GROUND_FEC_PERFORMANCE | 20 | 247 |
| 1218 | OPENHD_ONBOARD_COMPUTER_STATUS_EXTENSION | 11 | 239 |
| 1219 | OPENHD_CAMERA_STATUS_AIR | 21 | 71 |
| 1221 | OPENHD_VERSION_MESSAGE | 5 | 246 |
| 1222 | OPENHD_WIFBROADCAST_SUPPORTED_CHANNELS | 181 | 150 |
| 1223 | OPENHD_WIFBROADCAST_SCAN_CHANNELS_PROGRESS | 5 | 60 |
| 1224 | OPENHD_WIFBROADCAST_ANALYZE_CHANNELS_PROGRESS | 191 | 163 |
| 1225 | OPENHD_WIFBROADCAST_GND_OPERATING_MODE | 12 | 169 |
| 1226 | OPENHD_SYS_STATUS1 | 15 | 87 |
| 1227 | OPENHD_CORE_STATUS | 22 | 59 |
| 1228 | OPENHD_POWER_STATUS | 6 | 218 |
| 1229 | OPENHD_MICROHARD_STATUS | 10 | 224 |
| 1230 | OPENHD_RTSP_CONFIGURATION | 255 | 47 |

Field layouts: generate from the headers in `ohd_telemetry/lib/mavlink-headers/mavlink/v2.0/openhd/` (doc 05 should vendor a generated table, not hand-written structs).

### 7.3 Management frames (no stream header, no WBPacketHeader even in Gen B)

| Port | Body (after decrypt) | Rate |
|---|---|---|
| 20 (air→gnd) | `u8 id=0` ‖ `u32 center_frequency_mhz` ‖ `u8 bandwidth_mhz` (10/20/40) = 6 B → on air 22 B | 2 Hz; 50 Hz for 2 s after a change |
| 21 (gnd→air) | `u8 id=1` ‖ `u16 0` ‖ `u16 0` = 5 B → on air 21 B | 10 Hz |

Port 20 tells the dongle where the air unit actually is (it follows a channel change commanded from the ground). The dongle should follow it automatically (§10.4).

---

## 8. Identification information

| Information | Without key | With key (default `openhd` or the user's) |
|---|---|---|
| "This is an OpenHD link" | ✅ fingerprint §4.1 + 72-byte port-127 frames at 1 Hz | ✅ authenticated |
| Air vs ground transmitter | ✅ `unique_id` 01/02 | ✅ |
| TX boot instance | ✅ nonce range/jumps; session packet bytes (72 random bytes, constant per boot = stable per-session ID) | ✅ |
| Packet loss of the link at the dongle | ✅ nonce gaps per unit | ✅ |
| Bind phrase correct? | ❌ | ✅ session packet opens (also a cheap "is it the default phrase?" test) |
| Active streams, cameras | ✅ port bytes (10/11 = 2 cameras, 30 = audio) | ✅ |
| Protocol generation | ⚠️ heuristic on plaintext video | ✅ §2 |
| Video codec / resolution / fps / bitrate | ✅ (unencrypted video: RTP payload, SPS) | ✅ |
| FEC config (k per frame, overhead %) | ✅ `FECPayloadHdr` plaintext | ✅ |
| Frequency / bandwidth the air *intends* | ❌ (only what you tuned to) | ✅ management port 20 |
| Flight data (GPS, attitude, battery, mode, arm state) | ❌ | ✅ MAVLink |
| OpenHD version, camera status, air link stats, CPU temp | ❌ | ✅ OpenHD dialect (1221, 1219, 1214–1215, 1226–1228) |
| FC identity | ❌ | ✅ `HEARTBEAT.autopilot/type`, `AUTOPILOT_VERSION` if requested by the GCS |
| MAC addresses | none: OpenHD puts IDs/nonce there; no hardware MAC is ever sent | – |
| RF: RSSI, noise, rate | ✅ from the dongle's `rx_ctrl` (C5 limits: doc 08 A1) | ✅ |

There is no persistent hardware identifier on air. The closest thing to a "drone ID" is the bind-phrase keys (shared by every default-configured OpenHD system) and the per-boot session key.

---

## 9. Receive algorithm (normative summary)

```
on 802.11 frame f (FCS stripped):
  if !fingerprint(f) → count foreign; return
  dir = f[10]; port = f[15]>>1; enc = f[15]&1; nonce = LE64(f[11..15] ‖ f[17..21]); p = f[24..]
  loss_tracker[dir].on(nonce)
  if port == 127:
      require !enc && len(p) == 72
      if p == last_session_pkt[dir]: return               // cheap repeat
      key = secretbox_open(p[24..72], p[0..24], K[dir])  // fail → "wrong bind phrase" counter
      if key != session[dir]: session[dir] = key; reset streams of dir
      return
  if session[dir] unknown:
      if port in {10,11} && !enc && no-auth-mode: deliver(p[..−16]) unverified   // optional "keyless video"
      return
  body = enc ? aead_decrypt(nonce, p) : poly1305_check(nonce, p)   // fail → auth_fail++
  gen = detect_or_get(dir, port, body)                               // §2
  if gen == B: hdr = body[0..8]; body = body[8..]   (type 3 from gnd = ARQ request: just count it)
  switch port:
     10,11 → fec[port].push(body) → RTP packets in order → depacketise → decoder
     4,3   → dedupe(u64 seq) → MAVLink parser
     20,21 → management struct
     30    → dedupe → audio (future)
```

Latency budget on the host: one frame's arrival → RTP → decoder, no block wait on a clean link. FEC recovery adds at most the remaining airtime of that block (the secondaries follow immediately).

---

## 10. Consequences for capturetonkeli

### 10.1 Filter preset with the *existing* v1 protocol (no firmware change)

Doc 03 `SET_FILTER` allows ≤4 terms per rule. The OpenHD fingerprint fits in 3 terms, one rule per port class. Byte 16 is the uid copy, so the port and the uid can be matched together:

```
term A: off 0,  len 2, value 08 01
term B: off 4,  len 7, value FF FF FF FF FF FF <uid>
term C: off 15, len 2, value <port> <uid>,  mask <pmask> FF
rules (air downlink, uid=01), value/pmask:
   FE/FF (session) · 14/FC (video 10+11, plain or encrypted) · 08/FE (telemetry 4)
   28/FE (mgmt 20) · 3C/FE (audio 30)                                   → 5 rules × 3 terms
optional uplink rules (uid=02): FE/FF, 06/FE (telemetry 3), 2A/FE (mgmt 21)
```
The UI gets an "OpenHD" preset button (doc 04) that loads these rules plus snaplen 0.

### 10.2 USB bandwidth budget ⚠️ decision needed

| Case | Air bytes/s reaching the dongle | Must cross USB |
|---|---|---|
| OpenHD default 5.8 GHz, 20 MHz, MCS2, FEC 20 % | ≈13.2 Mbit/s ≈ 1.65 MB/s (all ports) | ≈1.65 MB/s raw ⇒ **impossible** over FS (≤1.216 MB/s theoretical) |
| Same, dongle drops secondaries of complete blocks (§10.3 level 1) | – | ≈1.4 MB/s ⇒ still too much |
| OpenHD video capped at 6 Mbit/s (user setting "variable bitrate" off / fixed bitrate) | ≈7.5 Mbit/s | ≈0.8 MB/s raw, ≈0.75 MB/s trimmed ⇒ OK if spike C2 reaches it |
| MCS0 20 MHz (≈4.7 Mbit/s link) | ≈0.6 MB/s | OK |

Conclusions:
- (a) Spike C2 (doc 08) must measure the *real* USJ bulk rate. The OpenHD use case sets the target to **≥ 1.0 MB/s**, not 0.5.
- (b) Document a "passive monitor" requirement: OpenHD video bitrate ≤ ~6–7 Mbit/s.
- (c) Implement trimming on the dongle (§10.3) so that FEC overhead never crosses USB on a good link.
- Truncating frames with snaplen is useless for video, because every byte is needed.

### 10.3 Proposed firmware "OpenHD assist" mode (new MINOR feature, caps bit)

Stateless or nearly stateless; needs no crypto for unencrypted video. Levels, each optional:

1. **Secondary drop.**
   - Track per (uid, port) the last ~8 blocks: `k` and a primary bitmap.
   - Forward primaries always.
   - Forward a secondary only while the block is still missing primaries and fewer than (missing) secondaries have been forwarded for it.
   - Saves the FEC overhead (≈17 % at 20 %) on a clean link. Loss behaviour is unchanged, because the host still receives enough parity to recover.
   - Needs Gen A/B awareness: the FEC header starts at payload offset 0 or 8. The host tells the dongle via config, or the dongle uses the §2 heuristic.
2. **Header compaction.** Replace the 24-byte 802.11 header + 16-byte tag (40 B/packet, ~3 %) with a compact OpenHD PKT variant (`uid, port, nonce` = 10 B).
   - Dropping the tag drops authenticity. Only allowed when the user selects "unverified" mode.
3. **On-dongle FEC decode + verify** (PSRAM block buffers: k ≤ 80 × 1449 B ≈ 116 KB per block).
   - The dongle forwards only reconstructed primaries.
   - It requires the session key on the dongle: the host sends `K_air` (32 B), and the dongle does XSalsa20-Poly1305 once per session plus Poly1305/HChaCha20 per packet. That is cheap on a 240 MHz RISC-V, and mbedTLS has ChaCha20 and Poly1305.
   - USB load then equals the video bitrate exactly. Worth it only if level 1 is not enough.

Recommendation: implement level 1 first. It keeps the firmware a generic sniffer with one OpenHD-specific filter action; all crypto stays on the host. Level 3 is a later option.

### 10.4 Auto-follow channel

With the key, the host reads management port 20 (frequency + width) and issues `SET_CHANNEL` when it differs. A frequency → channel map is needed (all OpenHD race-band freqs are standard 5 GHz channel centres).
The same applies to HT40: the secondary-channel side comes from OpenHD's channel table (HT40+ on most, see `wifi_channel.h` `in_40Mhz_ht40_plus`).
Without the key: a scan mode that dwells ~1.2 s per channel (one session packet interval) on the race-band list.

### 10.5 Host implementation notes

- **Browser (doc 04), zero-dependency.**
  - Needs hand-written XSalsa20+HSalsa20, Poly1305, ChaCha20, HChaCha20, about 400 lines total. These are fast enough for ~2,000 packets/s in a Worker.
  - X25519 is **not needed** if keys are given as `K_air`/`K_gnd` (default constants, or derived once).
  - For custom phrases: let the user load `txrx.key` (128 B) and compute `beforenm` with WebCrypto X25519 where available. Argon2id in JS (64 MiB, 2 passes) is the optional last resort, a few seconds once.
  - FEC: a log/exp table GF(256) as in the reference decoder.
  - Video: RTP depacketiser → Annex B / AVCC → **WebCodecs `VideoDecoder`** (H.264 broadly; H.265 only where the platform decoder exists) → canvas. Plus an optional raw `.h264/.h265` or pcapng recording.
- **Go (doc 05).**
  - ⚠️ The Go module is stdlib-only (CLAUDE.md), so `golang.org/x/crypto` is not available: port the hand-written primitives of js/ohdcrypto.js and js/ohdkeys.js (stdlib has `crypto/sha512`; ChaCha20/Poly1305/Salsa20/X25519/Argon2 must be written) and test them on the same fixtures. Remember the AEAD caveat in §4.6.
  - Provide `openhd.Receiver` with outputs: RTP packets (and an optional UDP 5600 forward so QOpenHD can be the display), MAVLink frames, link stats.
- **Test bench (doc 06):** the golden vectors (§11) become unit tests for firmware level 1, the JS decoder and the Go decoder.

---

## 11. Verification and artifacts

- `gen_vectors.cpp` links OpenHD's **own** `Encryption.cpp`, `Encryptor.cpp`, `KeyPair.cpp`, `FECEncoder.cpp`, `FEC.cpp`, `fec_base.cpp` and `Ieee80211Header.hpp` from wifibroadcast `14d5fdb`.
  - It reproduces `WBTxRx::tx_inject_packet` / `send_session_key` frame assembly and emits `vectors-2.6.json` and `vectors-2.7.json`.
  - Each file contains: a session packet, 2 video FEC blocks (k=6/m=1 and k=4/m=2) with one primary per block marked "dropped on air", a telemetry packet with a MAVLink v2 HEARTBEAT injected twice, and an air management frame (5745 MHz / 20 MHz).
- `openhd_rx_ref.py` is an **independent** reference receiver written from this spec, using libsodium only for primitives. It decodes both files:
  - session key opened with the `openhd` keys;
  - both dropped primaries recovered by FEC;
  - all 10 RTP fragments delivered in order;
  - telemetry deduplicated and its MAVLink parsed;
  - management frame decoded;
  - generation auto-detected correctly.
- The ChaCha20-Poly1305 *original* formula in §4.6 and the Poly1305/HChaCha20 auth path were checked separately against libsodium, using only a raw ChaCha20 core.

Files: `docs/13-assets/` holds the reference decoder (`openhd_rx_ref.py`, written against an earlier revision of this doc whose section numbers differ) and the generators of the test fixtures (§13.3). The vector files themselves are `test/js/fixtures/ohd-*.json`.

---

## 12. Open questions / risks

| # | Item | Severity | How to close |
|---|---|---|---|
| R1 | USJ throughput vs OpenHD bitrate (§10.2) | **high** | spike C2 with target ≥ 1.0 MB/s; decide on level 1 trimming |
| R2 | C5 RX of OpenHD frames: HT40 secondary side, LDPC/STBC if enabled, ToDS data frames with broadcast addr1 in promiscuous mode, FCS included | high | spike C1 against a real OpenHD air unit (or `wfb_tx`-style injection from a Linux 8812au) |
| R3 | Does the C5 report HT MCS / bandwidth for received frames (needed for UI only) | low | doc 08 A1 |
| R4 | Gen B is still a moving development branch (ARQ fields, `VIDEO_KEYFRAME` type 4 reserved) | medium | re-check `WBPacketHeader.h` before release; auto-detect keeps Gen A working |
| R5 | OpenHD ≤ 2.4 wire format | low | out of scope unless needed |
| R6 | Intra-refresh streams vs WebCodecs key-frame requirement | medium | test with a real stream |
| R7 | Audio stream format (port 30) | low | later |
| R8 | Ethics/legal: decrypting telemetry of links you don't own. UI text: "for monitoring your own OpenHD system" (doc 04 §15 legal notice) | – | wording |

---

## 13. Implementation (web UI, 2026-11-06)

The browser receiver follows §9 and runs in the existing worker (docs/04 §2); the page only decodes video (WebCodecs) and draws. No firmware or CTP change.

### 13.1 Modules

| File | Content |
|---|---|
| js/ohdcrypto.js | Salsa20/HSalsa20/XSalsa20, ChaCha20/HChaCha20, Poly1305 (13-bit limbs, no BigInt in the hot path), secretbox open/seal, auth-only packets, original ChaCha20-Poly1305 (§4.6) |
| js/ohdkeys.js, js/ohdkeys-worker.js | X25519 (BigInt), SHA-512 via WebCrypto, BLAKE2b, Argon2id (RFC 9106, one lane) → `keysFromPhrase`, `keysFromKeyFile` (txrx.key). A custom phrase takes ~5 s per key in a short-lived worker; "openhd" uses the built-in constants |
| js/ohdfec.js | Cauchy FEC (§5.3) on the GF(256) tables of js/hxfec.js |
| js/openhd.js | `Receiver` (§9), `FecStream` (§5.4), `Sender` (demo, tests), `captureFilter` (§10.1), channel helpers (`freqToChannel`, `ht40Plus` = OpenHD's wifi_channel.h table) |
| js/rtp.js | RTP → H.264/H.265 access units (Annex B, parameter sets re-inserted before key frames, codec auto-detect, WebCodecs codec string), `Packetizer` (demo) |
| js/mavlink.js, js/mavlink-crc.js | MAVLink v1/v2 framing + CRC_EXTRA (common + ardupilotmega + OpenHD dialect, generated by tools/mavlink_crc_table.py), decoders for the shown messages, `FlightState`, .tlog records |
| js/ohddemo.js | Demo OpenHD system on channel 149 (§13.4) |
| js/ui/openhd.js | FPV tab → System "OpenHD" |

### 13.2 Behaviour and decisions

- **Units.** Air units are told apart by session key (several OpenHD systems with known phrases work at once). A data packet is verified against the unit with the closest nonce first. A new session from a direction whose unit has been silent > 0.5 s is treated as a restart of that unit (counted, streams reset); otherwise it is a new unit. Units disappear after 15 s of silence. The ground unit is shown as "seen" and its uplink telemetry is parsed when the capture filter includes it.
- **Keys.** Default: the "openhd" constants (§4.6). The user can enter a phrase (Argon2id in the browser) or load txrx.key; the derived beforenm keys (not the phrase) are stored in localStorage until "Default".
- **Unverified video** (opt-in): with no session for a direction, unencrypted video is shown without authentication and marked UNVERIFIED. Telemetry is never shown unverified (it is encrypted anyway).
- **FEC queue** Q = 1 by default (like OpenHD), 2 or 4 selectable; "forward packets after a gap" is an option (§5.4). Packets are only forwarded from the oldest open block, so RTP order is kept.
- **Generation** is detected per unit on the first video/telemetry packet (§2) and re-detected after 5 packets that do not fit Gen B.
- **Video.** One depacketizer per shown unit/camera (primary or secondary). Access units go to the page; the page uses a `VideoDecoder` configured from the SPS (`avc1.PPCCLL` / `hev1.…`). If more than 20 are in flight or the decoder queue exceeds 8, sending stops until the next key frame; with intra refresh (no IDR) a frame with cached parameter sets is used after 3 s (§12 R6, counted as "starts without key frame").
- **Telemetry.** Parsed in the worker (`FlightState` per unit): vehicle/autopilot, armed, flight mode (ArduPilot copter/plane, PX4), GPS, position, VFR_HUD, attitude, battery (SYS_STATUS or BATTERY_STATUS), RC RSSI, STATUSTEXT, OpenHD version/camera/core status/link stats; per-system message counts. Shown in tables and as a one-line OSD under the video.
- **Follow channel** (default on): when the air unit announces a different frequency/width (port 20), the dongle is retuned (at most every 5 s), HT40± per OpenHD's channel table.
- **Frequency.** Presets (race band + 2452) or a custom frequency in MHz. The ESP32-C5 tunes only to Wi-Fi channel centres (2.4 GHz 2412–2472 + 2484; 5 GHz 5180–5885 in 20 MHz steps, as in the regdom tables), which is also all OpenHD uses; any other value is refused with the nearest channel named. A note shows the channel and warns when the dongle's country does not enable it.
- **Picture flip** (none / rotate 180° / mirror / flip vertically, for cameras mounted upside down): a CSS `transform` on the canvas, so the compositor does it at display time with no per-frame pixel work or extra latency; the OSD stays upright. Snapshots are flipped like the screen; recordings keep the original bitstream. Shared with the hx viewer (js/ui/flip.js, separate setting per system).
- **Recording** (in the worker, continues while the tab is hidden): raw `.h264/.h265` Annex B starting at a key frame (plays in VLC/ffmpeg) and `.tlog` (u64 big-endian µs + MAVLink frame; Mission Planner / QGroundControl).
- **Capture filter** = §10.1 (5 rules, ground: 3 more), snaplen 0; the previous filter is saved and can be restored.
- **Hints:** wrong bind phrase (session packets do not open), waiting for the session packet, USB full (`drop_ring` rising: lower the OpenHD bitrate, §10.2), snaplen, silent unit, authentication failures, browser without (H.265) decoder.
- **Packet list:** OpenHD frames get an info text (direction, stream, encrypted, nonce).

### 13.3 Verification

- `test/js/openhd.test.mjs` (12 tests):
  - crypto against libsodium 1.0.22 (`ohd-sodium.json`, generator `docs/13-assets/sodium_vectors.py`);
  - keys: X25519, BLAKE2b, Argon2id, seed keypair, beforenm against libsodium; the custom phrase and the txrx.key layout against OpenHD's own `generate_keypair_from_bind_phrase`;
  - receiver against frames built with OpenHD wifibroadcast's own Encryptor, FECEncoder (fec_base), Ieee80211Header and FECDisabledEncoder, Gen A and Gen B (`ohd-gen-{a,b}.json`, generator `docs/13-assets/gen_vectors.cpp`): session keys, 4 lost primaries recovered, both cameras, encrypted ("ultra secure") video, telemetry dedupe, ground uplink, management, a corrupted tag, a restart;
  - RTP against GStreamer x264/x265 + rtph26xpay mtu 1440 (`ohd-rtp.json`, `docs/13-assets/make_rtp_fixture.sh`): the depacketized NAL units equal the encoder's elementary stream;
  - MAVLink against frames packed by the OpenHD MAVLink C headers.
- Throughput (node, one core): ~20 000 frames/s through auth + FEC + depacketizing, about 10× what the USB link can deliver.
- Headless Chromium against the demo: H.264 decoded by WebCodecs at 30 fps with ~3 % loss, flight data in the OSD, recording (the .h264 decodes cleanly with ffmpeg; the .tlog parses), wrong phrase → hint, back to default → video again, phone layout.

### 13.4 Demo mode

js/ohddemo.js: on channel 149 the demo dongle hears an OpenHD air unit (default phrase): 640×360 H.264 from WebCodecs `VideoEncoder` (when available; otherwise telemetry only) at 30 fps / 1.5 Mbit/s, one FEC block per frame (max 20 primaries, 20 % secondaries), session packet every second, management every 500 ms, MAVLink at 10 Hz (ArduCopter circling in Loiter + OpenHD air unit messages), and a ground unit (session + GCS heartbeat). 3 % of frames are lost. The demo country must allow channel 149 (e.g. FI).

### 13.5 Not done yet

- Firmware "OpenHD assist" level 1 (§10.3) and the USB throughput target (§10.2, R1): needs spike C2 and a real air unit.
- Hardware test against a real OpenHD air unit (R2), intra-refresh streams (R6), LDPC/STBC.
- Scanning the race band without the key (§10.4), audio (R7), MAVLink forwarding (UDP is not possible from a browser; the Go library would provide UDP 5600/14550, §10.5).
- Go library `openhd.Receiver` (§10.5, stdlib-only port).

---

## Sources

Primary (code, read 2026-11-06):
- OpenHD core: https://github.com/OpenHD/OpenHD (branches `2.7-evo`, `2.6-evo`, tags v2.5.3, v2.6.0). Key files:
  - `ohd_common/inc/openhd_global_constants.hpp` (radio ports, key file path)
  - `ohd_interface/src/wb_link.cpp` (stream setup, encryption flags, radiotap params, bitrate)
  - `ohd_interface/src/wb_link_manager.cpp` (management frames)
  - `ohd_interface/inc/wb_link_settings.h` (defaults)
  - `ohd_interface/inc/wb_link_rate_helper.hpp` (MCS rate table)
  - `ohd_interface/src/ohd_interface.cpp` (bind phrase → key file)
  - `ohd_common/src/openhd_platform.cpp` (max FEC block size)
  - `ohd_video/inc/gst_helper.hpp`, `ohd_video/src/gstreamerstream.cpp`, `ohd_video/src/openhd_rtp.cpp` (RTP)
  - `ohd_telemetry/src/mav_include.h`, `ohd_telemetry/src/endpoints/WBEndpoint.cpp` (MAVLink aggregation, sysids)
- wifibroadcast (OpenHD fork): https://github.com/OpenHD/wifibroadcast, commits `14d5fdb`, `99e53e7`, `a64e5467`, `c22c564d`. Key files:
  - `src/Ieee80211Header.hpp`, `src/WBTxRx.{h,cpp}`
  - `src/encryption/*`
  - `src/fec/{FECPayloadHdr.hpp, FECConstants.hpp, FECEncoder.cpp, FECDecoder.cpp, RxBlock.cpp, FEC.cpp}`, `lib/fec/fec_base.cpp`
  - `src/SimpleStream.hpp`, `src/WBStreamTx.cpp`, `src/WBStreamRx.cpp`, `src/WBPacketHeader.h`
  - `src/radiotap/RadiotapHeaderTx.hpp`
- OpenHD MAVLink dialect: https://github.com/OpenHD/mavlink-headers (`mavlink/v2.0/openhd/`)
- libsodium docs (crypto_box, original ChaCha20-Poly1305 construction): https://doc.libsodium.org/
- OpenHD releases page: https://github.com/OpenHD/OpenHD/releases
- ESP32-C5 Wi-Fi capability (802.11n 20/40 MHz MCS0–7, 5180–5885 MHz): https://documentation.espressif.com/esp32-c5_datasheet_en.html
- XIAO ESP32-C5 memory (384 KB SRAM, 8 MB PSRAM): https://wiki.seeedstudio.com/xiao_esp32c5_getting_started/
