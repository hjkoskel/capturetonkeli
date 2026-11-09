# 14 – Multi-dongle receive diversity – spec draft v2 (2026-11-06)

> **Status (2026-11-09):** browser host side implemented for the firmware as it is (no protocol change): group of up to 4 dongles all forwarding everything, dedup with first copy wins, clock alignment, bad copies, per-antenna radiotap, per-dongle statistics, member loss and rejoin, demo with several dongles. See §9. Not yet: PKT trailer, SET_SHARD and spill (firmware + docs/03 + Go reference first, CLAUDE.md order of work), combining broken frames (needs the FCS, see §9.1), Go `OpenGroup`.

Scope: using 2–3 capturetonkeli dongles at the same time on one host (browser or Go). The dongles can have different antennas (omni, patch, different polarisation) and are treated as **one logical capture device**.

Goals:
1. **Reliability:** a frame missed or corrupted by one antenna is recovered from another.
2. **USB headroom:** under heavy traffic the dongles split the work, so the USB capacity of N dongles adds up.
3. **Low latency:** merging adds no delay on the live path.
4. **Fully backward compatible:** one dongle behaves exactly as before (same bytes on the wire); old firmware and old hosts keep working.

Non-goals (v1):
- Different channels or settings per dongle. **All dongles always follow the same channel, bandwidth, country, filter and snaplen.**
- More than 4 dongles (the format allows it, but it isn't tested).
- Transmitting.

Extends doc 03 (CTP protocol) and doc 05 (Go library). Wire changes are additive, so this is a **firmware MINOR bump**, with no protocol `ver` change.

Changes in v2: the PKT trailer is now lean and conditional (§2.2). It costs 0 B with a single dongle, 4 B for full frames in a group and 8 B for truncated frames. `sflags` was removed; the host derives ownership itself.

---

## 1. Core idea: owned frames vs extra copies

Every dongle sees the same air. For every received frame, each dongle **independently** computes the same shard slot from the frame's own FCS:

```
slot  = fcs % n                          (fcs = the frame's received CRC-32, last 4 bytes)
owned = ((my_index - slot + n) % n) < owners
```

- **Owned** frames are enqueued with normal priority.
- **Extra copies** (not owned, *or* the FCS is bad) are enqueued only while the ring buffer fill level is below `spill_pct`. Above that they are dropped and counted as `drop_spill`.

What this gives:

| Traffic | Behaviour |
|---|---|
| Light (ring rarely above `spill_pct`) | Every dongle forwards everything, so you get full diversity |
| Heavy | Each dongle forwards only its owned share, so USB load per dongle ≈ owners/n |
| In between | Shifts smoothly, frame by frame, with no host round trip |

The dongles never talk to each other. They agree on ownership because they all compute it from the same FCS. A bad-FCS frame has a garbage hash, so it is **always an extra copy**.

Recommended defaults:

| Dongles | n | owners | Load per dongle under saturation | Copies of each frame under saturation |
|---|---|---|---|---|
| 1 | 1 | 1 | 1 | 1 (identical to today) |
| 2 | 2 | 1 | 1/2 | 1 |
| 3 | 3 | 2 | 2/3 | 2 |

`spill_pct` default: **50**.

---

## 2. Firmware changes

### 2.1 HELLO caps (new bits)
```
bit3  PKT_TRAILER   the firmware can append the trailer of §2.2 (it is only sent when enabled)
bit4  SHARD         SET_SHARD is supported, plus the extra counters of §2.5
```

### 2.2 PKT trailer: lean and conditional
New PKT flag (doc 03 §9 `flags`): **bit5 `trailer`**, set when a trailer follows `data[cap_len]`.

Layout, determined by the PKT flags alone (no config lookup needed):
```
if flags.trailer:
    u32 hw_ts        rx_ctrl.timestamp: µs, latched by the MAC on receive (32-bit, wraps ~71 min)
    if flags.truncated (bit1, i.e. cap_len < orig_len):
        u32 fcs      the frame's received FCS (last 4 bytes of the air frame)
```
- **Frame not truncated:** the FCS is already the last 4 bytes of `data` (`orig_len` includes the FCS). Sending it again would be redundant.
- **Frame truncated** (including a frame cut inside its own FCS): the FCS is sent in the trailer.
- `fcs` is always the received value, even when `fcs_err` is set. The host never uses it as a key for bad frames.
- `hw_ts` is used for clock alignment (§4.4). It jitters less than `ts_us` (esp_timer read in the callback). The host unwraps it using `ts_us`.
- Not sent anymore (vs v1): `sflags`/reserved. The host computes `owned` from `fcs` and the shard config valid for that frame's `cfg_gen`. "Extra copy because the FCS is bad" is the existing `fcs_err` flag.
- Sanity check on the host: bytes left after `data` must be exactly 0, 4 or 8, matching the flags. Anything else → drop the frame and increment `rx_bad_trailer`.
- **Future trailer fields need a new PKT flag bit.** Older hosts ignore unknown trailing bytes (doc 03 §11), so this stays compatible.

When the trailer is sent: controlled by `trailer_mode` in SET_SHARD (§2.3):
- `0 = auto` (default): on iff `n > 1`.
- `1 = always on`: e.g. a single-dongle host that wants `hw_ts`.
- `2 = off`.

USB cost (baseline ≈ 35 B overhead per frame; the link is byte-limited when writes are batched):

| Case | Trailer | Frame-rate cost at snaplen 32 | Cost at 1500 B frames |
|---|---|---|---|
| Single dongle (auto) | 0 B | 0 | 0 |
| Group, frame not truncated | 4 B | n/a (snaplen 0) | ~0.3% |
| Group, truncated frame | 8 B | ~10% (67 → 75 B per frame) | – |

In sharded mode this is easily repaid: with 3 dongles and 2 owners, each dongle carries ~2/3 of the traffic.

### 2.3 SET_SHARD (new request, type 0x16)
```
u8  n             1..4   (1 = sharding off, the default after boot)
u8  my_index      0..n-1
u8  owners        1..n
u8  spill_pct     0..100 (100 = only drop extra copies when the ring is completely full)
u32 group_id      host-chosen random value; 0 = not in a group
u8  trailer_mode  0 = auto (on iff n > 1), 1 = always on, 2 = off
```
- Validation: `my_index < n`, `1 ≤ owners ≤ n`, `trailer_mode ≤ 2`. Otherwise `RSP_ERR(ERR_BAD_ARG)`.
- Applied atomically between two frames; increments `cfg_gen` like every other SET.
- Idempotent and absolute, like all SETs (doc 03 §5).
- Boot default: `n=1, my_index=0, owners=1, spill_pct=100, group_id=0, trailer_mode=0`. This is exactly the current behaviour, with no trailer on the wire.

### 2.4 CONFIG struct trailer
The CONFIG readback gets `{u8 n, u8 my_index, u8 owners, u8 spill_pct, u32 group_id, u8 trailer_mode, u8 trailer_on}` appended at the end. `trailer_on` = the effective state after `auto` is resolved.

### 2.5 Counters (EVT_STATS / GET_STATS trailer)
```
u32 drop_spill     extra copies dropped on purpose (ring fill ≥ spill_pct)
u32 drop_owned     owned frames dropped because the ring was full (this is real loss)
u32 tx_owned       owned frames handed to USB
u32 tx_spill       extra-copy frames handed to USB
```
The existing `drop_ring` stays and still means the total (`drop_spill + drop_owned`), so old hosts keep working.

### 2.6 Where the decision is made
In the promiscuous callback, after the user filter and before the copy into the ring:
1. Read the FCS from the last 4 bytes of the received buffer.
2. Compute `owned` (one modulo and one compare).
3. If not owned and `ring_fill_pct ≥ spill_pct`: `drop_spill++`, then return.
4. Otherwise enqueue as today, storing `hw_ts` and the FCS alongside the frame. If the ring is full: count as `drop_owned` or `drop_spill` by class.

The USB TX task appends the trailer when encoding, if `trailer_on`. Control messages still take priority over everything (doc 03 §5).

### 2.7 Frames with a bad FCS
These are only forwarded if the user filter allows them (`gflags.drop_fcs_err = 0`). In a multi-dongle group the host turns this on by default when combining (§4.5) is enabled. They are always in the extra-copy class.

---

## 3. USB topology requirement

- Every dongle is a USB full-speed device (12 Mbit/s).
- On a **single-TT USB 2.0 hub, all full-speed devices share one 12 Mbit/s transaction translator.** Two or three dongles then gain *nothing* on throughput, only diversity.
- Required for the throughput benefit: **separate root ports, or a multi-TT hub**.
- The host can't reliably detect the TT type from the browser. The UI therefore shows a saturation hint: if the sum of `tx_bytes`/s across dongles plateaus near a single dongle's maximum while `drop_owned` grows, it shows "dongles appear to share a USB link (single-TT hub?)".
- Android OTG: several WebUSB devices behind a hub is allowed, but a **powered hub** is recommended. Treat it as best effort.

---

## 4. Host side (browser and Go library: same algorithm)

### 4.1 Group model
```
Group
  id          u32 random (group_id)
  members[]   { port, mac, label, index, caps, cfg_gen, shard_cfg_by_gen, state, clock }
  config      the one desired config (channel, bw, country, filter, snaplen, shard params)
```
- **Identity:** a member is identified by the MAC from HELLO. The user can label each one ("patch N", "omni"). Labels persist in localStorage, wrapped in try/catch (browser), or in a config file (Go).
- **Index assignment:** sorted by MAC, so it stays stable across reconnects.
- **`shard_cfg_by_gen`:** the host remembers the shard config confirmed for each recent `cfg_gen`, so it can work out `owned` for any frame (stats only; nothing depends on it for correctness).
- **Members without SHARD:** take part as *forward-everything* members. They aren't counted in `n` and never get SET_SHARD.
- **Members without PKT_TRAILER:** fallback dedup key (§4.3); excluded from combining and clock lock.
- **A single dongle** is a group of one. Nothing is sent beyond today's sequence (or SET_SHARD with defaults); no trailer; the merge stage is a pass-through.

### 4.2 Configuration and membership changes
**Start:**
1. Run HELLO on every port in parallel.
2. Push the same config to every member (doc 03 §7 step 3), plus SET_SHARD with `spill_pct=100`.
3. Verify the readback. Every member must report an identical CONFIG apart from `my_index`.
4. Send CAPTURE 1 to all.
5. Set the final `spill_pct`.

**Any change to n or ownership: make before break.** This avoids an ownership hole while members apply the change at slightly different times:
- A. SET_SHARD `spill_pct=100` to all members. Everyone now forwards everything (bandwidth permitting).
- B. SET_SHARD with the new `n / my_index / owners` to all.
- C. Wait until every readback has been confirmed, then restore `spill_pct`.

Going from 2 members to 1 sets `n=1`. With `trailer_mode=auto` the trailer turns off; the host handles both forms using the PKT flags, so the switch needs no special case.

**Channel or filter changes:** pushed to all members. Each member's `cfg_gen` marks where the new config starts in its own stream. The merge treats frames from different config generations as different epochs and never dedups across them.

**Member lost** (link dead, unplug, EVT_RESET):
- Immediately run the re-shard procedure with the remaining members.
- If it was a reset: once the member comes back, push the full config again and re-shard to include it.

**Mismatch:** if a member can't apply the config (for example the channel isn't allowed by its country table), the UI shows it as *degraded* and excludes it from the group until it is fixed.

