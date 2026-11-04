# 10 – ESP-IDF v6 API audit for the firmware (2026-11-04)

Pinned IDF: **v6.0.2** (doc 07). This doc lists every ESP-IDF API, type and Kconfig option the firmware plan relies on. Each is checked against the v5.5 → v6.0 migration guide and the v6.0 release notes, with the action needed.
**Goal: no surprises when implementation starts.** Anything not verifiable from documentation is caught by the API smoke build (§5) in M0, before any feature code is written.

Note: the ESP-IDF mirror listing shows **v6.0.3** (bugfix, 2026-09) and **v6.1** (2026-08) also released. Recommendation: stay on the v6.0.x line. Consider bumping the pin from v6.0.2 to v6.0.3 after reading its release notes; it's a bugfix release, so the audit below applies unchanged. Decide in M0.

---

## 1. Project-wide v6.0 changes that affect us

| Change in v6.0 | Effect on our plan | Action |
|---|---|---|
| **Compiler warnings are errors by default** (`CONFIG_COMPILER_DISABLE_DEFAULT_ERRORS` to opt out) | Matches our `-Werror` policy (doc 07 §4) | Keep default. Never set the opt-out |
| **Default C standard is gnu23** (C++: gnu++26) | Our core is written as C11. IDF and its headers are compiled as gnu23 | `ctp_core` must compile warning-free as **both** `-std=c11` (host, strict) and IDF's gnu23. CI builds the host tests in both modes. Don't override `-std` inside IDF components. Avoid identifiers that are keywords in C23 (`bool`, `true`, `false`, `nullptr`, `typeof`, `static_assert`, `alignas`, …); use `<stdbool.h>` and `_Static_assert` only through a `CTP_STATIC_ASSERT` macro |
| **Default libc is Picolibc** (Newlib still selectable) | Core has no stdio. Ports use `snprintf` only for EVT_LOG text | Keep Picolibc. Nothing relies on per-task stdin/stdout/stderr or `struct _reent` |
| `COMPILER_ASSERT_NDEBUG_EVALUATE` default now `n` | Expressions inside `assert()` are no longer evaluated under NDEBUG | We never use `assert()`; `CTP_ASSERT` is our own and always on (doc 07 §4 rule 9). Add a CI grep banning `assert(` in our code |
| **Driver public headers no longer include FreeRTOS headers** | The `usb_serial_jtag` API uses `TickType_t` | Port files include `freertos/FreeRTOS.h` explicitly before driver headers |
| **FreeRTOS functions placed in flash by default** (`CONFIG_FREERTOS_IN_IRAM` restores IRAM) | Fine for us: we never write flash at runtime, so the cache is never disabled and no IRAM-safety concerns arise. Possible small performance cost in the hot path | Default first. M6 measures callback/link-task timing with and without `CONFIG_FREERTOS_IN_IRAM` |
| Removed FreeRTOS compat functions (`vTaskDelayUntil`, `xQueueGenericReceive`, function forms of `ulTaskNotifyTake`/`xTaskNotifyWait`, `xTaskGetAffinity`, …) | Easy to type from habit | Use `xTaskDelayUntil`, `xQueueReceive`, the macro forms of task notifications |
| Removed `esp_log_buffer_hex()` / `esp_log_buffer_char()` | Only relevant for debug logs | Use `ESP_LOG_BUFFER_HEX*` macros (debug builds only) |
| Build: min **CMake 3.22**, **Python 3.10**; orphan linker sections can fail the link; non-priority constructor order reversed | Native installs must meet the minimums (v6.0.2 EIM installs do). We don't use constructors | Makefile `doctor` target checks cmake/python versions |
| Changed tracking of **default values in sdkconfig** | Committed `sdkconfig` files can drift between IDF versions | Commit **only `sdkconfig.defaults`** (plus `sdkconfig.ci` if needed). The generated `sdkconfig` is git-ignored; CI regenerates it and checks that `idf.py save-defconfig` produces no diff |
| `esp_rom_uart.h` deprecated → `esp_rom_serial_output.h`; `usb` (host) component removed; legacy drivers removed | Not used | None |
| PSA Crypto replaces legacy mbedTLS primitives | Not used (no TLS; build ID comes from the app descriptor) | None |

