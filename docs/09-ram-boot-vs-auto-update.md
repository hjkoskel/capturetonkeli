# 09 – Boot firmware from RAM vs. host-driven auto-update (decision memo, 2026-11-04)

**Question:** could a small resident bootloader let the host upload the real firmware into the ESP32-C5's RAM on every USB connect, so flash is never written and the UI and firmware can never mismatch?

**Decision (2026-11-04): accepted with a user-prompted, semver-based policy.**
- Firmware uses semantic versioning. A mismatch is **significant only when MAJOR or MINOR differ**.
- On mismatch the web UI prompts (update / continue unsupported / cancel). The CLI fails with exit code 9 and offers `fw update` or `--allow-fw-mismatch`.
- Flashing a **bare ESP32-C5** is supported in both the web UI and the CLI.
- Normative text: doc 03 §11 (rules), doc 04 §4.1 + §11.1 (UI), doc 05 §7b (Go `fwver`/`romflash`), doc 06 Firmware commands, doc 07 §4/M3/M7.
- The "automatic update without asking" variant in §3.1 below is superseded by the prompt.
- **Update 2026-11-04 (doc 11):** users always run the latest page from GitHub Pages; the page's own `fw/manifest.json` is the expected firmware, so the typical prompt is "dongle older → Update (default)".

**Short answer:** not for this firmware, at least not robustly. The same two goals (no version mismatch, no meaningful flash wear) are met better by **host-driven auto-update with differential flashing**, plus **zero flash writes at runtime**.

---

## 1. What the chip can do

- **The ROM bootloader already loads code into RAM.** The C5's mask-ROM download mode (entered over USB-Serial-JTAG with the usual DTR/RTS sequence) supports loading segments into RAM and jumping to them. That is esptool's `load-ram` command. No custom bootloader and no flash are involved.
  - Limitation: the image must contain only IRAM/DRAM segments; flash-mapped segments will not load.
- **ESP-IDF has a matching build type:** `CONFIG_APP_BUILD_TYPE_RAM` ("Build app runs entirely in RAM", marked EXPERIMENTAL). Its own help text says that IRAM/DRAM are very limited, so complex applications can't be built this way.

## 2. Why it does not fit our firmware

| Issue | Detail |
|---|---|
| **Size** | Our app = FreeRTOS + Wi-Fi driver/PHY blobs + USB + our core. A Wi-Fi IDF app is normally several hundred KB of code that runs from flash through the cache. The C5 has **384 KB** HP SRAM in total, which must also hold the Wi-Fi RX buffers, our 96 KB ring and the stacks. It is very unlikely to fit; confirm with `idf.py size` on the M4 build. |
| **PSRAM route** | The XIAO C5 has 8 MB PSRAM, and IDF can execute code from PSRAM (`CONFIG_SPIRAM_XIP_FROM_PSRAM`), but only by **copying from flash at startup**. A RAM-loaded loader that initialises PSRAM, receives the app over USB, maps it with the MMU and jumps to IDF's startup code is theoretically possible. It would mean re-implementing and maintaining parts of IDF startup, cache/MMU setup and image loading for every IDF version. That contradicts the "fool-proof" goal. |
| **Boot time per connect** | Every plug-in, Android permission re-grant or host reconnect would re-upload ~1 MB through the ROM loader before capture can start (seconds). |
| **Recovery semantics** | A dongle that does nothing until a host uploads code is fine. But the protocol's "reconnect → re-HELLO → re-push" (doc 03 §7) would become "reconnect → re-upload firmware", and the USB re-enumeration between ROM mode and app mode adds another failure path on Android. |

**Verdict:** keep `load-ram` as a *development tool* for small test programs (e.g. the M3 USB throughput spike, if it fits). Do not use it for the product firmware.

## 3. Recommended design: auto-update + zero runtime flash writes

### 3.1 Version pairing (no mismatch)
- The web UI (and `ctpbench`) **bundles the exact firmware image** it was built with: `fw/ctk-<version>.bin` + a manifest `{version, sha256, protocol_ver, idf}`.
- HELLO already returns `fw_version`. Add a **`fw_build_id`** (first 8 bytes of the image SHA-256, embedded at build time) to `DeviceInfo` (doc 03 §6, appended field, forward-compatible).
- *(Superseded by the decision at the top: only a MAJOR.MINOR difference prompts; a differing build_id alone is shown as info.)* On connect, if versions differ, the UI shows "Firmware update required (1.2.0 → 1.3.0)" and offers:
  1. REBOOT mode 1 (download mode), or the DTR/RTS reset sequence if the app is unresponsive or bricked;
  2. flash through the **ROM download protocol**;
  3. reset;
  4. HELLO again.
