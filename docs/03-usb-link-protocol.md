# 03 – USB link protocol "CTP" (capturetonkeli protocol) – draft v1 (2026-11-04)

Scope: the byte protocol between the dongle (XIAO ESP32-C5, USB Serial/JTAG CDC-ACM) and the browser (Web Serial on desktop, WebUSB CDC shim on Android).
One channel at a time; the host changes it by command.
This doc **supersedes §6 "USB frame format" in doc 02**.

---

## 1. What can actually go wrong on this link

| Failure | Real? | Consequence |
|---|---|---|
| Bit errors on the wire | Practically no. USB bulk carries a CRC16 per data packet and retries in hardware; delivery is guaranteed or the device drops off the bus | No need for retransmission at our layer |
| Host joins mid-stream (page reload, port reopen, Android re-grant) | **Yes, every session** | First bytes are the middle of a frame → must resync |
| Host app too slow / tab throttled | **Yes** | Chrome Web Serial errors the stream with `BufferOverrunError`, bytes lost, stream must be reopened |
| Dongle ring buffer full (radio > USB rate) | **Yes, by design** | Whole frames dropped → must be *counted* and reported, never half-frames |
| USJ TX blocked with no reader (~50 ms stall once) | Yes | Wi-Fi task must never block on USB |
| Dongle reboot / brown-out / unplug | Yes | Host must notice and resync config |
| Command lost / no response | Rare but possible (e.g. sent during reconnect) | Need timeout + idempotent retry |

So "reliable" here means **resync fast, detect and count loss, never deliver a corrupted or spliced frame, and make commands idempotent**. It does *not* mean retransmitting captured packets. That would add latency, and the radio data is a lossy sample anyway.

---

## 2. Existing standards considered

| Option | Verdict |
|---|---|
| **SLIP (RFC 1055)** framing – used e.g. by nRF Sniffer for BLE | Simple, but escape-based. Worst case doubles the frame; variable overhead |
| **HDLC / PPP async framing** | Same escape problem (100% worst case) plus FCS. Heavier than needed |
| **COBS** | Worst case +1 byte per 254, ~0.4%. Fixed delimiter `0x00`, trivial resync. ESP-IDF component exists. **Chosen** |
| MAVLink v2 | Good model (seq, CRC, msg IDs) but payload max 255 B. Wi-Fi frames up to ~4 KB → no |
| Protobuf (nanopb) / CBOR / MessagePack for commands | Clean, but adds a dependency on both ends for ~15 small fixed messages. Not worth it; fixed little-endian structs are smaller, faster and easy to read in a `DataView` |
| nRF Sniffer-style header (board ID, proto version, packet counter, packet ID) | **Borrowed** as the idea for the common header: version + counter + type is exactly what is needed |
| pcap/pcapng directly over serial | Has no command channel and no framing for resync. The browser *produces* pcapng from our frames instead |

**Decision: COBS framing + fixed binary header + CRC-32 + custom message set.** No third-party dependencies on either side.

---

## 3. Layer 1 – Framing

```
wire:  COBS( frame ) 0x00
frame: header(8) | body(N) | crc32(4)
```
- Delimiter is `0x00`. COBS guarantees no `0x00` inside the encoded frame.
- **Resync rule:** the receiver discards bytes until the first `0x00`, then decodes frame by frame. On a page reload the host loses at most one frame.
- Sender may also *prefix* a `0x00` before each frame (an empty frame is ignored). Cheap insurance after a stall.
- **Max decoded frame = 4096 + 32 bytes.** The receiver drops anything longer (prevents runaway on garbage) and counts it as `rx_oversize`.
- **CRC-32 (IEEE, reflected, same as Ethernet/zlib)** over header+body.
  - Why a CRC when USB has one? It protects against **splices**: a frame whose middle was lost in a host buffer overrun or a reconnect, but which still decodes as valid COBS. Without the CRC it would parse as a bogus but plausible Wi-Fi packet.
  - Cost: ESP32-C5 ROM has `esp_rom_crc32_le()`; JS table-driven CRC-32 easily handles ≫10 MB/s.
  - Frame with bad CRC → drop + `rx_crc_err++`. Never "repair".

## 4. Layer 2 – Common header (8 bytes, little-endian)

