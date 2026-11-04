# 08 – Open research items before implementation (2026-11-04)

What still needs desk research or a short hardware spike before (or early in) implementation.
Ordered by how much a wrong assumption would cost.
Items already scheduled in doc 07 milestones (USB throughput, DTR/RTS, channel map, frame-type bug) are only referenced here.

---

## A. Findings from a first pass (already change assumptions)

### A1. ESP32-C5 `rx_ctrl` metadata differs from older chips → PKT fields must be re-mapped ⚠️ high
- Older chips (ESP32/S3/C3) expose `sig_mode`, `mcs`, `cwb`, `aggregation`, `ampdu_cnt`, `noise_floor`, 32-bit `timestamp`.
- The C5 (Wi-Fi 6 MAC) exposes a different set: `rssi`, `rate` (valid **only for non-HT**), `sig_len` (incl. FCS), `rx_state`, `dump_len`, `he_sigb_len`, `cur_bb_format`, `channel`, `secondary_channel`, `noise_floor`, `is_group`, `rxend_state`, `rxmatch0..3`, `timestamp` (source: esp-radio C5 `RxControlInfo`, which mirrors the IDF struct).
- Consequences:
  - doc 03 PKT `sig_mode` / `rate` / MCS fields and the radiotap mapping (doc 04 §10, doc 05 §7) were modelled on the old layout;
  - HT/HE MCS may **not** be available at all on C5;
  - `cur_bb_format` probably replaces `sig_mode`.
- **Research:**
  - read `esp_wifi_he_types.h` / `esp_wifi_types_native.h` for target esp32c5 in IDF v6.0.2;
  - the meaning of `cur_bb_format` values;
  - `dump_len` vs `sig_len` (does the driver truncate payloads, e.g. for HE/MU or long A-MSDU?);
  - timestamp width and clock source;
  - **what `rxmatch0..3` are.** If they are hardware address-match flags, they could become a free pre-filter.
- **Action:** freeze the PKT metadata layout (doc 03 §9) only after this. Proposed change: replace `rate`+`sig_mode` with `bb_format` + `rate_or_mcs` (+ flag "mcs unknown"). It is still v1, since nothing has shipped.

### A2. Android: kernel CDC driver may hold the interface → Chrome added forced detach ✅ mostly resolved
- Some Android devices ship with kernel drivers for USB CDC devices even though the Android SDK has no API for them.
- Chromium added a detach allowlist that includes `cdc_acm` with ACM subclass, so WebUSB can take over the interface on Android. An early version of that change was reverted once and later relanded (the allowlist file is in current Chromium source).
- **Research:** the first Chrome stable version containing it, and a test on 2–3 phones (Pixel, Samsung, one budget phone) during M5.

### A3. Web Serial works inside Dedicated Workers ✅ resolved (spec-wise)
- `WorkerNavigator.serial` is part of the spec and available in Dedicated Web Workers. That allows the doc 04 design "transport + parser in the Worker". Still feature-detect per browser: it is marked experimental, and Firefox's implementation is new.
- **Research:** WebUSB inside workers for the Android path (expected yes; verify).

### A4. XIAO ESP32-C5 ships with a 2.4 GHz-only antenna ⚠️ medium
- The kit includes a 2.4 GHz FPC antenna on the U.FL connector. For real 5 GHz capture a **dual-band U.FL antenna** is needed.
- **Research/spike:** pick 2–3 dual-band U.FL antennas (FPC and small rod) and compare RSSI on ch 36/149 vs the stock antenna. Also include the antenna in the enclosure design (§C1).

### A5. HT40 capture on the C5 ⏸ postponed (decided 2026-11-04)
- M3 finding: in `WIFI_MODE_NULL`, IDF v6.0.2 rejects every STA bandwidth/protocol call (`esp_wifi_set_bandwidth`, `esp_wifi_set_bandwidths`, `esp_wifi_set_protocols` → `ESP_ERR_INVALID_ARG`). v1 firmware therefore clears `caps.HT40`.
- HT40 is believed to be achievable (e.g. idle `WIFI_MODE_STA` with 11n-only protocols, or another API path). Postponed: revisit after M5. Check that nothing is transmitted in the chosen mode and what PPDU formats are lost while in HT40.

---

## B. Desk research still to do (hours each)

