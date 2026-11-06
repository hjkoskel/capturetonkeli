# 02 – Capture filtering, snaplen and radio configuration (research, 2026-11-04)

Decisions so far: transport = USB CDC serial (Web Serial on desktop, WebUSB+CDC shim on Android, see doc 01).
This doc covers what goes over that link and how the radio is controlled.

---

## 1. What the ESP32-C5 sniffer actually gives us

ESP-IDF API: `esp_wifi_set_promiscuous(true)` + `esp_wifi_set_promiscuous_rx_cb(cb)`.

- Delivered to the callback: management frames, data frames (MPDU, A-MPDU, A-MSDU), control frames (only if enabled), and **frames with CRC errors**. For MIMO frames only the length is reported. Other PHY-level error frames are not delivered.
- Works in `WIFI_MODE_NULL` (also STA/AP/APSTA). **We use `WIFI_MODE_NULL`**: no STA, no AP, so no beacons and no probe requests are transmitted. The dongle stays purely receive-only.
- Callback buffer = `wifi_promiscuous_pkt_t` = `wifi_pkt_rx_ctrl_t rx_ctrl` (RSSI, rate/MCS, sig_len, rx_state, channel, secondary channel, timestamp, noise floor, …) + `payload[]` (802.11 header onward). `sig_len` includes the 4-byte FCS.
- **The callback runs inside the Wi-Fi driver task.** It must stay short: run the filter, copy the (truncated) frame into a ring buffer, return. A separate task drains the ring buffer to USB.

### Driver-side filters (coarse)
- `esp_wifi_set_promiscuous_filter()` – type mask (MGMT / CTRL / DATA / MISC / …). The default passes everything except MISC.
- `esp_wifi_set_promiscuous_ctrl_filter()` – control subtypes (RTS, CTS, ACK, BA, …).
- ⚠️ **Known C5 issue (IDF v5.4.2, May 2026 forum report):** on C5 the driver sometimes reports the wrong frame type (DATA/CTRL reported as MGMT). That makes the type mask unreliable. The suggested workaround is MASK_ALL plus your own Frame Control parsing.
  → **Decision: set the driver filter to MASK_ALL (+ control frames) and do all filtering in our own rule engine on the raw bytes.** Only use the driver masks as an optional "pre-filter" once the IDF bug is fixed. Re-check on the IDF version we pin.

---

## 2. User filter rules (marker bytes + mask)

The user's idea: byte patterns with value + mask at a given position. Proposed concrete design:

### Rule model
```
Term  = { offset: u16, len: u8 (1..16), value[len], mask[len], negate: bool }
Rule  = AND of up to N terms                 (N = 4)
Set   = OR of up to M rules                  (M = 16)
match(term) := for i in 0..len: (pkt[offset+i] & mask[i]) == (value[i] & mask[i])
               (out-of-range offset => term false)
```
- **Empty rule set = pass everything.**
- **Offsets are relative to the start of the 802.11 MAC header (Frame Control byte 0).** That is exactly what `payload[]` contains, and it matches the radiotap-less view in Wireshark.
- `negate` gives "everything except X" (e.g. drop beacons) without a separate deny list.
- Optional **meta terms** that are cheap and very useful. They test `rx_ctrl`, not the payload:
  - `rssi_min` (drop weak/far frames)
  - `len_min` / `len_max`
  - `fcs_ok_only` (drop CRC-error frames, default on)
- Examples (FC = byte 0..1, addr1 = 4..9, addr2 = 10..15, addr3 = 16..21):
  - Beacons only: `{off 0, val 0x80, mask 0xFC}`
  - All mgmt: `{off 0, val 0x00, mask 0x0C}`
  - Frames from one transmitter: `{off 10, val AA BB CC DD EE FF, mask FF×6}`
  - Drop beacons: same as "beacons only" with `negate=1`
  - EAPOL in data frames: needs LLC at variable offset (QoS adds 2 bytes) → offer two rules, or add later an "auto header length" offset base (`base = MAC_HDR | PAYLOAD`).

### Cost
Fixed-size tables, no allocation, worst case M×N×16 byte compares per frame. Trivial at 240 MHz compared with the USB budget. Evaluate in the callback **before** copying, so rejected frames cost nothing on USB.

---

## 3. Per-packet byte limit (snaplen)

- `snaplen` = max bytes of 802.11 frame sent per packet; **`0` = no limit** (as requested).
- Truncation happens after filtering. The packet header always carries the **original length** (`orig_len`) so the browser can write a correct pcap (`incl_len` / `orig_len`).
- Practical presets for the UI: 0 (full), 24/32 (MAC header only), 64, 128, 256.
- Why this matters: the USB Full-Speed link is around 0.5–1 MB/s in practice (doc 01). A busy 5 GHz channel easily exceeds that. Filter + snaplen are the main tools for keeping latency low.

---

## 4. Radio configuration

