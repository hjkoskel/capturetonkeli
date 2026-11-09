# 12 – FPV viewer for hx-esp32-cam-fpv (web UI)

Status: implemented 2026-11-06 (js/hxfec.js, js/hxfpv.js, js/ui/fpv.js, js/hxdemo.js; tests test/js/hxfpv.test.mjs).

## 1. Purpose
Show the live video of an [hx-esp32-cam-fpv](https://github.com/RomanLut/hx-esp32-cam-fpv) air unit (ESP32-CAM / ESP32-S3 Sense, firmware 0.3, packet version 2) in the browser, using the capture dongle as a passive receiver. The page **never transmits**: the native ground station (GS) still links with the air unit (Connect/Config packets go GS → air). The page shows the same air → ground stream, so it works as a second screen or a field monitor. Several air units can be in range; they are told apart by their air device ID.

No firmware or protocol (CTP) change is needed: the dongle captures full frames and the page decodes them. Everything runs in the existing worker (docs/04 §2).

## 2. Over-the-air format (as implemented by hx-esp32-cam-fpv `components/common`)
All little-endian, packed.

| Layer | Bytes | Content |
|---|---|---|
| 802.11 header | 24 | data frame (`fc[0] & 0x0c == 0x08`), addr1 = broadcast, **addr2 = addr3 = 11:22:33:44:55:66** (GS → air uses 66:55:44:33:22:11) |
| Packet_Header | 12 | `u8 version (2)`, `u8 signature (56)`, `u16 fromDeviceId` (air ID), `u16 toDeviceId` (GS ID, 0 = not linked), `u16 size`, `u24 block_index`, `u8 packet_index` |
| FEC slot | mtu (default 1464) | packets 0…k-1: data; k…n-1: FEC (default k = 6, n = 12) |

Each data slot holds exactly one **Air2Ground** packet, zero-padded (the air unit flushes the FEC encoder after every packet):

`Air2Ground_Header` (12 B): `u8 type (0 video, 1 telemetry, 2 OSD, 3 config)`, `u32 size` (whole packet), `u8 pong`, `u8 version`, `u8 crc`, `u16 airDeviceId`, `u16 gsDeviceId`.
`crc` = CRC-8 (poly 0x07, init 0) over the type's header struct with the crc byte set to 0: video 18 B, OSD 1244 B (whole packet), config 53 B, telemetry 12 B.

- **Video** (18 B header): `u8 resolution`, `u8 part_index:7 | last_part:1`, `u32 frame_index`, then JPEG bytes. A frame is the concatenation of parts 0…last.
- **OSD**: `AirStats` (32 B at offset 12: air RSSI, noise floor, camera fps, quality, resolution, channel, rate, temperature, SD card, `fec_codec_k`, …) + OSD character buffer (1200 B, not shown).
- **Config**: `CameraConfig` (30 B at 12) + `DataChannelConfig` (11 B at 42: power, rate, channel, `fec_codec_k`, `fec_codec_n`, `fec_codec_mtu`).

## 3. FEC
Systematic Reed-Solomon erasure code over GF(2^8), polynomial x^8+x^4+x^3+x^2+1, from Rizzo's Vandermonde construction (as in zfec): V has row 0 = e0 and row i+1 = [α^(i·j)]; the encoding matrix is V · inv(V top k×k). Row r does not depend on n. Any k packets of a block recover the k data packets. js/hxfec.js is written from the math; tests compare it byte-for-byte with vectors generated from the hx-esp32-cam-fpv C code (test/js/fixtures/hxfpv-fec.json).

## 4. Receiver (js/hxfpv.js, mirrors the native GS)
Per air unit:
- k starts at 6 and follows `fec_codec_k` from OSD and config packets (changing k restarts decoding). Slot size = frame payload − 12 (a change restarts decoding).
- Blocks: data packets are passed on in order as soon as they arrive (low latency); when a block has k packets, missing data packets are recovered by FEC. A block that is still incomplete when more than 3 newer blocks are waiting is given up (lost). Packets of finished blocks are dropped (FEC not needed / too late). A block index more than 100 behind means the air unit restarted.
- Frames: same rules as the GS: parts must arrive in order for the current `frame_index`; a newer index abandons the current frame (lost). Complete frames must start with FF D8.
- Statistics: packets (data/FEC/unneeded/duplicates/late), blocks (complete/repaired/lost), frames (clean/via FEC/lost), CRC and size errors, restarts, dongle RSSI; OSD and config contents.
- Frames of the shown unit (chosen by the user, or "auto" = most frames in the last 3 s) are sent to the page, at most 2 in flight; extra frames are counted as "not shown" instead of queueing.

## 5. Dongle setup (FPV tab)
- **Channel**: hx default is 7; the field follows the channel from config packets. "Tune dongle" sets it (HT20).
- **FPV capture filter** (one rule, App. A): `fc & 0x0c == 0x08`, addr2 = 11:22:33:44:55:66 (offset 10), `02 38` at offset 24 (version, signature), and optionally `packet_index & 0xf8 == 0` (offset 35) to keep packets 0…7 of each block. The dongle's USB link carries ~960 KB/s (docs/10); at k = 6, n = 12 all 12 packets need twice the video bit rate, packets 0…7 need 1.33×, which still repairs 2 losses per block. Options: 0…7 (default), 0…15, all. Snaplen is set to full frames. The previous user filter is saved and can be restored.
- Warnings: snaplen cutting frames, dongle `drop_ring` increasing (USB full), wrong packet version, unit silent, decoder errors.

## 6. UI (js/ui/fpv.js)
Tab "FPV", system "hx-esp32-cam-fpv" (the other system is OpenHD, docs/13): setup, video (canvas, overlay with unit, frame number, size, fps, FEC marker), Large view (video replaces the packet list on desktop), Fullscreen, Save JPEG, Pause, picture flip (none / rotate 180° / mirror / vertical for a camera mounted upside down; CSS transform done by the compositor, no per-frame cost, OSD stays upright; Save JPEG then re-encodes the flipped last frame, otherwise it saves the camera's JPEG untouched), Reset decoder, air unit table (ID, GS ID, packets/s, RSSI, frames, last seen, selection), link statistics and air unit status. Frames are drawn with `createImageBitmap`; JPEG decode errors are counted, not fatal. Frames are only sent from the worker while the tab, the large view or fullscreen is visible.

## 7. Demo mode
js/hxdemo.js simulates air unit 0x1234 on channel 7: 15 fps animated test picture encoded to JPEG with OffscreenCanvas, OSD packets every 200 ms, k = 6, n = 12, 3 % random packet loss.

## 8. Not done yet
- Recording video (AVI/MJPEG like the GS) – today: Save JPEG and pcapng recording of the raw packets.
- OSD character overlay (font rendering), telemetry/MAVLink output, GS → air packets, other packet versions.
- Hardware test with a real air unit (only demo mode and the C-generated vectors so far).