## 2. Wi-Fi APIs used

| API / type | v6 status | Notes / action |
|---|---|---|
| `esp_wifi_init(&cfg)` with `WIFI_INIT_CONFIG_DEFAULT()` | **Behaviour change:** calling it again when already initialised returns `ESP_ERR_INVALID_STATE` instead of `ESP_OK` | Init exactly once in `app_main`. Never re-init on reconfiguration |
| `wifi_init_config_t.nvs_enable = 0`, `esp_wifi_set_storage(WIFI_STORAGE_RAM)` | Unchanged | Zero-flash-write policy (doc 07 §4). Also set `CONFIG_ESP_WIFI_NVS_ENABLED=n` in `sdkconfig.defaults` |
| `esp_wifi_set_mode(WIFI_MODE_NULL)`, `esp_wifi_start()` | Unchanged | — |
| `esp_wifi_set_band_mode(WIFI_BAND_MODE_2G_ONLY / 5G_ONLY)` | Present in v6 | Must be called after `esp_wifi_start()`. **Never use `WIFI_BAND_MODE_AUTO`**: `esp_wifi_set_bandwidth` returns `ESP_ERR_NOT_SUPPORTED` in AUTO mode |
| `esp_wifi_set_channel(primary, second)` | Unchanged | On 5 GHz the secondary channel is derived automatically |
| `esp_wifi_set_bandwidth(ifx, bw)` / `esp_wifi_set_bandwidths` | **Enum rename:** `WIFI_BW_HT20` / `WIFI_BW_HT40` removed → use **`WIFI_BW20` / `WIFI_BW40`** | It takes an **interface** (`WIFI_IF_STA`). **Answered in M3 (v6.0.2, XIAO C5):** in `WIFI_MODE_NULL`, `esp_wifi_set_bandwidth(WIFI_IF_STA, WIFI_BW40)`, `esp_wifi_set_protocols` and `esp_wifi_set_bandwidths` all return `ESP_ERR_INVALID_ARG`. v1 therefore stays in NULL mode and does **not** advertise `caps.HT40` (HT40 requests → `ERR_NOT_SUPPORTED`). Enabling HT40 would need idle `WIFI_MODE_STA` plus 11n-only protocols (losing HE/VHT decoding while in HT40): a product decision, not taken |
| `wifi_interface_t`, `WIFI_IF_STA` | `esp_interface.h` removed; the enum lives in `esp_wifi_types_generic.h`; `ESP_IF_WIFI_STA` macros removed | Use `WIFI_IF_STA` only |
| `esp_wifi_set_country_code(cc, ieee80211d_enabled)` | Present; supported list includes "01", FI, US, JP, …; doc still says "stored into flash" | Call with `ieee80211d_enabled = false`. Storage must be RAM-only with Wi-Fi NVS disabled. The M4 gate checks the flash-write counter stays 0 |
| `esp_wifi_set_country(const wifi_country_t*)` (legacy, manual `schan/nchan`) | Discouraged since v5. **Presence in the v6.0.2 headers not confirmed from the docs** | Covered by the API smoke build (§5). If removed: the "manual range" mode (doc 02 §5, doc 04 §5) is dropped from v1 and the UI hides it, controlled by a capability bit (`caps.bit2 MANUAL_COUNTRY`), so no protocol change is needed |
| `esp_wifi_set_promiscuous`, `_rx_cb`, `_filter`, `_ctrl_filter`, `wifi_promiscuous_filter_t`, `WIFI_PROMIS_FILTER_MASK_*`, `WIFI_PROMIS_CTRL_FILTER_MASK_*` | No breaking changes listed | Driver type classification may be wrong on C5 (doc 02 §1); we filter on raw bytes |
| `wifi_promiscuous_pkt_t` → `wifi_pkt_rx_ctrl_t` **(C5 layout)** | Chip-specific layout (`esp_wifi_rxctrl_t`, `SOC_WIFI_MAC_VERSION_NUM == 3`, checked in v6.0.2 `esp_wifi_he_types.h`): `rssi`, `rate` (11b rate code, else L-SIG rate field), `sig_len` (incl. FCS), `rx_state`, `dump_len` (excl. FCS), `sigb_len`, `cur_bb_format`, `channel`, `second`, `he_siga1` (HT-SIG/VHT-SIG/HE-SIGA1, source of MCS), `noise_floor`, `is_group`, `rxend_state`, `rxmatch0..3`, `timestamp` | `rxmatch0..3` = "frame is from interface N" (not useful as a pre-filter in NULL mode). The `timestamp` is precise only while modem/light sleep is off → call `esp_wifi_set_ps(WIFI_PS_NONE)`. Read fields **only by name** (bitfields; never `memcpy` the struct). PKT `rate`/`sig_mode` remapping per doc 08 A1 |
| `esp_wifi_set_ps(WIFI_PS_NONE)` | Unchanged | Required for timestamp accuracy and no RX gaps |
| Antenna APIs `esp_wifi_set_ant*` | Removed → `esp_phy_set_ant*` | Not used (XIAO has one antenna port) |