```
off size field
0   1    ver       protocol version (=1). Mismatch → host shows "update firmware/page"
1   1    type      message type (§6)
2   2    seq       per-direction frame counter, +1 for every frame sent, wraps
4   2    req_id    host-chosen ID echoed in the response; 0 for unsolicited frames
6   1    flags     bit0 = more-to-follow (reserved), bit1 = cfg changed since last frame
7   1    cfg_gen   device config generation (see §7)
```
- **seq gap ⇒ loss detected at link level.** Combined with the device's own drop counters, the host can tell "dropped on the dongle because USB too slow" apart from "lost on host side (overrun/reconnect)". That distinction matters for tuning snaplen/filters.
- `req_id` makes request/response matching independent of ordering. Packets and responses are interleaved on the same stream.

## 5. Layer 3 – Request/response semantics

- Host → device frames are **requests** only. Device → host frames are **responses**, **events**, or **captured packets**.
- Every request gets **exactly one** response with the same `req_id`. The response is either `RSP_OK` (with readback payload) or `RSP_ERR` (with error code + esp_err_t).
- **All SET commands are idempotent and carry absolute state** (e.g. "channel = 44", never "next channel"). Host retry policy: timeout 200 ms → resend same `req_id` up to 3× → declare link dead.
  - If the device sees a duplicate `req_id` it has just executed, it re-sends the cached response and does not re-apply. Keep the last 4 (req_id, response) pairs.
- **Every SET response returns the value actually applied** (readback), not an echo of the request. For example, SET_CHANNEL 165 under country FI → `RSP_ERR(ERR_CHANNEL_NOT_ALLOWED)`. A successful SET_BW returns the bandwidth the driver really accepted.
- **Priority:** responses and events must not queue behind a backlog of captured packets. The firmware TX task has two queues: `ctrl` (always drained first) and `pkt`. So command round-trip time stays ~1–2 ms even under full capture load.

## 6. Message set (v1)

### Host → device (requests)
| type | name | body |
|---|---|---|
| 0x01 | HELLO | u32 host_nonce, u8 host_proto_ver |
| 0x02 | PING | u32 host_time_ms |
| 0x10 | GET_CONFIG | – |
| 0x11 | SET_CHANNEL | u8 channel, u8 bw (0=HT20, 1=HT40+, 2=HT40−) |
| 0x12 | SET_COUNTRY | char cc[3], u8 mode (0=validated set_country_code, 1=manual range) , u8 schan, u8 nchan |
| 0x13 | SET_FILTER | rule set, encoding below (semantics doc 02 §2), replaced atomically |
| 0x14 | SET_SNAPLEN | u16 snaplen (0 = unlimited) |
| 0x15 | SET_DRIVER_MASK | u32 type_mask, u32 ctrl_mask |
| 0x20 | CAPTURE | u8 on (0/1) |
| 0x21 | GET_STATS | u8 reset_after_read |
| 0x30 | REBOOT | u8 mode (0 = normal, 1 = download/bootloader for web flashing) |

### Device → host
| type | name | body |
|---|---|---|
| 0x81 | RSP_OK | request-specific readback (HELLO → device info, GET_CONFIG/SET_* → full CONFIG struct) |
| 0x82 | RSP_ERR | u16 err_code, i32 esp_err, u8 detail_len, char detail[] |
| 0x90 | EVT_STATS | sent every 1 s while host alive: counters (§8) |
| 0x91 | EVT_LOG | u8 level, text (debug; can be disabled) |
| 0x92 | EVT_RESET | sent once at boot: u8 reset_reason, fw version |
| 0xA0 | PKT | captured 802.11 frame (§9) |

`SET_FILTER` body (little-endian; defined in doc 04 Appendix A):
```
u8 n_rules | u8 gflags (bit0 drop_fcs_err, bit1 rssi_min_en, bit2 len_min_en, bit3 len_max_en)
i8 rssi_min | u8 reserved | u16 len_min | u16 len_max
repeat n_rules:  u8 n_terms (1..4)
  repeat n_terms: u16 offset | u8 len (1..16) | u8 tflags (bit0 negate) | value[len] | mask[len]
```
Pass = global conditions AND (n_rules == 0 OR any rule with all terms matching). Terms are evaluated on the full frame before snaplen. `rules_crc32` = CRC-32 over this body. Limits: ≤16 rules × ≤4 terms × ≤16 bytes → max body 2 328 B, always fits in one frame.

`HELLO` response = `{u8 proto_ver, u32 fw_version, u8 fw_flags, u8 fw_build_id[8], u8 mac[6], u32 caps (bit0 5GHz, bit1 HT40, bit2 MANUAL_COUNTRY (legacy manual channel range available, doc 10 §2), …), u16 max_frame, u16 max_rules, u64 device_time_us, u32 host_nonce (echo)}`.
- `fw_version` = semantic version packed as `major << 24 | minor << 16 | patch` (u8 major, u8 minor, u16 patch). Taken from the git tag `vMAJOR.MINOR.PATCH` at build time (§11).
- `fw_flags`: bit0 `dirty` (built from a modified tree), bit1 `idf_mismatch` (built with a non-pinned IDF), bit2 `dev` (untagged build; the version is the next patch + commits since the tag).
- `fw_build_id` = first 8 bytes of the SHA-256 of the app image. It identifies the exact build; hosts use it to show "this is exactly the bundled firmware" vs "same version, different build".