### 4.3 Dedup key and window
- **FCS source:** last 4 bytes of `data` when not truncated, otherwise the trailer `fcs`.
- Key for good frames: **`(fcs, orig_len)`**. Two frames with the same key are the same air frame **only if** their aligned times are within tolerance `tol`:
  - Clock locked (§4.4): `tol = 20 µs`.
  - Not locked: `tol = 2 ms`, using `ts_us`. Rare false merges of identical control frames (ACK/CTS to the same RA) are accepted until lock.
  - Identical ACK/CTS frames to the same station are at least one data frame plus SIFS apart (≫ 20 µs), so a locked group doesn't merge them by mistake.
- Without a trailer (member lacks PKT_TRAILER) and the frame is truncated: key = `(CRC-32 over the captured bytes, orig_len, cap_len)`. This only works when every member uses the same snaplen, which the group config guarantees.
- Entries expire after 100 ms. Size the table for ~5k frames/s × 0.1 s.

### 4.4 Clock alignment
- Each member's time = unwrapped `hw_ts`. The reference is the member with index 0. For each other member, keep a model `t_ref = a·t + b`.
- **Anchors:** good-FCS frames whose key appears **exactly once** per member inside ±50 ms. Beacons, probe responses, data frames and OpenHD packets naturally qualify; repeated ACK/CTS frames naturally don't.
- Fit: weighted least squares over the last ~2 s of anchors; reject residuals > 50 µs. Typical crystal tolerance (±20–40 ppm) means the drift term is required.
- **Lock** = at least 8 anchors and residual RMS < 5 µs. Lock state and estimated ppm are shown per member.
- Loss of anchors (quiet channel): keep the last model and widen `tol` gradually back to 2 ms.
- Future option (not v1): a shared GPIO sync pulse between the XIAOs.