## 3. PHY, USB, system APIs used

| API / option | v6 status | Notes / action |
|---|---|---|
| PHY calibration storage (`CONFIG_ESP_PHY_CALIBRATION_AND_DATA_STORAGE`, `CONFIG_ESP_PHY_CALIBRATION_MODE`) | No breaking change listed | Disable storage (full calibration at every boot; zero flash writes). The smoke build checks the option names exist; M4 measures boot time |
| `driver/usb_serial_jtag.h` (component `esp_driver_usb_serial_jtag`): `usb_serial_jtag_driver_install(&cfg{tx_buffer_size, rx_buffer_size})`, `usb_serial_jtag_read_bytes(buf, len, ticks)`, `usb_serial_jtag_write_bytes(buf, len, ticks)`, `usb_serial_jtag_is_connected()` | API unchanged. Its VFS helpers moved out of the `vfs` component (we don't use VFS) | `PRIV_REQUIRES esp_driver_usb_serial_jtag`. Include FreeRTOS headers explicitly (§1). Console: `CONFIG_ESP_CONSOLE_NONE=y`, no secondary console, bootloader log level none |
| **USB pins and PHY** | Hardware rule (Espressif download guidelines): if the app **disables the USB PHY or reuses GPIO13/14**, USB auto-download stops working. Then only the BOOT strapping pin (GPIO28 = XIAO BOOT button) gets into download mode | The firmware must **never** touch GPIO13/14 or the USB PHY. Enforced by a CI grep in ports and a conformance test (`REBOOT 1` then ROM SYNC) |
| Download mode from software (`REBOOT mode 1`) | No public IDF API. **Verified in M3:** `REG_SET_FIELD(LP_AON_SYS_CFG_REG, LP_AON_FORCE_DOWNLOAD_BOOT, 1)` + `esp_restart()` boots the ROM in download mode (UART/USB-Serial-JTAG); the bit does not survive the following watchdog reset (3/3 cycles) | **`REBOOT 1` is now the preferred path.** M3 findings on Linux + esptool 5.6.0: the host DTR/RTS reset-into-bootloader sequence wedged the chip 4 times (ROM did not drain USB, needed a replug), and esptool's RTS "hard reset" did not leave ROM. Leave ROM with `--after watchdog-reset`. `firmware/tools/flash.sh` (make flash) uses REBOOT 1 → `--before no-reset` → watchdog reset; DTR/RTS only as last resort, BOOT button as manual recovery. Hosts must re-find the port after re-enumeration (`/dev/ttyACM0` ↔ `ACM1`). Further M3 rules for any ROM client (Go `romflash`, JS flasher): (1) never change DTR/RTS (keep the kernel's open state DTR=1/RTS=1, which also does not reset the app); actively lowering RTS while the ROM runs wedged it; (2) never send non-SLIP data (e.g. CTP frames) to the ROM: probe with SLIP SYNC first, CTP second; (3) skip FLASH_END: the ROM rejects it (status 1/0x06) once SPI_FLASH_MD5 ran after the last FLASH_DATA; leave with the watchdog reset. With these rules `ctpbench fw flash` survived repeated cycles and an interrupted write + retry without replugs |
| Bootloader / partitions | Bootloader linker file renamed (`bootloader.ld.in`); no-optimisation bootloader option removed; **C5 bootloader at flash offset `0x2000`** (esptool boot docs) | We use the stock bootloader. Manifest offsets come from `flasher_args.json` (doc 04 §11.1) |
| `esp_app_get_description()` → `esp_app_desc_t.version`, `.idf_ver`, `.app_elf_sha256` | No breaking change listed | Source of `fw_version` (via `PROJECT_VER`) and `fw_build_id` |
| `esp_timer_get_time()`, `esp_restart()`, `esp_reset_reason()` | Unchanged | — |
| `RTC_NOINIT_ATTR` | v6 removed RTC_ memory attributes on chips without RTC memory. C5 has 16 KB LP SRAM | For the assert record (doc 07 §4 rule 9) use **`__NOINIT_ATTR`** (HP RAM, survives software reset) unless the smoke build confirms `RTC_NOINIT_ATTR` on C5. Losing it on power loss is acceptable |
| `esp_rom_crc32_le()` (`esp_rom_crc.h`) | No change listed | Optional fast path for CRC-32; the portable table version is the reference (must give identical results; tested) |
| Task WDT (`esp_task_wdt_add/reset`), `xTaskCreateStatic`, `xQueueCreateStatic`, `uxTaskGetStackHighWaterMark` | Unchanged | All tasks and queues are static (no heap after init) |
| Logging | `esp_log_buffer_*` removed | Runtime logs go to EVT_LOG, not ESP_LOG (the console is off) |