`CONFIG` struct = everything the device holds:
`{cfg_gen, channel, bw, band, country cc[3] + mode, snaplen, driver masks, capture_on, n_rules, rules_crc32}`.
The host doesn't need the rule bytes echoed; it compares `rules_crc32` with its own.

## 7. Session lifecycle and liveness

1. Host opens port (desktop: set DTR/RTS explicitly to avoid USJ reset sequence, see doc 01 risks) and starts the read loop.
2. Host sends `HELLO`. The device answers with info. The device **does not stream packets before a HELLO** and boots with capture **off**. That keeps the USJ TX path idle when nobody listens, which avoids the 50 ms stall.
3. Host pushes its desired full config: SET_COUNTRY → SET_CHANNEL → SET_SNAPLEN → SET_FILTER → CAPTURE 1. Each is verified via readback.
4. **Keepalive:** host sends `PING` every 500 ms. If the device hears nothing from the host for 2 s, it sets capture off and stops sending (host died, tab closed, cable to phone flaky).
5. **cfg_gen:** a u8 incremented by the device on every applied config change, carried in *every* frame header.
   - Every captured packet is tagged with the config in force when it was captured, so after SET_CHANNEL the host knows exactly where the new channel's packets start.
   - If the host sees a `cfg_gen` it didn't cause, or an `EVT_RESET`, the device rebooted or someone else changed it. The host then re-issues GET_CONFIG and re-pushes its config.
6. Reconnect after unplug/overrun: same as step 1. Because SETs are absolute and idempotent, "re-push everything" is always safe.

## 8. Counters (EVT_STATS / GET_STATS)

```
u64 device_time_us
u32 rx_total        frames seen by promiscuous callback
u32 rx_passed       passed user filter
u32 drop_ring       dropped: ring buffer full (USB too slow)
u32 drop_fcs        dropped: bad FCS (if filtered)
u32 tx_frames       frames handed to USB
u32 tx_bytes
u32 host_rx_crc_err / host_rx_oversize  (counted by device on host→device direction)
u16 ring_high_water  max ring buffer fill since last read, in %
u8  channel, i8 noise_floor
```
The host adds its own: seq gaps, CRC errors, overruns. The UI shows them so the user sees when filters/snaplen need tightening.

## 9. PKT message body (fixed 20 bytes + data)

```
off size field
0   8    ts_us       device µs (esp_timer, 64-bit: no wrap handling needed)
8   1    channel
9   1    flags       bit0 fcs_err, bit1 truncated, bit2 5GHz, bit3 HT40, bit4 ampdu
10  1    rssi        (i8, dBm)
11  1    noise_floor (i8, dBm)
12  1    rate        legacy rate code or MCS index
13  1    sig_mode    0=11bg, 1=HT, 3=VHT/HE (as rx_ctrl reports)
14  2    orig_len    original 802.11 length incl. FCS
16  2    cap_len     bytes that follow
18  2    rx_seq      per-capture counter of *passed* frames. A gap with seq continuous = dongle-side drop
20  …    data[cap_len]
```
Total overhead per captured frame: 8 hdr + 20 pkt + 4 CRC + COBS (~1 + 1 per 254) + delimiter ≈ **35 bytes**. Fine against typical 100–1500 B frames. With snaplen 32 the frame is ~67 B; FS USB then carries roughly 10–15k frames/s.

The browser converts PKT → pcapng EPB with a radiotap header (LINKTYPE 127: TSFT, flags, rate, channel freq, antenna signal/noise) for saving and Wireshark.

## 10. Low-latency implementation rules

**Firmware**
- Promiscuous callback: filter → copy into a lock-free SPSC ring buffer (PSRAM is too slow here; use ~64–128 KB internal SRAM) → return. No COBS or CRC in the callback.
- USB TX task: pops ctrl queue first, then packets. It encodes (COBS+CRC) into a contiguous buffer and writes with `usb_serial_jtag_write_bytes()` with a short/zero timeout. On timeout it re-queues, never blocks the Wi-Fi task.
- **Batching without delay:** if more frames are waiting, encode several into one write so 64-byte USB packets go out full. If the queue is empty, write and flush immediately. Latency ≈ 1 USB frame (1 ms) + encode time; throughput maxes out under load.
- Ring full → drop newest, `drop_ring++`. Never drop half a frame.