### 4.5 Combining broken frames
Only for frames where `cap_len == orig_len` (snaplen 0 or large enough). Truncated frames can't be repaired.

Candidates: bad-FCS copies from different members with the same `orig_len` and aligned time within `tol`.
- **If any member has a good copy** with the same length and time, use it; the bad copies only contribute RSSI.
- **Two bad copies:** D = set of bit positions where they differ. If |D| ≤ 16, search 2^|D| combinations. CRC-32 is linear, so precompute the CRC delta per bit position: each candidate costs one XOR, and 65k candidates take well under a millisecond.
- **Three bad copies:** bitwise majority vote, then check the FCS. If that fails, run the same search over the bits that weren't unanimous (≤ 16).
- A recovered frame is emitted as good and flagged `combined` (§4.7).
- Wait budget: hold a bad-FCS copy up to **3 ms** for other copies, then give up. If the user asked to see bad frames, emit it as bad.

False-positive risk: CRC-32 over a search of ≤ 2^16 candidates gives about 2^16/2^32 ≈ 1.5e-5 chance of a wrong "repair" per attempt. That's acceptable for monitoring. The UI counts combined frames separately so they're visible.

### 4.6 Output paths
| Path | Rule | Added latency |
|---|---|---|
| **Live** (UI, OpenHD decoder, Go `Frames()` channel) | **First good copy wins** and is emitted immediately. Later copies only update its metadata. | 0 |
| **Combined frames** | Emitted when recovered. | ≤ 3 ms |
| **Archive** (pcapng) | Jitter buffer of 20 ms, sorted by group time, all metadata final. | 20 ms (doesn't matter) |

The OpenHD decoder (doc 13) consumes the live path; its own FEC sits on top of this (see §9.3: it must tolerate late copies).

### 4.7 pcapng representation
- Default: **one interface (IDB) for the group**. Each EPB carries a radiotap header with:
  - the common fields, as in doc 03 §9;
  - **per-antenna signal**, using radiotap extended presence words (one radiotap namespace per copy, with fields Antenna = member index and dBm antenna signal). Wireshark then shows the RSSI from each dongle;
  - an EPB comment (`opt_comment`) when the frame is `combined`, or when it was received by only one member (optional, off by default).
- Debug option: **one IDB per member** with no dedup (raw streams), named by label + MAC. Used to verify the merge.

### 4.8 Browser architecture
```
port A ─ read loop ─▶ worker A (COBS/CRC/parse) ─┐
port B ─ read loop ─▶ worker B                   ├─▶ merge worker ─▶ live consumers / pcapng writer / UI
port C ─ read loop ─▶ worker C                   ┘
```
- One `requestPort()` per dongle. Web Serial supports several open ports at once.
- Parsed frames are passed to the merge worker as transferable ArrayBuffers (no SharedArrayBuffer: GitHub Pages can't set the COOP/COEP headers it needs).
- The merge worker owns the dedup table, the clock models and combining.

### 4.9 Go library (doc 05)
```go
g, err := capt.OpenGroup(ctx, []string{"/dev/ttyACM0", "/dev/ttyACM1"}, capt.GroupOptions{
    SpillPct: 50, Owners: 0 /* 0 = default from table §1 */, Combine: true,
})
g.SetChannel(44, capt.HT20)   // applied to every member with readback verification
for f := range g.Frames() { /* f.Copies []CopyMeta{Member, RSSI, HwTs}, f.Combined bool */ }
g.Stats()                     // per member + group
```
`capt.Open` (single device) stays unchanged. Internally it is a group of one.

---

## 5. Statistics and UI

Per member:
- label, MAC, firmware, link state, `cfg_gen`, trailer on/off
- clock lock, offset, drift ppm
- `tx_owned`/s, `tx_spill`/s, `drop_spill`/s, **`drop_owned`/s** (red when > 0)
- **unique contribution %**: share of output frames that only this member delivered. This shows directly which antenna earns its place.
- mean RSSI per transmitter address (top N TAs), which shows antenna pattern effects
- host-side `rx_bad_trailer`

Group:
- unique frames/s, duplicate ratio, combined frames/s, failed combine attempts/s
- total USB bytes/s vs single-dongle ceiling (the single-TT hint from §3)

Rule of thumb shown in the UI: *`drop_owned` > 0 means the group is saturated. Tighten filters or snaplen, or add a dongle on a separate root port.*

---

## 6. Test plan (testbench, doc 06)

1. **Single-dongle regression:** a group of one produces byte-identical wire traffic (no trailer), and the same pcapng and live stream as the pre-group code.
2. **Trailer forms:** the parser handles all three forms (none, 4 B, 8 B) driven only by `flags.trailer` + `flags.truncated`, including a frame cut inside its own FCS. Wrong lengths → `rx_bad_trailer`, no crash. Old-host parser ignores the trailer.
3. **Trailer mode switching:** auto mode across 1→2→1 members under load; no misparsed frames around the switch.
4. **Ownership agreement:** feed the same recorded frame stream to N firmware instances (PC unit build). Every good frame is owned by exactly `owners` members.
5. **Spill behaviour:** synthetic ring pressure. Extra copies drop first; `drop_owned` stays 0 until the ring is full; counters add up to `drop_ring`.
6. **Make before break:** re-shard 2→3→2 under full load with simulated apply skew of 0–20 ms. No owned-frame holes (every good input frame appears in the output at least once, unless `drop_owned` counted it).
7. **Dedup correctness:** a replay containing bursts of identical ACKs to one RA. Locked: no false merges. Unlocked: false merges counted and bounded.
8. **Clock fit:** simulated ±40 ppm drift, 1 µs quantisation, 32-bit wrap. Lock within 1 s; residual < 5 µs.
9. **Combining:** random bit errors (1–16 bits, plus bursts) on 2 and 3 copies. Recovery rate, false-repair rate and time per attempt are measured.
10. **Mixed firmware:** one member without SHARD/PKT_TRAILER behaves as forward-everything with the fallback key.
11. **Member loss:** unplug one member at full load. Re-shard completes < 1.5 s; the output gap is limited to the detection time.
12. **Bench (real hardware):** 2 and 3 dongles on separate root ports vs on a single-TT hub. Measure the aggregate throughput ceiling and the measured trailer cost at snaplen 32.

---

## 7. Phases

1. **Firmware:** PKT trailer + `trailer_mode` (SET_SHARD with n=1 only is enough here). **Host:** group model with all members forwarding everything, dedup, first-copy-wins live path, per-member stats. *Gives diversity immediately.*
2. **Host:** clock alignment, per-antenna radiotap, combining broken frames.
3. **Firmware:** full SET_SHARD, spill class, new counters. **Host:** re-sharding with make-before-break. *Gives the USB headroom.*
4. Optional: EVT_DIGEST (below), GPIO sync pulse.

### Future: EVT_DIGEST (not v1)
For every dropped extra copy, the dongle could send a 12-byte digest `{u32 fcs, u32 hw_ts, i8 rssi, u8 flags, u16 len}`, batched (≤ 64 per message or every ≤ 5 ms) at the lowest priority. The host would then know which members heard a frame even when they didn't send it. That gives exact per-antenna reception statistics, and later maybe fetching the payload on demand from a short retention buffer. Deferred until the phase 3 results show it is needed.

---

## 8. Open items to verify on hardware / IDF v6

1. Does the ESP32-C5 promiscuous buffer always include the 4-byte FCS, and is `sig_len` = length including FCS (doc 03 assumes yes)? **The lean trailer depends on this** for frames that aren't truncated.
2. `rx_ctrl.timestamp` on the C5 (HE-capable rx_ctrl struct in IDF v6): width, unit and latch point (start or end of the frame). Only the consistency across dongles matters, because duplicates have the same length.
3. Are frames with a bad FCS delivered with the full payload when the FCSFAIL filter is enabled?
4. How much CPU the extra modulo and ring-fill read cost in the callback at the maximum frame rate (expected to be negligible).
5. Real aggregate throughput of 2–3 XIAO C5 USB Serial/JTAG ports on a typical laptop's root ports and on a multi-TT hub.
6. Android: several WebUSB CDC devices behind one OTG hub, power budget and permission UX.

---

## 9. Implementation status and findings (browser, 2026-11-09)

### 9.1 Reconciliation with docs/03 rev 3 (read before the firmware phases)
- **The ESP32-C5 delivers no FCS** (docs/03 rev 3, PKT flag bit6 `no_fcs`, found on hardware): the last 4 bytes the driver counts are not the FCS and the firmware strips them. Consequences for this spec:
  - §1 sharding by `fcs % n` needs another hash the dongles agree on. Proposal: the firmware computes CRC-32 over the received MPDU (what the FCS would be for good frames; ROM `esp_rom_crc32_le`, cost to be measured, §8.4), or a cheaper hash over the first bytes + length.
  - §2.2 trailer: `fcs` is not available; the trailer would carry the firmware's own hash, or only `hw_ts`.
  - §4.3 dedup key: implemented as the fallback key (CRC-32 over the captured bytes + orig_len + channel, same snaplen across the group), which is the FCS-equivalent for frames that are not truncated.
  - §4.5 combining two bad copies cannot be verified without the received FCS, so it is not implemented. Bad copies are handled as in §9.2.
- **PKT flag bit5 is taken** (`rate_unknown`, docs/03 §9); bit6 is `no_fcs`. The `trailer` flag must be **bit7**.
- §8 item 1 is answered (no FCS); item 3 (bad-FCS frames delivered?) is still open.

### 9.2 What the browser does
- **Group:** Device tab → "Dongles (receive diversity)": *Add dongle…* (USB chooser; demo: *Add demo dongle*), names per MAC (localStorage), *Remove*. Up to 4. The first connected dongle is the **primary**: the panels show it, and every config action (channel, country, filter, snaplen, driver masks, capture) is applied to all members (`app.act`); per-dongle actions (reboot, stats readback) only to the primary (`app.actOne`). A member whose readback differs from the primary's is shown *not merged* and excluded (§4.2 mismatch). A dongle with a different firmware MAJOR.MINOR is refused (connect it alone and update it first).
- **Merge** (js/diversity.js `Merger`, in the packet worker, one worker for all dongles instead of §4.8's worker per port: COBS/CRC parsing is cheap and one hop less): only active with 2+ merged members, so a single dongle is a pass-through and byte-identical to before. Key = CRC-32(captured bytes) · 4096 + orig_len, plus same channel and cap_len. Copies of a frame are accepted for 100 ms (host time). First copy wins and goes to the packet list, hx and OpenHD receivers at once; later copies add their RSSI to the stored frame (packet detail: "Received by").
- **Time:** two axes per copy. *Rough*: each dongle's host − device offset (lower envelope, may rise 200 µs/s); tolerance 10 ms, which must exceed the USB delivery skew between dongles (8 ms in tests). *Fine*: `t_ref = a·t + b` per dongle, fitted over anchors (frames heard by the reference and that dongle, not control frames), median pre-filter then least squares with outlier rejection; lock at ≥ 8 anchors and RMS < 50 µs; tolerance max(20 µs, 4·RMS + 10 µs). Copies are compared on the fine axis when both have it. Without `hw_ts` the fit uses `ts_us` (demo: lock in < 2 s, ±3 µs, drift within 1 ppm of the simulated ±23/−31 ppm). The reference is the lowest MAC; when it changes the fine models restart.
- **Bad copies** (`fcs_err`): attached to a good copy of the same channel/length/time that differs in ≤ 16 bits (then they only add RSSI, counted as "bad copies replaced"), else held 15 ms for a good copy, then emitted as they are.
- **pcapng** (§4.7): one IDB for the group (its name lists the members); with a group, a frame is written when its copy window closes, with one radiotap namespace per copy (dBm antenna signal + antenna = dongle number). Wireshark shows them as `radiotap.dbm_antsignal`/`radiotap.antenna` lists. A single dongle writes exactly the old bytes (tested).
- **Member loss:** the others keep capturing; a lost member rejoins by itself within 30 s when it comes back (same MAC). If the primary is lost, the next member becomes primary. Re-shard does not apply yet (everyone forwards everything).
- **Statistics** (§5): per dongle pkt/s, *only this one* (frames no other dongle delivered), *first* (share of first copies), clock state/ppm, dongle drops/s; group frames/s out, extra copies per frame, bad copies replaced, USB total; single-TT hint when dongles drop while the USB total stays near one dongle's maximum (§3).
- **Demo:** all demo dongles hear one simulated air (js/demo-device.js `DemoAir`), each with its own loss (4/10/16/22 %), antenna gain, crystal error (0/+23/−31/+12 ppm), time-stamp jitter and 1.2 % bit-error copies.

### 9.3 OpenHD (docs/13) with several dongles
First copy wins delivers another dongle's copy of a lost packet a few ms later, after newer packets. OpenHD sends a video frame's FEC blocks back to back, so with the ground default Q = 1 that copy usually arrives after its block was given up. While a group is active the receiver therefore holds an incomplete block up to 15 ms before giving it up (`FecStream` `holdMs`; no wait on a clean stream). The loss figure subtracts gaps that late copies fill. Test (two receivers, 15 % independent loss each, 1–9 ms USB skew): without the hold 590 of 900 blocks are lost; with it exactly the blocks neither dongle can rebuild (2). Demo, OpenHD channel: loss 7 % with one dongle, ~3 % (the demo's own floor) with three.

### 9.4 Tests
`test/js/diversity.test.mjs`: each air frame exactly once with drift, jitter and USB skew; lock and ppm; unique contribution; identical ACKs 300 µs apart not merged after lock; bad copies; stale anchors; a member leaving mid-window; radiotap namespaces (and unchanged bytes for one dongle); OpenHD FEC with late copies. Headless Chromium against the demo: 3 dongles merged and locked, packet detail, pcapng read by tshark with per-antenna RSSI, unplug + rejoin, primary loss + handover, OpenHD, back to one dongle.

### 9.5 Next
1. Firmware phase 1 per §9.1 (trailer flag bit7, `hw_ts`; hash for sharding), docs/03 bump, Go reference + vectors.
2. Go library `OpenGroup` (§4.9) with the same algorithm (port js/diversity.js; stdlib only).
3. Hardware: 2–3 XIAO C5 on separate root ports; measure the USB delivery skew between dongles (sets the rough tolerance and the OpenHD hold) and `ts_us` jitter (fine lock threshold).