### Bands and channels
- ESP32-C5 radio: 2412–2484 MHz and 5180–5885 MHz → 2.4 GHz ch 1–14, 5 GHz ch 36–177 (whatever the country table allows).
- **Single radio, one band at a time.** The C5 cannot be on 2.4 and 5 GHz simultaneously, so "capture both bands" means channel hopping.
- Band selection: `esp_wifi_set_band_mode(WIFI_BAND_MODE_2G_ONLY | WIFI_BAND_MODE_5G_ONLY | WIFI_BAND_MODE_AUTO)`. Must be called after `esp_wifi_start()`. A band change with no channel set defaults to ch 1 / ch 36.
  → Firmware: when the user picks a channel, set the band mode matching that channel first, then `esp_wifi_set_channel()`. Avoid relying on AUTO.
- `esp_wifi_set_channel(primary, second)`: call after `esp_wifi_start()`. On 5 GHz the secondary channel is derived automatically from the primary (a manual value is ignored). Not stored in NVS, which is good for us (no flash wear on hopping).
- Bandwidth: `esp_wifi_set_bandwidths()`. HT40 only for 11n; with 11ax/11ac enabled, only BW20. Expose **HT20 (default) / HT40±** in the UI. Note that a 20 MHz receiver still decodes HT40/VHT80 frames' legacy parts and many control frames, but not the wide-channel data PPDUs.

### Channel hopping (firmware feature)
- Command: list of channels + dwell time (ms). Firmware hops in a timer task; each packet header carries the channel it was received on.
- Fast hopping inside one band is cheaper than crossing bands (band switch = PHY reconfig). Order the hop list band-by-band. Measure the switch time on hardware.

### Command set (host → dongle)
| Cmd | Args |
|---|---|
| `SET_CHANNEL` | channel, bw (20 / 40+ / 40−) |
| `SET_HOP` | n, channels[], dwell_ms (n=0 stops hopping) |
| `SET_COUNTRY` | 2–3 char code (see §5) |
| `SET_FILTER` | full rule set (atomic replace, double-buffered in firmware) |
| `SET_SNAPLEN` | u16 (0 = unlimited) |
| `SET_DRIVER_MASK` | type mask, ctrl mask (optional pre-filter) |
| `START` / `STOP` | capture on/off |
| `GET_STATUS` | → channel, counters: rx, passed, dropped_ringbuf, dropped_usb, fcs_err |

Every command gets an ACK/NAK with the ESP error code. That way, for example, "channel 13 rejected by country table" is visible in the UI.

---

## 5. Does regulatory domain matter for passive monitoring?

### Legally / spectrum-wise: essentially no
- Regulatory domains (channel lists, max TX power, DFS/radar detection, "passive scan only"/NO-IR flags) **all constrain transmission**. DFS rules require radar detection before an AP *transmits* on those channels. "Passive scan" requirements mean a device may only listen, not probe, until it hears a beacon.
- A receive-only device emits nothing, so there is nothing to regulate on the RF side. Listening on DFS channels, ch 12–13 or ch 14 is fine RF-wise as long as the dongle never transmits. With `WIFI_MODE_NULL` + promiscuous it doesn't.
- ⚠️ The separate legal issue is **content**, not spectrum. Capturing and using other people's traffic can be illegal under privacy and communications-secrecy laws (in Finland, for example, the communications secrecy provisions of the Criminal Code and the Information Society Code). The tool is fine; users monitor only networks they own or are authorized to test. Add a short notice to the UI. (Not legal advice.)

### Technically in ESP-IDF: **yes, it matters**
In ESP-IDF the country setting is also the **channel whitelist** the driver accepts:
- Default country is `"01"` (world safe mode) with 802.11d enabled. The 2.4 GHz range is channels 1–11 (`schan=1, nchan=11`). So **channels 12–13 (and 14) are not usable out of the box**, and the 5 GHz set is likewise limited to whatever "01" allows.
- `esp_wifi_set_country_code(cc, ieee80211d_enabled)` validates per-country rules. The supported list is limited: "01", AT, AU, BE, BG, BR, CA, CH, CN, CY, CZ, DE, DK, EE, ES, **FI**, FR, GB, GR, HK, HR, HU, IE, IN, IS, IT, JP, KR, LI, LT, LU, LV, MT, MX, NL, NO, NZ, PL, PT, RO, SE, SI, SK, TW, US.
- The legacy `esp_wifi_set_country(wifi_country_t*)` allows a manual `schan/nchan` but is discouraged because it doesn't validate rules. It is the way to get a custom range.
- Country config **is written to flash** on every call → set it rarely (only on user change), never per hop.
- 802.11d: in NULL mode there is no associated AP, but **pass `ieee80211d_enabled=false`** anyway so nothing changes the table behind our back.

### Recommendation
1. UI has a **"Regulatory domain / country"** selector. Its job is to "unlock channels", not to "comply". Default = user's country (FI → EU channel set incl. 12–13 and the EU 5 GHz set).
2. Firmware validates channels against the active table and reports a NAK with a clear reason.
3. Offer an advanced **"Receive-only: all channels"** option that uses `esp_wifi_set_country()` with the widest manual range the driver accepts (e.g. `schan=1, nchan=14`) plus a JP or US profile for 5 GHz extremes. **Verify on hardware** which 2.4 GHz and 5 GHz channels the C5 driver actually tunes to under each setting. Channel 14 (Japan, 11b only) and the 5.8–5.9 GHz channels (US UNII-4, ch 169–177) are the uncertain ones.
4. TX power settings are irrelevant (nothing is transmitted). Don't expose them.