**Browser**
- Open the port with a large `bufferSize` (e.g. 1 MiB; Chrome default is small) and keep a tight `read()` loop that does nothing but push chunks to a parser. Do COBS/CRC/parsing in a **Worker** (transfer the ArrayBuffers) so UI rendering never starves the reader.
- On `BufferOverrunError` or other read errors: release, reopen the stream, resync (§7 step 1). Count it.
- WebUSB path (Android): keep ≥8 IN transfers of 4–16 KB in flight; same parser.
- Display throttled to ~30 fps from a ring of recent packets. Writing pcapng happens incrementally via a file handle (desktop: File System Access API; Android: OPFS then download).

## 11. Versioning

Two version numbers exist, with different jobs:

| | Protocol version (`ver`, header byte 0) | Firmware version (`fw_version`, semver) |
|---|---|---|
| Answers | "Can we parse each other's frames at all?" | "Do UI and firmware implement the same features and behaviour?" |
| Changes when | wire format is incompatibly changed | every release |
| Mismatch handling | **hard stop**: the host can't talk to the device except to update it (download mode via DTR/RTS) | **user decision**: prompt (UI) / error + override flag (CLI), see below |

**Firmware semantic versioning rules (normative):**
- **MAJOR**: incompatible change for hosts: a protocol `ver` bump, a removed message, or a changed meaning of a field. MAJOR always increments when `ver` increments.
- **MINOR**: a compatible but meaningful change that a matching host may depend on: new messages or fields, new caps, changed defaults, behaviour visible in captures (e.g. filter semantics fixes, new PKT flags).
- **PATCH**: bug fixes with no interface or behaviour change that a host must know about.
- **Compatibility check:** host version H (the firmware version the host was built and tested with) and device version D are **compatible iff `H.major == D.major && H.minor == D.minor`**. PATCH differences are accepted silently (shown as info). Pre-release and dirty flags never make a version compatible that wouldn't otherwise be. They are shown as a warning.
- On mismatch the host offers: **(1) flash the bundled firmware H**, **(2) continue anyway** (unsupported mode, permanent warning, features still gated by `caps`), or **(3) cancel**. Details per host in doc 04 §4.1 and doc 06 §1/§2.
- Forward-compat body rules below still apply, so "continue anyway" degrades gracefully instead of mis-parsing.

**Wire-level evolution (unchanged):**
- `ver` in every header. The host refuses anything ≠ its supported version and tells the user (and offers flashing).
- New messages use new type IDs. New fields go only at the **end** of bodies; receivers ignore extra trailing bytes and treat missing trailing fields as defaults. That allows minor additions without bumping `ver`.
- `caps` bitmask in HELLO for optional features (hopping later, HT40, etc.).

## 12. Test plan for the link
1. Fuzz: random bytes / truncated frames into both parsers → no crash, counters increment, recovery on next `0x00`.
2. Reload page during full-rate capture → first valid frame within 1 frame time, config re-pushed, no bogus packets.
3. Command RTT (PING) under 100% capture load: target p99 < 5 ms desktop, < 10 ms Android.
4. Throttle host (block main thread 2 s) → overrun detected, reconnect, counters correct.
5. Unplug/replug → EVT_RESET or link-dead detection < 1.5 s.
6. Max-size frames (4 KB A-MSDU) with snaplen 0.

## Sources
- COBS paper (Cheshire & Baker): https://stuartcheshire.org/papers/COBSforToN/
- COBS vs SLIP overhead comparison: https://blog.mbedded.ninja/programming/serialization-formats/consistent-overhead-byte-stuffing-cobs/
- COBS (Wikipedia, HDLC worst case): https://en.wikipedia.org/wiki/Consistent_Overhead_Byte_Stuffing
- nRF Sniffer for BLE UART packet format (Wireshark dissector): https://gitea.osmocom.org/osmith/wireshark/src/branch/master/epan/dissectors/packet-nordic_ble.c
- USB endpoint types / bulk CRC and retry: https://www-user.tu-chemnitz.de/~heha/hs/chm/usb.chm/usb4.htm
- Web Serial spec: https://wicg.github.io/serial/
- Web Serial BufferOverrunError report (blink-dev): https://mail-archive.com/blink-dev@chromium.org/msg07458.html
- ESP-IDF USB Serial/JTAG console notes (50 ms stall, flow control): https://docs.espressif.com/projects/esp-idf/en/v5.4/esp32c5/api-guides/usb-serial-jtag-console.html