- Because the UI always carries its matching firmware, **a UI version and a firmware version can never run together unmatched**. Old firmware simply gets upgraded (or downgraded) to what the page expects.

### 3.2 Minimal flash wear
- **Differential flashing:** compare 4 KB sectors (MD5 per sector via the ROM's flash-MD5 command) and erase/write **only changed sectors**. esptool's "fast reflashing" does the same.
- IDF's `CONFIG_APP_BUILD_MINIMIZE_BINARY_CHANGES` lays out the binary so that changes stay localised, which reduces changed sectors further.
- Writes happen **only when the UI version changes**, i.e. on releases, not per connect. Typical SPI NOR flash is rated around 100 000 erase cycles per sector, so even daily development reflashing stays far from the limit. For end users the wear is negligible.

### 3.3 Zero flash writes during normal operation
Currently the design writes flash in one place: the country code (doc 02 §5, doc 07 §4).
- Initialise Wi-Fi with **NVS disabled** (`wifi_init_config_t.nvs_enable = 0`, or `esp_wifi_set_storage(WIFI_STORAGE_RAM)`). Wi-Fi config then lives in RAM only.
- **Do not persist the country at all.** The host re-pushes the full config on every HELLO anyway (doc 03 §7). Boot default = "01" in RAM.
- No other NVS use. If the NVS partition isn't needed by anything, it can be omitted from the partition table entirely.
- Result: **flash is written only by explicit firmware updates.**
- To verify in M4: that with NVS disabled `esp_wifi_set_country_code` really stores nothing (watch NVS stats / flash write counter). The PHY calibration data mode must also be RAM-only (`CONFIG_ESP_PHY_CALIBRATION_AND_DATA_STORAGE` off, full calibration at boot) and boot time measured.

### 3.4 Cost of this choice
- The browser needs a **vanilla-JS implementation of the ESP ROM download protocol** (SLIP framing; SYNC, FLASH_BEGIN/DATA/END, SPI_FLASH_MD5, …). That is a few hundred lines; using the ROM loader directly (no stub) avoids bundling third-party stub binaries and their licence question. The Go library gets the same in a `ctp/romflash` package, which `ctpbench flash` also uses (doc 06 previously excluded flashing).
- Web UI spec (doc 04 §11) changes from "flashing out of scope for v1" to **"auto-update in v1"**.
- Firmware gets `fw_build_id` in HELLO.

## 4. Changes to other docs if accepted
| Doc | Change |
|---|---|
| 03 | HELLO response += `u8 fw_build_id[8]` (appended) |
| 04 | §11: auto-update flow, firmware bundle, ROM-protocol flasher module (`js/romflash.js`); §17 coverage row |
| 05 | new package `ctp/romflash` (ROM protocol, differential flash, md5 per sector) |
| 06 | `ctpbench flash [--image FILE] [--diff]` and `ensure-firmware` (flash only if build id differs); remove "flashing" from non-goals |
| 07 | Fail-safe/boot: no NVS, country RAM-only, PHY cal in RAM; risk table: NVS wear row removed; M3 adds ROM-protocol flashing test |

## Sources
- esptool `load_ram` (IRAM/DRAM-only images): https://codex.ac6.fr/Zephyr/xref/hal_espressif-latest/tools/esptool_py/docs/en/esptool/advanced-commands.rst
- ESP-IDF Kconfig (C5): `CONFIG_APP_BUILD_TYPE_RAM`, `APP_BUILD_MINIMIZE_BINARY_CHANGES`: https://docs.espressif.com/projects/esp-idf/en/release-v6.1/esp32c5/api-reference/kconfig-reference.html
- ESP-IDF external RAM, XiP from PSRAM (C5): https://docs.espressif.com/projects/esp-idf/en/v6.0/esp32c5/api-guides/external-ram.html
- esptool fast reflashing (C5): https://docs.espressif.com/projects/esptool/en/latest/esp32c5/esptool/basic-commands.html
- Wi-Fi storage RAM vs flash: https://esp32.com/viewtopic.php?p=72935