---

## 6. USB frame format (SUPERSEDED – see doc 03 `research/03-usb-link-protocol.md`)

```
SLIP- or COBS-framed, then:
u8   type        (0x01 = packet, 0x02 = status, 0x03 = ack/nak, 0x04 = log)
--- packet ---
u32  ts_us       (rx_ctrl timestamp or esp_timer, µs)
u8   channel
u8   flags       (bit0 fcs_err, bit1 truncated, bit2 bw40, bit3 5GHz …)
i8   rssi
i8   noise_floor
u8   rate/mcs
u16  orig_len    (802.11 length incl. FCS)
u16  cap_len
u8   data[cap_len]
```
- 13 bytes of overhead per packet. The browser converts it to pcap/pcapng with **radiotap** (channel freq, RSSI, rate, flags) so Wireshark opens files directly. LINKTYPE_IEEE802_11_RADIOTAP = 127.
- COBS preferred: bounded overhead (≤1/254) and trivial to resync after a dropped byte.
- Drop policy: if the ring buffer is full, drop the **new** packet and increment `dropped_ringbuf`. Never block the Wi-Fi task.

---

## 7. Open items to test on hardware

*M3/M4 results (2026-11-04, IDF v6.0.2, XIAO ESP32-C5):*
- **1. Channel map** → `docs/measurements/channel-map-v6.0.2.json` (01: 2.4 1–11, 5 GHz 36–64 only; FI/DE: 1–13, 36–64, 100–140, 149–173; US: 1–11, 36–64, 100–144, 149–165, 173–177 (169 rejected); JP: 1–14, 36–64, 100–144; CN: 1–13, 36–64, 149–165; GB: 1–13, 36–64, 100–144, 149–165). The manual range 1..14 (legacy `esp_wifi_set_country`) unlocks ch 14 under every code and keeps the code's 5 GHz set. `ctp/regdom` mirrors these values.
- **2. Frame-type misreport:** not present in v6.0.2. With the diagnostics variant (`make fw-diag flash-diag`), ~3,000 good-FCS frames on ch 13 gave a perfectly diagonal driver-type × FC-type matrix; mixed types occur only for FCS-error frames, which the driver files under DATA. The MASK_ALL + own FC parsing decision stays (it costs nothing).
- **3. Channel switch latency:** SET_CHANNEL round trip 2.0 ms within a band, 3.0 ms across bands (incl. ~1 ms USB RTT).
- **5. HT40:** not available in NULL mode (doc 10 §2, doc 08 A5, postponed).
- Additional: the C5 driver delivers frames without FCS (doc 03 §9 rev 3, `no_fcs`).

1. Exact channel list accepted per country code ("01", FI, US, JP) on 2.4 and 5 GHz; does manual `esp_wifi_set_country` unlock ch 14 / ch 169–177?
2. Is the C5 frame-type misreport still present in our pinned IDF version?
3. Channel switch latency within a band vs across bands.
4. Sniffer callback rate at 100% channel load, plus how many frames the ring buffer drops before USB becomes the bottleneck. *(M4: callback cost avg 3.6 µs/frame (850 cycles), max ≈ 35 µs; USB saturates at ≈ 960 KB/s, see doc 01.)*
5. Does HT40 setting in NULL mode actually widen the RX (capture HT40 data PPDUs)?

## Sources
- ESP-IDF Wi-Fi Modes – Sniffer mode (ESP32-C5): https://docs.espressif.com/projects/esp-idf/en/v6.0.3/esp32c5/api-guides/wifi-driver/wifi-modes.html
- ESP-IDF Wi-Fi API reference (set_channel, set_country(_code), set_band_mode, set_bandwidths, promiscuous filters): https://docs.espressif.com/projects/esp-idf/en/latest/esp32/api-reference/network/esp_wifi.html
- esp_wifi_set_band_mode docs (esp-idf-sys mirror): https://esp-rs.github.io/esp-idf-hal/esp_idf_sys/fn.esp_wifi_set_band_mode.html
- Espressif ESP32-C5 workshop, Lecture 1 (single band at a time): https://developer.espressif.com/workshops/esp-idf-with-esp32-c5/lecture-1/
- ESP32-C5 datasheet (frequency ranges): https://documentation.espressif.com/esp32-c5_datasheet_en.html
- ESP-FAQ Wi-Fi (passive scan, DFS on 5 GHz): https://docs.espressif.com/projects/espressif-esp-faq/en/latest/software-framework/wifi.html
- ESP32 forum – supported country codes: https://www.esp32.com/viewtopic.php?t=38645
- ESP32 forum – C5 promiscuous wrong frame type: https://esp32.com/viewtopic.php?p=154671
- netmodule WLAN regulatory DB (DFS / PASSIVE-SCAN / NO-IR flag meanings): https://share.netmodule.com/public/system-software/archive/4.0/4.0.0.100/WLAN_REGULATORY_DATABASE.pdf