| # | Topic | Why it matters | Output |
|---|---|---|---|
| B1 ✅ | **Resolved → doc 10 (IDF v6 API audit).** Remaining unknowns (legacy `esp_wifi_set_country` presence, bandwidth in NULL mode) are caught by the API smoke build in M0. Original item: **IDF v6 API changes** for: `esp_wifi_set_country` (legacy manual range; still present in v6?), `esp_wifi_set_country_code`, `esp_wifi_set_bandwidths` on C5, `esp_wifi_set_band_mode` in NULL mode, promiscuous on 5 GHz, `usb_serial_jtag` driver API, console config names | The "manual country range" feature (doc 02 §5) depends on the legacy API; v6 is a major version | Short API table pinned to v6.0.2 in doc 07 |
| B2 (partly resolved, doc 10 §3) | Host-side USB-JTAG reset sequence is the authoritative path; firmware must never touch GPIO13/14 / USB PHY. Still open: optional firmware-side forced download boot. **Reboot into download mode from software** on C5 (REBOOT mode 1): which register / ROM call (LP_AON force-download-boot) | Needed for future web flashing and `ctpbench reboot --download` | Code snippet + verified on hardware in M3 |
| B3 | **Boot-time output on USJ**: ROM bootloader prints over USB-Serial-JTAG at reset; 2nd-stage bootloader and app logs must be silenced (`CONFIG_BOOTLOADER_LOG_LEVEL_NONE`, console none) | Host sees garbage after every reset. COBS resync handles it, but the conformance test must allow it. **Do not burn eFuses** to silence ROM logs (irreversible) | sdkconfig.defaults entries + note in doc 03 §3 |
| B4 | **USJ USB descriptors**: is the serial-number string unique per chip (MAC-derived)? What product string appears in the Web Serial chooser? | Multi-dongle setups; users may pick another Espressif board in the chooser (same VID/PID) → HELLO must reject non-CTP devices gracefully | UI text + doc 04 §4 tweak |
| B5 | **Browser storage on Android & Firefox**: `showSaveFilePicker` availability (Android Chrome? Firefox no), OPFS quota on Android, max file size | Long captures on a phone | Decide recording backend defaults (doc 04 §10) |
| B6 | **Background behaviour on Android Chrome**: tab freezing, Screen Wake Lock API to keep capture alive, OTG behaviour when screen locks | Phones will stop capture otherwise (the device watchdog already makes this safe, but the UX should keep it running) | Wake-lock toggle in UI spec |
| B7 | **Radiotap for Wi-Fi 6 frames**: which radiotap fields (HE, HE-MU) Wireshark expects given what C5 provides (A1) | Correct Wireshark display | pcapng writer field list |
| B8 | **PHY calibration on channel/band switch** (`CONFIG_ESP_PHY_CALIBRATION_MODE`, calibration data in NVS) | Channel-switch latency (target: fast manual sweeping) | Config choice; measured in M4 |
| B9 | **Wi-Fi RX buffer tuning for sniffer load** (static/dynamic RX buffers, `CONFIG_ESP_WIFI_*_RX_BUFFER_NUM`, IRAM options) on C5 | Frames lost *inside the driver* are invisible to our counters | Starting values for M4 |
| B10 | **Disable unused radios/stacks** (BLE, 802.15.4) to free RAM and avoid coexistence time-slicing | More RAM for the ring, no RX gaps | sdkconfig.defaults |

---

## C. Hardware spikes worth doing *before* M1 (1–2 days total)

These answer "can the C5 do the job at all"; everything else is engineering. Each is a throw-away ESP-IDF example, not production code.

| # | Spike | Pass criterion |
|---|---|---|
| C1 | **Sniffer capability**: promiscuous in NULL mode on a busy 2.4 GHz and 5 GHz channel; log per-format counts (11b/g, HT20/40, VHT, HE SU/MU), FCS presence at the end of the payload, `dump_len` vs `sig_len`, frames/s | Captures beacons, data and control at realistic rates; we know which PPDU formats we will *not* see (state that in UI docs) |
| C2 | **USB raw throughput** with USJ (= M3 early) | ≥ 0.5 MB/s sustained; else re-plan snaplen defaults |
| C3 | **Android OTG smoke test**: XIAO on 2 phones, Chrome WebUSB opens it (A2), power OK | Data both ways from a test page |
| C4 | **Antenna**: stock vs dual-band U.FL on 5 GHz (A4) | Choose the antenna for the BOM |

If C1 shows that HE (Wi-Fi 6) data frames are only partially visible, that is a documented limitation, not a blocker. If C2 is far below 0.5 MB/s, the PKT header compaction and filter-first defaults matter more.

---

## D. Practical / product items (no technical risk, but decide early)

| # | Item | Note |
|---|---|---|
| D1 | **Form factor**: XIAO has a USB-C *receptacle*, so a "dongle" needs a short C-to-C cable or a male-to-female adapter, an enclosure, and an antenna mount | 3D-printed case with U.FL antenna pigtail |
| D2 | **Licences** (needed before the GitHub repo goes public): choose the project licence. Compatible pieces: ESP-IDF Apache-2.0, Unity MIT; Espressif Wi-Fi blobs are redistributable binaries | Affects what can be bundled in releases |
| D3 ✅ | **Web UI hosting & version pairing: resolved → doc 11.** GitHub Pages from repo root, always latest, no per-version URLs; old dongles are updated via the mismatch prompt | doc 03 §11, doc 04 §4.1 |
| D4 | **Legal notice wording** (doc 02 §5, doc 04 §15) | Keep short; not legal advice |

---

## Sources
- esp-radio (Espressif Rust HAL) ESP32-C5 `RxControlInfo` fields: https://docs.espressif.com/projects/rust/esp-radio/1.0.0-beta.0/esp32c5/esp_radio/wifi/struct.RxControlInfo.html
- Older-chip `wifi_pkt_rx_ctrl_t` layout (IDF): https://sourcevu.sysprogs.com/espressif/esp-idf/symbols/wifi_pkt_rx_ctrl_t
- Chromium "Allow detaching USB drivers on Android" (revert record): https://chromium.googlesource.com/chromium/src/+/17e2068198a09bf61505b5e87f44c9e87e4ff746
- Chromium USB interface detach allowlist: https://chromium.googlesource.com/chromium/src.git/+/lkgr-android-internal/services/device/usb/usb_interface_detach_allowlist.cc
- MDN WorkerNavigator.serial: https://developer.mozilla.org/docs/Web/API/WorkerNavigator/serial
- XIAO ESP32-C5 kit contents (Switch Science): https://www.switch-science.com/collections/seeed/products/10978