## 4. Kconfig baseline (`firmware/sdkconfig.defaults`, to be validated by the smoke build)
```
CONFIG_IDF_TARGET="esp32c5"
CONFIG_ESP_CONSOLE_NONE=y
CONFIG_BOOTLOADER_LOG_LEVEL_NONE=y
CONFIG_LOG_DEFAULT_LEVEL_NONE=y            # debug builds override
CONFIG_ESP_WIFI_NVS_ENABLED=n
CONFIG_ESP_PHY_CALIBRATION_AND_DATA_STORAGE=n
CONFIG_BT_ENABLED=n
CONFIG_IEEE802154_ENABLED=n
CONFIG_ESP_TASK_WDT_EN=y
CONFIG_ESP_BROWNOUT_DET=y
CONFIG_COMPILER_OPTIMIZATION_PERF=y
CONFIG_APP_REPRODUCIBLE_BUILD=y
CONFIG_SPIRAM=n                            # not needed; keeps boot simple (enable later only with a reason)
```
*M0 finding (v6.0.2 build):* `CONFIG_APP_BUILD_MINIMIZE_BINARY_CHANGES` is an unknown symbol for esp32c5 in v6.0.2, so it was removed from `sdkconfig.defaults` (docs/09 §3.2 differential flashing relies on sector MD5s only).

Option names that changed between versions make `idf.py` warn about unknown symbols. The smoke build treats that warning as an error.

## 5. API smoke build (M0 gate item)

`firmware/test_apps/idf_api_smoke/`, a tiny ESP-IDF app that **only compiles and links** (and optionally runs a 5-second hardware check). It references every symbol in §2–§3:
- Takes the address of every function we call (`(void)&esp_wifi_set_country;` …), so a removed symbol fails the link with a clear name.
- Reads every `wifi_pkt_rx_ctrl_t` field we use by name (`(void)pkt->rx_ctrl.cur_bb_format;` …), so renamed fields fail compilation.
- Uses every enum and macro we use (`WIFI_BW20`, `WIFI_BAND_MODE_5G_ONLY`, `WIFI_PROMIS_FILTER_MASK_ALL`, `WIFI_STORAGE_RAM`, `WIFI_PS_NONE`, …).
- Built with our `sdkconfig.defaults`, treating unknown-Kconfig warnings as errors.
- **On hardware** (first M3 session, ~5 s): NULL-mode init → band 5G → channel 36 → `esp_wifi_set_bandwidth(WIFI_IF_STA, WIFI_BW20)` → promiscuous on → count frames for 2 s → print the results of each call. This answers the NULL-mode bandwidth question (§2) immediately.

The **symbol list lives in one file** (`firmware/idf-api.txt`) and is the input for both the smoke app (generated `smoke_symbols.c`) and this doc. Bumping the IDF pin = rebuild the smoke app first. A failure points at exactly what to re-audit.

## 6. Summary of decisions taken from this audit
1. Use `WIFI_BW20/WIFI_BW40`, explicit band modes, never AUTO.
2. Initialise Wi-Fi once; RAM-only storage; `WIFI_PS_NONE`.
3. Manual country range depends on the legacy API → gated by `caps.bit2` (doc 03 caps list updated).
4. Download mode is driven from the host (USB-JTAG reset sequence); the firmware never touches the USB pins or PHY.
5. Core is C11 and C23-clean; IDF code uses its gnu23 default; no `assert()`; FreeRTOS headers included explicitly.
6. Only `sdkconfig.defaults` is committed; generated sdkconfig is reproducible and checked in CI.
7. API smoke build + symbol list in M0; re-run on every IDF bump.

## Sources
- ESP-IDF v6.0 Wi-Fi migration guide: https://docs.espressif.com/projects/esp-idf/en/v6.0/esp32/migration-guides/release-6.x/6.0/wifi.html
- ESP-IDF v6.0 System migration guide (Picolibc, FreeRTOS): https://docs.espressif.com/projects/esp-idf/en/latest/esp32c5/migration-guides/release-6.x/6.0/system.html
- ESP-IDF v6.0 Peripherals migration guide: https://docs.espressif.com/projects/esp-idf/en/v6.0/esp32/migration-guides/release-6.x/6.0/peripherals.html
- v6.0-beta1 / beta2 release notes (breaking-change lists): https://newreleases.io/project/github/espressif/esp-idf/release/v6.0-beta1 , https://newreleases.io/project/github/espressif/esp-idf/release/v6.0-beta2
- Espressif v6.0 release blog: https://developer.espressif.com/blog/2026/03/idf-v6-0-release/
- `esp_wifi_set_bandwidth` / `esp_wifi_set_country_code` attention notes: https://sourcevu.sysprogs.com/espressif/esp-idf/symbols/esp_wifi_set_bandwidth , https://sourcevu.sysprogs.com/espressif/esp-idf/symbols/esp_wifi_set_country_code
- C5 rx_ctrl fields (esp-radio): https://docs.espressif.com/projects/rust/esp-radio/1.0.0-beta.0/esp32c5/esp_radio/wifi/struct.RxControlInfo.html ; `rxmatch` meaning: https://docs.espressif.com/projects/rust/esp-wifi/0.14.1/esp32c6/esp_wifi/wifi/struct.RxControlInfo.html
- USB auto-download disabled if USB PHY/pins are reused: https://docs.espressif.com/projects/esp-hardware-design-guidelines/zh_CN/latest/esp32c5/download-guidelines.html
- ESP32-C5 boot mode / bootloader at 0x2000, BOOT = GPIO28: https://docs.espressif.com/projects/esptool/en/latest/esp32c5/advanced-topics/boot-mode-selection.html
- ESP-IDF mirror (v6.0.3 / v6.1 availability): https://sourceforge.net/projects/esp-idf.mirror/files/v6.0
